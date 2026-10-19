package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type MediaRecord struct {
	ID, Path, FolderID, Name, Kind, MIME, Format string
	Size, Modified                               int64
	Width, Height                                int
	Duration                                     float64
	Thumbnail                                    string
}
type FolderRecord struct{ ID, Path, Name, ParentID string }
type Metadata struct {
	Width, Height int
	Duration      float64
	Container     string
	VideoCodec    string
	AudioCodecs   []string
}
type Scanner struct {
	Config   Config
	DB       *sql.DB
	Log      *slog.Logger
	running  atomic.Bool
	deferred atomic.Bool
	Process  func(context.Context, *os.File, string, string) (Metadata, error)
	Sprite   func(context.Context, *os.File, float64, string) error
	// Derive classifies a probed video and enqueues conversion when the browser
	// cannot play the original. Nil disables derivation (tests, one-shot scans).
	Derive func(context.Context, MediaRecord, Metadata) error
}

var ErrScanRunning = errors.New("scan already running")

func NewScanner(c Config, db *sql.DB, log *slog.Logger) *Scanner {
	if log == nil {
		log = slog.Default()
	}
	s := &Scanner{Config: c, DB: db, Log: log}
	s.Process = s.process
	s.Sprite = s.sprite
	return s
}
func stableID(prefix, p string) string {
	h := sha256.Sum256([]byte(prefix + ":" + p))
	return hex.EncodeToString(h[:16])
}
func openMediaRoot(c Config) (*os.Root, error) {
	root, e := os.OpenRoot(c.MediaRoot)
	if e != nil {
		return nil, e
	}
	if c.MediaMarker != "" {
		f, e := root.Open(c.MediaMarker)
		if e != nil {
			root.Close()
			return nil, e
		}
		st, e := f.Stat()
		f.Close()
		if e != nil || !st.Mode().IsRegular() {
			root.Close()
			return nil, errors.New("media marker is not a readable file")
		}
	}
	return root, nil
}
func (s *Scanner) Run(ctx context.Context) {
	s.runOnce(ctx)
	timer := time.NewTimer(s.nextDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.runOnce(ctx)
			timer.Reset(s.nextDelay())
		}
	}
}

// Files that were mid-copy are skipped until they settle. Retrying on the normal
// interval would leave a freshly installed library empty for up to that whole
// interval, so a deferred file pulls the next scan forward.
func (s *Scanner) nextDelay() time.Duration {
	delay := s.Config.ScanInterval
	if s.deferred.Load() {
		settle := max(s.Config.ScanSettle+2*time.Second, 5*time.Second)
		delay = min(delay, settle)
	}
	var next sql.NullInt64
	if err := s.DB.QueryRow("SELECT MIN(next_ns) FROM media_retries WHERE next_ns>0").Scan(&next); err != nil {
		s.Log.Warn("retry schedule unavailable", "error", err)
	} else if next.Valid {
		delay = min(delay, max(time.Until(time.Unix(0, next.Int64)), 5*time.Second))
	}
	return delay
}
func (s *Scanner) runOnce(ctx context.Context) {
	if e := s.Scan(ctx); e != nil && !errors.Is(e, context.Canceled) {
		s.Log.Warn("media scan incomplete; catalogue retained", "error", e)
	}
}
func mediaType(p string) (kind, mime, format string) {
	ext := strings.ToLower(filepath.Ext(p))
	switch ext {
	case ".jpg", ".jpeg":
		return "image", "image/jpeg", "JPG"
	case ".png":
		return "image", "image/png", "PNG"
	case ".gif":
		return "image", "image/gif", "GIF"
	case ".webp":
		return "image", "image/webp", "WEBP"
	case ".avif":
		return "image", "image/avif", "AVIF"
	case ".heic", ".heif":
		return "image", "image/heic", "HEIC"
	case ".mp4", ".m4v":
		return "video", "video/mp4", "MP4"
	case ".webm":
		return "video", "video/webm", "WEBM"
	case ".mkv":
		return "video", "video/x-matroska", "MKV"
	case ".mov":
		return "video", "video/quicktime", "MOV"
	case ".avi":
		return "video", "video/x-msvideo", "AVI"
	case ".mpeg", ".mpg":
		return "video", "video/mpeg", "MPEG"
	}
	return "", "", ""
}
func scanMedia(row interface{ Scan(...any) error }) (MediaRecord, error) {
	var m MediaRecord
	e := row.Scan(&m.ID, &m.Path, &m.FolderID, &m.Name, &m.Kind, &m.MIME, &m.Format, &m.Size, &m.Modified, &m.Width, &m.Height, &m.Duration, &m.Thumbnail)
	return m, e
}

const mediaCols = "id,path,folder_id,name,kind,mime,format,size_bytes,modified_ns,width,height,duration,thumbnail"

type mediaRetry struct {
	Stage, Reason string
	Attempt       int
	Next          int64
}

type retryExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func mediaSourceKey(m MediaRecord) string {
	return stableID("source", fmt.Sprintf("%s:%d:%d", m.Path, m.Size, m.Modified))
}

func retryDelay(attempt int) time.Duration {
	delays := [...]time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour}
	return delays[min(max(attempt-1, 0), len(delays)-1)]
}

func retryReason(err error) string {
	reason := []rune(err.Error())
	return string(reason[:min(len(reason), 4096)])
}

func saveRetry(ctx context.Context, db retryExecer, m MediaRecord, r mediaRetry) error {
	_, err := db.ExecContext(ctx, `INSERT INTO media_retries(media_id,stage,source_key,attempt,next_ns,reason) VALUES(?,?,?,?,?,?)
		ON CONFLICT(media_id,stage) DO UPDATE SET source_key=excluded.source_key,attempt=excluded.attempt,next_ns=excluded.next_ns,reason=excluded.reason`, m.ID, r.Stage, mediaSourceKey(m), r.Attempt, r.Next, r.Reason)
	return err
}

func (s *Scanner) skippedMedia(ctx context.Context) (map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT media_id FROM media_skips")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	skipped := map[string]bool{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		skipped[id] = true
	}
	return skipped, rows.Err()
}

func (s *Scanner) retrySprite(ctx context.Context, root *os.Root) (bool, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+prefixedMediaCols()+",r.source_key,r.attempt FROM media_retries r JOIN media m ON m.id=r.media_id WHERE r.stage='sprite' AND r.next_ns>0 AND r.next_ns<=? AND NOT EXISTS(SELECT 1 FROM media_skips k WHERE k.media_id=m.id) ORDER BY r.next_ns,m.id", time.Now().UnixNano())
	if err != nil {
		return false, err
	}
	type job struct {
		m       MediaRecord
		key     string
		attempt int
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err = rows.Scan(&j.m.ID, &j.m.Path, &j.m.FolderID, &j.m.Name, &j.m.Kind, &j.m.MIME, &j.m.Format, &j.m.Size, &j.m.Modified, &j.m.Width, &j.m.Height, &j.m.Duration, &j.m.Thumbnail, &j.key, &j.attempt); err != nil {
			rows.Close()
			return false, err
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	deferred := false
	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			return deferred, err
		}
		m := j.m
		if j.key != mediaSourceKey(m) {
			continue
		}
		if s.spriteExists(m.Path, m.Size, m.Modified) {
			_, err = s.DB.ExecContext(ctx, "DELETE FROM media_retries WHERE media_id=? AND stage='sprite' AND source_key=?", m.ID, j.key)
			if err != nil {
				return deferred, err
			}
			continue
		}
		f, openErr := root.Open(m.Path)
		var before os.FileInfo
		if openErr == nil {
			before, openErr = f.Stat()
		}
		if openErr == nil && (before.Size() != m.Size || before.ModTime().UnixNano() != m.Modified || time.Since(before.ModTime()) < s.Config.ScanSettle) {
			f.Close()
			deferred = true
			continue
		}
		started := time.Now()
		target := s.spriteTarget(m.Path, m.Size, m.Modified)
		spriteErr := openErr
		if openErr == nil {
			spriteErr = s.Sprite(ctx, f, m.Duration, target)
			after, statErr := f.Stat()
			current, currentErr := root.Stat(m.Path)
			f.Close()
			if ctx.Err() != nil {
				return deferred, ctx.Err()
			}
			if statErr != nil || currentErr != nil || !os.SameFile(before, current) || after.Size() != before.Size() || after.ModTime() != before.ModTime() || current.Size() != before.Size() || current.ModTime() != before.ModTime() {
				if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return true, removeErr
				}
				return true, nil
			}
		} else if f != nil {
			f.Close()
		}
		if ctx.Err() != nil {
			return deferred, ctx.Err()
		}
		if spriteErr == nil {
			_, err = s.DB.ExecContext(ctx, "DELETE FROM media_retries WHERE media_id=? AND stage='sprite' AND source_key=?", m.ID, j.key)
			return deferred, err
		}
		r := mediaRetry{Stage: "sprite", Attempt: j.attempt + 1, Reason: retryReason(spriteErr)}
		if r.Attempt < 3 {
			r.Next = time.Now().Add(retryDelay(r.Attempt)).UnixNano()
		}
		err = saveRetry(ctx, s.DB, m, r)
		s.Log.Warn("sprite generation failed", "item", m.ID, "path", m.Path, "size_bytes", m.Size, "duration_seconds", m.Duration, "elapsed", time.Since(started), "timeout", s.Config.SpriteTimeout, "attempt", r.Attempt, "retry_scheduled", r.Next > 0, "next_retry", r.Next, "error", spriteErr)
		return deferred, err
	}
	return deferred, nil
}

func (s *Scanner) Scan(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return ErrScanRunning
	}
	defer s.running.Store(false)
	root, e := openMediaRoot(s.Config)
	if e != nil {
		return e
	}
	defer root.Close()
	skipped, e := s.skippedMedia(ctx)
	if e != nil {
		return e
	}
	records := map[string]MediaRecord{}
	probed := []struct {
		m  MediaRecord
		md Metadata
	}{}
	retries := map[string]mediaRetry{}
	attempts := map[string]int{}
	folders := map[string]FolderRecord{}
	seen := map[string]bool{}
	walkFailed := false
	deferred := false
	defer func() { s.deferred.Store(deferred) }()
	e = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, walkErr error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if walkErr != nil {
			walkFailed = true
			s.Log.Warn("unable to inspect media directory", "error", walkErr)
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() {
			if p != "." {
				parent := ""
				if path.Dir(p) != "." {
					parent = stableID("folder", path.Dir(p))
				}
				folders[p] = FolderRecord{stableID("folder", p), p, d.Name(), parent}
			}
			return nil
		}
		// A skipped file stays in the catalogue so an administrator can unskip it,
		// but it is neither probed nor reconciled until then.
		if skipped[stableID("media", p)] {
			seen[p] = true
			return nil
		}
		kind, mime, format := mediaType(p)
		if kind == "" {
			return nil
		}
		seen[p] = true
		st, err := d.Info()
		if err != nil {
			walkFailed = true
			return nil
		}
		if !st.Mode().IsRegular() {
			return nil
		}
		existing, err := scanMedia(s.DB.QueryRowContext(ctx, "SELECT "+mediaCols+" FROM media WHERE path=?", p))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		unchanged := err == nil && existing.Size == st.Size() && existing.Modified == st.ModTime().UnixNano()
		attempt := 0
		if unchanged {
			var next int64
			var key string
			err = s.DB.QueryRowContext(ctx, "SELECT source_key,attempt,next_ns FROM media_retries WHERE media_id=? AND stage='probe'", existing.ID).Scan(&key, &attempt, &next)
			if errors.Is(err, sql.ErrNoRows) {
				var waiting bool
				if err = s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM playback_derivations WHERE media_id=? AND status='waiting_source')", existing.ID).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return nil
				}
			} else {
				if err != nil {
					return err
				}
				if key != mediaSourceKey(existing) || next <= 0 || next > time.Now().UnixNano() {
					return nil
				}
			}
		}
		if time.Since(st.ModTime()) < s.Config.ScanSettle {
			deferred = true
			return nil
		}
		f, err := root.Open(p)
		if err != nil {
			walkFailed = true
			return nil
		}
		before, err := f.Stat()
		if err != nil {
			f.Close()
			walkFailed = true
			return nil
		}
		id := stableID("media", p)
		if before.Size() != st.Size() || before.ModTime() != st.ModTime() || time.Since(before.ModTime()) < s.Config.ScanSettle {
			f.Close()
			deferred = true
			return nil
		}
		fingerprint := fmt.Sprintf("%s:%d:%d", p, before.Size(), before.ModTime().UnixNano())
		thumbKey := stableID("thumb", fingerprint) + ".jpg"
		attempts[p] = attempt
		md, probeErr := s.Process(ctx, f, kind, filepath.Join(s.Config.DataDir, "thumbs", thumbKey))
		after, statErr := f.Stat()
		f.Close()
		current, currentErr := root.Stat(p)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if statErr != nil || currentErr != nil {
			walkFailed = true
			return nil
		}
		if !os.SameFile(before, current) || before.Size() != after.Size() || before.ModTime() != after.ModTime() || after.Size() != current.Size() || after.ModTime() != current.ModTime() {
			deferred = true
			return nil
		}
		usable := probeErr == nil || errors.Is(probeErr, errNoVisualStream)
		if errors.Is(probeErr, errNoVisualStream) {
			thumbKey = ""
		}
		if !usable {
			md = Metadata{}
			thumbKey = ""
			r := mediaRetry{Stage: "probe", Attempt: attempt + 1, Next: time.Now().Add(retryDelay(attempt + 1)).UnixNano(), Reason: retryReason(probeErr)}
			retries[p] = r
			s.Log.Warn("media metadata unavailable", "item", id, "path", p, "attempt", r.Attempt, "next_retry", time.Unix(0, r.Next), "error", probeErr)
		}
		folderID := ""
		if path.Dir(p) != "." {
			folderID = stableID("folder", path.Dir(p))
		}
		records[p] = MediaRecord{id, p, folderID, path.Base(p), kind, mime, format, after.Size(), after.ModTime().UnixNano(), md.Width, md.Height, md.Duration, thumbKey}
		if usable && kind == "video" && s.Derive != nil {
			probed = append(probed, struct {
				m  MediaRecord
				md Metadata
			}{records[p], md})
		}
		return nil
	})
	if e != nil {
		return e
	}
	// Never reconcile a stale mount after it has been replaced during a scan.
	current, e := openMediaRoot(s.Config)
	if e != nil {
		return e
	}
	defer current.Close()
	oldInfo, e := root.Stat(".")
	if e != nil {
		return e
	}
	newInfo, e := current.Stat(".")
	if e != nil {
		return e
	}
	if !os.SameFile(oldInfo, newInfo) {
		return errors.New("media root changed during scan")
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, f := range folders {
		if _, e = tx.ExecContext(ctx, "INSERT INTO folders VALUES(?,?,?,?) ON CONFLICT(path) DO UPDATE SET name=excluded.name,parent_id=excluded.parent_id", f.ID, f.Path, f.Name, f.ParentID); e != nil {
			return e
		}
	}
	for _, m := range records {
		if _, e = tx.ExecContext(ctx, "DELETE FROM media_retries WHERE media_id=? AND source_key<>?", m.ID, mediaSourceKey(m)); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "DELETE FROM playback_derivations WHERE media_id=? AND (status='waiting_source' OR EXISTS(SELECT 1 FROM media WHERE id=? AND (size_bytes<>? OR modified_ns<>?)))", m.ID, m.ID, m.Size, m.Modified); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO media VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(path) DO UPDATE SET folder_id=excluded.folder_id,name=excluded.name,kind=excluded.kind,mime=excluded.mime,format=excluded.format,size_bytes=excluded.size_bytes,modified_ns=excluded.modified_ns,width=excluded.width,height=excluded.height,duration=excluded.duration,thumbnail=excluded.thumbnail`, m.ID, m.Path, m.FolderID, m.Name, m.Kind, m.MIME, m.Format, m.Size, m.Modified, m.Width, m.Height, m.Duration, m.Thumbnail); e != nil {
			return e
		}
		if r, failed := retries[m.Path]; failed {
			if e = saveRetry(ctx, tx, m, r); e != nil {
				return e
			}
		} else {
			if _, e = tx.ExecContext(ctx, "DELETE FROM media_retries WHERE media_id=? AND stage='probe'", m.ID); e != nil {
				return e
			}
			if m.Kind == "video" && s.Derive != nil {
				if e = saveRetry(ctx, tx, m, mediaRetry{Stage: "probe", Next: time.Now().UnixNano()}); e != nil {
					return e
				}
			}
			if m.Kind == "video" && m.Width > 0 && m.Height > 0 && m.Duration > 0 && !s.spriteExists(m.Path, m.Size, m.Modified) {
				if _, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO media_retries(media_id,stage,source_key,attempt,next_ns) VALUES(?,'sprite',?,0,?)", m.ID, mediaSourceKey(m), time.Now().UnixNano()); e != nil {
					return e
				}
			}
		}
	}
	if !walkFailed {
		rows, err := tx.QueryContext(ctx, "SELECT id,path FROM media")
		if err != nil {
			return err
		}
		missing := []string{}
		for rows.Next() {
			var id, p string
			if err = rows.Scan(&id, &p); err != nil {
				rows.Close()
				return err
			}
			if !seen[p] {
				missing = append(missing, id)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range missing {
			if _, e = tx.ExecContext(ctx, "DELETE FROM media WHERE id=?", id); e != nil {
				return e
			}
		}
		rows, err = tx.QueryContext(ctx, "SELECT id,path FROM folders")
		if err != nil {
			return err
		}
		missing = nil
		for rows.Next() {
			var id, p string
			if err = rows.Scan(&id, &p); err != nil {
				rows.Close()
				return err
			}
			if _, ok := folders[p]; !ok {
				missing = append(missing, id)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, id := range missing {
			if _, e = tx.ExecContext(ctx, "DELETE FROM folders WHERE id=?", id); e != nil {
				return e
			}
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	for _, v := range probed {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.Derive(ctx, v.m, v.md); err != nil {
			attempt := attempts[v.m.Path] + 1
			r := mediaRetry{Stage: "probe", Attempt: attempt, Next: time.Now().Add(retryDelay(attempt)).UnixNano(), Reason: retryReason(err)}
			if e = saveRetry(ctx, s.DB, v.m, r); e != nil {
				return e
			}
			s.Log.Warn("derivation enqueue failed", "item", v.m.ID, "path", v.m.Path, "next_retry", time.Unix(0, r.Next), "error", err)
		} else if _, e = s.DB.ExecContext(ctx, "DELETE FROM media_retries WHERE media_id=? AND stage='probe' AND source_key=?", v.m.ID, mediaSourceKey(v.m)); e != nil {
			return e
		}
	}
	spriteDeferred, err := s.retrySprite(ctx, root)
	deferred = deferred || spriteDeferred
	if err != nil {
		return err
	}
	if walkFailed {
		return errors.New("some media paths were unreadable; missing entries were retained")
	}
	s.Log.Info("media scan completed", "found", len(seen), "indexed", len(records), "deferred_until_settled", deferred)
	return nil
}

var errNoVisualStream = errors.New("no visual stream")

func probeMedia(ctx context.Context, f *os.File, timeout time.Duration) (Metadata, error) {
	var md Metadata
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, "ffprobe", "-v", "error", "-protocol_whitelist", "file,pipe,fd", "-format_whitelist", safeDemuxers, "-threads", "1", "-print_format", "json", "-show_format", "-show_streams", "/proc/self/fd/3")
	cmd.ExtraFiles = []*os.File{f}
	var out limitedBuffer
	cmd.Stdout = &out
	stderr := &limitedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if probeCtx.Err() != nil {
			return md, fmt.Errorf("probe failed: %w", probeCtx.Err())
		}
		detail := []rune(stderr.String())
		return md, fmt.Errorf("probe failed: %w: %s", err, strings.TrimSpace(string(detail[:min(len(detail), 4096)])))
	}
	var result struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Duration  string `json:"duration"`
		} `json:"streams"`
		Format struct {
			FormatName string `json:"format_name"`
			Duration   string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return md, err
	}
	md.Container = result.Format.FormatName
	md.Duration = parseSeconds(result.Format.Duration)
	for _, v := range result.Streams {
		switch v.CodecType {
		case "video":
			if md.VideoCodec == "" {
				md.VideoCodec = v.CodecName
				md.Width = v.Width
				md.Height = v.Height
				if md.Duration == 0 {
					md.Duration = parseSeconds(v.Duration)
				}
			}
		case "audio":
			md.AudioCodecs = append(md.AudioCodecs, v.CodecName)
		}
	}
	visual := false
	for _, stream := range result.Streams {
		visual = visual || stream.CodecType == "video"
	}
	if len(result.Streams) > 0 && !visual {
		return md, errNoVisualStream
	}
	if md.VideoCodec == "" || md.Width <= 0 || md.Height <= 0 || md.Width > 65536 || md.Height > 65536 {
		return md, errors.New("visual metadata is incomplete or invalid")
	}
	return md, nil
}
func (s *Scanner) process(ctx context.Context, f *os.File, kind, target string) (md Metadata, err error) {
	md, err = probeMedia(ctx, f, s.Config.ToolTimeout)
	if err != nil {
		return md, err
	}
	defer func() {
		if err != nil && ctx.Err() == nil {
			s.Log.Warn("thumbnail generation failed", "error", err)
			err = nil
		}
	}()
	if kind == "image" {
		md.Duration = 0
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return md, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".thumb-*.jpg")
	if err != nil {
		return md, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	if _, err = f.Seek(0, 0); err != nil {
		return md, err
	}
	thumbCtx, stop := context.WithTimeout(ctx, s.Config.ToolTimeout)
	defer stop()
	args := []string{"-nostdin", "-v", "error", "-protocol_whitelist", "file,pipe,fd", "-format_whitelist", safeDemuxers, "-threads", "1", "-i", "/proc/self/fd/3", "-frames:v", "1", "-vf", "scale=480:-2", "-threads", "1", "-f", "image2", "-y", tmpPath}
	cmd := exec.CommandContext(thumbCtx, "ffmpeg", args...)
	cmd.ExtraFiles = []*os.File{f}
	cmd.Stderr = &limitedBuffer{}
	if err = cmd.Run(); err != nil {
		return md, err
	}
	if err = os.Rename(tmpPath, target); err != nil {
		return md, err
	}
	return md, nil
}

const safeDemuxers = "mov,matroska,webm,avi,mpeg,mpegts,image2,jpeg_pipe,png_pipe,webp_pipe,gif,avif,heif"

// Broken media can report absurd or non-finite durations, which would break JSON responses.
func parseSeconds(v string) float64 {
	d, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(d) || math.IsInf(d, 0) || d < 0 || d > 86400*30 {
		return 0
	}
	return d
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len() < 1048576 {
		remaining := 1048576 - b.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
