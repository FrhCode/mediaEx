package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// spritePlan is the contract shared by the generator and the client, so its shape is
// pinned directly rather than only through a rendered sheet.
func TestSpritePlan(t *testing.T) {
	for _, tc := range []struct {
		name              string
		duration          float64
		cols, rows, count int
		interval          float64
		ok                bool
	}{
		{name: "zero duration is refused", duration: 0},
		{name: "negative duration is refused", duration: -5},
		{name: "NaN is refused", duration: math.NaN()},
		{name: "infinity is refused", duration: math.Inf(1)},
		{name: "one second", duration: 1, cols: 10, rows: 1, count: 2, interval: 1, ok: true},
		{name: "demo clip", duration: 20.687, cols: 10, rows: 3, count: 21, interval: 1, ok: true},
		{name: "five minutes reaches the cap", duration: 300, cols: 10, rows: 30, count: 300, interval: 1, ok: true},
		{name: "two hours is capped at 300 tiles", duration: 7200, cols: 10, rows: 30, count: 300, interval: 24, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cols, rows, count, interval, ok := spritePlan(tc.duration)
			if ok != tc.ok {
				t.Fatalf("ok=%v want=%v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if cols != tc.cols || rows != tc.rows || count != tc.count {
				t.Errorf("grid=%dx%d count=%d want=%dx%d count=%d", cols, rows, count, tc.cols, tc.rows, tc.count)
			}
			if math.Abs(interval-tc.interval) > 1e-9 {
				t.Errorf("interval=%v want=%v", interval, tc.interval)
			}
			if count > spriteMaxTiles || rows*spriteCols < count {
				t.Errorf("plan does not fit the tile budget: count=%d rows=%d", count, rows)
			}
		})
	}
}

func TestSpriteConfig(t *testing.T) {
	for _, key := range []string{"SPRITE_TIMEOUT", "TOOL_TIMEOUT", "SCAN_INTERVAL", "SCAN_SETTLE", "SESSION_TTL", "COOKIE_SECURE", "PUBLIC_ORIGIN", "MEDIA_MARKER"} {
		t.Setenv(key, "")
	}
	t.Setenv("MEDIA_ROOT", t.TempDir())
	t.Setenv("DATA_DIR", t.TempDir())
	c, err := LoadConfig()
	if err != nil || c.SpriteTimeout != 2*time.Minute || c.ToolTimeout != 30*time.Second {
		t.Fatalf("defaults: sprite=%v tool=%v err=%v", c.SpriteTimeout, c.ToolTimeout, err)
	}
	t.Setenv("SPRITE_TIMEOUT", "3m")
	t.Setenv("TOOL_TIMEOUT", "5s")
	c, err = LoadConfig()
	if err != nil || c.SpriteTimeout != 3*time.Minute || c.ToolTimeout != 5*time.Second {
		t.Fatalf("overrides: sprite=%v tool=%v err=%v", c.SpriteTimeout, c.ToolTimeout, err)
	}
	for _, value := range []string{"0s", "-1s", "invalid"} {
		t.Setenv("SPRITE_TIMEOUT", value)
		if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "SPRITE_TIMEOUT") {
			t.Fatalf("value=%q err=%v", value, err)
		}
	}
}

func TestSpriteCommandFailures(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		timeout      time.Duration
		deadline     bool
	}{
		{"deadline", "exec sleep 10", 50 * time.Millisecond, true},
		{"stderr", "printf 'decoder rejected fixture' >&2; exit 7", time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := setup(t)
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			s.Config.ToolTimeout = time.Nanosecond
			s.Config.SpriteTimeout = tc.timeout
			f, err := os.Create(filepath.Join(s.Config.MediaRoot, "clip.mp4"))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			target := filepath.Join(s.Config.DataDir, "sprites", "failed.jpg")
			err = NewScanner(s.Config, s.DB, s.log).sprite(context.Background(), f, 30, target)
			if tc.deadline {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deadline error=%v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "decoder rejected fixture") {
				t.Fatalf("stderr error=%v", err)
			}
			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published failed image: %v", err)
			}
			files, err := os.ReadDir(filepath.Dir(target))
			if err != nil || len(files) != 0 {
				t.Fatalf("temporary files=%v err=%v", files, err)
			}
		})
	}
}

func TestSpriteHandler(t *testing.T) {
	s, ts, c := setup(t)
	id := addMedia(t, s, "Movies/clip.mp4", "video")
	loginTest(t, c, ts.URL, "admin", "admin-password-123")

	// No sheet on disk yet.
	req(t, c, ts.URL, "GET", "/api/items/"+id+"/sprite", nil, 404)

	dir := filepath.Join(s.Config.DataDir, "sprites")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	body := []byte("sheet-bytes")
	key := spriteCacheKey("Movies/clip.mp4", 36, 0)
	if e := os.WriteFile(filepath.Join(dir, key), body, 0600); e != nil {
		t.Fatal(e)
	}
	got := req(t, c, ts.URL, "GET", "/api/items/"+id+"/sprite", nil, 200)
	if string(got) != string(body) {
		t.Fatalf("body=%q want=%q", got, body)
	}

	res, e := c.Get(ts.URL + "/api/items/" + id + "/sprite")
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("content-type=%q want image/jpeg", ct)
	}

	// An image has no time axis, so it never has a sheet.
	img := addMedia(t, s, "Movies/photo.jpg", "image")
	req(t, c, ts.URL, "GET", "/api/items/"+img+"/sprite", nil, 404)

	// Protected like every other media route.
	req(t, &http.Client{}, ts.URL, "GET", "/api/items/"+id+"/sprite", nil, 401)
}

func TestSpriteVersionedCache(t *testing.T) {
	s, ts, c := setup(t)
	m := addIndexedVideo(t, s, "Movies/cached.mp4")
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	key := spriteCacheKey(m.Path, m.Size, m.Modified)
	url := mediaDTO(m, 0).Sprite.URL
	if !strings.HasSuffix(url, "?v="+key) {
		t.Fatalf("unversioned DTO URL=%s", url)
	}
	changed := m
	changed.Modified++
	if mediaDTO(changed, 0).Sprite.URL == url {
		t.Fatal("changed fingerprint reused URL")
	}
	check := func(path, etag string, status int, cache string) string {
		t.Helper()
		r, err := http.NewRequest("GET", ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if etag != "" {
			r.Header.Set("If-None-Match", etag)
		}
		res, err := c.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != status || res.Header.Get("Cache-Control") != cache {
			t.Fatalf("path=%s status=%d cache=%q", path, res.StatusCode, res.Header.Get("Cache-Control"))
		}
		return res.Header.Get("ETag")
	}
	check(url, "", 404, "private, no-store")
	dir := filepath.Join(s.Config.DataDir, "sprites")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, key), []byte("jpeg-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	// A sheet is private content gated by the same grant as the video, so even a
	// versioned request must not be given a shared-cacheable lifetime.
	etag := check(url, "", 200, "private, no-store")
	if etag != `"`+key+`"` {
		t.Fatalf("etag=%q", etag)
	}
	check(url, etag, 304, "private, no-store")
	check(strings.Split(url, "?")[0], "", 200, "private, no-store")
	check(strings.Split(url, "?")[0]+"?v=stale", "", 404, "private, no-store")
	req(t, &http.Client{}, ts.URL, "GET", url, nil, 401)
	if _, err := s.DB.Exec("UPDATE media SET modified_ns=modified_ns+1 WHERE id=?", m.ID); err != nil {
		t.Fatal(err)
	}
	check(url, "", 404, "private, no-store")
}

func TestSpriteFailureLogs(t *testing.T) {
	for _, retried := range []bool{false, true} {
		t.Run(fmt.Sprintf("retry=%v", retried), func(t *testing.T) {
			s, _, _ := setup(t)
			sc := newTestScanner(t, s)
			p := "Movies/failure.mp4"
			createFile(t, s.Config.MediaRoot, p)
			if retried {
				if err := sc.Scan(context.Background()); err != nil {
					t.Fatal(err)
				}
				if _, err := s.DB.Exec("INSERT INTO media_retries(media_id,stage,source_key,attempt,next_ns) SELECT id,'sprite',?,1,1 FROM media", mediaSourceKey(addIndexedVideo(t, s, p))); err != nil {
					t.Fatal(err)
				}
			}
			var logs bytes.Buffer
			sc.Log = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			sc.Sprite = func(_ context.Context, f *os.File, _ float64, target string) error {
				st, err := f.Stat()
				if err != nil {
					t.Fatal(err)
				}
				if target != sc.spriteTarget(p, st.Size(), st.ModTime().UnixNano()) {
					t.Fatalf("wrong sprite target=%s", target)
				}
				return context.DeadlineExceeded
			}
			if err := sc.Scan(context.Background()); err != nil {
				t.Fatal(err)
			}
			var entry map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
				t.Fatalf("logs=%s: %v", &logs, err)
			}
			for _, key := range []string{"item", "path", "size_bytes", "duration_seconds", "elapsed", "timeout", "error"} {
				if _, ok := entry[key]; !ok {
					t.Errorf("missing %s: %s", key, &logs)
				}
			}
			if entry["path"] != p || entry["item"] != stableID("media", p) || entry["error"] != "context deadline exceeded" {
				t.Fatalf("incorrect diagnostics: %s", &logs)
			}
		})
	}
}

func addIndexedVideo(t *testing.T, s *Server, p string) MediaRecord {
	t.Helper()
	createFile(t, s.Config.MediaRoot, p)
	st, e := os.Stat(filepath.Join(s.Config.MediaRoot, p))
	if e != nil {
		t.Fatal(e)
	}
	id := stableID("media", p)
	var m MediaRecord
	if e := s.DB.QueryRow("SELECT "+mediaCols+" FROM media WHERE id=?", id).Scan(&m.ID, &m.Path, &m.FolderID, &m.Name, &m.Kind, &m.MIME, &m.Format, &m.Size, &m.Modified, &m.Width, &m.Height, &m.Duration, &m.Thumbnail); e == nil {
		return m
	}
	if _, e := s.DB.Exec("INSERT INTO media VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", id, p, "", filepath.Base(p), "video", "video/mp4", "MP4", st.Size(), st.ModTime().UnixNano(), 1920, 1080, 30, ""); e != nil {
		t.Fatal(e)
	}
	m = MediaRecord{ID: id, Path: p, Name: filepath.Base(p), Kind: "video", Size: st.Size(), Modified: st.ModTime().UnixNano(), Duration: 30}
	return m
}

func TestScanGeneratesMissingSprite(t *testing.T) {
	s, _, _ := setup(t)
	sc := newTestScanner(t, s)
	createFile(t, s.Config.MediaRoot, "Movies/older.mp4")
	st, err := os.Stat(filepath.Join(s.Config.MediaRoot, "Movies/older.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	m := MediaRecord{Path: "Movies/older.mp4", Size: st.Size(), Modified: st.ModTime().UnixNano()}
	createFile(t, s.Config.MediaRoot, "Movies/photo.jpg")
	oldKey := stableID("sprite", fmt.Sprintf("%s:%d:%d", m.Path, m.Size, m.Modified)) + ".jpg"
	dir := filepath.Join(s.Config.DataDir, "sprites")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, oldKey), []byte("old 100-tile sheet"), 0600); err != nil {
		t.Fatal(err)
	}
	if oldKey == spriteCacheKey(m.Path, m.Size, m.Modified) {
		t.Fatal("old geometry shares the new cache key")
	}

	var calls atomic.Int32
	var target atomic.Value
	sc.Sprite = func(_ context.Context, _ *os.File, _ float64, tgt string) error {
		calls.Add(1)
		target.Store(tgt)
		if e := os.MkdirAll(filepath.Dir(tgt), 0700); e != nil {
			return e
		}
		return os.WriteFile(tgt, []byte("sheet"), 0600)
	}

	if e := sc.Scan(context.Background()); e != nil {
		t.Fatal(e)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("sprite calls=%d want=1 (only the video, and only once)", n)
	}
	want := sc.spriteTarget(m.Path, m.Size, m.Modified)
	if got := target.Load().(string); got != want {
		t.Fatalf("target=%q want=%q", got, want)
	}

	// Now that the sheet exists, an unchanged file must not be regenerated.
	if e := sc.Scan(context.Background()); e != nil {
		t.Fatal(e)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("sprite calls=%d after second scan, want 1", n)
	}
}

func TestScanDefersFailedSprites(t *testing.T) {
	s, _, _ := setup(t)
	sc := newTestScanner(t, s)
	createFile(t, s.Config.MediaRoot, "Movies/broken.mp4")

	var calls atomic.Int32
	sc.Sprite = func(context.Context, *os.File, float64, string) error {
		calls.Add(1)
		return os.ErrInvalid
	}
	for i := 0; i < 3; i++ {
		if e := sc.Scan(context.Background()); e != nil {
			t.Fatal(e)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("sprite calls=%d across three scans, want 1", n)
	}
}

func TestRealSpriteGeneration(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, e := exec.LookPath(tool); e != nil {
			t.Skip("ffmpeg is validated in the runtime container")
		}
	}
	s, _, _ := setup(t)
	clip := filepath.Join(s.Config.MediaRoot, "clip.mp4")
	// A four-second GOP exceeds the two-second sampling interval and exercises all 300 tiles.
	const clipSeconds = 600
	gen := exec.Command("ffmpeg", "-nostdin", "-v", "error", "-f", "lavfi",
		"-i", "testsrc=duration=600:size=320x180:rate=2", "-c:v", "mpeg4", "-g", "8", "-y", clip)
	if out, e := gen.CombinedOutput(); e != nil {
		t.Skipf("cannot synthesise a test clip: %v %s", e, out)
	}
	f, e := os.Open(clip)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()

	cols, rows, count, _, ok := spritePlan(clipSeconds)
	if !ok {
		t.Fatal("spritePlan refused the test clip")
	}
	target := filepath.Join(s.Config.DataDir, "sprites", "clip.jpg")
	sc := NewScanner(s.Config, s.DB, s.log)
	if e := sc.sprite(context.Background(), f, clipSeconds, target); e != nil {
		t.Fatal(e)
	}

	out, e := os.Open(target)
	if e != nil {
		t.Fatal(e)
	}
	defer out.Close()
	cfg, e := jpeg.DecodeConfig(out)
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Width != cols*spriteTileW || cfg.Height != rows*spriteTileH {
		t.Fatalf("sheet=%dx%d want=%dx%d", cfg.Width, cfg.Height, cols*spriteTileW, rows*spriteTileH)
	}

	// Every tile the client can reach must hold a real frame. Asserting only that the
	// last tile differs from the first is not enough — a black padding cell differs
	// from the first tile too, so that check passed while three quarters of the sheet
	// were blank and the end of every video scrubbed to black.
	if _, e := out.Seek(0, 0); e != nil {
		t.Fatal(e)
	}
	img, e := jpeg.Decode(out)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < count; i++ {
		ox, oy := (i%cols)*spriteTileW, (i/cols)*spriteTileH
		brightest := uint32(0)
		for _, quarter := range []int{1, 2, 3} {
			r, g, b, _ := img.At(ox+spriteTileW*quarter/4, oy+spriteTileH*quarter/4).RGBA()
			if sum := r>>8 + g>>8 + b>>8; sum > brightest {
				brightest = sum
			}
		}
		// A padding cell is uniformly black; even the darkest testsrc frame is not.
		if brightest < 24 {
			t.Fatalf("tile %d of %d is blank, so the sheet stops before the end of the clip", i, count)
		}
	}
}
