package app

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Conversion is deliberately serialized: a NAS transcodes one file at a time
// rather than racing N ffmpeg processes against the disks the player reads from.
const (
	derivMaxAttempts  = 3
	derivBackoffBase  = 30 * time.Second
	derivPollInterval = 5 * time.Second
)

var errSourceChanged = errors.New("conversion source changed; waiting for a fresh scan")

type Converter struct {
	Config  Config
	DB      *sql.DB
	Log     *slog.Logger
	Convert func(ctx context.Context, src *os.File, strategy, target string) error
	wake    chan struct{}
}

func NewConverter(c Config, db *sql.DB, log *slog.Logger) *Converter {
	if log == nil {
		log = slog.Default()
	}
	cv := &Converter{Config: c, DB: db, Log: log, wake: make(chan struct{}, 1)}
	cv.Convert = cv.convert
	return cv
}

// The key versions the derivation recipe: any change to source bytes, strategy,
// or codec signature mints a new artifact and resets attempts.
func derivationKey(m MediaRecord, md Metadata, strategy string) string {
	return stableID("deriv-v1", fmt.Sprintf("%s:%d:%d:%s:%s:%s:%s", m.Path, m.Size, m.Modified, strategy, md.Container, md.VideoCodec, strings.Join(md.AudioCodecs, ",")))
}

// PlanFor records the playback decision for a freshly probed video. A row whose
// source key still matches is left alone unless it is waiting for a fresh probe.
func (c *Converter) PlanFor(ctx context.Context, m MediaRecord, md Metadata) error {
	if m.Kind != "video" {
		return nil
	}
	plan := playbackPlan(m.Format, md)
	strategy, status := plan.Strategy, "pending"
	if plan.Strategy == PlanDirect {
		strategy, status = "none", "ready"
	}
	if plan.Strategy == PlanUnsupported {
		strategy, status = "none", "unsupported"
	}
	key := derivationKey(m, md, strategy)
	_, err := c.DB.ExecContext(ctx, `INSERT INTO playback_derivations(media_id,source_key,strategy,status,artifact,reason,attempt,updated_ns) VALUES(?,?,?,?,'',?,0,?)
		ON CONFLICT(media_id) DO UPDATE SET source_key=excluded.source_key,strategy=excluded.strategy,status=excluded.status,artifact='',reason=excluded.reason,attempt=0,updated_ns=excluded.updated_ns
		WHERE playback_derivations.source_key<>excluded.source_key OR playback_derivations.status='waiting_source'`, m.ID, key, strategy, status, plan.Reason, time.Now().UnixNano())
	if err == nil && status == "pending" {
		c.nudge()
	}
	return err
}

func (c *Converter) Run(ctx context.Context) {
	// A crash or restart leaves jobs behind mid-conversion; none of them finished,
	// so every one is queued again rather than trusting a half-written artifact.
	for {
		if err := c.resetProcessing(ctx); err == nil {
			break
		} else {
			c.Log.Warn("derivation recovery failed", "error", err)
		}
		if !c.wait(ctx) {
			return
		}
	}
	c.cleanup(ctx)
	for {
		job, err := c.claim(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.Log.Warn("derivation claim failed", "error", err)
			if !c.wait(ctx) {
				return
			}
			continue
		}
		if job == nil {
			c.cleanup(ctx)
			if !c.wait(ctx) {
				return
			}
			continue
		}
		c.process(ctx, *job)
		c.cleanup(ctx)
	}
}

func (c *Converter) wait(ctx context.Context) bool {
	timer := time.NewTimer(derivPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-c.wake:
		return true
	case <-timer.C:
		return true
	}
}

func (c *Converter) nudge() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Converter) resetProcessing(ctx context.Context) error {
	_, err := c.DB.ExecContext(ctx, "UPDATE playback_derivations SET status='pending',updated_ns=? WHERE status='processing'", time.Now().UnixNano())
	return err
}

type derivJob struct {
	MediaID, SourceKey, Strategy string
	Attempt                      int
	Source                       MediaRecord
}

func (c *Converter) claim(ctx context.Context) (*derivJob, error) {
	now := time.Now().UnixNano()
	var j derivJob
	err := c.DB.QueryRowContext(ctx, `SELECT media_id,source_key,strategy,attempt FROM playback_derivations WHERE status='pending' AND ?-updated_ns>=attempt*? AND NOT EXISTS(SELECT 1 FROM media_skips k WHERE k.media_id=playback_derivations.media_id) ORDER BY updated_ns LIMIT 1`, now, int64(derivBackoffBase)).Scan(&j.MediaID, &j.SourceKey, &j.Strategy, &j.Attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.Source, err = scanMedia(c.DB.QueryRowContext(ctx, "SELECT "+mediaCols+" FROM media WHERE id=?", j.MediaID))
	if err != nil {
		return nil, err
	}
	res, err := c.DB.ExecContext(ctx, "UPDATE playback_derivations SET status='processing',updated_ns=? WHERE media_id=? AND source_key=? AND status='pending'", now, j.MediaID, j.SourceKey)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	return &j, nil
}

func (c *Converter) process(ctx context.Context, j derivJob) {
	started := time.Now()
	err := c.runJob(ctx, j)
	if err == nil {
		artifact := j.SourceKey + ".mp4"
		if _, err = c.DB.ExecContext(ctx, "UPDATE playback_derivations SET status='ready',artifact=?,reason='',updated_ns=? WHERE media_id=? AND source_key=? AND status='processing'", artifact, time.Now().UnixNano(), j.MediaID, j.SourceKey); err != nil {
			c.Log.Warn("derivation publish failed", "item", j.MediaID, "error", err)
		}
		if err != nil {
			c.resetProcessing(ctx)
			return
		}
		c.Log.Info("derivation ready", "item", j.MediaID, "strategy", j.Strategy, "elapsed", time.Since(started))
		return
	}
	if ctx.Err() != nil {
		// Shutdown mid-job: leave the row at processing; the next boot requeues it.
		return
	}
	if errors.Is(err, errSourceChanged) {
		if _, dberr := c.DB.ExecContext(ctx, "UPDATE playback_derivations SET status='waiting_source',reason=?,updated_ns=? WHERE media_id=? AND source_key=? AND status='processing'", err.Error(), time.Now().UnixNano(), j.MediaID, j.SourceKey); dberr != nil {
			c.Log.Warn("derivation source wait update failed", "item", j.MediaID, "error", dberr)
		}
		c.Log.Info("derivation waiting for source", "item", j.MediaID, "elapsed", time.Since(started))
		return
	}
	attempt := j.Attempt + 1
	status := "pending"
	if attempt >= derivMaxAttempts {
		status = "failed"
	}
	reason := err.Error()
	if len(reason) > 500 {
		reason = reason[:500]
	}
	if _, dberr := c.DB.ExecContext(ctx, "UPDATE playback_derivations SET status=?,attempt=?,reason=?,updated_ns=? WHERE media_id=? AND source_key=? AND status='processing'", status, attempt, reason, time.Now().UnixNano(), j.MediaID, j.SourceKey); dberr != nil {
		c.Log.Warn("derivation failure update failed", "item", j.MediaID, "error", dberr)
	}
	c.Log.Warn("derivation failed", "item", j.MediaID, "strategy", j.Strategy, "attempt", attempt, "elapsed", time.Since(started), "error", err)
}

func (c *Converter) runJob(ctx context.Context, j derivJob) error {
	m := j.Source
	root, err := openMediaRoot(c.Config)
	if err != nil {
		return err
	}
	defer root.Close()
	src, err := root.Open(m.Path)
	if err != nil {
		return err
	}
	defer src.Close()
	before, err := src.Stat()
	if err != nil {
		return err
	}
	current, err := root.Stat(m.Path)
	if err != nil || !before.Mode().IsRegular() || !os.SameFile(before, current) || before.Size() != m.Size || before.ModTime().UnixNano() != m.Modified || current.Size() != m.Size || current.ModTime().UnixNano() != m.Modified {
		return errSourceChanged
	}
	dir := filepath.Join(c.Config.DataDir, "derivations")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".deriv-*.mp4")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	convertErr := c.Convert(ctx, src, j.Strategy, tmpPath)
	after, err := src.Stat()
	if err != nil {
		return err
	}
	current, err = root.Stat(m.Path)
	if err != nil || !os.SameFile(before, current) || after.Size() != before.Size() || after.ModTime() != before.ModTime() || current.Size() != before.Size() || current.ModTime() != before.ModTime() {
		return errSourceChanged
	}
	if convertErr != nil {
		return convertErr
	}
	st, err := os.Stat(tmpPath)
	if err != nil || st.Size() == 0 {
		return errors.New("conversion produced no output")
	}
	return os.Rename(tmpPath, filepath.Join(dir, j.SourceKey+".mp4"))
}

// cleanup removes only this feature's files: temp files from crashed conversions
// and MP4 artifacts no live row references. Thumbnails and sprites are untouched.
func (c *Converter) cleanup(ctx context.Context) {
	dir := filepath.Join(c.Config.DataDir, "derivations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	rows, err := c.DB.QueryContext(ctx, "SELECT source_key,artifact FROM playback_derivations WHERE status IN ('pending','processing','ready')")
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for rows.Next() {
		var key, artifact string
		if err := rows.Scan(&key, &artifact); err != nil {
			rows.Close()
			return
		}
		keep[key+".mp4"] = true
		keep[artifact] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		_, keyErr := hex.DecodeString(strings.TrimSuffix(name, ".mp4"))
		owned := strings.HasSuffix(name, ".mp4") && len(name) == 36 && keyErr == nil
		if strings.HasPrefix(name, ".deriv-") || (owned && !keep[name]) {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

func (c *Converter) convert(ctx context.Context, src *os.File, strategy, target string) error {
	if _, err := src.Seek(0, 0); err != nil {
		return err
	}
	convCtx, cancel := context.WithTimeout(ctx, c.Config.ConvertTimeout)
	defer cancel()
	cmd := exec.CommandContext(convCtx, "ffmpeg", conversionArgs(strategy, target)...)
	cmd.ExtraFiles = []*os.File{src}
	stderr := &limitedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if convCtx.Err() != nil {
			return fmt.Errorf("convert: %w", convCtx.Err())
		}
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 2048 {
			detail = detail[:2048]
		}
		return fmt.Errorf("convert: %w: %s", err, detail)
	}
	st, err := os.Stat(target)
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		return errors.New("convert: ffmpeg produced no output")
	}
	// A green ffmpeg exit is not proof of a playable file; probe the result and
	// require exactly the format the player contract promises.
	out, err := os.Open(target)
	if err != nil {
		return err
	}
	defer out.Close()
	md, err := probeMedia(ctx, out, c.Config.ToolTimeout)
	if err != nil {
		return fmt.Errorf("convert: output failed validation: %w", err)
	}
	if md.VideoCodec != "h264" || !mp4Audio(md.AudioCodecs) || !strings.Contains(md.Container, "mp4") {
		return fmt.Errorf("convert: unexpected output %s/%s", md.Container, md.VideoCodec)
	}
	return nil
}
