package app

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{2,63}$`)

func validateUsername(v string) error {
	if !usernamePattern.MatchString(v) {
		return errors.New("Username must contain 3–64 letters, numbers, dots, dashes, or underscores.")
	}
	return nil
}
func validatePassword(v string) error {
	if len(v) < 12 || len(v) > 72 {
		return errors.New("Password must contain 12–72 bytes.")
	}
	return nil
}
func validateName(v string) error {
	if strings.TrimSpace(v) == "" || utf8.RuneCountInString(v) > 100 {
		return errors.New("Display name must contain 1–100 characters.")
	}
	return nil
}
func dummyHash() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("unused-dummy-password-for-lookup"), bcrypt.DefaultCost)
	return h
}

// allow is the shared fixed-window throttle; callers namespace the key per lane.
func (s *Server) allow(key string) bool {
	s.throttleMu.Lock()
	defer s.throttleMu.Unlock()
	now := time.Now()
	for k, a := range s.attempts {
		if now.After(a.Until) {
			delete(s.attempts, k)
		}
	}
	a := s.attempts[key]
	if a.Count >= 10 {
		return false
	}
	if len(s.attempts) >= 4096 && a.Count == 0 {
		return false
	}
	a.Count++
	if a.Until.IsZero() {
		a.Until = now.Add(15 * time.Minute)
	}
	s.attempts[key] = a
	return true
}
func (s *Server) clearAttempt(key string) {
	s.throttleMu.Lock()
	delete(s.attempts, key)
	s.throttleMu.Unlock()
}
func (s *Server) allowLogin(ip string) bool { return s.allow("login:" + ip) }

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !s.allowLogin(s.clientIP(r)) {
		w.Header().Set("Retry-After", "900")
		fail(w, 429, "Too many sign-in attempts. Please try again later.")
		return
	}
	var u User
	var hash string
	e := s.DB.QueryRowContext(r.Context(), "SELECT "+userCols+",password_hash FROM users WHERE username=?", body.Username).Scan(&u.ID, &u.Username, &u.Name, &u.Role, &u.Active, &u.CreatedAt, &hash)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		s.internal(w, e)
		return
	}
	compare := []byte(hash)
	if e != nil {
		compare = s.dummy
	}
	valid := bcrypt.CompareHashAndPassword(compare, []byte(body.Password)) == nil
	if !valid || e != nil || !u.Active {
		fail(w, 401, "Invalid username or password.")
		return
	}
	token := randomID()
	sum := sha256.Sum256([]byte(token))
	expiry := time.Now().Add(s.Config.SessionTTL)
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		s.internal(w, e)
		return
	}
	defer tx.Rollback()
	var active bool
	var currentHash string
	if e = tx.QueryRowContext(r.Context(), "SELECT active,password_hash FROM users WHERE id=?", u.ID).Scan(&active, &currentHash); e != nil || !active || currentHash != hash {
		fail(w, 401, "Invalid username or password.")
		return
	}
	now := time.Now().Unix()
	if _, e = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE expires_at<=?", now); e != nil {
		s.internal(w, e)
		return
	}
	if e = purgeExpiredLockRows(r.Context(), tx, now); e != nil {
		s.internal(w, e)
		return
	}
	if _, e = tx.ExecContext(r.Context(), "INSERT INTO sessions VALUES(?,?,?)", hex.EncodeToString(sum[:]), u.ID, expiry.Unix()); e != nil {
		s.internal(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		s.internal(w, e)
		return
	}
	s.throttleMu.Lock()
	delete(s.attempts, "login:"+s.clientIP(r))
	s.throttleMu.Unlock()
	// A new login is a new unlocked session; any unlock cookie from the old one is cleared.
	s.clearUnlockCookie(w)
	http.SetCookie(w, &http.Cookie{Name: "mediaex_session", Value: token, Path: "/", HttpOnly: true, Secure: s.Config.CookieSecure, SameSite: http.SameSiteLaxMode, Expires: expiry, MaxAge: int(s.Config.SessionTTL.Seconds())})
	dto, e := s.sessionDTO(r.Context(), u, false)
	if e != nil {
		s.internal(w, e)
		return
	}
	writeJSON(w, 200, dto)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("mediaex_session"); e == nil {
		sum := sha256.Sum256([]byte(c.Value))
		// Lock rows, grants, and challenges are FK-cascaded with the session.
		if _, e = s.DB.ExecContext(r.Context(), "DELETE FROM sessions WHERE token_hash=?", hex.EncodeToString(sum[:])); e != nil {
			s.internal(w, e)
			return
		}
	}
	s.clearUnlockCookie(w)
	http.SetCookie(w, &http.Cookie{Name: "mediaex_session", Value: "", Path: "/", HttpOnly: true, Secure: s.Config.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	rows, e := s.DB.QueryContext(r.Context(), "SELECT "+userCols+" FROM users ORDER BY username COLLATE NOCASE")
	if e != nil {
		s.internal(w, e)
		return
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, e := scanUser(rows)
		if e != nil {
			s.internal(w, e)
			return
		}
		out = append(out, u)
	}
	if e = rows.Err(); e != nil {
		s.internal(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"users": out})
}

type userInput struct {
	Username *string `json:"username"`
	Name     *string `json:"name"`
	Password *string `json:"password"`
	Role     *string `json:"role"`
	Active   *bool   `json:"active"`
}

func validateUserInput(b userInput, create bool) error {
	if create && (b.Username == nil || b.Password == nil) {
		return errors.New("Username and password are required.")
	}
	if b.Username != nil {
		if e := validateUsername(*b.Username); e != nil {
			return e
		}
	}
	if b.Password != nil {
		if e := validatePassword(*b.Password); e != nil {
			return e
		}
	}
	if b.Name != nil {
		if e := validateName(*b.Name); e != nil {
			return e
		}
	}
	if b.Role != nil && *b.Role != "admin" && *b.Role != "user" {
		return errors.New("Role must be admin or user.")
	}
	return nil
}
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var b userInput
	if !decode(w, r, &b) {
		return
	}
	if e := validateUserInput(b, true); e != nil {
		fail(w, 400, e.Error())
		return
	}
	u := User{ID: randomID(), Username: *b.Username, Name: *b.Username, Role: "user", Active: true, CreatedAt: time.Now().Unix()}
	if b.Name != nil {
		u.Name = *b.Name
	}
	if b.Role != nil {
		u.Role = *b.Role
	}
	if b.Active != nil {
		u.Active = *b.Active
	}
	h, e := bcrypt.GenerateFromPassword([]byte(*b.Password), bcrypt.DefaultCost)
	if e != nil {
		s.internal(w, e)
		return
	}
	_, e = s.DB.ExecContext(r.Context(), "INSERT INTO users VALUES(?,?,?,?,?,?,?)", u.ID, u.Username, u.Name, string(h), u.Role, u.Active, u.CreatedAt)
	if e != nil {
		if strings.Contains(e.Error(), "UNIQUE constraint") {
			fail(w, 409, "This username is already in use.")
			return
		}
		s.internal(w, e)
		return
	}
	writeJSON(w, 201, u)
}
func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	var b userInput
	if !decode(w, r, &b) {
		return
	}
	if e := validateUserInput(b, false); e != nil {
		fail(w, 400, e.Error())
		return
	}
	var hash []byte
	var e error
	if b.Password != nil {
		hash, e = bcrypt.GenerateFromPassword([]byte(*b.Password), bcrypt.DefaultCost)
		if e != nil {
			s.internal(w, e)
			return
		}
	}
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		s.internal(w, e)
		return
	}
	defer tx.Rollback()
	u, e := scanUser(tx.QueryRowContext(r.Context(), "SELECT "+userCols+" FROM users WHERE id=?", r.PathValue("id")))
	if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "This account could not be found.")
		return
	}
	if e != nil {
		s.internal(w, e)
		return
	}
	revoke := false
	if (b.Active != nil && !*b.Active) || (b.Role != nil && *b.Role != "admin") {
		if e = finalAdminGuard(r.Context(), tx, u.ID); e != nil {
			if errors.Is(e, errFinalAdmin) {
				fail(w, 409, e.Error())
				return
			}
			s.internal(w, e)
			return
		}
	}
	if b.Username != nil {
		u.Username = *b.Username
	}
	if b.Name != nil {
		u.Name = *b.Name
	}
	if b.Role != nil {
		revoke = *b.Role != u.Role
		u.Role = *b.Role
	}
	if b.Active != nil {
		revoke = revoke || !*b.Active
		u.Active = *b.Active
	}
	_, e = tx.ExecContext(r.Context(), "UPDATE users SET username=?,name=?,role=?,active=? WHERE id=?", u.Username, u.Name, u.Role, u.Active, u.ID)
	if e != nil {
		if strings.Contains(e.Error(), "UNIQUE constraint") {
			fail(w, 409, "This username is already in use.")
			return
		}
		s.internal(w, e)
		return
	}
	if hash != nil {
		revoke = true
		if _, e = tx.ExecContext(r.Context(), "UPDATE users SET password_hash=? WHERE id=?", string(hash), u.ID); e != nil {
			s.internal(w, e)
			return
		}
	}
	if revoke {
		// Revoking sessions FK-cascades to lock policy, grants, and challenges.
		if _, e = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE user_id=?", u.ID); e != nil {
			s.internal(w, e)
			return
		}
	}
	if e = tx.Commit(); e != nil {
		s.internal(w, e)
		return
	}
	writeJSON(w, 200, u)
}
func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		s.internal(w, e)
		return
	}
	defer tx.Rollback()
	e = finalAdminGuard(r.Context(), tx, r.PathValue("id"))
	if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "This account could not be found.")
		return
	}
	if errors.Is(e, errFinalAdmin) {
		fail(w, 409, e.Error())
		return
	}
	if e != nil {
		s.internal(w, e)
		return
	}
	if _, e = tx.ExecContext(r.Context(), "DELETE FROM users WHERE id=?", r.PathValue("id")); e != nil {
		s.internal(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		s.internal(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
