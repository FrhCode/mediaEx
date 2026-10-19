package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlaybackSchemaFreshDatabase(t *testing.T) {
	db, err := OpenDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, table := range []string{"playback_derivations", "media_retries"} {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatal(table, count)
		}
	}
}

func conversionFixture(t *testing.T) (*Server, *Converter, MediaRecord) {
	t.Helper()
	s, _, _ := setup(t)
	createFile(t, s.Config.MediaRoot, "clip.mkv")
	if err := newTestScanner(t, s).Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	m, err := scanMedia(s.DB.QueryRow("SELECT " + mediaCols + " FROM media"))
	if err != nil {
		t.Fatal(err)
	}
	cv := NewConverter(s.Config, s.DB, s.log)
	if err := cv.PlanFor(context.Background(), m, md("matroska,webm", "h264", "aac")); err != nil {
		t.Fatal(err)
	}
	return s, cv, m
}

func TestConverterReadyAndDirectReplacement(t *testing.T) {
	s, cv, m := conversionFixture(t)
	ctx := context.Background()
	cv.Convert = func(_ context.Context, _ *os.File, strategy, target string) error {
		if strategy != PlanRemux {
			t.Fatal(strategy)
		}
		return os.WriteFile(target, []byte("playable"), 0600)
	}
	j, err := cv.claim(ctx)
	if err != nil || j == nil {
		t.Fatalf("claim: %v %v", j, err)
	}
	cv.process(ctx, *j)
	var status, artifact string
	if err = s.DB.QueryRow("SELECT status,artifact FROM playback_derivations").Scan(&status, &artifact); err != nil {
		t.Fatal(err)
	}
	if status != "ready" || artifact != j.SourceKey+".mp4" {
		t.Fatal(status, artifact)
	}
	if b, err := os.ReadFile(filepath.Join(s.Config.DataDir, "derivations", artifact)); err != nil || string(b) != "playable" {
		t.Fatal(string(b), err)
	}
	m.Format = "MP4"
	if err = cv.PlanFor(ctx, m, md("mov,mp4,m4a", "h264", "aac")); err != nil {
		t.Fatal(err)
	}
	var strategy string
	if err = s.DB.QueryRow("SELECT status,strategy,artifact FROM playback_derivations").Scan(&status, &strategy, &artifact); err != nil {
		t.Fatal(err)
	}
	if status != "ready" || strategy != "none" || artifact != "" {
		t.Fatal(status, strategy, artifact)
	}
	cv.cleanup(ctx)
	if _, err = os.Stat(filepath.Join(s.Config.DataDir, "derivations", j.SourceKey+".mp4")); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestScannerInvalidatesDerivationWhenChangedVideoProbeFails(t *testing.T) {
	s, cv, m := conversionFixture(t)
	sc := newTestScanner(t, s)
	sc.Derive = cv.PlanFor
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) {
		return Metadata{}, errors.New("invalid replacement")
	}
	if err := os.WriteFile(filepath.Join(s.Config.MediaRoot, m.Path), []byte("broken replacement video"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM playback_derivations WHERE media_id=?", m.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("probe failure created %d playback derivations", count)
	}
}

func TestConverterCleanupOnlyRemovesOwnedFiles(t *testing.T) {
	s, cv, _ := conversionFixture(t)
	dir := filepath.Join(s.Config.DataDir, "derivations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := s.DB.QueryRow("SELECT source_key FROM playback_derivations").Scan(&key); err != nil {
		t.Fatal(err)
	}
	stale := strings.Repeat("f", 32) + ".mp4"
	for _, name := range []string{key + ".mp4", stale, ".deriv-interrupted.mp4", "unrelated.mp4"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("video"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cv.cleanup(context.Background())
	for _, name := range []string{key + ".mp4", "unrelated.mp4"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(name, err)
		}
	}
	for _, name := range []string{stale, ".deriv-interrupted.mp4"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal(name, err)
		}
	}
}

func TestConverterRetriesRecoveryAndStaleJob(t *testing.T) {
	s, cv, m := conversionFixture(t)
	ctx := context.Background()
	cv.Convert = func(context.Context, *os.File, string, string) error { return errors.New("decoder failed") }
	for attempt := 1; attempt <= derivMaxAttempts; attempt++ {
		j, err := cv.claim(ctx)
		if err != nil || j == nil {
			t.Fatalf("claim %d: %v", attempt, err)
		}
		cv.process(ctx, *j)
		var status string
		var got int
		if err = s.DB.QueryRow("SELECT status,attempt FROM playback_derivations").Scan(&status, &got); err != nil {
			t.Fatal(err)
		}
		if got != attempt {
			t.Fatal(got)
		}
		if attempt < derivMaxAttempts {
			if status != "pending" {
				t.Fatal(status)
			}
			if next, err := cv.claim(ctx); err != nil || next != nil {
				t.Fatalf("backoff bypassed: %v %v", next, err)
			}
			if _, err = s.DB.Exec("UPDATE playback_derivations SET updated_ns=0"); err != nil {
				t.Fatal(err)
			}
		} else if status != "failed" {
			t.Fatal(status)
		}
	}
	if _, err := s.DB.Exec("UPDATE playback_derivations SET status='processing'"); err != nil {
		t.Fatal(err)
	}
	if err := cv.resetProcessing(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE playback_derivations SET updated_ns=0"); err != nil {
		t.Fatal(err)
	}
	j, err := cv.claim(ctx)
	if err != nil || j == nil {
		t.Fatal(j, err)
	}
	cv.Convert = func(_ context.Context, _ *os.File, _ string, target string) error {
		newer := m
		newer.Modified++
		if err := cv.PlanFor(ctx, newer, md("matroska,webm", "mpeg2video", "ac3")); err != nil {
			return err
		}
		return os.WriteFile(target, []byte("stale output"), 0600)
	}
	cv.process(ctx, *j)
	var status, strategy string
	if err = s.DB.QueryRow("SELECT status,strategy FROM playback_derivations").Scan(&status, &strategy); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || strategy != PlanTranscodeFull {
		t.Fatal(status, strategy)
	}
}

func TestConverterRestartRecoversJob(t *testing.T) {
	s, cv, _ := conversionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cv.Convert = func(ctx context.Context, _ *os.File, _, _ string) error { cancel(); return ctx.Err() }
	job, err := cv.claim(ctx)
	if err != nil || job == nil {
		t.Fatal(job, err)
	}
	cv.process(ctx, *job)
	var status string
	if err := s.DB.QueryRow("SELECT status FROM playback_derivations").Scan(&status); err != nil || status != "processing" {
		t.Fatal(status, err)
	}
	restarted := NewConverter(s.Config, s.DB, s.log)
	restarted.Convert = func(_ context.Context, _ *os.File, _, target string) error {
		return os.WriteFile(target, []byte("recovered"), 0600)
	}
	if err := restarted.resetProcessing(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, err = restarted.claim(context.Background())
	if err != nil || job == nil {
		t.Fatal(job, err)
	}
	restarted.process(context.Background(), *job)
	if err := s.DB.QueryRow("SELECT status FROM playback_derivations").Scan(&status); err != nil || status != "ready" {
		t.Fatal(status, err)
	}
}

func TestConverterRejectsChangedSourceAndCleansTemps(t *testing.T) {
	s, cv, _ := conversionFixture(t)
	ctx := context.Background()
	cv.Convert = func(_ context.Context, _ *os.File, _ string, target string) error {
		if err := os.WriteFile(filepath.Join(s.Config.MediaRoot, "clip.mkv"), []byte("changed during conversion"), 0600); err != nil {
			return err
		}
		return os.WriteFile(target, []byte("output"), 0600)
	}
	j, err := cv.claim(ctx)
	if err != nil || j == nil {
		t.Fatal(j, err)
	}
	cv.process(ctx, *j)
	var status string
	var attempts int
	if err = s.DB.QueryRow("SELECT status,attempt FROM playback_derivations").Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "waiting_source" || attempts != j.Attempt {
		t.Fatal(status, attempts)
	}
	if next, err := cv.claim(ctx); err != nil || next != nil {
		t.Fatalf("changed source immediately reclaimed: %v %v", next, err)
	}
	entries, err := os.ReadDir(filepath.Join(s.Config.DataDir, "derivations"))
	if err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	if _, err = s.DB.Exec("DELETE FROM media"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM playback_derivations").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestConverterSourceChangesDoNotConsumeAttempts(t *testing.T) {
	for _, change := range []string{"before conversion", "decoder failure", "replacement"} {
		t.Run(change, func(t *testing.T) {
			s, cv, m := conversionFixture(t)
			ctx := context.Background()
			if _, err := s.DB.Exec("UPDATE playback_derivations SET attempt=1,updated_ns=0"); err != nil {
				t.Fatal(err)
			}
			job, err := cv.claim(ctx)
			if err != nil || job == nil {
				t.Fatal(job, err)
			}
			source := filepath.Join(s.Config.MediaRoot, m.Path)
			if change == "before conversion" {
				if err := os.WriteFile(source, []byte("changed before conversion"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cv.Convert = func(_ context.Context, src *os.File, _, target string) error {
				if change == "before conversion" {
					t.Fatal("conversion started for a stale source")
				}
				if err := os.WriteFile(target, []byte("partial output"), 0600); err != nil {
					return err
				}
				if change == "replacement" {
					before, err := src.Stat()
					if err != nil {
						return err
					}
					replacement := filepath.Join(t.TempDir(), "replacement.mkv")
					if err := os.WriteFile(replacement, make([]byte, before.Size()), 0600); err != nil {
						return err
					}
					if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
						return err
					}
					return os.Rename(replacement, source)
				}
				if err := os.WriteFile(source, []byte("changed during failed conversion"), 0600); err != nil {
					return err
				}
				return errors.New("decoder failed")
			}
			cv.process(ctx, *job)
			var status, artifact string
			var attempts int
			if err := s.DB.QueryRow("SELECT status,attempt,artifact FROM playback_derivations WHERE media_id=?", m.ID).Scan(&status, &attempts, &artifact); err != nil {
				t.Fatal(err)
			}
			if status != "waiting_source" || attempts != 1 || artifact != "" {
				t.Fatal(status, attempts, artifact)
			}
			if err := cv.resetProcessing(ctx); err != nil {
				t.Fatal(err)
			}
			if next, err := cv.claim(ctx); err != nil || next != nil {
				t.Fatalf("waiting source reclaimed after restart recovery: %v %v", next, err)
			}
			entries, err := os.ReadDir(filepath.Join(s.Config.DataDir, "derivations"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatal(entries)
			}
		})
	}
}

func TestConverterChangedSourceDoesNotOverwriteNewPlan(t *testing.T) {
	s, cv, m := conversionFixture(t)
	ctx := context.Background()
	job, err := cv.claim(ctx)
	if err != nil || job == nil {
		t.Fatal(job, err)
	}
	var newerKey string
	cv.Convert = func(_ context.Context, _ *os.File, _, target string) error {
		source := filepath.Join(s.Config.MediaRoot, m.Path)
		if err := os.WriteFile(source, []byte("new source with new plan"), 0600); err != nil {
			return err
		}
		st, err := os.Stat(source)
		if err != nil {
			return err
		}
		newer := m
		newer.Size, newer.Modified = st.Size(), st.ModTime().UnixNano()
		if _, err := s.DB.Exec("UPDATE media SET size_bytes=?,modified_ns=? WHERE id=?", newer.Size, newer.Modified, newer.ID); err != nil {
			return err
		}
		metadata := md("matroska,webm", "mpeg2video", "ac3")
		newerKey = derivationKey(newer, metadata, PlanTranscodeFull)
		if err := cv.PlanFor(ctx, newer, metadata); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte("stale partial output"), 0600); err != nil {
			return err
		}
		return errors.New("old decoder failed")
	}
	cv.process(ctx, *job)
	var status, key, strategy string
	var attempts int
	if err := s.DB.QueryRow("SELECT status,source_key,strategy,attempt FROM playback_derivations WHERE media_id=?", m.ID).Scan(&status, &key, &strategy, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || key != newerKey || strategy != PlanTranscodeFull || attempts != 0 {
		t.Fatal(status, key, strategy, attempts)
	}
	next, err := cv.claim(ctx)
	if err != nil || next == nil || next.SourceKey != newerKey {
		t.Fatal(next, err)
	}
}

func TestDerivedPlaybackRangeAndDTO(t *testing.T) {
	s, ts, client := setup(t)
	id := addMedia(t, s, "clip.mp4", "video")
	key := strings.Repeat("a", 32)
	dir := filepath.Join(s.Config.DataDir, "derivations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, key+".mp4"), []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO playback_derivations VALUES(?,?,?,'ready',?,'',0,?)", id, key, PlanRemux, key+".mp4", time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	req(t, client, ts.URL, "GET", "/api/items/"+id+"/playback", nil, 401)
	loginTest(t, client, ts.URL, "admin", "admin-password-123")
	var media Media
	if err := json.Unmarshal(req(t, client, ts.URL, "GET", "/api/items/"+id, nil, 200), &media); err != nil {
		t.Fatal(err)
	}
	if media.Playback == nil || media.Playback.Status != "ready" || !strings.Contains(media.Playback.Src, key) {
		t.Fatal(media)
	}
	for _, tc := range []struct {
		value  string
		status int
		body   string
	}{{"bytes=2-5", 206, "2345"}, {"bytes=99-", 416, ""}} {
		r, _ := http.NewRequest("GET", ts.URL+media.Playback.Src, nil)
		r.Header.Set("Range", tc.value)
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != tc.status || (tc.body != "" && string(b) != tc.body) {
			t.Fatal(res.StatusCode, string(b))
		}
		if tc.status == 206 && res.Header.Get("Cache-Control") != "private, no-store" {
			t.Fatal(res.Header)
		}
	}
	req(t, client, ts.URL, "GET", "/api/items/"+id+"/playback?v=old", nil, 404)
	var userID string
	if err := s.DB.QueryRow("SELECT id FROM users WHERE username='admin'").Scan(&userID); err != nil {
		t.Fatal(err)
	}
	lockSession(t, s, userID, sessionHash(t, s, userID))
	req(t, client, ts.URL, "GET", "/api/items/"+id+"/playback", nil, 423)
	if _, err := s.DB.Exec("DELETE FROM session_locks"); err != nil {
		t.Fatal(err)
	}
	var library struct {
		Items []Media `json:"items"`
	}
	if err := json.Unmarshal(req(t, client, ts.URL, "GET", "/api/library", nil, 200), &library); err != nil {
		t.Fatal(err)
	}
	if len(library.Items) != 1 || library.Items[0].Playback.Status != "ready" {
		t.Fatal(library)
	}
}
