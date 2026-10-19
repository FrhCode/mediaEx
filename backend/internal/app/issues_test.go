package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

type issuePage struct {
	Tab    string      `json:"tab"`
	Counts issueCounts `json:"counts"`
	Total  int         `json:"total"`
	Items  []issueItem `json:"items"`
}

func readIssuePage(t *testing.T, b []byte) issuePage {
	t.Helper()
	var p issuePage
	if e := json.Unmarshal(b, &p); e != nil {
		t.Fatal(e)
	}
	return p
}

func countRows(t *testing.T, s *Server, query string, args ...any) int {
	t.Helper()
	var n int
	if e := s.DB.QueryRow(query, args...).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

func TestSkipStopsUnprobedFileAndUnskipRestoresIt(t *testing.T) {
	s, ts, c := setup(t)
	createFile(t, s.Config.MediaRoot, "broken.mp4")
	id := stableID("media", "broken.mp4")
	sc := newTestScanner(t, s)
	calls := 0
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) {
		calls++
		return Metadata{}, errors.New("source cannot be read")
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || countRows(t, s, "SELECT COUNT(*) FROM media_retries WHERE media_id=?", id) != 1 {
		t.Fatalf("probe calls=%d", calls)
	}
	loginTest(t, c, ts.URL, "admin", "admin-password-123")

	req(t, c, ts.URL, "POST", "/api/admin/issues/"+id+"/skip", map[string]any{"reason": "corrupt"}, 200)
	if n := countRows(t, s, "SELECT COUNT(*) FROM media_retries WHERE media_id=?", id); n != 0 {
		t.Fatalf("retries left behind: %d", n)
	}
	if n := countRows(t, s, "SELECT COUNT(*) FROM media_skips WHERE media_id=?", id); n != 1 {
		t.Fatalf("skip row missing: %d", n)
	}
	// The failed file had no metadata, so it disappears from the library until restored.
	library := req(t, c, ts.URL, "GET", "/api/library", nil, 200)
	var view struct {
		Items []Media `json:"items"`
	}
	if err := json.Unmarshal(library, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Items) != 0 {
		t.Fatalf("skipped unprobed item still listed: %+v", view.Items)
	}
	page := readIssuePage(t, req(t, c, ts.URL, "GET", "/api/admin/issues?tab=skipped", nil, 200))
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Reason != "corrupt" || page.Counts.Skipped != 1 {
		t.Fatalf("skipped page=%+v", page)
	}
	if page.Items[0].Actor != "admin" {
		t.Fatalf("skipped actor=%q", page.Items[0].Actor)
	}
	req(t, c, ts.URL, "POST", "/api/admin/issues/"+id+"/retry", map[string]any{}, 409)
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("skipped file was probed again: calls=%d", calls)
	}

	req(t, c, ts.URL, "DELETE", "/api/admin/issues/"+id+"/skip", nil, 200)
	if n := countRows(t, s, "SELECT COUNT(*) FROM media_skips WHERE media_id=?", id); n != 0 {
		t.Fatalf("skip row survived unskip: %d", n)
	}
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) {
		calls++
		return retryMetadata(), nil
	}
	if err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("unskipped file was not reprocessed: calls=%d", calls)
	}
	raw := req(t, c, ts.URL, "GET", "/api/library", nil, 200)
	var libraryBody struct {
		Items []Media `json:"items"`
	}
	if err := json.Unmarshal(raw, &libraryBody); err != nil {
		t.Fatal(err)
	}
	if len(libraryBody.Items) != 1 {
		t.Fatalf("restored item missing from library: %+v", libraryBody.Items)
	}
}

func TestSkipKeepsPlayableItemInLibraryAndCancelsQueue(t *testing.T) {
	s, ts, c := setup(t)
	id := addMedia(t, s, "movie.mp4", "video")
	_, err := s.DB.Exec("INSERT INTO playback_derivations(media_id,source_key,strategy,status,artifact,reason,attempt,updated_ns) VALUES(?,'key','transcode_full','pending','','',0,?)", id, time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	page := readIssuePage(t, req(t, c, ts.URL, "GET", "/api/admin/issues?tab=queue", nil, 200))
	if page.Total != 1 || page.Items[0].Stage != "queue" || page.Counts.Queue != 1 {
		t.Fatalf("queue page=%+v", page)
	}
	req(t, c, ts.URL, "POST", "/api/admin/issues/"+id+"/skip", map[string]any{}, 200)
	if n := countRows(t, s, "SELECT COUNT(*) FROM playback_derivations WHERE media_id=?", id); n != 0 {
		t.Fatalf("queued derivation survived skip: %d", n)
	}
	cv := NewConverter(s.Config, s.DB, s.log)
	job, err := cv.claim(context.Background())
	if err != nil || job != nil {
		t.Fatalf("skipped derivation was claimed: job=%+v error=%v", job, err)
	}
	library := req(t, c, ts.URL, "GET", "/api/library", nil, 200)
	var body struct {
		Items []Media `json:"items"`
	}
	if err := json.Unmarshal(library, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("probed item was hidden from library: %+v", body.Items)
	}
}

func TestRetryFailedConversionQueuesItAgain(t *testing.T) {
	s, ts, c := setup(t)
	id := addMedia(t, s, "movie.mp4", "video")
	now := time.Now().UnixNano()
	if _, err := s.DB.Exec("INSERT INTO playback_derivations(media_id,source_key,strategy,status,artifact,reason,attempt,updated_ns) VALUES(?,'key','transcode_full','failed','','encoder crashed',3,?)", id, now); err != nil {
		t.Fatal(err)
	}
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	page := readIssuePage(t, req(t, c, ts.URL, "GET", "/api/admin/issues", nil, 200))
	if page.Total != 1 || page.Items[0].Stage != "conversion" || page.Counts.Conversions != 1 {
		t.Fatalf("failed page=%+v", page)
	}
	if page.Items[0].Max == nil || *page.Items[0].Max != derivMaxAttempts {
		t.Fatalf("max attempts=%v", page.Items[0].Max)
	}
	req(t, c, ts.URL, "POST", "/api/admin/issues/"+id+"/retry", map[string]any{}, 200)
	var status string
	var attempt int
	if err := s.DB.QueryRow("SELECT status,attempt FROM playback_derivations WHERE media_id=?", id).Scan(&status, &attempt); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || attempt != 0 {
		t.Fatalf("retry status=%s attempt=%d", status, attempt)
	}
	if n := countRows(t, s, "SELECT COUNT(*) FROM media_retries WHERE media_id=? AND stage='probe'", id); n != 1 {
		t.Fatalf("probe retry not scheduled: %d", n)
	}
}

func TestBulkSkipAndUnskip(t *testing.T) {
	s, ts, c := setup(t)
	first := addMedia(t, s, "one.mp4", "video")
	second := addMedia(t, s, "two.mp4", "video")
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	b := req(t, c, ts.URL, "POST", "/api/admin/issues/skip", map[string]any{"ids": []string{first, second, "missing"}, "reason": "batch"}, 200)
	var result struct {
		Skipped int `json:"skipped"`
	}
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	if result.Skipped != 2 {
		t.Fatalf("bulk skipped=%d", result.Skipped)
	}
	if n := countRows(t, s, "SELECT COUNT(*) FROM media_skips"); n != 2 {
		t.Fatalf("skip rows=%d", n)
	}
	page := readIssuePage(t, req(t, c, ts.URL, "GET", "/api/admin/issues?tab=skipped", nil, 200))
	if page.Total != 2 || page.Counts.Skipped != 2 {
		t.Fatalf("skipped page=%+v", page)
	}
	req(t, c, ts.URL, "DELETE", "/api/admin/issues/"+first+"/skip", nil, 200)
	if n := countRows(t, s, "SELECT COUNT(*) FROM media_skips"); n != 1 {
		t.Fatalf("skip rows after restore=%d", n)
	}
}
