package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sessionHash returns the opaque session hash a user's current login is keyed by.
func sessionHash(t *testing.T, s *Server, userID string) string {
	t.Helper()
	var h string
	if e := s.DB.QueryRow("SELECT token_hash FROM sessions WHERE user_id=?", userID).Scan(&h); e != nil {
		t.Fatal(e)
	}
	return h
}

// lockSession enables the policy directly, standing in for a registration ceremony.
func lockSession(t *testing.T, s *Server, userID, hash string) {
	t.Helper()
	if _, e := s.DB.Exec("INSERT INTO session_locks(session_hash,user_id,enabled,created_at) VALUES(?,?,1,unixepoch())", hash, userID); e != nil {
		t.Fatal(e)
	}
}

// seedGrant writes a grant with an explicit lifetime, so expiry needs no sleeping.
func seedGrant(t *testing.T, s *Server, userID, hash string, ttl time.Duration) string {
	t.Helper()
	token := randomID()
	now := time.Now()
	if _, e := s.DB.Exec("INSERT INTO unlock_grants(token_hash,session_hash,user_id,created_at,expires_at) VALUES(?,?,?,?,?)", grantHash(token), hash, userID, now.Unix(), now.Add(ttl).Unix()); e != nil {
		t.Fatal(e)
	}
	return token
}

func grantCount(t *testing.T, s *Server, hash string) int {
	t.Helper()
	var n int
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM unlock_grants WHERE session_hash=?", hash).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

func grantRowExpiry(t *testing.T, s *Server, token string) int64 {
	t.Helper()
	var expires int64
	if e := s.DB.QueryRow("SELECT expires_at FROM unlock_grants WHERE token_hash=?", grantHash(token)).Scan(&expires); e != nil {
		t.Fatal(e)
	}
	return expires
}

// epochOf reports the session's ceremony epoch, 0 when none has been bumped yet.
func epochOf(t *testing.T, s *Server, hash string) int64 {
	t.Helper()
	var epoch int64
	if e := s.DB.QueryRow("SELECT COALESCE((SELECT epoch FROM session_lock_epochs WHERE session_hash=?),0)", hash).Scan(&epoch); e != nil {
		t.Fatal(e)
	}
	return epoch
}

func progressCount(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM playback_progress").Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}

// unlockCookieOf returns the unlock cookie a response set, or nil when it set none.
func unlockCookieOf(res *http.Response) *http.Cookie {
	for _, c := range res.Cookies() {
		if c.Name == unlockCookie {
			return c
		}
	}
	return nil
}

func readBody(t *testing.T, res *http.Response) []byte {
	t.Helper()
	defer res.Body.Close()
	b, e := io.ReadAll(res.Body)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// unlockReq builds a request with the client's jar plus an explicit unlock cookie.
func unlockReq(t *testing.T, base, method, path string, body any, token string) *http.Request {
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
	if token != "" {
		r.AddCookie(&http.Cookie{Name: unlockCookie, Value: token})
	}
	return r
}

func do(t *testing.T, c *http.Client, r *http.Request) *http.Response {
	t.Helper()
	res, e := c.Do(r)
	if e != nil {
		t.Fatal(e)
	}
	return res
}

func doUnlock(t *testing.T, c *http.Client, base, method, path string, body any, token string) *http.Response {
	t.Helper()
	return do(t, c, unlockReq(t, base, method, path, body, token))
}

// reqGrant is req with the unlock cookie presented explicitly.
func reqGrant(t *testing.T, c *http.Client, base, method, path string, body any, grant string, want int) []byte {
	t.Helper()
	res := doUnlock(t, c, base, method, path, body, grant)
	b := readBody(t, res)
	if res.StatusCode != want {
		t.Fatalf("%s %s status=%d want=%d body=%s", method, path, res.StatusCode, want, b)
	}
	return b
}

func bodyCode(t *testing.T, b []byte) string {
	t.Helper()
	var v struct {
		Code string `json:"code"`
	}
	if e := json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	return v.Code
}

// The session DTO carries the lock fields, including on a fresh sign-in.
func TestLockSessionDTOAndLogin(t *testing.T) {
	s, ts, c := setup(t)
	s.Config.PublicOrigin = ts.URL
	b := req(t, c, ts.URL, "POST", "/api/login", map[string]any{"username": "admin", "password": "admin-password-123"}, 200)
	var sess Session
	if e := json.Unmarshal(b, &sess); e != nil {
		t.Fatal(e)
	}
	if sess.ID == "" || sess.LockEnabled || sess.HasPasskey || !sess.PasskeyAvailable {
		t.Fatalf("login DTO=%+v", sess)
	}
	if !bytes.Contains(b, []byte(`"lockEnabled"`)) || !bytes.Contains(b, []byte(`"passkeyAvailable"`)) {
		t.Fatalf("login body lacks lock fields: %s", b)
	}
}

// Lock on makes content and admin routes demand a live unlock cookie: 423 APP_LOCKED, not 401.
func TestLockGateRequiresGrant(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	id := addMedia(t, s, "video.mp4", "video")

	req(t, c, ts.URL, "GET", "/api/items/"+id+"/content", nil, 200)
	req(t, c, ts.URL, "GET", "/api/library", nil, 200)
	req(t, c, ts.URL, "GET", "/api/admin/users", nil, 200)

	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)

	// A signed-in session without an unlock cookie is locked out of every guarded route.
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/library"},
		{"GET", "/api/items/" + id},
		{"PUT", "/api/items/" + id + "/progress"},
		{"GET", "/api/items/" + id + "/content"},
		{"GET", "/api/items/" + id + "/thumbnail"},
		{"GET", "/api/items/" + id + "/sprite"},
		{"GET", "/api/admin/users"},
	} {
		b := reqGrant(t, c, ts.URL, rt.method, rt.path, nil, "", 423)
		if code := bodyCode(t, b); code != "APP_LOCKED" {
			t.Fatalf("%s %s code=%q body=%s", rt.method, rt.path, code, b)
		}
	}
	reqGrant(t, c, ts.URL, "GET", "/api/items/"+id+"/content", nil, strings.Repeat("a", 64), 423)

	var locked Session
	if e := json.Unmarshal(req(t, c, ts.URL, "GET", "/api/session", nil, 200), &locked); e != nil {
		t.Fatal(e)
	}
	if !locked.LockEnabled {
		t.Fatalf("session should report the lock policy: %+v", locked)
	}

	live := seedGrant(t, s, admin.ID, hash, time.Minute)
	reqGrant(t, c, ts.URL, "GET", "/api/items/"+id+"/content", nil, live, 200)
	reqGrant(t, c, ts.URL, "GET", "/api/library", nil, live, 200)

	// The retired header is not a bypass: a real grant sent as X-Mediaex-Grant stays locked.
	r := unlockReq(t, ts.URL, "GET", "/api/items/"+id+"/content", nil, "")
	r.Header.Set("X-Mediaex-Grant", live)
	res := do(t, c, r)
	body := readBody(t, res)
	if res.StatusCode != 423 {
		t.Fatalf("grant header accepted: status=%d body=%s", res.StatusCode, body)
	}
	if code := bodyCode(t, body); code != "APP_LOCKED" {
		t.Fatalf("code=%q body=%s", code, body)
	}
}

// A grant is bound to its session and lease: foreign or expired grants never open the gate.
func TestLockGrantSessionBindingAndLease(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	id := addMedia(t, s, "video.mp4", "video")
	u := parseUser(t, req(t, c, ts.URL, "POST", "/api/admin/users", map[string]any{"username": "alice", "password": "alice-password-123"}, 201))
	jar, _ := cookiejar.New(nil)
	alice := &http.Client{Jar: jar}
	loginTest(t, alice, ts.URL, "alice", "alice-password-123")

	adminHash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, adminHash)

	live := seedGrant(t, s, admin.ID, adminHash, time.Minute)
	reqGrant(t, c, ts.URL, "GET", "/api/items/"+id+"/content", nil, live, 200)

	// The same cookie value is worthless in the other account's session.
	aliceHash := sessionHash(t, s, u.ID)
	lockSession(t, s, u.ID, aliceHash)
	aliceGrant := seedGrant(t, s, u.ID, aliceHash, time.Minute)

	reqGrant(t, alice, ts.URL, "GET", "/api/items/"+id+"/content", nil, aliceGrant, 200)
	reqGrant(t, c, ts.URL, "GET", "/api/items/"+id+"/content", nil, aliceGrant, 423)
	reqGrant(t, alice, ts.URL, "GET", "/api/items/"+id+"/content", nil, live, 423)

	// Lease: an expired grant of the right session is refused too.
	expired := seedGrant(t, s, admin.ID, adminHash, -time.Second)
	reqGrant(t, c, ts.URL, "GET", "/api/items/"+id+"/content", nil, expired, 423)
}

func TestLockStatusIdentityOnly(t *testing.T) {
	s, ts, c := setup(t)
	s.Config.PublicOrigin = ts.URL
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	lockSession(t, s, admin.ID, sessionHash(t, s, admin.ID))

	var st struct {
		LockEnabled      bool  `json:"lockEnabled"`
		HasPasskey       bool  `json:"hasPasskey"`
		PasskeyAvailable bool  `json:"passkeyAvailable"`
		GrantActive      bool  `json:"grantActive"`
		ExpiresAt        int64 `json:"expiresAt"`
	}
	if e := json.Unmarshal(req(t, c, ts.URL, "GET", "/api/lock/status", nil, 200), &st); e != nil {
		t.Fatal(e)
	}
	if !st.LockEnabled || st.HasPasskey || !st.PasskeyAvailable || st.GrantActive || st.ExpiresAt != 0 {
		t.Fatalf("status=%+v", st)
	}
	req(t, &http.Client{}, ts.URL, "GET", "/api/lock/status", nil, 401)

	// Without a trustworthy origin, passkeys are unavailable, not silently downgraded.
	s.Config.PublicOrigin = ""
	var down struct {
		PasskeyAvailable bool `json:"passkeyAvailable"`
	}
	if e := json.Unmarshal(req(t, c, ts.URL, "GET", "/api/lock/status", nil, 200), &down); e != nil {
		t.Fatal(e)
	}
	if down.PasskeyAvailable {
		t.Fatal("passkeys must be unavailable without a configured origin")
	}
}

// Status reports the presented token's own row and extends nothing.
func TestLockStatusReportsPresentedGrantOnly(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)

	expired := seedGrant(t, s, admin.ID, hash, -time.Second)
	live := seedGrant(t, s, admin.ID, hash, 20*time.Second)
	before := grantRowExpiry(t, s, live)

	var st struct {
		GrantActive bool  `json:"grantActive"`
		ExpiresAt   int64 `json:"expiresAt"`
	}
	// The expired cookie must not borrow the other tab's live lease.
	if e := json.Unmarshal(reqGrant(t, c, ts.URL, "GET", "/api/lock/status", nil, expired, 200), &st); e != nil {
		t.Fatal(e)
	}
	if st.GrantActive || st.ExpiresAt != 0 {
		t.Fatalf("expired cookie reported live: %+v", st)
	}
	if e := json.Unmarshal(reqGrant(t, c, ts.URL, "GET", "/api/lock/status", nil, live, 200), &st); e != nil {
		t.Fatal(e)
	}
	if !st.GrantActive || st.ExpiresAt != before {
		t.Fatalf("status=%+v want expiry %d", st, before)
	}
	// Reading the status never renews the lease in the database.
	if after := grantRowExpiry(t, s, live); after != before {
		t.Fatalf("status renewed the lease: %d -> %d", before, after)
	}
	if !bytes.Contains(reqGrant(t, c, ts.URL, "GET", "/api/lock/status", nil, live, 200), []byte(`"expiresAt"`)) {
		t.Fatal("status lacks expiresAt")
	}
}

func TestLockRegisterOptionsAndRejection(t *testing.T) {
	s, ts, c := setup(t)
	s.Config.PublicOrigin = ts.URL
	loginTest(t, c, ts.URL, "admin", "admin-password-123")

	// A wrong activation password is 403 INVALID_PASSWORD, never 401.
	b := req(t, c, ts.URL, "POST", "/api/lock/register/options", map[string]any{"password": "wrong-password-123"}, 403)
	if bodyCode(t, b) != "INVALID_PASSWORD" {
		t.Fatalf("body=%s", b)
	}

	b = req(t, c, ts.URL, "POST", "/api/lock/register/options", map[string]any{"password": "admin-password-123"}, 200)
	var opts struct {
		CeremonyID string `json:"ceremonyId"`
		PublicKey  struct {
			Challenge string `json:"challenge"`
			RP        struct {
				ID string `json:"id"`
			} `json:"rp"`
			AuthenticatorSelection struct {
				Attachment       string `json:"authenticatorAttachment"`
				ResidentKey      string `json:"residentKey"`
				UserVerification string `json:"userVerification"`
			} `json:"authenticatorSelection"`
		} `json:"publicKey"`
	}
	if e := json.Unmarshal(b, &opts); e != nil {
		t.Fatal(e)
	}
	if opts.CeremonyID == "" || opts.PublicKey.Challenge == "" || opts.PublicKey.RP.ID != "localhost" {
		t.Fatalf("options=%s", b)
	}
	sel := opts.PublicKey.AuthenticatorSelection
	if sel.Attachment != "platform" || sel.ResidentKey != "required" || sel.UserVerification != "required" {
		t.Fatalf("authenticator selection=%+v", sel)
	}

	// A structurally valid but cryptographically invalid credential is refused...
	cred := map[string]any{"id": "AAAA", "rawId": "AAAA", "type": "public-key", "response": map[string]any{"clientDataJSON": "eyJ0eXBlIjoid2VibmNyZWF0ZSJ9", "attestationObject": "AAAA"}}
	res := doUnlock(t, c, ts.URL, "POST", "/api/lock/register/finish", map[string]any{"ceremonyId": opts.CeremonyID, "credential": cred}, "")
	finish := readBody(t, res)
	if res.StatusCode != 400 || bodyCode(t, finish) != "CEREMONY_INVALID" {
		t.Fatalf("status=%d body=%s", res.StatusCode, finish)
	}
	// A refused ceremony leaves no unlock cookie behind.
	if ck := unlockCookieOf(res); ck != nil {
		t.Fatalf("failed finish sent an unlock cookie: %+v", ck)
	}
	// ...and the challenge is single-use, so the same ceremony cannot be replayed.
	b = req(t, c, ts.URL, "POST", "/api/lock/register/finish", map[string]any{"ceremonyId": opts.CeremonyID, "credential": cred}, 400)
	if bodyCode(t, b) != "CEREMONY_INVALID" {
		t.Fatalf("body=%s", b)
	}

	var sess Session
	if e := json.Unmarshal(req(t, c, ts.URL, "GET", "/api/session", nil, 200), &sess); e != nil {
		t.Fatal(e)
	}
	if sess.LockEnabled || sess.HasPasskey {
		t.Fatalf("a failed registration must not enable anything: %+v", sess)
	}
	for _, q := range []string{"SELECT COUNT(*) FROM webauthn_credentials", "SELECT COUNT(*) FROM lock_challenges", "SELECT COUNT(*) FROM session_locks"} {
		var n int
		if e := s.DB.QueryRow(q).Scan(&n); e != nil {
			t.Fatal(e)
		}
		if n != 0 {
			t.Fatalf("%s left %d rows", q, n)
		}
	}
}

func TestLockUnlockOptionsAndPurposeBinding(t *testing.T) {
	s, ts, c := setup(t)
	s.Config.PublicOrigin = ts.URL
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)

	// No credential yet.
	b := req(t, c, ts.URL, "POST", "/api/lock/unlock/options", map[string]any{}, 409)
	if bodyCode(t, b) != "NO_PASSKEY" {
		t.Fatalf("body=%s", b)
	}
	if _, e := s.DB.Exec("INSERT INTO webauthn_credentials(id,user_id,credential,created_at) VALUES('AAAA',?,?,unixepoch())", admin.ID, `{"id":"AAAA","publicKey":"AAAA","flags":{},"authenticator":{"signCount":0}}`); e != nil {
		t.Fatal(e)
	}
	b = req(t, c, ts.URL, "POST", "/api/lock/unlock/options", map[string]any{}, 200)
	var opts struct {
		CeremonyID string `json:"ceremonyId"`
		PublicKey  struct {
			Challenge        string `json:"challenge"`
			UserVerification string `json:"userVerification"`
			AllowCredentials []any  `json:"allowCredentials"`
		} `json:"publicKey"`
	}
	if e := json.Unmarshal(b, &opts); e != nil {
		t.Fatal(e)
	}
	if opts.CeremonyID == "" || opts.PublicKey.Challenge == "" || opts.PublicKey.UserVerification != "required" || len(opts.PublicKey.AllowCredentials) != 1 {
		t.Fatalf("options=%s", b)
	}

	// Purpose is part of the challenge lookup, so a registration id fails here.
	var n int
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM lock_challenges WHERE purpose='unlock'").Scan(&n); e != nil {
		t.Fatal(e)
	}
	b = req(t, c, ts.URL, "POST", "/api/lock/unlock/finish", map[string]any{"ceremonyId": "deadbeef", "credential": map[string]any{"id": "AAAA", "rawId": "AAAA", "type": "public-key", "response": map[string]any{"clientDataJSON": "AAAA", "authenticatorData": "AAAA", "signature": "AAAA"}}}, 403)
	if bodyCode(t, b) != "UNLOCK_FAILED" {
		t.Fatalf("body=%s", b)
	}
}

// The unlock responses carry only the expiry; the grant itself travels as the HttpOnly cookie.
func TestUnlockResultShape(t *testing.T) {
	for _, v := range []any{grantResult{}, registerResult{}} {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		if bytes.Contains(b, []byte("grant")) {
			t.Fatalf("unlock result exposes a grant field: %s", b)
		}
	}
}

// The unlock cookie carries the documented attributes and refreshes on renewal.
func TestUnlockCookieAttributes(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)

	for _, secure := range []bool{false, true} {
		s.Config.CookieSecure = secure
		res := doUnlock(t, c, ts.URL, "POST", "/api/lock/password", map[string]any{"password": "admin-password-123"}, "")
		body := readBody(t, res)
		if res.StatusCode != 200 {
			t.Fatalf("secure=%v status=%d body=%s", secure, res.StatusCode, body)
		}
		ck := unlockCookieOf(res)
		if ck == nil {
			t.Fatalf("secure=%v no unlock cookie: %s", secure, body)
		}
		if len(ck.Value) != 64 || ck.Path != "/api" || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
			t.Fatalf("cookie=%+v", ck)
		}
		if ck.Secure != secure {
			t.Fatalf("Secure=%v want %v", ck.Secure, secure)
		}
		if ck.MaxAge != 60 || grantTTL != 60*time.Second {
			t.Fatalf("MaxAge=%d grantTTL=%v", ck.MaxAge, grantTTL)
		}
		if d := time.Until(ck.Expires); d < grantTTL-5*time.Second || d > grantTTL+5*time.Second {
			t.Fatalf("Expires=%v", d)
		}
	}
}

// Renewal extends the same token, refreshes the cookie, and never revives or clears a stale one.
func TestLockRenewExtendsOnlyLiveGrant(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)

	// No cookie at all: locked, and no cookie is written.
	res := doUnlock(t, c, ts.URL, "POST", "/api/lock/renew", map[string]any{}, "")
	body := readBody(t, res)
	if res.StatusCode != 423 || bodyCode(t, body) != "APP_LOCKED" {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	if unlockCookieOf(res) != nil {
		t.Fatal("a failed renewal must not touch the cookie")
	}

	// An expired grant is never revived, and the failed renewal still leaves the cookie alone.
	expired := seedGrant(t, s, admin.ID, hash, -time.Second)
	before := grantRowExpiry(t, s, expired)
	res = doUnlock(t, c, ts.URL, "POST", "/api/lock/renew", map[string]any{}, expired)
	body = readBody(t, res)
	if res.StatusCode != 423 || bodyCode(t, body) != "APP_LOCKED" {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	if unlockCookieOf(res) != nil {
		t.Fatal("a stale renewal must not clear or replace the cookie")
	}
	if after := grantRowExpiry(t, s, expired); after != before {
		t.Fatalf("a renewal revived an expired grant: %d -> %d", before, after)
	}

	// A live grant is extended in place by 60s and the same token is re-sent.
	live := seedGrant(t, s, admin.ID, hash, 20*time.Second)
	res = doUnlock(t, c, ts.URL, "POST", "/api/lock/renew", map[string]any{}, live)
	body = readBody(t, res)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	var gr grantResult
	if e := json.Unmarshal(body, &gr); e != nil {
		t.Fatal(e)
	}
	if gr.ExpiresAt < time.Now().Add(grantTTL).Unix()-5 {
		t.Fatalf("renew=%+v", gr)
	}
	ck := unlockCookieOf(res)
	if ck == nil || ck.Value != live || ck.MaxAge != 60 || ck.Path != "/api" {
		t.Fatalf("renewed cookie=%+v", ck)
	}
	if grantRowExpiry(t, s, live) != gr.ExpiresAt {
		t.Fatal("the database expiry and the reported expiry disagree")
	}
	if bytes.Contains(body, []byte(live)) {
		t.Fatalf("renewal exposed the raw grant: %s", body)
	}

	// With the policy off the endpoint refuses, again without touching the cookie.
	if _, e := s.DB.Exec("DELETE FROM session_locks WHERE session_hash=?", hash); e != nil {
		t.Fatal(e)
	}
	res = doUnlock(t, c, ts.URL, "POST", "/api/lock/renew", map[string]any{}, live)
	body = readBody(t, res)
	if res.StatusCode != 409 || bodyCode(t, body) != "LOCK_NOT_ENABLED" {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	if unlockCookieOf(res) != nil {
		t.Fatal("a refusal must not clear the cookie")
	}
}

// Password unlock proves the password, then grants through the cookie with nothing in the body.
func TestLockPasswordUnlockAndThrottle(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	id := addMedia(t, s, "video.mp4", "video")

	// Nothing to unlock before the policy is on.
	b := req(t, c, ts.URL, "POST", "/api/lock/password", map[string]any{"password": "admin-password-123"}, 409)
	if bodyCode(t, b) != "LOCK_NOT_ENABLED" {
		t.Fatalf("body=%s", b)
	}

	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)

	b = req(t, c, ts.URL, "POST", "/api/lock/password", map[string]any{"password": "wrong-password-123"}, 403)
	if bodyCode(t, b) != "INVALID_PASSWORD" {
		t.Fatalf("body=%s", b)
	}
	res := doUnlock(t, c, ts.URL, "POST", "/api/lock/password", map[string]any{"password": "admin-password-123"}, "")
	body := readBody(t, res)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	ck := unlockCookieOf(res)
	if ck == nil {
		t.Fatalf("no unlock cookie: %s", body)
	}
	// The raw grant must not appear anywhere in the JSON.
	if bytes.Contains(body, []byte(ck.Value)) {
		t.Fatalf("the response echoed the raw grant: %s", body)
	}
	var gr grantResult
	if e := json.Unmarshal(body, &gr); e != nil {
		t.Fatal(e)
	}
	if gr.ExpiresAt < time.Now().Add(grantTTL).Unix()-5 {
		t.Fatalf("unlock=%+v", gr)
	}
	reqGrant(t, c, ts.URL, "GET", "/api/items/"+id+"/content", nil, ck.Value, 200)

	for i := 0; i < 10; i++ {
		req(t, c, ts.URL, "POST", "/api/lock/password", map[string]any{"password": "wrong-password-123"}, 403)
	}
	req(t, c, ts.URL, "POST", "/api/lock/password", map[string]any{"password": "admin-password-123"}, 429)
}

// A cookie-authorized stream keeps Range, HEAD, and 416 behaviour.
func TestUnlockCookieRangeAndHead(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	id := addMedia(t, s, "video.mp4", "video")
	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)
	live := seedGrant(t, s, admin.ID, hash, time.Minute)

	for _, tc := range []struct {
		method, rangeHeader string
		status              int
		body                string
	}{{"GET", "bytes=2-5", 206, "2345"}, {"HEAD", "", 200, ""}, {"GET", "bytes=999-", 416, ""}} {
		r := unlockReq(t, ts.URL, tc.method, "/api/items/"+id+"/content", nil, live)
		if tc.rangeHeader != "" {
			r.Header.Set("Range", tc.rangeHeader)
		}
		res := do(t, c, r)
		b := readBody(t, res)
		if res.StatusCode != tc.status {
			t.Fatalf("%s range=%q: %d %s", tc.method, tc.rangeHeader, res.StatusCode, b)
		}
		if tc.body != "" && string(b) != tc.body {
			t.Fatal(string(b))
		}
	}
	// Without the cookie the same stream is locked, not served.
	r := unlockReq(t, ts.URL, "GET", "/api/items/"+id+"/content", nil, "")
	r.Header.Set("Range", "bytes=2-5")
	if res := do(t, c, r); res.StatusCode != 423 {
		t.Fatalf("locked range status=%d", res.StatusCode)
	} else {
		readBody(t, res)
	}
}

// Manual lock revokes every grant of the session, bumps the epoch, and clears the cookie.
func TestLockManualLockRevokesSessionGrants(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	id := addMedia(t, s, "video.mp4", "video")
	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)

	mine := seedGrant(t, s, admin.ID, hash, time.Minute)
	other := seedGrant(t, s, admin.ID, hash, time.Minute)
	if _, e := s.DB.Exec("INSERT INTO lock_challenges(id,session_hash,user_id,purpose,data,created_at,expires_at) VALUES('c1',?,?,'unlock','{}',unixepoch(),unixepoch()+120)", hash, admin.ID); e != nil {
		t.Fatal(e)
	}
	before := epochOf(t, s, hash)

	res := doUnlock(t, c, ts.URL, "POST", "/api/lock/lock", map[string]any{"progress": map[string]any{"itemId": id, "seconds": 9}}, mine)
	body := readBody(t, res)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	if !bytes.Contains(body, []byte(`"locked":true`)) {
		t.Fatalf("body=%s", body)
	}
	if n := grantCount(t, s, hash); n != 0 {
		t.Fatalf("grants survived manual lock: %d", n)
	}
	// Every tab loses the lease, including the one that did not call lock.
	reqGrant(t, c, ts.URL, "GET", "/api/library", nil, other, 423)
	reqGrant(t, c, ts.URL, "GET", "/api/library", nil, mine, 423)

	// Pending ceremonies are cancelled with the same bump.
	var n int
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM lock_challenges WHERE session_hash=?", hash).Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n != 0 {
		t.Fatal("a pending ceremony survived manual lock")
	}
	if after := epochOf(t, s, hash); after != before+1 {
		t.Fatalf("epoch %d -> %d", before, after)
	}

	// The response clears the browser cookie on the same path it was set.
	ck := unlockCookieOf(res)
	if ck == nil || ck.Value != "" || ck.MaxAge >= 0 || ck.Path != "/api" {
		t.Fatalf("clearing cookie=%+v", ck)
	}

	var pos float64
	if e := s.DB.QueryRow("SELECT position FROM playback_progress WHERE user_id=? AND media_id=?", admin.ID, id).Scan(&pos); e != nil {
		t.Fatal(e)
	}
	if pos != 9 {
		t.Fatalf("position=%v", pos)
	}
}

// Without final progress the lock is identity-only, so a lapsed lease can still lock the session.
func TestLockManualLockWithoutGrant(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)
	seedGrant(t, s, admin.ID, hash, time.Minute)
	seedGrant(t, s, admin.ID, hash, -time.Second)

	// No cookie at all, and no progress: the revoke is still allowed.
	res := doUnlock(t, c, ts.URL, "POST", "/api/lock/lock", map[string]any{}, "")
	body := readBody(t, res)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	if n := grantCount(t, s, hash); n != 0 {
		t.Fatalf("grants survived the revoke: %d", n)
	}
	if unlockCookieOf(res) == nil {
		t.Fatal("manual lock must clear the unlock cookie")
	}

	// Without a session the endpoint stays identity-gated.
	if res := doUnlock(t, &http.Client{}, ts.URL, "POST", "/api/lock/lock", map[string]any{}, ""); res.StatusCode != 401 {
		t.Fatalf("status=%d", res.StatusCode)
	} else {
		readBody(t, res)
	}
}

// Final progress may only be written together with a live grant, atomically, or not at all.
func TestLockManualLockProgressNeedsLiveGrant(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	id := addMedia(t, s, "video.mp4", "video")
	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)
	expired := seedGrant(t, s, admin.ID, hash, -time.Second)

	for _, token := range []string{"", expired} {
		res := doUnlock(t, c, ts.URL, "POST", "/api/lock/lock", map[string]any{"progress": map[string]any{"itemId": id, "seconds": 9}}, token)
		body := readBody(t, res)
		if res.StatusCode != 423 || bodyCode(t, body) != "APP_LOCKED" {
			t.Fatalf("token=%q status=%d body=%s", token, res.StatusCode, body)
		}
		if n := progressCount(t, s); n != 0 {
			t.Fatalf("token=%q progress written without a live grant: %d", token, n)
		}
	}
	// The failed attempt rolled back, so the lock did not happen either.
	if n := grantCount(t, s, hash); n != 1 {
		t.Fatalf("a refused manual lock revoked grants: %d", n)
	}
}

func TestLockPasswordUnlockRefusesAfterManualLock(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)

	// A password proof snapshots the epoch before bcrypt; a manual lock in between moves it.
	epoch, e := s.lockEpoch(context.Background(), hash)
	if e != nil {
		t.Fatal(e)
	}
	if res := doUnlock(t, c, ts.URL, "POST", "/api/lock/lock", map[string]any{}, ""); res.StatusCode != 200 {
		t.Fatalf("lock status=%d", res.StatusCode)
	} else {
		readBody(t, res)
	}
	tx, e := s.DB.BeginTx(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	err := lockGrantGuardTx(context.Background(), tx, hash, epoch)
	tx.Rollback()
	if !errors.Is(err, errPolicyChanged) {
		t.Fatalf("a password proof predating the lock was accepted: %v", err)
	}
	if n := grantCount(t, s, hash); n != 0 {
		t.Fatalf("grants=%d", n)
	}

	// A fresh snapshot passes: the lock is on and the epoch is now current.
	epoch, e = s.lockEpoch(context.Background(), hash)
	if e != nil {
		t.Fatal(e)
	}
	tx, e = s.DB.BeginTx(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = lockGrantGuardTx(context.Background(), tx, hash, epoch); e != nil {
		t.Fatalf("current proof refused: %v", e)
	}
	tx.Rollback()
}

// The guard also refuses a proof that predates a disable, and one that predates a re-enable.
func TestLockGrantGuardEpochRace(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	hash := sessionHash(t, s, admin.ID)
	si := sessionInfo{User: admin, Hash: hash}
	ctx := context.Background()

	// Disable: the epoch moves and the policy goes.
	lockSession(t, s, admin.ID, hash)
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = bumpEpoch(ctx, tx, hash); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM session_locks WHERE session_hash=?", hash); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	tx, e = s.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = lockGrantGuardTx(ctx, tx, hash, 0); !errors.Is(e, errLockDisabled) {
		t.Fatalf("a proof for a disabled lock was accepted: %v", e)
	}
	tx.Rollback()

	// Re-enable: the policy is back, but a proof that predates the disable is still refused.
	tx, e = s.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = enableLockTx(ctx, tx, si, 1); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	tx, e = s.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = lockGrantGuardTx(ctx, tx, hash, 0); !errors.Is(e, errPolicyChanged) {
		t.Fatalf("a pre-disable proof survived a re-enable: %v", e)
	}
	tx.Rollback()

	// Manual lock: the policy stays on, the epoch moves, and the older proof is refused.
	tx, e = s.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = bumpEpoch(ctx, tx, hash); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	tx, e = s.DB.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = lockGrantGuardTx(ctx, tx, hash, 1); !errors.Is(e, errPolicyChanged) {
		t.Fatalf("a proof predating a manual lock was accepted: %v", e)
	}
	tx.Rollback()
}

// Enabling the policy must not cancel grants other tabs already hold.
func TestEnableLockKeepsOtherGrants(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	hash := sessionHash(t, s, admin.ID)
	si := sessionInfo{User: admin, Hash: hash}
	other := seedGrant(t, s, admin.ID, hash, time.Minute)

	tx, e := s.DB.BeginTx(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = enableLockTx(context.Background(), tx, si, 0); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	if n := grantCount(t, s, hash); n != 1 {
		t.Fatalf("enabling the lock cancelled a concurrent grant: %d", n)
	}
	if !s.grantValid(unlockReq(t, ts.URL, "GET", "/api/library", nil, other), hash) {
		t.Fatal("the concurrent grant stopped being valid")
	}
}

// Disabling needs a fresh password proof and clears the policy, grants, challenges, and cookie.
func TestLockDisableRevokes(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)
	seedGrant(t, s, admin.ID, hash, time.Minute)
	if _, e := s.DB.Exec("INSERT INTO lock_challenges(id,session_hash,user_id,purpose,data,created_at,expires_at) VALUES('c1',?,?,'register','{}',unixepoch(),unixepoch()+120)", hash, admin.ID); e != nil {
		t.Fatal(e)
	}

	b := req(t, c, ts.URL, "POST", "/api/lock/disable", map[string]any{"password": "wrong-password-123"}, 403)
	if bodyCode(t, b) != "INVALID_PASSWORD" {
		t.Fatalf("body=%s", b)
	}

	res := doUnlock(t, c, ts.URL, "POST", "/api/lock/disable", map[string]any{"password": "admin-password-123"}, "")
	body := readBody(t, res)
	if res.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", res.StatusCode, body)
	}
	var rsp struct {
		Session Session `json:"session"`
	}
	if e := json.Unmarshal(body, &rsp); e != nil {
		t.Fatal(e)
	}
	if rsp.Session.LockEnabled {
		t.Fatalf("lock still enabled: %+v", rsp.Session)
	}
	if ck := unlockCookieOf(res); ck == nil || ck.MaxAge >= 0 || ck.Path != "/api" {
		t.Fatalf("disable must clear the unlock cookie: %+v", ck)
	}
	for _, q := range []string{"SELECT COUNT(*) FROM session_locks", "SELECT COUNT(*) FROM unlock_grants", "SELECT COUNT(*) FROM lock_challenges"} {
		var n int
		if e := s.DB.QueryRow(q).Scan(&n); e != nil {
			t.Fatal(e)
		}
		if n != 0 {
			t.Fatalf("%s left %d rows", q, n)
		}
	}
	req(t, c, ts.URL, "GET", "/api/library", nil, 200)
}

// Logging out ends the session, and the lock rows keyed to it go with it.
func TestLockLogoutClearsRows(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)
	seedGrant(t, s, admin.ID, hash, time.Minute)

	res := doUnlock(t, c, ts.URL, "POST", "/api/logout", nil, "")
	if res.StatusCode != 200 {
		t.Fatalf("logout status=%d", res.StatusCode)
	}
	readBody(t, res)
	if ck := unlockCookieOf(res); ck == nil || ck.MaxAge >= 0 || ck.Path != "/api" {
		t.Fatalf("logout must clear the unlock cookie: %+v", ck)
	}
	for _, q := range []string{"SELECT COUNT(*) FROM session_locks", "SELECT COUNT(*) FROM unlock_grants"} {
		var n int
		if e := s.DB.QueryRow(q).Scan(&n); e != nil {
			t.Fatal(e)
		}
		if n != 0 {
			t.Fatalf("%s left %d rows after logout", q, n)
		}
	}
	req(t, c, ts.URL, "GET", "/api/library", nil, 401)
}

// A fresh sign-in is an unlocked session, so it drops any unlock cookie from the old one.
func TestLoginClearsUnlockCookie(t *testing.T) {
	s, ts, c := setup(t)
	request := func() *http.Response {
		return do(t, c, unlockReq(t, ts.URL, "POST", "/api/login", map[string]any{"username": "admin", "password": "admin-password-123"}, ""))
	}
	readBody(t, request())

	s.Config.PublicOrigin = ts.URL
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	hash := sessionHash(t, s, admin.ID)
	lockSession(t, s, admin.ID, hash)

	res := request()
	readBody(t, res)
	if ck := unlockCookieOf(res); ck == nil || ck.MaxAge >= 0 || ck.Path != "/api" {
		t.Fatalf("login must clear the unlock cookie: %+v", ck)
	}
}

// Changing a password revokes the sessions, cascading to lock policy and grants.
func TestLockAccountRevocationCascades(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	u := parseUser(t, req(t, c, ts.URL, "POST", "/api/admin/users", map[string]any{"username": "alice", "password": "alice-password-123"}, 201))
	jar, _ := cookiejar.New(nil)
	alice := &http.Client{Jar: jar}
	loginTest(t, alice, ts.URL, "alice", "alice-password-123")
	hash := sessionHash(t, s, u.ID)
	lockSession(t, s, u.ID, hash)
	grant := seedGrant(t, s, u.ID, hash, time.Minute)

	req(t, c, ts.URL, "PATCH", "/api/admin/users/"+u.ID, map[string]any{"password": "alice-new-password"}, 200)
	var n int
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM session_locks WHERE user_id=?", u.ID).Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n != 0 {
		t.Fatalf("lock rows survived account revocation: %d", n)
	}
	req(t, alice, ts.URL, "GET", "/api/library", nil, 401)
	// The orphaned cookie is worthless without its session.
	reqGrant(t, alice, ts.URL, "GET", "/api/library", nil, grant, 401)
}

// An existing database upgrades in place; a pre-feature session reads as unlocked.
func TestLockSchemaReopenAndLegacyUpgrade(t *testing.T) {
	dir := t.TempDir()
	token := strings.Repeat("0", 64)
	sum := sha256.Sum256([]byte(token))
	legacy, e := sql.Open("sqlite", filepath.Join(dir, "mediaex.db"))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = legacy.Exec(`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY);
CREATE TABLE users(id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE COLLATE NOCASE, name TEXT NOT NULL, password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('admin','user')), active INTEGER NOT NULL CHECK(active IN (0,1)), created_at INTEGER NOT NULL);
CREATE TABLE sessions(token_hash TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,expires_at INTEGER NOT NULL);
INSERT INTO schema_migrations(version) VALUES(1);
INSERT INTO users VALUES('u1','admin','Admin','x','admin',1,0);
INSERT INTO sessions VALUES((SELECT '` + hex.EncodeToString(sum[:]) + `'),'u1',9999999999);`); e != nil {
		t.Fatal(e)
	}
	if e = legacy.Close(); e != nil {
		t.Fatal(e)
	}

	db, e := OpenDB(dir)
	if e != nil {
		t.Fatal(e)
	}
	for _, tbl := range []string{"session_locks", "session_lock_epochs", "webauthn_credentials", "lock_challenges", "unlock_grants"} {
		var name string
		if e := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", tbl).Scan(&name); e != nil {
			t.Fatalf("table %s missing after upgrade: %v", tbl, e)
		}
	}
	var n int
	if e := db.QueryRow("SELECT COUNT(*) FROM users").Scan(&n); e != nil || n != 1 {
		t.Fatalf("legacy rows lost: n=%d err=%v", n, e)
	}

	s := NewServer(Config{}, db, nil)
	r, _ := http.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: "mediaex_session", Value: token})
	si, e := s.loadSession(r)
	if e != nil {
		t.Fatal(e)
	}
	if si.LockEnabled {
		t.Fatal("a pre-feature session must default to unlocked")
	}
	if e = db.Close(); e != nil {
		t.Fatal(e)
	}
	// Reopening an already-migrated database is a no-op.
	db2, e := OpenDB(dir)
	if e != nil {
		t.Fatal(e)
	}
	db2.Close()
}

// An account with a passkey can enable a new session's lock by assertion, not re-registration.
func TestLockEnableOptionsUsesExistingCredential(t *testing.T) {
	s, ts, c := setup(t)
	s.Config.PublicOrigin = ts.URL
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))

	// Without a credential there is nothing to assert.
	b := req(t, c, ts.URL, "POST", "/api/lock/enable/options", map[string]any{}, 409)
	if bodyCode(t, b) != "NO_PASSKEY" {
		t.Fatalf("body=%s", b)
	}
	if _, e := s.DB.Exec("INSERT INTO webauthn_credentials(id,user_id,credential,created_at) VALUES('AAAA',?,?,unixepoch())", admin.ID, `{"id":"AAAA","publicKey":"AAAA","flags":{},"authenticator":{"signCount":0}}`); e != nil {
		t.Fatal(e)
	}
	b = req(t, c, ts.URL, "POST", "/api/lock/enable/options", map[string]any{}, 200)
	var opts struct {
		CeremonyID string `json:"ceremonyId"`
		PublicKey  struct {
			Challenge        string `json:"challenge"`
			UserVerification string `json:"userVerification"`
			AllowCredentials []any  `json:"allowCredentials"`
		} `json:"publicKey"`
	}
	if e := json.Unmarshal(b, &opts); e != nil {
		t.Fatal(e)
	}
	if opts.CeremonyID == "" || opts.PublicKey.Challenge == "" || opts.PublicKey.UserVerification != "required" || len(opts.PublicKey.AllowCredentials) != 1 {
		t.Fatalf("options=%s", b)
	}
	// The ceremony is stored under its own purpose, so it cannot be redeemed as an unlock.
	var purpose string
	if e := s.DB.QueryRow("SELECT purpose FROM lock_challenges WHERE id=?", opts.CeremonyID).Scan(&purpose); e != nil {
		t.Fatal(e)
	}
	if purpose != "enable" {
		t.Fatalf("purpose=%q", purpose)
	}
	// A bad assertion is refused and consumes the challenge.
	b = req(t, c, ts.URL, "POST", "/api/lock/enable/finish", map[string]any{"ceremonyId": opts.CeremonyID, "credential": map[string]any{"id": "AAAA", "rawId": "AAAA", "type": "public-key", "response": map[string]any{"clientDataJSON": "AAAA", "authenticatorData": "AAAA", "signature": "AAAA"}}}, 403)
	if bodyCode(t, b) != "UNLOCK_FAILED" {
		t.Fatalf("body=%s", b)
	}
	var n int
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM lock_challenges WHERE id=?", opts.CeremonyID).Scan(&n); e != nil {
		t.Fatal(e)
	}
	if n != 0 {
		t.Fatal("the enable challenge survived a failed finish")
	}
}

// A credential ID belongs to one account: cross-account claims are refused.
func TestClaimCredentialOwnership(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	alice := parseUser(t, req(t, c, ts.URL, "POST", "/api/admin/users", map[string]any{"username": "alice", "password": "alice-password-123"}, 201))
	if _, e := s.DB.Exec("INSERT INTO webauthn_credentials(id,user_id,credential,created_at) VALUES('AAAA',?,'{}',unixepoch())", alice.ID); e != nil {
		t.Fatal(e)
	}

	tx, e := s.DB.BeginTx(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	if e = claimCredentialTx(context.Background(), tx, admin.ID, "AAAA", `{"stolen":true}`); !errors.Is(e, errCredentialTaken) {
		t.Fatalf("cross-account claim accepted: %v", e)
	}
	if e = claimCredentialTx(context.Background(), tx, alice.ID, "AAAA", `{"owner":true}`); e != nil {
		t.Fatal(e)
	}
	if e = claimCredentialTx(context.Background(), tx, admin.ID, "BBBB", `{"fresh":true}`); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}

	var owner, blob string
	if e := s.DB.QueryRow("SELECT user_id,credential FROM webauthn_credentials WHERE id='AAAA'").Scan(&owner, &blob); e != nil {
		t.Fatal(e)
	}
	if owner != alice.ID || !strings.Contains(blob, "owner") {
		t.Fatalf("ownership transferred: owner=%q blob=%s", owner, blob)
	}
	if e := s.DB.QueryRow("SELECT user_id FROM webauthn_credentials WHERE id='BBBB'").Scan(&owner); e != nil || owner != admin.ID {
		t.Fatalf("new credential not stored: owner=%q err=%v", owner, e)
	}
}

// enableLockTx refuses a ceremony predating a disable, so a pending finish cannot restore it.
func TestEnableLockEpochRace(t *testing.T) {
	s, ts, c := setup(t)
	loginTest(t, c, ts.URL, "admin", "admin-password-123")
	admin := parseUser(t, req(t, c, ts.URL, "GET", "/api/session", nil, 200))
	hash := sessionHash(t, s, admin.ID)
	si := sessionInfo{User: admin, Hash: hash}

	tx, e := s.DB.BeginTx(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = enableLockTx(context.Background(), tx, si, 0); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	var enabled int
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM session_locks WHERE session_hash=? AND enabled=1", hash).Scan(&enabled); e != nil || enabled != 1 {
		t.Fatalf("enable failed: n=%d err=%v", enabled, e)
	}

	// Disable bumps the epoch and clears the policy.
	tx, e = s.DB.BeginTx(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = bumpEpoch(context.Background(), tx, hash); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.ExecContext(context.Background(), "DELETE FROM session_locks WHERE session_hash=?", hash); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}

	// A ceremony that began before the disable carries epoch 0 and must be refused.
	tx, e = s.DB.BeginTx(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	err := enableLockTx(context.Background(), tx, si, 0)
	tx.Rollback()
	if !errors.Is(err, errPolicyChanged) {
		t.Fatalf("stale ceremony accepted: %v", err)
	}
	if e := s.DB.QueryRow("SELECT COUNT(*) FROM session_locks WHERE session_hash=? AND enabled=1", hash).Scan(&enabled); e != nil || enabled != 0 {
		t.Fatalf("policy was restored by a stale ceremony: n=%d err=%v", enabled, e)
	}
}
