package app

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProbeOutcomesAndDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		want         error
	}{
		{"no video", `printf '%s' '{"streams":[{"codec_type":"audio","codec_name":"aac"}]}'`, errNoVisualStream},
		{"no streams", `printf '%s' '{"streams":[]}'`, nil},
		{"partial video", `printf '%s' '{"streams":[{"codec_type":"video","codec_name":"h264","width":0,"height":0}]}'`, nil},
		{"stderr", `printf '%s' 'moov atom not found' >&2; exit 1`, nil},
		{"bounded stderr", `i=0; while [ "$i" -lt 5000 ]; do printf x >&2; i=$((i+1)); done; exit 1`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			f, err := os.CreateTemp(t.TempDir(), "video-*.mp4")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			_, err = probeMedia(context.Background(), f, time.Second)
			if err == nil {
				t.Fatal("expected probe failure")
			}
			if errors.Is(err, errNoVisualStream) != (tc.want != nil) {
				t.Fatal(err)
			}
			if tc.name == "stderr" && !strings.Contains(err.Error(), "moov atom not found") {
				t.Fatal(err)
			}
			if tc.name == "bounded stderr" && len(err.Error()) > 4200 {
				t.Fatalf("probe diagnostics not bounded: %d", len(err.Error()))
			}
		})
	}
}

func TestProbeTimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte("#!/bin/sh\nwhile :; do :; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	f, err := os.CreateTemp(t.TempDir(), "video-*.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = probeMedia(context.Background(), f, 50*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestThumbnailWriteFailureRetainsMetadata(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' '{\"streams\":[{\"codec_type\":\"video\",\"codec_name\":\"h264\",\"width\":64,\"height\":32}],\"format\":{\"format_name\":\"mov,mp4\"}}'\n"
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	s, _, _ := setup(t)
	createFile(t, s.Config.MediaRoot, "clip.mp4")
	f, err := os.Open(filepath.Join(s.Config.MediaRoot, "clip.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	blocker := filepath.Join(s.Config.DataDir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	metadata, err := NewScanner(s.Config, s.DB, s.log).process(context.Background(), f, "video", filepath.Join(blocker, "thumb.jpg"))
	if err != nil || metadata.Width != 64 || metadata.VideoCodec != "h264" {
		t.Fatal(metadata, err)
	}
}

func TestRealProbeAndThumbnail(t *testing.T) {
	for _, tool := range []string{"ffprobe", "ffmpeg"} {
		if _, e := exec.LookPath(tool); e != nil {
			t.Skip("ffprobe/ffmpeg are validated in runtime container")
		}
	}
	s, _, _ := setup(t)
	f, e := os.Create(filepath.Join(s.Config.MediaRoot, "photo.jpg"))
	if e != nil {
		t.Fatal(e)
	}
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{100, 150, 200, 255})
		}
	}
	if e = jpeg.Encode(f, img, nil); e != nil {
		t.Fatal(e)
	}
	f.Close()
	f, e = os.Open(filepath.Join(s.Config.MediaRoot, "photo.jpg"))
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	sc := NewScanner(s.Config, s.DB, s.log)
	target := filepath.Join(s.Config.DataDir, "thumbs", "test.jpg")
	md, e := sc.process(context.Background(), f, "image", target)
	if e != nil {
		t.Fatal(e)
	}
	if md.Width != 64 || md.Height != 32 {
		t.Fatal(md)
	}
	out, e := os.Open(target)
	if e != nil {
		t.Fatal(e)
	}
	defer out.Close()
	if _, e = jpeg.DecodeConfig(out); e != nil {
		t.Fatal(e)
	}
}
func TestProbeRejectsRenamedPlaylists(t *testing.T) {
	if _, e := exec.LookPath("ffprobe"); e != nil {
		t.Skip("ffprobe is validated in runtime container")
	}
	s, _, _ := setup(t)
	sc := NewScanner(s.Config, s.DB, s.log)
	for _, content := range []string{"#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:10,\nfile:///etc/passwd\n", "ffconcat version 1.0\nfile '/etc/passwd'\n"} {
		p := filepath.Join(t.TempDir(), "pretend.mp4")
		if e := os.WriteFile(p, []byte(content), 0600); e != nil {
			t.Fatal(e)
		}
		f, e := os.Open(p)
		if e != nil {
			t.Fatal(e)
		}
		_, e = sc.process(context.Background(), f, "video", filepath.Join(s.Config.DataDir, "bad.jpg"))
		f.Close()
		if e == nil {
			t.Fatal("renamed playlist accepted")
		}
	}
}
