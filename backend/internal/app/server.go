package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Server struct {
	Config     Config
	DB         *sql.DB
	Scanner    *Scanner
	log        *slog.Logger
	throttleMu sync.Mutex
	attempts   map[string]attempt
	dummy      []byte
}
type attempt struct {
	Count int
	Until time.Time
}

func NewServer(c Config, db *sql.DB, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{Config: c, DB: db, log: logger, attempts: map[string]attempt{}, dummy: dummyHash()}
}
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	m.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		root, e := openMediaRoot(s.Config)
		if root != nil {
			root.Close()
		}
		if e != nil || s.DB.PingContext(r.Context()) != nil {
			fail(w, 503, "Media library is not ready.")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	m.HandleFunc("GET /api/session", s.session)
	m.HandleFunc("POST /api/login", s.login)
	m.HandleFunc("POST /api/logout", s.logout)
	// Lock endpoints are identity-only so they stay reachable while the session is locked.
	m.Handle("GET /api/lock/status", s.identity(s.lockStatus))
	m.Handle("POST /api/lock/register/options", s.identity(s.lockRegisterOptions))
	m.Handle("POST /api/lock/register/finish", s.identity(s.lockRegisterFinish))
	m.Handle("POST /api/lock/unlock/options", s.identity(s.lockUnlockOptions))
	m.Handle("POST /api/lock/unlock/finish", s.identity(s.lockUnlockFinish))
	m.Handle("POST /api/lock/enable/options", s.identity(s.lockEnableOptions))
	m.Handle("POST /api/lock/enable/finish", s.identity(s.lockEnableFinish))
	m.Handle("POST /api/lock/password", s.identity(s.lockPassword))
	m.Handle("POST /api/lock/renew", s.identity(s.lockRenew))
	m.Handle("POST /api/lock/lock", s.identity(s.lockLock))
	m.Handle("POST /api/lock/disable", s.identity(s.lockDisable))
	m.Handle("GET /api/library", s.protected(false, s.library))
	m.Handle("GET /api/items/{id}", s.protected(false, s.item))
	m.Handle("PUT /api/items/{id}/progress", s.protected(false, s.progress))
	m.Handle("GET /api/items/{id}/content", s.protected(false, s.content))
	m.Handle("GET /api/items/{id}/playback", s.protected(false, s.playback))
	m.Handle("GET /api/items/{id}/thumbnail", s.protected(false, s.thumbnail))
	m.Handle("GET /api/items/{id}/sprite", s.protected(false, s.sprite))
	m.Handle("GET /api/admin/users", s.protected(true, s.users))
	m.Handle("POST /api/admin/users", s.protected(true, s.createUser))
	m.Handle("PATCH /api/admin/users/{id}", s.protected(true, s.updateUser))
	m.Handle("DELETE /api/admin/users/{id}", s.protected(true, s.deleteUser))
	m.Handle("GET /api/admin/issues", s.protected(true, s.issues))
	m.Handle("POST /api/admin/issues/skip", s.protected(true, s.skipIssues))
	m.Handle("POST /api/admin/issues/{id}/skip", s.protected(true, s.skipIssue))
	m.Handle("DELETE /api/admin/issues/{id}/skip", s.protected(true, s.unskipIssue))
	m.Handle("POST /api/admin/issues/{id}/retry", s.protected(true, s.retryIssue))
	m.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "This endpoint could not be found.") })
	m.HandleFunc("/", s.static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" && strings.HasPrefix(r.URL.Path, "/api/") {
			if !s.sameOrigin(r) {
				fail(w, 403, "Request origin is not allowed.")
				return
			}
			if r.Body != nil && r.ContentLength != 0 && r.Method != "DELETE" {
				ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if ct != "application/json" {
					fail(w, 415, "JSON content is required.")
					return
				}
			}
		}
		m.ServeHTTP(w, r)
	})
}
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	if s.Config.PublicOrigin != "" {
		return origin == s.Config.PublicOrigin
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return u.Scheme == scheme && u.Host == r.Host
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

// failCode adds a machine-readable code so the client can tell 401 from 423.
func failCode(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"code": code, "message": msg})
}
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeMax(w, r, dst, 16384)
}

// decodeMax bounds the body; WebAuthn finish payloads get a larger fixed budget.
func decodeMax(w http.ResponseWriter, r *http.Request, dst any, max int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		fail(w, 400, "Invalid request body.")
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		fail(w, 400, "Invalid request body.")
		return false
	}
	return true
}
func (s *Server) internal(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "error", err)
	fail(w, 500, "Something went wrong. Please try again.")
}

// sessionInfo is the resolved login: account, session hash, and session lock policy.
type sessionInfo struct {
	User        User
	Hash        string
	LockEnabled bool
}

// loadSession resolves the cookie to a user and its session-scoped lock policy.
func (s *Server) loadSession(r *http.Request) (sessionInfo, error) {
	c, err := r.Cookie("mediaex_session")
	if err != nil {
		return sessionInfo{}, sql.ErrNoRows
	}
	if len(c.Value) != 64 {
		return sessionInfo{}, sql.ErrNoRows
	}
	sum := sha256.Sum256([]byte(c.Value))
	var in sessionInfo
	in.Hash = hex.EncodeToString(sum[:])
	var enabled int
	err = s.DB.QueryRowContext(r.Context(), "SELECT u.id,u.username,u.name,u.role,u.active,u.created_at,COALESCE(l.enabled,0) FROM sessions s JOIN users u ON u.id=s.user_id LEFT JOIN session_locks l ON l.session_hash=s.token_hash WHERE s.token_hash=? AND s.expires_at>? AND u.active=1", in.Hash, time.Now().Unix()).Scan(&in.User.ID, &in.User.Username, &in.User.Name, &in.User.Role, &in.User.Active, &in.User.CreatedAt, &enabled)
	in.LockEnabled = enabled == 1
	return in, err
}

// lookupSession keeps the account-only view for callers that need just the user.
func (s *Server) lookupSession(r *http.Request) (User, error) {
	si, err := s.loadSession(r)
	return si.User, err
}

type userKey struct{}
type sessionKey struct{}

// currentSession exposes the session hash and lock policy to lock handlers.
func currentSession(r *http.Request) sessionInfo {
	if si, ok := r.Context().Value(sessionKey{}).(sessionInfo); ok {
		return si
	}
	return sessionInfo{}
}

// grantExpiry reports the presented grant's expiry, or 0/false when there is no live grant.
func (s *Server) grantExpiry(r *http.Request, sessionHash string) (int64, bool) {
	token := unlockToken(r)
	if token == "" {
		return 0, false
	}
	var expiresAt int64
	err := s.DB.QueryRowContext(r.Context(), "SELECT expires_at FROM unlock_grants WHERE token_hash=? AND session_hash=? AND expires_at>?", grantHash(token), sessionHash, time.Now().Unix()).Scan(&expiresAt)
	if err != nil {
		return 0, false
	}
	return expiresAt, true
}

// grantValid reports a live grant for this session; only the unlock cookie carries one.
func (s *Server) grantValid(r *http.Request, sessionHash string) bool {
	_, ok := s.grantExpiry(r, sessionHash)
	return ok
}

// identity requires only a valid session, keeping lock endpoints reachable while locked.
func (s *Server) identity(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		si, e := s.loadSession(r)
		if errors.Is(e, sql.ErrNoRows) {
			fail(w, 401, "Please sign in to continue.")
			return
		}
		if e != nil {
			s.internal(w, e)
			return
		}
		ctx := context.WithValue(r.Context(), userKey{}, si.User)
		ctx = context.WithValue(ctx, sessionKey{}, si)
		h(w, r.WithContext(ctx))
	})
}

// protected guards content and admin routes: 423 means unlock, 401 means invalid identity.
func (s *Server) protected(admin bool, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		si, e := s.loadSession(r)
		if errors.Is(e, sql.ErrNoRows) {
			fail(w, 401, "Please sign in to continue.")
			return
		}
		if e != nil {
			s.internal(w, e)
			return
		}
		if si.LockEnabled && !s.grantValid(r, si.Hash) {
			failCode(w, 423, "APP_LOCKED", "This session is locked. Unlock to continue.")
			return
		}
		if admin && si.User.Role != "admin" {
			fail(w, 403, "Administrator access is required.")
			return
		}
		ctx := context.WithValue(r.Context(), userKey{}, si.User)
		ctx = context.WithValue(ctx, sessionKey{}, si)
		h(w, r.WithContext(ctx))
	})
}
func currentUser(r *http.Request) User { return r.Context().Value(userKey{}).(User) }

// sessionDTO assembles the account view; callers pass the policy explicitly.
func (s *Server) sessionDTO(ctx context.Context, u User, lockEnabled bool) (Session, error) {
	var n int
	if err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM webauthn_credentials WHERE user_id=?", u.ID).Scan(&n); err != nil {
		return Session{}, err
	}
	return Session{ID: u.ID, Username: u.Username, Name: u.Name, Role: u.Role, LockEnabled: lockEnabled, HasPasskey: n > 0, PasskeyAvailable: s.passkeyAvailable()}, nil
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	si, e := s.loadSession(r)
	if errors.Is(e, sql.ErrNoRows) {
		writeJSON(w, 200, nil)
		return
	}
	if e != nil {
		s.internal(w, e)
		return
	}
	dto, e := s.sessionDTO(r.Context(), si.User, si.LockEnabled)
	if e != nil {
		s.internal(w, e)
		return
	}
	writeJSON(w, 200, dto)
}
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" && r.Method != "HEAD" {
		http.NotFound(w, r)
		return
	}
	if s.Config.StaticDir == "" {
		http.NotFound(w, r)
		return
	}
	root, err := os.OpenRoot(s.Config.StaticDir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		p = "index.html"
	}
	f, err := root.Open(p)
	if err != nil {
		if strings.Contains(filepath.Base(p), ".") {
			http.NotFound(w, r)
			return
		}
		f, err = root.Open("index.html")
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err == nil && st.IsDir() {
		// Opening "docs" or "docs/" succeeds as a directory on Linux; without this
		// branch both would 404 instead of serving the directory's index.html.
		f.Close()
		f, err = root.Open(filepath.Join(p, "index.html"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		st, err = f.Stat()
	}
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	if filepath.Base(f.Name()) == "index.html" {
		w.Header().Set("Cache-Control", "no-store")
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	trusted := func(ip netip.Addr) bool {
		for _, prefix := range s.Config.TrustedProxies {
			if prefix.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(peer) {
		return peer.String()
	}
	values := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	addresses := make([]netip.Addr, 0, len(values))
	for _, value := range values {
		ip, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil || ip.Zone() != "" {
			return peer.String()
		}
		addresses = append(addresses, ip.Unmap())
	}
	for i := len(addresses) - 1; i >= 0; i-- {
		if !trusted(addresses[i]) {
			return addresses[i].String()
		}
	}
	return peer.String()
}
