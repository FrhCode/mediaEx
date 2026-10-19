package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// One sprite sheet serves every hover preview for a video: ffmpeg samples frames
// across the whole duration and tiles them into a single JPEG, so the player renders
// a preview by moving a background position rather than seeking the video. Seeking
// per hover would mean one range request to the NAS for every pointer movement.
const (
	spriteCols        = 10
	spriteTileW       = 160
	spriteTileH       = 90
	spriteMaxTiles    = 300
	spriteMinInterval = 1.0
)

// spritePlan picks the grid and sampling interval for one video. It is the single
// source of truth: the generator renders with these numbers and mediaDTO publishes
// them to the client. If the two ever disagreed, every tile would point at the
// wrong frame and nothing would report an error.
func spritePlan(duration float64) (cols, rows, count int, interval float64, ok bool) {
	if duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return 0, 0, 0, 0, false
	}
	cols = spriteCols
	interval = duration / spriteMaxTiles
	if interval < spriteMinInterval {
		interval = spriteMinInterval
	}
	count = int(math.Floor(duration/interval)) + 1
	if count > spriteMaxTiles {
		count = spriteMaxTiles
	}
	if count < 1 {
		count = 1
	}
	rows = (count + cols - 1) / cols
	return cols, rows, count, interval, true
}

// Include layout and sampler version so old sheets cannot satisfy a different tile plan.
func spriteCacheKey(mediaPath string, size, modified int64) string {
	return stableID("sprite-v2", fmt.Sprintf("%d:%d:%d:%d:%g:%s:%d:%d", spriteCols, spriteTileW, spriteTileH, spriteMaxTiles, spriteMinInterval, mediaPath, size, modified)) + ".jpg"
}

func (s *Scanner) spriteTarget(mediaPath string, size, modified int64) string {
	return filepath.Join(s.Config.DataDir, "sprites", spriteCacheKey(mediaPath, size, modified))
}

func (s *Scanner) spriteExists(mediaPath string, size, modified int64) bool {
	st, err := os.Stat(s.spriteTarget(mediaPath, size, modified))
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

// Keyframe-only decoding limits the work done inside the scan.
func (s *Scanner) sprite(ctx context.Context, f *os.File, duration float64, target string) error {
	cols, rows, _, interval, ok := spritePlan(duration)
	if !ok {
		return nil
	}
	// ExtraFiles duplicates this descriptor, so the offset is shared with whatever
	// ffprobe or ffmpeg ran before it.
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	before, err := f.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".sprite-*.jpg")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	// fps resamples to one frame per interval, repeating the most recent keyframe to
	// fill the gaps. Selecting keyframes by timestamp instead would stop at the last
	// keyframe, leaving the tail of the grid black — and because spritePlan is a pure
	// function of duration, the count the client is told would not match the tiles
	// that actually hold a frame, so scrubbing the end of a video would show padding.
	// tpad clones that last frame a little longer so the grid always fills past count.
	vf := fmt.Sprintf(
		"fps=1/%s,tpad=stop_mode=clone:stop_duration=%s,"+
			"scale=%d:%d:force_original_aspect_ratio=decrease,"+
			"pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,tile=%dx%d",
		strconv.FormatFloat(interval, 'f', -1, 64),
		strconv.FormatFloat(interval*2+10, 'f', -1, 64),
		spriteTileW, spriteTileH, spriteTileW, spriteTileH, cols, rows)
	args := []string{
		"-nostdin", "-v", "error",
		"-protocol_whitelist", "file,pipe,fd",
		"-format_whitelist", safeDemuxers,
		"-threads", "1",
		"-skip_frame", "nokey",
		"-i", "/proc/self/fd/3",
		"-map", "0:v:0",
		"-vf", vf,
		"-frames:v", "1",
		"-q:v", "5",
		"-threads", "1",
		"-f", "image2",
		"-y", tmpPath,
	}
	spriteCtx, stop := context.WithTimeout(ctx, s.Config.SpriteTimeout)
	defer stop()
	cmd := exec.CommandContext(spriteCtx, "ffmpeg", args...)
	cmd.ExtraFiles = []*os.File{f}
	stderr := &limitedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if spriteCtx.Err() != nil {
			return fmt.Errorf("sprite: %w", spriteCtx.Err())
		}
		detail := stderr.String()
		if len(detail) > 4096 {
			detail = detail[:4096]
		}
		return fmt.Errorf("sprite: %w: %s", err, strings.TrimSpace(detail))
	}
	// CreateTemp already produced a zero-byte file; publishing it would serve an
	// empty JPEG as if it were a sheet.
	st, err := os.Stat(tmpPath)
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		return errors.New("sprite: ffmpeg produced no image")
	}
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if after.Size() != before.Size() || after.ModTime() != before.ModTime() {
		return errors.New("sprite source changed during processing")
	}
	return os.Rename(tmpPath, target)
}
