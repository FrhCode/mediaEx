package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func setup(t *testing.T) (*Server, *httptest.Server, *http.Client) {
	t.Helper()
	c := Config{MediaRoot: t.TempDir(), DataDir: t.TempDir(), ScanInterval: time.Minute, SessionTTL: time.Hour, ToolTimeout: 30 * time.Second, SpriteTimeout: 2 * time.Minute}
	db, e := OpenDB(c.DataDir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	if e = Bootstrap(context.Background(), db, "admin", "admin-password-123"); e != nil {
		t.Fatal(e)
	}
	s := NewServer(c, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Bind localhost, not 127.0.0.1: an IP literal is not a valid WebAuthn RP ID.
	ts := httptest.NewUnstartedServer(s.Handler())
	l, e := net.Listen("tcp", "localhost:0")
	if e != nil {
		t.Fatal(e)
	}
	ts.Listener = l
	ts.Start()
	if _, port, e := net.SplitHostPort(ts.Listener.Addr().String()); e == nil {
		ts.URL = "http://localhost:" + port
	}
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	return s, ts, client
}
func req(t *testing.T, c *http.Client, base, method, path string, body any, want int) []byte {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			t.Fatal(e)
		}
		reader = bytes.NewReader(b)
	}
	r, e := http.NewRequest(method, base+path, reader)
	if e != nil {
		t.Fatal(e)
	}
	r.Header.Set("Origin", base)
	r.Header.Set("Content-Type", "application/json")
	res, e := c.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	b, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	if res.StatusCode != want {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, path, res.StatusCode, want, b)
	}
	return b
}
func loginTest(t *testing.T, c *http.Client, base, user, password string) {
	t.Helper()
	req(t, c, base, "POST", "/api/login", map[string]any{"username": user, "password": password}, 200)
}
func parseUser(t *testing.T, b []byte) User {
	t.Helper()
	var u User
	if e := json.Unmarshal(b, &u); e != nil {
		t.Fatal(e)
	}
	return u
}
func addMedia(t *testing.T, s *Server, p, kind string) string {
	t.Helper()
	full := filepath.Join(s.Config.MediaRoot, p)
	if e := os.MkdirAll(filepath.Dir(full), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(full, []byte("0123456789abcdefghijklmnopqrstuvwxyz"), 0600); e != nil {
		t.Fatal(e)
	}
	id := stableID("media", p)
	_, e := s.DB.Exec("INSERT INTO media VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", id, p, "", filepath.Base(p), kind, "video/mp4", "MP4", 36, 0, 1920, 1080, 30, "")
	if e != nil {
		t.Fatal(e)
	}
	return id
}
func TestAuthAdminAndRevocation(t *testing.T) {
	s, ts, c := setup(t)
	if b := req(t, c, ts.URL, "GET", "/api/session", nil, 200); strings.TrimSpace(string(b)) != "null" {
		t.Fatal(string(b))
	}
	req(t, c, ts.URL, "GET", "/api/library", nil, 401)
	req(t, c, ts.URL, "POST", "/api/login", map[string]string{"username": "admin", "password": "wrong"}, 401)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	if admin.Role != "admin" {
		t.Fatal(admin)
	}
	for _, b := range []map[string]any{{"active": false}, {"role": "user"}} {
		req(t, c, ts.URL, "PATCH", "/api/admin/users/"+admin.ID, b, 409)
	}
	req(t, c, ts.URL, "DELETE", "/api/admin/users/"+admin.ID, nil, 409)
	u := parseUser(t, req(t, c, ts.URL, "POST", "/api/admin/users", map[string]any{"username": "alice", "name": "Alice", "password": "alice-password-123"}, 201))
	req(t, c, ts.URL, "POST", "/api/admin/users", map[string]any{"username": "ALICE", "password": "alice-password-123"}, 409)
	jar, _ := cookiejar.New(nil)
	alice := &http.Client{Jar: jar}
	loginTest(t, alice, ts.URL, "alice", "alice-password-123")
	req(t, alice, ts.URL, "GET", "/api/admin/users", nil, 403)
	id := addMedia(t, s, "video.mp4", "video")
	req(t, alice, ts.URL, "GET", "/api/items/"+id+"/content", nil, 200)
	req(t, c, ts.URL, "PATCH", "/api/admin/users/"+u.ID, map[string]any{"active": false}, 200)
	req(t, alice, ts.URL, "GET", "/api/library", nil, 401)
	req(t, alice, ts.URL, "GET", "/api/items/"+id+"/content", nil, 401)
	req(t, alice, ts.URL, "GET", "/api/items/"+id+"/thumbnail", nil, 401)
	req(t, alice, ts.URL, "POST", "/api/login", map[string]any{"username": "alice", "password": "alice-password-123"}, 401)
	req(t, c, ts.URL, "PATCH", "/api/admin/users/"+u.ID, map[string]any{"active": true}, 200)
	loginTest(t, alice, ts.URL, "alice", "alice-password-123")
	req(t, c, ts.URL, "PATCH", "/api/admin/users/"+u.ID, map[string]any{"password": "alice-new-password"}, 200)
	req(t, alice, ts.URL, "GET", "/api/library", nil, 401)
	loginTest(t, alice, ts.URL, "alice", "alice-new-password")
	req(t, c, ts.URL, "DELETE", "/api/admin/users/"+u.ID, nil, 200)
	req(t, alice, ts.URL, "GET", "/api/library", nil, 401)
	if _, e := os.Stat(filepath.Join(s.Config.MediaRoot, "video.mp4")); e != nil {
		t.Fatal("account delete touched NAS", e)
	}
	req(t, c, ts.URL, "POST", "/api/logout", nil, 200)
	req(t, c, ts.URL, "GET", "/api/library", nil, 401)
}
func TestSessionExpiryBootstrapAndCSRF(t *testing.T) {
	s, ts, c := setup(t)
	if e := Bootstrap(context.Background(), s.DB, "bad", "short"); e != nil {
		t.Fatal("bootstrap must ignore credentials after initialization", e)
	}
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	r, _ := http.NewRequest("POST", ts.URL+"/api/logout", nil)
	r.Header.Set("Origin", "https://evil.invalid")
	res, e := c.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.StatusCode)
	}
	r, _ = http.NewRequest("POST", ts.URL+"/api/logout", nil)
	res, e = c.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.StatusCode)
	}
	if _, e = s.DB.Exec("UPDATE sessions SET expires_at=0"); e != nil {
		t.Fatal(e)
	}
	req(t, c, ts.URL, "GET", "/api/library", nil, 401)
}
func TestContentRangesProgressAndContainment(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	id := addMedia(t, s, "video.mp4", "video")
	for _, tc := range []struct {
		method, rangeHeader string
		status              int
		body                string
	}{{"GET", "bytes=2-5", 206, "2345"}, {"HEAD", "", 200, ""}, {"GET", "bytes=999-", 416, ""}} {
		r, _ := http.NewRequest(tc.method, ts.URL+"/api/items/"+id+"/content", nil)
		r.Header.Set("Range", tc.rangeHeader)
		res, e := c.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatalf("range: %d %s", res.StatusCode, b)
		}
		if tc.body != "" && string(b) != tc.body {
			t.Fatal(string(b))
		}
	}
	for _, p := range []float64{12, 5} {
		req(t, c, ts.URL, "PUT", "/api/items/"+id+"/progress", map[string]float64{"progress": p}, 200)
	}
	var m Media
	_ = json.Unmarshal(req(t, c, ts.URL, "GET", "/api/items/"+id, nil, 200), &m)
	if m.Progress != 5 {
		t.Fatal(m)
	}
	u := parseUser(t, req(t, c, ts.URL, "POST", "/api/admin/users", map[string]any{"username": "other", "password": "other-password-123"}, 201))
	_ = u
	jar, _ := cookiejar.New(nil)
	other := &http.Client{Jar: jar}
	loginTest(t, other, ts.URL, "other", "other-password-123")
	_ = json.Unmarshal(req(t, other, ts.URL, "GET", "/api/items/"+id, nil, 200), &m)
	if m.Progress != 0 {
		t.Fatal("progress leaked", m.Progress)
	}
	req(t, c, ts.URL, "PUT", "/api/items/"+id+"/progress", map[string]any{}, 400)
	req(t, c, ts.URL, "PUT", "/api/items/"+id+"/progress", map[string]any{"progress": 31}, 200)
	_ = json.Unmarshal(req(t, c, ts.URL, "GET", "/api/items/"+id, nil, 200), &m)
	if m.Progress != 0 {
		t.Fatal("completed video must start from beginning", m.Progress)
	}
	out := filepath.Join(t.TempDir(), "private.mp4")
	_ = os.WriteFile(out, []byte("private"), 0600)
	_ = os.Remove(filepath.Join(s.Config.MediaRoot, "video.mp4"))
	if e := os.Symlink(out, filepath.Join(s.Config.MediaRoot, "video.mp4")); e != nil {
		t.Fatal(e)
	}
	req(t, c, ts.URL, "GET", "/api/items/"+id+"/content", nil, 404)
}
func fakeProcessor(ctx context.Context, f *os.File, kind, target string) (Metadata, error) {
	return Metadata{Width: 1920, Height: 1080, Duration: 30}, nil
}
func newTestScanner(t *testing.T, s *Server) *Scanner {
	t.Helper()
	sc := NewScanner(s.Config, s.DB, s.log)
	sc.Process = fakeProcessor
	sc.Sprite = func(context.Context, *os.File, float64, string) error { return nil }
	return sc
}
func countMedia(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if e := db.QueryRow("SELECT COUNT(*) FROM media").Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func createFile(t *testing.T, root, p string) {
	t.Helper()
	f := filepath.Join(root, p)
	if e := os.MkdirAll(filepath.Dir(f), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(f, []byte("sample"), 0600); e != nil {
		t.Fatal(e)
	}
}
func TestScannerRecursiveStableAndMissingRoot(t *testing.T) {
	s, ts, c := setup(t)
	createFile(t, s.Config.MediaRoot, "Movies/Trips/one.mp4")
	createFile(t, s.Config.MediaRoot, "cover.jpg")
	sc := newTestScanner(t, s)
	if e := sc.Scan(context.Background()); e != nil {
		t.Fatal(e)
	}
	if n := countMedia(t, s.DB); n != 2 {
		t.Fatal(n)
	}
	if e := sc.Scan(context.Background()); e != nil {
		t.Fatal(e)
	}
	if n := countMedia(t, s.DB); n != 2 {
		t.Fatal(n)
	}
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	var l struct {
		Items       []Media  `json:"items"`
		Navigation  []Folder `json:"navigation"`
		Breadcrumbs []Folder `json:"breadcrumbs"`
	}
	_ = json.Unmarshal(req(t, c, ts.URL, "GET", "/api/library", nil, 200), &l)
	if len(l.Items) != 2 || len(l.Navigation) != 1 {
		t.Fatal(l)
	}
	folder := stableID("folder", "Movies/Trips")
	_ = json.Unmarshal(req(t, c, ts.URL, "GET", "/api/library?folder="+folder, nil, 200), &l)
	if len(l.Items) != 1 || len(l.Breadcrumbs) != 2 {
		t.Fatal(l)
	}
	old := s.Config.MediaRoot
	s.Config.MediaRoot = filepath.Join(t.TempDir(), "missing")
	sc.Config = s.Config
	if e := sc.Scan(context.Background()); e == nil {
		t.Fatal("missing root should fail")
	}
	if countMedia(t, s.DB) != 2 {
		t.Fatal("missing root deleted catalogue")
	}
	s.Config.MediaRoot = old
	sc.Config = s.Config
	_ = os.Remove(filepath.Join(old, "cover.jpg"))
	if e := sc.Scan(context.Background()); e != nil {
		t.Fatal(e)
	}
	if countMedia(t, s.DB) != 1 {
		t.Fatal("missing file not removed")
	}
}
func TestLibraryScopes(t *testing.T) {
	s, ts, c := setup(t)
	for _, path := range []string{"root.jpg", "Movies/movie.mp4", "Movies/Trips/trip.mp4", "Family/photo.jpg"} {
		createFile(t, s.Config.MediaRoot, path)
	}
	if err := newTestScanner(t, s).Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	movies := stableID("folder", "Movies")
	trips := stableID("folder", "Movies/Trips")
	for _, tc := range []struct {
		name, path           string
		items                []string
		folders, breadcrumbs int
	}{
		{"feed", "?scope=all", []string{"movie.mp4", "photo.jpg", "root.jpg", "trip.mp4"}, 2, 0},
		{"root", "?scope=folder", []string{"root.jpg"}, 2, 0},
		{"direct", "?scope=folder&folder=" + movies, []string{"movie.mp4"}, 1, 1},
		{"nested", "?scope=folder&folder=" + trips, []string{"trip.mp4"}, 0, 2},
		{"global search", "?scope=all&q=trip", []string{"trip.mp4"}, 0, 0},
		{"root search", "?scope=folder&q=trip", []string{"trip.mp4"}, 0, 0},
		{"subtree search", "?scope=folder&folder=" + movies + "&q=trip", []string{"trip.mp4"}, 0, 1},
		{"sibling excluded", "?scope=folder&folder=" + movies + "&q=photo", []string{}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result struct {
				Items       []Media  `json:"items"`
				Folders     []Folder `json:"folders"`
				Breadcrumbs []Folder `json:"breadcrumbs"`
			}
			if err := json.Unmarshal(req(t, c, ts.URL, "GET", "/api/library"+tc.path, nil, 200), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Items) != len(tc.items) || len(result.Folders) != tc.folders || len(result.Breadcrumbs) != tc.breadcrumbs {
				t.Fatalf("unexpected result: %+v", result)
			}
			for i, name := range tc.items {
				if result.Items[i].Name != name {
					t.Fatalf("got %q want %q", result.Items[i].Name, name)
				}
			}
		})
	}
	req(t, c, ts.URL, "GET", "/api/library?scope=invalid", nil, 400)
	req(t, c, ts.URL, "GET", "/api/library?scope=all&folder="+movies, nil, 400)
	req(t, c, ts.URL, "GET", "/api/library?scope=folder&folder=missing", nil, 404)
}

func TestScannerSettleMarkerFailureAndOverlap(t *testing.T) {
	s, _, _ := setup(t)
	root := s.Config.MediaRoot
	createFile(t, root, "one.mp4")
	createFile(t, root, ".ready")
	sc := newTestScanner(t, s)
	sc.Config.MediaMarker = ".ready"
	if e := sc.Scan(context.Background()); e != nil {
		t.Fatal(e)
	}
	sc.Config.ScanSettle = time.Hour
	createFile(t, root, "two.mp4")
	if e := sc.Scan(context.Background()); e != nil {
		t.Fatal(e)
	}
	if countMedia(t, s.DB) != 1 {
		t.Fatal("copying file indexed")
	}
	_ = os.Remove(filepath.Join(root, "one.mp4"))
	_ = os.Remove(filepath.Join(root, ".ready"))
	if e := sc.Scan(context.Background()); e == nil {
		t.Fatal("missing marker accepted")
	}
	if countMedia(t, s.DB) != 1 {
		t.Fatal("missing marker deleted catalogue")
	}
	createFile(t, root, ".ready")
	sc.Config.ScanSettle = 0
	entered := make(chan struct{})
	release := make(chan struct{})
	sc.Process = func(context.Context, *os.File, string, string) (Metadata, error) {
		close(entered)
		<-release
		return Metadata{}, errors.New("corrupt")
	}
	done := make(chan error)
	go func() { done <- sc.Scan(context.Background()) }()
	<-entered
	if e := sc.Scan(context.Background()); !errors.Is(e, ErrScanRunning) {
		t.Fatal(e)
	}
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if countMedia(t, s.DB) != 1 {
		t.Fatal("corrupt item should be visible")
	}
}

// A fresh install copies its library and starts the server immediately, so the first
// scan sees files younger than the settle window. Those must not leave the library
// empty until the next scheduled interval.
func TestScannerRetriesSoonAfterDeferredFiles(t *testing.T) {
	s, _, _ := setup(t)
	sc := newTestScanner(t, s)
	sc.Config.ScanInterval = 15 * time.Minute
	sc.Config.ScanSettle = 30 * time.Second
	if delay := sc.nextDelay(); delay != 15*time.Minute {
		t.Fatalf("idle delay = %v, want the scan interval", delay)
	}
	createFile(t, s.Config.MediaRoot, "fresh.mp4")
	if e := sc.Scan(context.Background()); e != nil {
		t.Fatal(e)
	}
	if n := countMedia(t, s.DB); n != 0 {
		t.Fatalf("file younger than the settle window was indexed: %d", n)
	}
	if delay := sc.nextDelay(); delay != 32*time.Second {
		t.Fatalf("deferred file must pull the next scan forward, got %v", delay)
	}
	sc.Config.ScanSettle = time.Hour
	if delay := sc.nextDelay(); delay != 15*time.Minute {
		t.Fatalf("delay must never exceed the scan interval, got %v", delay)
	}
	sc.Config.ScanSettle = time.Second
	if delay := sc.nextDelay(); delay != 5*time.Second {
		t.Fatalf("delay must stay above the minimum, got %v", delay)
	}
}
func TestScannerPartialTraversalDoesNotDelete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires normal user permissions")
	}
	s, _, _ := setup(t)
	root := s.Config.MediaRoot
	createFile(t, root, "private/one.mp4")
	createFile(t, root, "gone.mp4")
	sc := newTestScanner(t, s)
	if e := sc.Scan(context.Background()); e != nil {
		t.Fatal(e)
	}
	_ = os.Remove(filepath.Join(root, "gone.mp4"))
	dir := filepath.Join(root, "private")
	if e := os.Chmod(dir, 0); e != nil {
		t.Fatal(e)
	}
	defer os.Chmod(dir, 0700)
	if e := sc.Scan(context.Background()); e == nil {
		t.Fatal("unreadable subtree must report failure")
	}
	if countMedia(t, s.DB) != 2 {
		t.Fatal("partial traversal deleted catalogue")
	}
}
func TestConcurrentFinalAdminGuard(t *testing.T) {
	_, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	a := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	b := parseUser(t, req(t, c, ts.URL, "POST", "/api/admin/users", map[string]any{"username": "admin2", "password": "admin2-password", "role": "admin"}, 201))
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for _, id := range []string{a.ID, b.ID} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			r, _ := http.NewRequest("DELETE", ts.URL+"/api/admin/users/"+id, nil)
			r.Header.Set("Origin", ts.URL)
			res, e := c.Do(r)
			if e != nil {
				results <- 0
				return
			}
			res.Body.Close()
			results <- res.StatusCode
		}(id)
	}
	wg.Wait()
	close(results)
	success := 0
	for status := range results {
		if status == 200 {
			success++
		} else if status != 409 && status != 401 {
			t.Fatal(status)
		}
	}
	if success != 1 {
		t.Fatal("expected one admin removal", success)
	}
}
func TestPersistenceAndConfig(t *testing.T) {
	dir := t.TempDir()
	db, e := OpenDB(dir)
	if e != nil {
		t.Fatal(e)
	}
	if e = Bootstrap(context.Background(), db, "admin", "admin-password-123"); e != nil {
		t.Fatal(e)
	}
	db.Close()
	db, e = OpenDB(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n)
	if n != 1 {
		t.Fatal(n)
	}
	t.Setenv("SCAN_INTERVAL", "2s")
	t.Setenv("SCAN_SETTLE", "0s")
	t.Setenv("MEDIA_ROOT", t.TempDir())
	t.Setenv("DATA_DIR", t.TempDir())
	c, e := LoadConfig()
	if e != nil || c.ScanInterval != 2*time.Second || c.ScanSettle != 0 {
		t.Fatal(c, e)
	}
	t.Setenv("MEDIA_MARKER", "../escape")
	if _, e = LoadConfig(); e == nil {
		t.Fatal("unsafe marker accepted")
	}
}
func TestLoginThrottle(t *testing.T) {
	_, ts, c := setup(t)
	for i := 0; i < 10; i++ {
		req(t, c, ts.URL, "POST", "/api/login", map[string]string{"username": "missing", "password": "wrong"}, 401)
	}
	req(t, c, ts.URL, "POST", "/api/login", map[string]string{"username": "admin", "password": "admin-password-123"}, 429)
}
func TestScannerSymlinkAndCancellation(t *testing.T) {
	s, _, _ := setup(t)
	createFile(t, s.Config.MediaRoot, "one.mp4")
	outside := t.TempDir()
	createFile(t, outside, "secret.mp4")
	if e := os.Symlink(outside, filepath.Join(s.Config.MediaRoot, "external")); e != nil {
		t.Fatal(e)
	}
	sc := newTestScanner(t, s)
	if e := sc.Scan(context.Background()); e != nil {
		t.Fatal(e)
	}
	if countMedia(t, s.DB) != 1 {
		t.Fatal("followed symlink")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := sc.Scan(ctx); !errors.Is(e, context.Canceled) {
		t.Fatal(fmt.Sprint(e))
	}
}
