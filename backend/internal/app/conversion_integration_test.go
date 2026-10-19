package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRealProbeRetryHTTPRecovery(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("FFmpeg integration runs in backend-test container")
		}
	}
	s, server, client := setup(t)
	loginTest(t, client, server.URL, "admin", "admin-password-123")
	path := filepath.Join(s.Config.MediaRoot, "recover.mkv")
	if err := os.WriteFile(path, []byte("incomplete download"), 0600); err != nil {
		t.Fatal(err)
	}
	id := stableID("media", "recover.mkv")
	s.Config.ConvertTimeout = time.Minute
	sc := NewScanner(s.Config, s.DB, s.log)
	cv := NewConverter(s.Config, s.DB, s.log)
	sc.Derive = cv.PlanFor
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var media Media
	if err := json.Unmarshal(req(t, client, server.URL, "GET", "/api/items/"+id, nil, 200), &media); err != nil {
		t.Fatal(err)
	}
	if media.Playback.Status != "processing" {
		t.Fatal(media.Playback)
	}
	r, _ := readMediaRetry(t, s.DB, id, "probe")
	if !bytes.Contains([]byte(r.Reason), []byte("probe failed")) {
		t.Fatal(r)
	}
	args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=64x32:r=10", "-t", "1", "-c:v", "libx264", "-threads", "1", "-pix_fmt", "yuv420p", "-y", path}
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, err := cv.claim(context.Background())
	if err != nil || job == nil || job.Strategy != PlanRemux {
		t.Fatal(job, err)
	}
	cv.process(context.Background(), *job)
	if err := json.Unmarshal(req(t, client, server.URL, "GET", "/api/items/"+id, nil, 200), &media); err != nil {
		t.Fatal(err)
	}
	if media.Playback.Status != "ready" || media.Playback.Src == "" {
		t.Fatal(media.Playback)
	}
	request, err := http.NewRequest("GET", server.URL+media.Playback.Src, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=0-31")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusPartialContent {
		t.Fatal(response.StatusCode)
	}
}

func TestRealConversionPipeline(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("FFmpeg integration runs in backend-test container")
		}
	}
	s, server, client := setup(t)
	loginTest(t, client, server.URL, "admin", "admin-password-123")
	s.Config.ConvertTimeout = time.Minute
	cv := NewConverter(s.Config, s.DB, s.log)
	sc := NewScanner(s.Config, s.DB, s.log)
	sc.Sprite = func(context.Context, *os.File, float64, string) error { return nil }
	sc.Derive = cv.PlanFor
	cases := []struct{ name, video, audio, strategy string }{
		{"direct.mp4", "libx264", "aac", PlanDirect},
		{"remux.mkv", "libx264", "aac", PlanRemux},
		{"audio.mp4", "libx264", "ac3", PlanTranscodeAudio},
		{"full.mkv", "mpeg2video", "ac3", PlanTranscodeFull},
	}
	originals := map[string][]byte{}
	for _, tc := range cases {
		target := filepath.Join(s.Config.MediaRoot, tc.name)
		args := []string{"-nostdin", "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=64x32:r=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "1", "-c:v", tc.video, "-threads", "1", "-pix_fmt", "yuv420p", "-c:a", tc.audio, "-y", target}
		if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture: %v %s", err, out)
		}
		originals[tc.name], _ = os.ReadFile(target)
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		id := stableID("media", tc.name)
		var strategy, status string
		if err := s.DB.QueryRow("SELECT strategy,status FROM playback_derivations WHERE media_id=?", id).Scan(&strategy, &status); err != nil {
			t.Fatal(err)
		}
		want := tc.strategy
		if want == PlanDirect {
			want = "none"
		}
		if strategy != want {
			t.Fatalf("%s got %s want %s", tc.name, strategy, want)
		}
	}
	for {
		j, err := cv.claim(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if j == nil {
			break
		}
		cv.process(context.Background(), *j)
	}
	for _, tc := range cases {
		id := stableID("media", tc.name)
		var status, artifact string
		if err := s.DB.QueryRow("SELECT status,artifact FROM playback_derivations WHERE media_id=?", id).Scan(&status, &artifact); err != nil {
			t.Fatal(err)
		}
		if status != "ready" {
			var reason string
			s.DB.QueryRow("SELECT reason FROM playback_derivations WHERE media_id=?", id).Scan(&reason)
			t.Fatalf("%s: %s: %s", tc.name, status, reason)
		}
		if tc.strategy != PlanDirect {
			target := filepath.Join(s.Config.DataDir, "derivations", artifact)
			f, err := os.Open(target)
			if err != nil {
				t.Fatal(err)
			}
			md, err := probeMedia(context.Background(), f, time.Second*30)
			f.Close()
			if err != nil || md.VideoCodec != "h264" || !mp4Audio(md.AudioCodecs) {
				t.Fatal(md, err)
			}
			b, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			moov, mdat := bytes.Index(b, []byte("moov")), bytes.Index(b, []byte("mdat"))
			if moov < 0 || mdat < 0 || moov > mdat {
				t.Fatal("MP4 is not faststart")
			}
			r, err := http.NewRequest("GET", server.URL+"/api/items/"+id+"/playback", nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Range", "bytes=0-31")
			res, err := client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil || res.StatusCode != http.StatusPartialContent || !bytes.Equal(body, b[:32]) {
				t.Fatalf("derived range: %d %v", res.StatusCode, err)
			}
		}
		b, _ := os.ReadFile(filepath.Join(s.Config.MediaRoot, tc.name))
		if !bytes.Equal(b, originals[tc.name]) {
			t.Fatal("source modified")
		}
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM playback_derivations WHERE status='ready'").Scan(&n); err != nil || n != len(cases) {
		t.Fatal(n, err)
	}
}
