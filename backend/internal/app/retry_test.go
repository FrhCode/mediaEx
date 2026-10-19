package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func retryMetadata() Metadata {
	return Metadata{Width: 1920, Height: 1080, Duration: 30, Container: "mov,mp4", VideoCodec: "h264", AudioCodecs: []string{"aac"}}
}

func readMediaRetry(t *testing.T, db *sql.DB, id, stage string) (mediaRetry, string) {
	t.Helper()
	var r mediaRetry
	var key string
	r.Stage = stage
	if err := db.QueryRow("SELECT source_key,attempt,next_ns,reason FROM media_retries WHERE media_id=? AND stage=?", id, stage).Scan(&key, &r.Attempt, &r.Next, &r.Reason); err != nil {
		t.Fatal(err)
	}
	return r, key
}

func dueMediaRetry(t *testing.T, db *sql.DB, id, stage string) {
	t.Helper()
	res, err := db.Exec("UPDATE media_retries SET next_ns=1 WHERE media_id=? AND stage=?", id, stage)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("force retry due: rows=%d error=%v", n, err)
	}
}

func retryCount(t *testing.T, db *sql.DB, id string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM media_retries WHERE media_id=?", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPersistentProbeRetryRecoversUnchangedSource(t *testing.T) {
	s, server, client := setup(t)
	createFile(t, s.Config.MediaRoot, "pending.mp4")
	id := stableID("media", "pending.mp4")
	cv := NewConverter(s.Config, s.DB, s.log)
	sc := newTestScanner(t, s)
	sc.Derive = cv.PlanFor
	calls := 0
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) {
		calls++
		if calls == 1 {
			return Metadata{}, errors.New("source is incomplete")
		}
		return retryMetadata(), nil
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, key := readMediaRetry(t, s.DB, id, "probe")
	if r.Attempt != 1 || r.Next <= time.Now().UnixNano() || r.Reason != "source is incomplete" {
		t.Fatalf("unexpected retry: %+v", r)
	}
	m, err := scanMedia(s.DB.QueryRow("SELECT "+mediaCols+" FROM media WHERE id=?", id))
	if err != nil || key != mediaSourceKey(m) {
		t.Fatalf("source key mismatch: media=%+v key=%q error=%v", m, key, err)
	}
	var derivations int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM playback_derivations WHERE media_id=?", id).Scan(&derivations); err != nil || derivations != 0 {
		t.Fatalf("failed probe was classified: count=%d error=%v", derivations, err)
	}
	loginTest(t, client, server.URL, "admin", "admin-password-123")
	var media Media
	if err := json.Unmarshal(req(t, client, server.URL, "GET", "/api/items/"+id, nil, 200), &media); err != nil {
		t.Fatal(err)
	}
	if media.Playback == nil || media.Playback.Status != "processing" {
		t.Fatalf("pending metadata playback=%+v", media.Playback)
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("retried before due: calls=%d", calls)
	}
	dueMediaRetry(t, s.DB, id, "probe")
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || retryCount(t, s.DB, id) != 0 {
		t.Fatalf("recovery calls=%d retries=%d", calls, retryCount(t, s.DB, id))
	}
	if err := json.Unmarshal(req(t, client, server.URL, "GET", "/api/items/"+id, nil, 200), &media); err != nil {
		t.Fatal(err)
	}
	if media.Playback == nil || media.Playback.Status != "direct" || media.Duration != 30 {
		t.Fatalf("recovered media=%+v", media)
	}
}

func TestPersistentProbeBackoffAndRestart(t *testing.T) {
	s, _, _ := setup(t)
	createFile(t, s.Config.MediaRoot, "pending.mp4")
	id := stableID("media", "pending.mp4")
	calls := 0
	processor := func(context.Context, *os.File, string, string) (Metadata, error) {
		calls++
		return Metadata{}, errors.New("still incomplete")
	}
	sc := newTestScanner(t, s)
	sc.Process = processor
	delays := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, 6 * time.Hour, 6 * time.Hour}
	for i, want := range delays {
		if i > 0 {
			dueMediaRetry(t, s.DB, id, "probe")
		}
		before := time.Now()
		if err := sc.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
		after := time.Now()
		r, key := readMediaRetry(t, s.DB, id, "probe")
		if r.Attempt != i+1 || r.Next < before.Add(want).UnixNano() || r.Next > after.Add(want).UnixNano() {
			t.Fatalf("attempt=%d got=%+v want delay=%s", i+1, r, want)
		}
		sc = newTestScanner(t, s)
		sc.Process = processor
		if err := sc.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
		persisted, persistedKey := readMediaRetry(t, s.DB, id, "probe")
		if persisted != r || persistedKey != key || calls != i+1 {
			t.Fatalf("restart changed retry: before=%+v after=%+v calls=%d", r, persisted, calls)
		}
	}
	if err := os.WriteFile(filepath.Join(s.Config.MediaRoot, "pending.mp4"), []byte("a newly changed incomplete source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, _ := readMediaRetry(t, s.DB, id, "probe")
	if r.Attempt != 1 {
		t.Fatalf("new fingerprint retained attempts: %+v", r)
	}
	if err := os.Remove(filepath.Join(s.Config.MediaRoot, "pending.mp4")); err != nil {
		t.Fatal(err)
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if retryCount(t, s.DB, id) != 0 {
		t.Fatal("deleted media retained retries")
	}
}

func TestPersistentSpriteAttemptsAndSourceReset(t *testing.T) {
	s, _, _ := setup(t)
	createFile(t, s.Config.MediaRoot, "preview.mp4")
	id := stableID("media", "preview.mp4")
	calls := 0
	sprite := func(context.Context, *os.File, float64, string) error {
		calls++
		return errors.New("sprite decoder failed")
	}
	sc := newTestScanner(t, s)
	cv := NewConverter(s.Config, s.DB, s.log)
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) { return retryMetadata(), nil }
	sc.Derive = cv.PlanFor
	sc.Sprite = sprite
	var firstKey string
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			dueMediaRetry(t, s.DB, id, "sprite")
		}
		if err := sc.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
		r, key := readMediaRetry(t, s.DB, id, "sprite")
		if attempt == 1 {
			firstKey = key
		}
		if r.Attempt != attempt || calls != attempt || key != firstKey {
			t.Fatalf("attempt=%d retry=%+v calls=%d", attempt, r, calls)
		}
		if (attempt == 3) != (r.Next == 0) {
			t.Fatalf("incorrect terminal status: %+v", r)
		}
		var playback string
		if err := s.DB.QueryRow("SELECT status FROM playback_derivations WHERE media_id=?", id).Scan(&playback); err != nil || playback != "ready" {
			t.Fatalf("sprite failure changed playback: %q error=%v", playback, err)
		}
		sc = newTestScanner(t, s)
		sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) { return retryMetadata(), nil }
		sc.Derive = cv.PlanFor
		sc.Sprite = sprite
		if err := sc.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
		if calls != attempt {
			t.Fatalf("restart bypassed sprite schedule: calls=%d attempt=%d", calls, attempt)
		}
	}
	if err := os.WriteFile(filepath.Join(s.Config.MediaRoot, "preview.mp4"), []byte("replacement with a different size"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, key := readMediaRetry(t, s.DB, id, "sprite")
	if r.Attempt != 1 || key == firstKey || calls != 4 || r.Next == 0 {
		t.Fatalf("new source did not reset sprite: retry=%+v key=%q calls=%d", r, key, calls)
	}
}

func TestPersistentRetryQueuesPlaybackBeforeOneSpritePerScan(t *testing.T) {
	s, _, _ := setup(t)
	for _, p := range []string{"a.mp4", "b.mp4", "c.mp4"} {
		createFile(t, s.Config.MediaRoot, p)
	}
	sc := newTestScanner(t, s)
	cv := NewConverter(s.Config, s.DB, s.log)
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) { return retryMetadata(), nil }
	planned, sprites := 0, 0
	sc.Derive = func(ctx context.Context, m MediaRecord, md Metadata) error {
		planned++
		return cv.PlanFor(ctx, m, md)
	}
	sc.Sprite = func(context.Context, *os.File, float64, string) error {
		sprites++
		if planned != 3 {
			t.Fatalf("sprite started before all playback plans: %d", planned)
		}
		return nil
	}
	for pass := 1; pass <= 3; pass++ {
		if err := sc.Scan(context.Background()); err != nil {
			t.Fatal(err)
		}
		if sprites != pass || planned != 3 {
			t.Fatalf("pass=%d sprites=%d planned=%d", pass, sprites, planned)
		}
	}
	var remaining int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM media_retries").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("remaining=%d error=%v", remaining, err)
	}
}

func TestPersistentRetryChangedSourceDuringSprite(t *testing.T) {
	s, _, _ := setup(t)
	createFile(t, s.Config.MediaRoot, "changed.mp4")
	id := stableID("media", "changed.mp4")
	sc := newTestScanner(t, s)
	var target string
	sc.Sprite = func(_ context.Context, _ *os.File, _ float64, p string) error {
		target = p
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte("stale sheet"), 0600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(s.Config.MediaRoot, "changed.mp4"), []byte("source changed while generating sheet"), 0600)
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sc.deferred.Load() {
		t.Fatal("source change did not request an earlier scan")
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale sheet retained: %v", err)
	}
	r, _ := readMediaRetry(t, s.DB, id, "sprite")
	if r.Attempt != 0 {
		t.Fatalf("source change consumed attempt: %+v", r)
	}
}

func TestPersistentRetryChangedSourceDuringProbe(t *testing.T) {
	s, _, _ := setup(t)
	createFile(t, s.Config.MediaRoot, "changed.mp4")
	sc := newTestScanner(t, s)
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) {
		if err := os.WriteFile(filepath.Join(s.Config.MediaRoot, "changed.mp4"), []byte("changed while reading metadata"), 0600); err != nil {
			return Metadata{}, err
		}
		return Metadata{}, errors.New("probe failed on changing source")
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !sc.deferred.Load() || countMedia(t, s.DB) != 0 {
		t.Fatalf("source change: deferred=%v indexed=%d", sc.deferred.Load(), countMedia(t, s.DB))
	}
	if n := retryCount(t, s.DB, stableID("media", "changed.mp4")); n != 0 {
		t.Fatalf("source change recorded %d retries", n)
	}
}

func TestPersistentProbeRetrySurvivesDatabaseReopen(t *testing.T) {
	cfg := Config{MediaRoot: t.TempDir(), DataDir: t.TempDir(), ScanInterval: 15 * time.Minute}
	createFile(t, cfg.MediaRoot, "restart.mp4")
	db, err := OpenDB(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	sc := NewScanner(cfg, db, nil)
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) {
		return Metadata{}, errors.New("incomplete")
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	id := stableID("media", "restart.mp4")
	before, key := readMediaRetry(t, db, id, "probe")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenDB(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	after, afterKey := readMediaRetry(t, db, id, "probe")
	if before != after || key != afterKey {
		t.Fatal(before, after, key, afterKey)
	}
	sc = NewScanner(cfg, db, nil)
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) {
		t.Fatal("reopened scanner ignored persisted retry deadline")
		return Metadata{}, nil
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPersistentRetryCancellationPreservesSpriteAttempt(t *testing.T) {
	s, _, _ := setup(t)
	createFile(t, s.Config.MediaRoot, "cancel.mp4")
	id := stableID("media", "cancel.mp4")
	sc := newTestScanner(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc.Sprite = func(context.Context, *os.File, float64, string) error {
		cancel()
		return context.Canceled
	}
	if err := sc.Scan(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error=%v", err)
	}
	r, _ := readMediaRetry(t, s.DB, id, "sprite")
	if r.Attempt != 0 || r.Reason != "" {
		t.Fatalf("cancellation counted as failure: %+v", r)
	}
}

func TestPersistentRetryCancellationDoesNotRecordProbeFailure(t *testing.T) {
	s, _, _ := setup(t)
	createFile(t, s.Config.MediaRoot, "cancel.mp4")
	sc := newTestScanner(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) {
		cancel()
		return Metadata{}, context.Canceled
	}
	if err := sc.Scan(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error=%v", err)
	}
	if countMedia(t, s.DB) != 0 || retryCount(t, s.DB, stableID("media", "cancel.mp4")) != 0 {
		t.Fatal("canceled probe published a record or failure")
	}
}

func TestPersistentRetrySchedulerUsesEarliestActiveDeadline(t *testing.T) {
	s, _, _ := setup(t)
	createFile(t, s.Config.MediaRoot, "deadline.mp4")
	id := stableID("media", "deadline.mp4")
	sc := newTestScanner(t, s)
	sc.Config.ScanInterval = 15 * time.Minute
	sc.Sprite = func(context.Context, *os.File, float64, string) error { return errors.New("temporary") }
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	if _, err := s.DB.Exec("UPDATE media_retries SET next_ns=? WHERE media_id=?", deadline.UnixNano(), id); err != nil {
		t.Fatal(err)
	}
	if delay := sc.nextDelay(); delay < 19*time.Second || delay > 20*time.Second {
		t.Fatalf("earliest retry delay=%s", delay)
	}
	dueMediaRetry(t, s.DB, id, "sprite")
	if delay := sc.nextDelay(); delay != 5*time.Second {
		t.Fatalf("overdue delay=%s", delay)
	}
	if _, err := s.DB.Exec("UPDATE media_retries SET next_ns=0 WHERE media_id=?", id); err != nil {
		t.Fatal(err)
	}
	if delay := sc.nextDelay(); delay != 15*time.Minute {
		t.Fatalf("terminal retry affected delay=%s", delay)
	}
	sc.Config.ScanSettle = 30 * time.Second
	sc.deferred.Store(true)
	if delay := sc.nextDelay(); delay != 32*time.Second {
		t.Fatalf("settle delay=%s", delay)
	}
}

func TestPersistentProbeRetryNoVisualStreamIsUnsupported(t *testing.T) {
	s, _, _ := setup(t)
	createFile(t, s.Config.MediaRoot, "audio-only.mp4")
	id := stableID("media", "audio-only.mp4")
	sc := newTestScanner(t, s)
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) {
		return Metadata{Container: "mov,mp4", AudioCodecs: []string{"aac"}}, errNoVisualStream
	}
	cv := NewConverter(s.Config, s.DB, s.log)
	sc.Derive = cv.PlanFor
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.DB.QueryRow("SELECT status FROM playback_derivations WHERE media_id=?", id).Scan(&status); err != nil || status != "unsupported" {
		t.Fatalf("status=%q error=%v", status, err)
	}
	if retryCount(t, s.DB, id) != 0 {
		t.Fatal("confirmed no-visual outcome scheduled retries")
	}
}

func TestPersistentRetryReclassifiesWaitingSource(t *testing.T) {
	s, cv, m := conversionFixture(t)
	job, err := cv.claim(context.Background())
	if err != nil || job == nil {
		t.Fatal(job, err)
	}
	cv.Convert = func(_ context.Context, src *os.File, _, target string) error {
		st, err := src.Stat()
		if err != nil {
			return err
		}
		replacement := filepath.Join(t.TempDir(), "replacement.mkv")
		if err := os.WriteFile(replacement, make([]byte, st.Size()), 0600); err != nil {
			return err
		}
		if err := os.Chtimes(replacement, st.ModTime(), st.ModTime()); err != nil {
			return err
		}
		return os.Rename(replacement, filepath.Join(s.Config.MediaRoot, m.Path))
	}
	cv.process(context.Background(), *job)
	sc := newTestScanner(t, s)
	sc.Derive = cv.PlanFor
	probes := 0
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) {
		probes++
		return md("matroska,webm", "h264", "aac"), nil
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	next, err := cv.claim(context.Background())
	if err != nil || next == nil || probes != 1 {
		t.Fatal(next, probes, err)
	}
}

func TestPersistentProbeRetryRetainsFailedPlaybackEnqueue(t *testing.T) {
	s, _, _ := setup(t)
	createFile(t, s.Config.MediaRoot, "enqueue.mp4")
	id := stableID("media", "enqueue.mp4")
	sc := newTestScanner(t, s)
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) { return retryMetadata(), nil }
	cv := NewConverter(s.Config, s.DB, s.log)
	calls := 0
	sc.Derive = func(ctx context.Context, m MediaRecord, md Metadata) error {
		calls++
		if calls == 1 {
			return errors.New("enqueue temporarily unavailable")
		}
		return cv.PlanFor(ctx, m, md)
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, _ := readMediaRetry(t, s.DB, id, "probe")
	if r.Attempt != 1 || r.Next == 0 {
		t.Fatalf("enqueue retry=%+v", r)
	}
	dueMediaRetry(t, s.DB, id, "probe")
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := s.DB.QueryRow("SELECT status FROM playback_derivations WHERE media_id=?", id).Scan(&status); err != nil || status != "ready" || calls != 2 {
		t.Fatalf("status=%q enqueue calls=%d error=%v", status, calls, err)
	}
}
