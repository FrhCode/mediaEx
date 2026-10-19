package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"golang.org/x/crypto/bcrypt"
)

const (
	// grantTTL is the page-unlock lease; a renewal never revives an expired grant.
	grantTTL = 60 * time.Second
	// challengeTTL bounds how long a ceremony may stay open.
	challengeTTL = 2 * time.Minute
	// maxCeremonyBody bounds a WebAuthn finish payload, which is larger than ordinary JSON.
	maxCeremonyBody = 32768
)

var (
	errLockUnavailable  = errors.New("passkeys are unavailable")
	errChallengeInvalid = errors.New("challenge is missing, expired, or already used")
	errPolicyChanged    = errors.New("lock policy changed during the ceremony")
	errCredentialTaken  = errors.New("credential belongs to another account")
	// errLockDisabled is the commit-time refusal of a proof whose policy is already gone.
	errLockDisabled = errors.New("the lock is not enabled")
)

// ceremonyData binds a ceremony to the policy epoch, so it cannot resurrect a disabled lock.
type ceremonyData struct {
	Session webauthn.SessionData `json:"session"`
	Epoch   int64                `json:"epoch"`
}

// grantResult is the unlock response body: the lease expiry only, never the grant itself.
type grantResult struct {
	ExpiresAt int64 `json:"expiresAt"`
}

// registerResult adds the account view for the ceremonies that also change the policy.
type registerResult struct {
	ExpiresAt int64   `json:"expiresAt"`
	Session   Session `json:"session"`
}

// unlockCookie carries the unlock grant: HttpOnly and /api-scoped, it stays out of JS and URLs.
const unlockCookie = "mediaex_unlock"

// unlockToken reads the presented grant; an absent or malformed cookie counts as no grant.
func unlockToken(r *http.Request) string {
	c, err := r.Cookie(unlockCookie)
	if err != nil || len(c.Value) != 64 {
		return ""
	}
	return c.Value
}

// setUnlockCookie mirrors the session cookie on the /api path; the database expiry stays truth.
func (s *Server) setUnlockCookie(w http.ResponseWriter, token string, expiresAt int64) {
	http.SetCookie(w, &http.Cookie{Name: unlockCookie, Value: token, Path: "/api", HttpOnly: true, Secure: s.Config.CookieSecure, SameSite: http.SameSiteLaxMode, Expires: time.Unix(expiresAt, 0), MaxAge: int(grantTTL / time.Second)})
}

// clearUnlockCookie drops the grant cookie; callers pair it with a committed revocation.
func (s *Server) clearUnlockCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: unlockCookie, Value: "", Path: "/api", HttpOnly: true, Secure: s.Config.CookieSecure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

type optionsResult struct {
	CeremonyID string `json:"ceremonyId"`
	PublicKey  any    `json:"publicKey"`
}

// rpConfig derives RP ID/origin from PUBLIC_ORIGIN only, never the untrusted Host header.
func (s *Server) rpConfig() (rpID, origin string, ok bool) {
	if s.Config.PublicOrigin == "" {
		return "", "", false
	}
	u, err := url.Parse(s.Config.PublicOrigin)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", false
	}
	host := u.Hostname()
	if net.ParseIP(host) != nil {
		return "", "", false
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && host == "localhost":
	default:
		return "", "", false
	}
	return host, s.Config.PublicOrigin, true
}

// passkeyAvailable reports a configured deployment, not any account property.
func (s *Server) passkeyAvailable() bool {
	_, _, ok := s.rpConfig()
	return ok
}

func (s *Server) webauthn() (*webauthn.WebAuthn, error) {
	rpID, origin, ok := s.rpConfig()
	if !ok {
		return nil, errLockUnavailable
	}
	return webauthn.New(&webauthn.Config{RPID: rpID, RPDisplayName: "mediaEx", RPOrigins: []string{origin}})
}

// waUser adapts an account to the library; the user handle is the stable account ID.
type waUser struct {
	user  User
	creds []webauthn.Credential
}

func (w waUser) WebAuthnID() []byte                         { return []byte(w.user.ID) }
func (w waUser) WebAuthnName() string                       { return w.user.Username }
func (w waUser) WebAuthnDisplayName() string                { return w.user.Name }
func (w waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

func (s *Server) loadWAUser(ctx context.Context, u User) (waUser, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT id,credential FROM webauthn_credentials WHERE user_id=? ORDER BY created_at,id", u.ID)
	if err != nil {
		return waUser{}, err
	}
	defer rows.Close()
	w := waUser{user: u}
	for rows.Next() {
		var id, raw string
		if err = rows.Scan(&id, &raw); err != nil {
			return waUser{}, err
		}
		var c webauthn.Credential
		if err = json.Unmarshal([]byte(raw), &c); err != nil {
			return waUser{}, err
		}
		w.creds = append(w.creds, c)
	}
	return w, rows.Err()
}

func credentialKey(id []byte) string { return base64.RawURLEncoding.EncodeToString(id) }

func (s *Server) newChallenge(ctx context.Context, si sessionInfo, purpose string, sd *webauthn.SessionData) (string, error) {
	id := randomID()
	now := time.Now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	// Opening a ceremony is a natural point to drop expired rows without a scheduler.
	if err = purgeExpiredLockRows(ctx, tx, now.Unix()); err != nil {
		return "", err
	}
	epoch, err := lockEpochTx(ctx, tx, si.Hash)
	if err != nil {
		return "", err
	}
	blob, err := json.Marshal(ceremonyData{Session: *sd, Epoch: epoch})
	if err != nil {
		return "", err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO lock_challenges(id,session_hash,user_id,purpose,data,created_at,expires_at) VALUES(?,?,?,?,?,?,?)", id, si.Hash, si.User.ID, purpose, string(blob), now.Unix(), now.Add(challengeTTL).Unix()); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// consumeChallenge deletes before verifying, so a challenge is single-use even on failure.
func (s *Server) consumeChallenge(ctx context.Context, ceremonyID, purpose, sessionHash, userID string) (*webauthn.SessionData, int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	var data, storedUser string
	var expires int64
	err = tx.QueryRowContext(ctx, "SELECT data,user_id,expires_at FROM lock_challenges WHERE id=? AND purpose=? AND session_hash=?", ceremonyID, purpose, sessionHash).Scan(&data, &storedUser, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, errChallengeInvalid
	}
	if err != nil {
		return nil, 0, err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM lock_challenges WHERE id=?", ceremonyID); err != nil {
		return nil, 0, err
	}
	if err = tx.Commit(); err != nil {
		return nil, 0, err
	}
	if storedUser != userID || expires <= time.Now().Unix() {
		return nil, 0, errChallengeInvalid
	}
	var cd ceremonyData
	if err = json.Unmarshal([]byte(data), &cd); err != nil {
		return nil, 0, err
	}
	return &cd.Session, cd.Epoch, nil
}

// sessionValidTx re-checks the session inside the transaction, so an expired one gets no grant.
func sessionValidTx(ctx context.Context, tx *sql.Tx, sessionHash string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>? AND u.active=1", sessionHash, time.Now().Unix()).Scan(&n)
	return n > 0, err
}

// lockEnabledTx checks the policy inside the transaction, so a committed disable wins.
func lockEnabledTx(ctx context.Context, tx *sql.Tx, sessionHash string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM session_locks WHERE session_hash=? AND enabled=1", sessionHash).Scan(&n)
	return n > 0, err
}

// lockEpochTx reads the session's ceremony epoch inside a transaction.
func lockEpochTx(ctx context.Context, tx *sql.Tx, sessionHash string) (int64, error) {
	var epoch int64
	err := tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT epoch FROM session_lock_epochs WHERE session_hash=?),0)", sessionHash).Scan(&epoch)
	return epoch, err
}

// lockEpoch snapshots the epoch before a slow verification such as bcrypt.
func (s *Server) lockEpoch(ctx context.Context, sessionHash string) (int64, error) {
	var epoch int64
	err := s.DB.QueryRowContext(ctx, "SELECT COALESCE((SELECT epoch FROM session_lock_epochs WHERE session_hash=?),0)", sessionHash).Scan(&epoch)
	return epoch, err
}

// lockGrantGuardTx re-checks the policy and the caller's epoch snapshot before granting.
func lockGrantGuardTx(ctx context.Context, tx *sql.Tx, sessionHash string, epoch int64) error {
	enabled, err := lockEnabledTx(ctx, tx, sessionHash)
	if err != nil {
		return err
	}
	if !enabled {
		return errLockDisabled
	}
	current, err := lockEpochTx(ctx, tx, sessionHash)
	if err != nil {
		return err
	}
	if current != epoch {
		return errPolicyChanged
	}
	return nil
}

// lockGuardError maps a commit-time policy refusal onto its response.
func (s *Server) lockGuardError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errLockDisabled):
		failCode(w, 409, "LOCK_NOT_ENABLED", "This session is not locked.")
	case errors.Is(err, errPolicyChanged):
		failCode(w, 423, "APP_LOCKED", "This session is locked. Unlock to continue.")
	default:
		s.internal(w, err)
	}
}

// enableLockTx enables the policy and refuses a stale epoch; other tabs' live grants stay.
func enableLockTx(ctx context.Context, tx *sql.Tx, si sessionInfo, epoch int64) error {
	current, err := lockEpochTx(ctx, tx, si.Hash)
	if err != nil {
		return err
	}
	if current != epoch {
		return errPolicyChanged
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO session_locks(session_hash,user_id,enabled,created_at) VALUES(?,?,1,?) ON CONFLICT(session_hash) DO UPDATE SET enabled=1,user_id=excluded.user_id", si.Hash, si.User.ID, time.Now().Unix())
	return err
}

// bumpEpoch invalidates any in-flight ceremony that would re-enable the policy.
func bumpEpoch(ctx context.Context, tx *sql.Tx, sessionHash string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO session_lock_epochs(session_hash,epoch) VALUES(?,1) ON CONFLICT(session_hash) DO UPDATE SET epoch=session_lock_epochs.epoch+1", sessionHash)
	return err
}

// claimCredentialTx refuses an ID owned by another account; the owner's repeat refreshes it.
func claimCredentialTx(ctx context.Context, tx *sql.Tx, userID, id, blob string) error {
	var owner string
	err := tx.QueryRowContext(ctx, "SELECT user_id FROM webauthn_credentials WHERE id=?", id).Scan(&owner)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if owner != "" && owner != userID {
		return errCredentialTaken
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO webauthn_credentials(id,user_id,credential,created_at) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET credential=excluded.credential", id, userID, blob, time.Now().Unix())
	return err
}

// verifyAssertion consumes a purpose-bound assertion and returns the refreshed credential.
func (s *Server) verifyAssertion(ctx context.Context, wa *webauthn.WebAuthn, u waUser, si sessionInfo, ceremonyID, purpose string, raw json.RawMessage) (*webauthn.Credential, int64, error) {
	sd, epoch, err := s.consumeChallenge(ctx, ceremonyID, purpose, si.Hash, si.User.ID)
	if err != nil {
		return nil, 0, err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(raw)
	if err != nil {
		return nil, 0, err
	}
	cred, err := wa.ValidateLogin(u, *sd, parsed)
	return cred, epoch, err
}

func issueGrant(ctx context.Context, tx *sql.Tx, sessionHash, userID string) (string, int64, error) {
	now := time.Now()
	// Issuing a grant also reaps expired rows, keeping a long-lived session bounded.
	if err := purgeExpiredLockRows(ctx, tx, now.Unix()); err != nil {
		return "", 0, err
	}
	token := randomID()
	sum := sha256.Sum256([]byte(token))
	expiresAt := now.Add(grantTTL).Unix()
	if _, err := tx.ExecContext(ctx, "INSERT INTO unlock_grants(token_hash,session_hash,user_id,created_at,expires_at) VALUES(?,?,?,?,?)", hex.EncodeToString(sum[:]), sessionHash, userID, now.Unix(), expiresAt); err != nil {
		return "", 0, err
	}
	return token, expiresAt, nil
}

// grantHash is the stored form of the plaintext grant carried in the unlock cookie.
func grantHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Server) passwordMatches(ctx context.Context, userID, password string) (bool, error) {
	var hash string
	err := s.DB.QueryRowContext(ctx, "SELECT password_hash FROM users WHERE id=? AND active=1", userID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil, nil
}

// verifyPassword throttles account and IP; a wrong password is 403, since the identity is valid.
func (s *Server) verifyPassword(w http.ResponseWriter, r *http.Request, userID, password string) bool {
	acctKey, ipKey := "lock:"+userID, "lock-ip:"+s.clientIP(r)
	if !s.allow(acctKey) || !s.allow(ipKey) {
		w.Header().Set("Retry-After", "900")
		fail(w, 429, "Too many attempts. Please try again later.")
		return false
	}
	ok, err := s.passwordMatches(r.Context(), userID, password)
	if err != nil {
		s.internal(w, err)
		return false
	}
	if !ok {
		failCode(w, 403, "INVALID_PASSWORD", "That password is incorrect.")
		return false
	}
	s.clearAttempt(acctKey)
	s.clearAttempt(ipKey)
	return true
}

func (s *Server) lockStatus(w http.ResponseWriter, r *http.Request) {
	si := currentSession(r)
	var n int
	if e := s.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM webauthn_credentials WHERE user_id=?", si.User.ID).Scan(&n); e != nil {
		s.internal(w, e)
		return
	}
	// Status is read-only: it reports the presented token's own row and never renews it.
	var expiresAt int64
	active := false
	if si.LockEnabled {
		expiresAt, active = s.grantExpiry(r, si.Hash)
	}
	writeJSON(w, 200, map[string]any{
		"lockEnabled":      si.LockEnabled,
		"hasPasskey":       n > 0,
		"passkeyAvailable": s.passkeyAvailable(),
		"grantActive":      active,
		"expiresAt":        expiresAt,
	})
}

func (s *Server) lockRegisterOptions(w http.ResponseWriter, r *http.Request) {
	si := currentSession(r)
	var body struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !s.verifyPassword(w, r, si.User.ID, body.Password) {
		return
	}
	wa, err := s.webauthn()
	if errors.Is(err, errLockUnavailable) {
		failCode(w, 409, "LOCK_UNAVAILABLE", "Passkeys need a trusted HTTPS origin or a localhost development origin.")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	u, err := s.loadWAUser(r.Context(), si.User)
	if err != nil {
		s.internal(w, err)
		return
	}
	exclusions := make([]protocol.CredentialDescriptor, 0, len(u.creds))
	for i := range u.creds {
		exclusions = append(exclusions, u.creds[i].Descriptor())
	}
	creation, sd, err := wa.BeginRegistration(u,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			AuthenticatorAttachment: protocol.Platform,
			ResidentKey:             protocol.ResidentKeyRequirementRequired,
			UserVerification:        protocol.VerificationRequired,
		}),
		webauthn.WithExclusions(exclusions),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
	)
	if err != nil {
		s.internal(w, err)
		return
	}
	id, err := s.newChallenge(r.Context(), si, "register", sd)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, 200, optionsResult{CeremonyID: id, PublicKey: creation.Response})
}

func (s *Server) lockRegisterFinish(w http.ResponseWriter, r *http.Request) {
	si := currentSession(r)
	var body struct {
		CeremonyID string          `json:"ceremonyId"`
		Credential json.RawMessage `json:"credential"`
	}
	if !decodeMax(w, r, &body, maxCeremonyBody) {
		return
	}
	if body.CeremonyID == "" || len(body.Credential) == 0 {
		fail(w, 400, "Invalid request body.")
		return
	}
	wa, err := s.webauthn()
	if errors.Is(err, errLockUnavailable) {
		failCode(w, 409, "LOCK_UNAVAILABLE", "Passkeys need a trusted HTTPS origin or a localhost development origin.")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	u, err := s.loadWAUser(r.Context(), si.User)
	if err != nil {
		s.internal(w, err)
		return
	}
	sd, epoch, err := s.consumeChallenge(r.Context(), body.CeremonyID, "register", si.Hash, si.User.ID)
	if err != nil {
		failCode(w, 400, "CEREMONY_INVALID", "This registration could not be completed. Please start again.")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(body.Credential)
	if err != nil {
		failCode(w, 400, "CEREMONY_INVALID", "This registration could not be completed. Please start again.")
		return
	}
	cred, err := wa.CreateCredential(u, *sd, parsed)
	if err != nil {
		failCode(w, 400, "CEREMONY_INVALID", "This registration could not be completed. Please start again.")
		return
	}
	blob, err := json.Marshal(cred)
	if err != nil {
		s.internal(w, err)
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer tx.Rollback()
	ok, err := sessionValidTx(r.Context(), tx, si.Hash)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !ok {
		fail(w, 401, "Please sign in to continue.")
		return
	}
	if err = claimCredentialTx(r.Context(), tx, si.User.ID, credentialKey(cred.ID), string(blob)); err != nil {
		if errors.Is(err, errCredentialTaken) {
			failCode(w, 409, "CREDENTIAL_IN_USE", "This passkey is already registered to another account.")
			return
		}
		s.internal(w, err)
		return
	}
	if err = enableLockTx(r.Context(), tx, si, epoch); err != nil {
		if errors.Is(err, errPolicyChanged) {
			failCode(w, 409, "LOCK_CHANGED", "The lock settings changed. Please start again.")
			return
		}
		s.internal(w, err)
		return
	}
	grant, expiresAt, err := issueGrant(r.Context(), tx, si.Hash, si.User.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		s.internal(w, err)
		return
	}
	dto, err := s.sessionDTO(r.Context(), si.User, true)
	if err != nil {
		s.internal(w, err)
		return
	}
	// The grant leaves the server only as the HttpOnly cookie.
	s.setUnlockCookie(w, grant, expiresAt)
	writeJSON(w, 200, registerResult{ExpiresAt: expiresAt, Session: dto})
}

func (s *Server) lockUnlockOptions(w http.ResponseWriter, r *http.Request) {
	si := currentSession(r)
	var body struct{}
	if !decodeMax(w, r, &body, 1024) {
		return
	}
	wa, err := s.webauthn()
	if errors.Is(err, errLockUnavailable) {
		failCode(w, 409, "LOCK_UNAVAILABLE", "Passkeys need a trusted HTTPS origin or a localhost development origin.")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if !si.LockEnabled {
		failCode(w, 409, "LOCK_NOT_ENABLED", "This session is not locked.")
		return
	}
	u, err := s.loadWAUser(r.Context(), si.User)
	if err != nil {
		s.internal(w, err)
		return
	}
	if len(u.creds) == 0 {
		failCode(w, 409, "NO_PASSKEY", "No passkey is registered for this account.")
		return
	}
	assertion, sd, err := wa.BeginLogin(u, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		s.internal(w, err)
		return
	}
	id, err := s.newChallenge(r.Context(), si, "unlock", sd)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, 200, optionsResult{CeremonyID: id, PublicKey: assertion.Response})
}

func (s *Server) lockUnlockFinish(w http.ResponseWriter, r *http.Request) {
	si := currentSession(r)
	var body struct {
		CeremonyID string          `json:"ceremonyId"`
		Credential json.RawMessage `json:"credential"`
	}
	if !decodeMax(w, r, &body, maxCeremonyBody) {
		return
	}
	if body.CeremonyID == "" || len(body.Credential) == 0 {
		fail(w, 400, "Invalid request body.")
		return
	}
	wa, err := s.webauthn()
	if errors.Is(err, errLockUnavailable) {
		failCode(w, 409, "LOCK_UNAVAILABLE", "Passkeys need a trusted HTTPS origin or a localhost development origin.")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	u, err := s.loadWAUser(r.Context(), si.User)
	if err != nil {
		s.internal(w, err)
		return
	}
	cred, epoch, err := s.verifyAssertion(r.Context(), wa, u, si, body.CeremonyID, "unlock", body.Credential)
	if err != nil {
		failCode(w, 403, "UNLOCK_FAILED", "Unlock could not be verified. Please try again.")
		return
	}
	blob, err := json.Marshal(cred)
	if err != nil {
		s.internal(w, err)
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer tx.Rollback()
	ok, err := sessionValidTx(r.Context(), tx, si.Hash)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !ok {
		fail(w, 401, "Please sign in to continue.")
		return
	}
	// The epoch snapshot taken when the challenge opened refuses a proof predating a lock.
	if err = lockGrantGuardTx(r.Context(), tx, si.Hash, epoch); err != nil {
		s.lockGuardError(w, err)
		return
	}
	// The assertion moved the counter and backup flags, so write the record back.
	if _, err = tx.ExecContext(r.Context(), "UPDATE webauthn_credentials SET credential=? WHERE id=? AND user_id=?", string(blob), credentialKey(cred.ID), si.User.ID); err != nil {
		s.internal(w, err)
		return
	}
	grant, expiresAt, err := issueGrant(r.Context(), tx, si.Hash, si.User.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		s.internal(w, err)
		return
	}
	s.setUnlockCookie(w, grant, expiresAt)
	writeJSON(w, 200, grantResult{ExpiresAt: expiresAt})
}

// lockEnableOptions lets an account with a passkey lock a new session by assertion.
func (s *Server) lockEnableOptions(w http.ResponseWriter, r *http.Request) {
	si := currentSession(r)
	var body struct{}
	if !decodeMax(w, r, &body, 1024) {
		return
	}
	wa, err := s.webauthn()
	if errors.Is(err, errLockUnavailable) {
		failCode(w, 409, "LOCK_UNAVAILABLE", "Passkeys need a trusted HTTPS origin or a localhost development origin.")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	u, err := s.loadWAUser(r.Context(), si.User)
	if err != nil {
		s.internal(w, err)
		return
	}
	if len(u.creds) == 0 {
		failCode(w, 409, "NO_PASSKEY", "No passkey is registered for this account.")
		return
	}
	assertion, sd, err := wa.BeginLogin(u, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		s.internal(w, err)
		return
	}
	id, err := s.newChallenge(r.Context(), si, "enable", sd)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, 200, optionsResult{CeremonyID: id, PublicKey: assertion.Response})
}

// lockEnableFinish turns a fresh assertion into the policy change and the page's grant.
func (s *Server) lockEnableFinish(w http.ResponseWriter, r *http.Request) {
	si := currentSession(r)
	var body struct {
		CeremonyID string          `json:"ceremonyId"`
		Credential json.RawMessage `json:"credential"`
	}
	if !decodeMax(w, r, &body, maxCeremonyBody) {
		return
	}
	if body.CeremonyID == "" || len(body.Credential) == 0 {
		fail(w, 400, "Invalid request body.")
		return
	}
	wa, err := s.webauthn()
	if errors.Is(err, errLockUnavailable) {
		failCode(w, 409, "LOCK_UNAVAILABLE", "Passkeys need a trusted HTTPS origin or a localhost development origin.")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	u, err := s.loadWAUser(r.Context(), si.User)
	if err != nil {
		s.internal(w, err)
		return
	}
	cred, epoch, err := s.verifyAssertion(r.Context(), wa, u, si, body.CeremonyID, "enable", body.Credential)
	if err != nil {
		failCode(w, 403, "UNLOCK_FAILED", "This could not be verified. Please try again.")
		return
	}
	blob, err := json.Marshal(cred)
	if err != nil {
		s.internal(w, err)
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer tx.Rollback()
	ok, err := sessionValidTx(r.Context(), tx, si.Hash)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !ok {
		fail(w, 401, "Please sign in to continue.")
		return
	}
	if _, err = tx.ExecContext(r.Context(), "UPDATE webauthn_credentials SET credential=? WHERE id=? AND user_id=?", string(blob), credentialKey(cred.ID), si.User.ID); err != nil {
		s.internal(w, err)
		return
	}
	if err = enableLockTx(r.Context(), tx, si, epoch); err != nil {
		if errors.Is(err, errPolicyChanged) {
			failCode(w, 409, "LOCK_CHANGED", "The lock settings changed. Please start again.")
			return
		}
		s.internal(w, err)
		return
	}
	grant, expiresAt, err := issueGrant(r.Context(), tx, si.Hash, si.User.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		s.internal(w, err)
		return
	}
	dto, err := s.sessionDTO(r.Context(), si.User, true)
	if err != nil {
		s.internal(w, err)
		return
	}
	// The grant leaves the server only as the HttpOnly cookie.
	s.setUnlockCookie(w, grant, expiresAt)
	writeJSON(w, 200, registerResult{ExpiresAt: expiresAt, Session: dto})
}

func (s *Server) lockPassword(w http.ResponseWriter, r *http.Request) {
	si := currentSession(r)
	var body struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !si.LockEnabled {
		failCode(w, 409, "LOCK_NOT_ENABLED", "This session is not locked.")
		return
	}
	// Snapshot the epoch before bcrypt, so a lock committing during the comparison is refused.
	epoch, err := s.lockEpoch(r.Context(), si.Hash)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !s.verifyPassword(w, r, si.User.ID, body.Password) {
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer tx.Rollback()
	ok, err := sessionValidTx(r.Context(), tx, si.Hash)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !ok {
		fail(w, 401, "Please sign in to continue.")
		return
	}
	if err = lockGrantGuardTx(r.Context(), tx, si.Hash, epoch); err != nil {
		s.lockGuardError(w, err)
		return
	}
	grant, expiresAt, err := issueGrant(r.Context(), tx, si.Hash, si.User.ID)
	if err != nil {
		s.internal(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		s.internal(w, err)
		return
	}
	s.setUnlockCookie(w, grant, expiresAt)
	writeJSON(w, 200, grantResult{ExpiresAt: expiresAt})
}

// lockRenew extends only the presented live grant; a failed renewal leaves the cookie alone.
func (s *Server) lockRenew(w http.ResponseWriter, r *http.Request) {
	si := currentSession(r)
	if !si.LockEnabled {
		failCode(w, 409, "LOCK_NOT_ENABLED", "This session is not locked.")
		return
	}
	grant := unlockToken(r)
	if grant == "" {
		failCode(w, 423, "APP_LOCKED", "This session is locked. Unlock to continue.")
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer tx.Rollback()
	ok, err := sessionValidTx(r.Context(), tx, si.Hash)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !ok {
		fail(w, 401, "Please sign in to continue.")
		return
	}
	enabled, err := lockEnabledTx(r.Context(), tx, si.Hash)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !enabled {
		failCode(w, 409, "LOCK_NOT_ENABLED", "This session is not locked.")
		return
	}
	now := time.Now()
	expiresAt := now.Add(grantTTL).Unix()
	res, err := tx.ExecContext(r.Context(), "UPDATE unlock_grants SET expires_at=? WHERE token_hash=? AND session_hash=? AND expires_at>?", expiresAt, grantHash(grant), si.Hash, now.Unix())
	if err != nil {
		s.internal(w, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 423 without touching the cookie: the browser may hold a newer grant from another tab.
		failCode(w, 423, "APP_LOCKED", "This session is locked. Unlock to continue.")
		return
	}
	if err = tx.Commit(); err != nil {
		s.internal(w, err)
		return
	}
	// The same token is re-sent with a refreshed browser expiry; the row above is the truth.
	s.setUnlockCookie(w, grant, expiresAt)
	writeJSON(w, 200, grantResult{ExpiresAt: expiresAt})
}

// lockLock revokes every session grant, bumps the epoch, and needs no grant without progress.
func (s *Server) lockLock(w http.ResponseWriter, r *http.Request) {
	si := currentSession(r)
	var body struct {
		Progress *struct {
			ItemID  string  `json:"itemId"`
			Seconds float64 `json:"seconds"`
		} `json:"progress"`
	}
	if !decode(w, r, &body) {
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer tx.Rollback()
	ok, err := sessionValidTx(r.Context(), tx, si.Hash)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !ok {
		fail(w, 401, "Please sign in to continue.")
		return
	}
	if body.Progress != nil {
		p := body.Progress.Seconds
		if math.IsNaN(p) || math.IsInf(p, 0) {
			fail(w, 400, "Invalid playback progress.")
			return
		}
		var kind string
		var duration float64
		err = tx.QueryRowContext(r.Context(), "SELECT kind,duration FROM media WHERE id=?", body.Progress.ItemID).Scan(&kind, &duration)
		if errors.Is(err, sql.ErrNoRows) {
			fail(w, 404, "This file could not be found.")
			return
		}
		if err != nil {
			s.internal(w, err)
			return
		}
		if kind != "video" {
			fail(w, 400, "Progress is only available for video.")
			return
		}
		// Progress is written only while the grant is live in this transaction, atomically.
		grant := unlockToken(r)
		if grant == "" {
			failCode(w, 423, "APP_LOCKED", "This session is locked. Unlock to continue.")
			return
		}
		res, err := tx.ExecContext(r.Context(), "DELETE FROM unlock_grants WHERE token_hash=? AND session_hash=? AND expires_at>?", grantHash(grant), si.Hash, time.Now().Unix())
		if err != nil {
			s.internal(w, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			failCode(w, 423, "APP_LOCKED", "This session is locked. Unlock to continue.")
			return
		}
		p = math.Max(0, p)
		if duration > 0 {
			p = math.Min(duration, p)
		}
		if _, err = tx.ExecContext(r.Context(), "INSERT INTO playback_progress(user_id,media_id,position,updated_at) VALUES(?,?,?,unixepoch()) ON CONFLICT(user_id,media_id) DO UPDATE SET position=excluded.position,updated_at=excluded.updated_at", si.User.ID, body.Progress.ItemID, p); err != nil {
			s.internal(w, err)
			return
		}
	}
	if err = bumpEpoch(r.Context(), tx, si.Hash); err != nil {
		s.internal(w, err)
		return
	}
	for _, stmt := range []string{
		"DELETE FROM unlock_grants WHERE session_hash=?",
		"DELETE FROM lock_challenges WHERE session_hash=?",
	} {
		if _, err = tx.ExecContext(r.Context(), stmt, si.Hash); err != nil {
			s.internal(w, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		s.internal(w, err)
		return
	}
	s.clearUnlockCookie(w)
	writeJSON(w, 200, map[string]bool{"locked": true})
}

// lockDisable needs a fresh password, then clears policy, grants, and challenges; keeps credentials.
func (s *Server) lockDisable(w http.ResponseWriter, r *http.Request) {
	si := currentSession(r)
	var body struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !s.verifyPassword(w, r, si.User.ID, body.Password) {
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer tx.Rollback()
	ok, err := sessionValidTx(r.Context(), tx, si.Hash)
	if err != nil {
		s.internal(w, err)
		return
	}
	if !ok {
		fail(w, 401, "Please sign in to continue.")
		return
	}
	// The epoch bump invalidates any ceremony already in flight for this session.
	if err = bumpEpoch(r.Context(), tx, si.Hash); err != nil {
		s.internal(w, err)
		return
	}
	for _, stmt := range []string{
		"DELETE FROM session_locks WHERE session_hash=?",
		"DELETE FROM unlock_grants WHERE session_hash=?",
		"DELETE FROM lock_challenges WHERE session_hash=?",
	} {
		if _, err = tx.ExecContext(r.Context(), stmt, si.Hash); err != nil {
			s.internal(w, err)
			return
		}
	}
	if err = tx.Commit(); err != nil {
		s.internal(w, err)
		return
	}
	s.clearUnlockCookie(w)
	dto, err := s.sessionDTO(r.Context(), si.User, false)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"session": dto})
}
