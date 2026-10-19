package app

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

// Lock tables are additive, so an existing database keeps its rows and defaults to unlocked.
const schema = `
CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY);
CREATE TABLE IF NOT EXISTS users(id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE COLLATE NOCASE, name TEXT NOT NULL, password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('admin','user')), active INTEGER NOT NULL CHECK(active IN (0,1)), created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(token_hash TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,expires_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);
CREATE TABLE IF NOT EXISTS folders(id TEXT PRIMARY KEY,path TEXT UNIQUE NOT NULL,name TEXT NOT NULL,parent_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS media(id TEXT PRIMARY KEY,path TEXT UNIQUE NOT NULL,folder_id TEXT NOT NULL,name TEXT NOT NULL,kind TEXT NOT NULL,mime TEXT NOT NULL,format TEXT NOT NULL,size_bytes INTEGER NOT NULL,modified_ns INTEGER NOT NULL,width INTEGER NOT NULL,height INTEGER NOT NULL,duration REAL NOT NULL,thumbnail TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS media_folder ON media(folder_id);
CREATE TABLE IF NOT EXISTS playback_progress(user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,media_id TEXT NOT NULL REFERENCES media(id) ON DELETE CASCADE,position REAL NOT NULL,updated_at INTEGER NOT NULL,PRIMARY KEY(user_id,media_id));
CREATE TABLE IF NOT EXISTS session_locks(session_hash TEXT PRIMARY KEY REFERENCES sessions(token_hash) ON DELETE CASCADE,user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,enabled INTEGER NOT NULL CHECK(enabled IN (0,1)),created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS session_locks_user ON session_locks(user_id);
CREATE TABLE IF NOT EXISTS session_lock_epochs(session_hash TEXT PRIMARY KEY REFERENCES sessions(token_hash) ON DELETE CASCADE,epoch INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS webauthn_credentials(id TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,credential TEXT NOT NULL,created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS webauthn_credentials_user ON webauthn_credentials(user_id);
CREATE TABLE IF NOT EXISTS lock_challenges(id TEXT PRIMARY KEY,session_hash TEXT NOT NULL REFERENCES sessions(token_hash) ON DELETE CASCADE,user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,purpose TEXT NOT NULL,data TEXT NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS lock_challenges_session ON lock_challenges(session_hash);
CREATE TABLE IF NOT EXISTS unlock_grants(token_hash TEXT PRIMARY KEY,session_hash TEXT NOT NULL REFERENCES sessions(token_hash) ON DELETE CASCADE,user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS unlock_grants_session ON unlock_grants(session_hash);
CREATE TABLE IF NOT EXISTS playback_derivations(media_id TEXT PRIMARY KEY REFERENCES media(id) ON DELETE CASCADE,source_key TEXT NOT NULL,strategy TEXT NOT NULL CHECK(strategy IN ('none','remux','transcode_audio','transcode_full')),status TEXT NOT NULL CHECK(status IN ('pending','processing','waiting_source','ready','failed','unsupported')),artifact TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL DEFAULT '',attempt INTEGER NOT NULL DEFAULT 0,updated_ns INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS playback_derivations_status ON playback_derivations(status);
CREATE TABLE IF NOT EXISTS media_retries(media_id TEXT NOT NULL REFERENCES media(id) ON DELETE CASCADE,stage TEXT NOT NULL CHECK(stage IN ('probe','sprite')),source_key TEXT NOT NULL,attempt INTEGER NOT NULL DEFAULT 0,next_ns INTEGER NOT NULL,reason TEXT NOT NULL DEFAULT '',PRIMARY KEY(media_id,stage));
CREATE INDEX IF NOT EXISTS media_retries_due ON media_retries(next_ns);
CREATE TABLE IF NOT EXISTS media_skips(media_id TEXT PRIMARY KEY REFERENCES media(id) ON DELETE CASCADE,reason TEXT NOT NULL DEFAULT '',created_by TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL);
INSERT OR IGNORE INTO schema_migrations(version) VALUES(1);
INSERT OR IGNORE INTO schema_migrations(version) VALUES(2);
INSERT OR IGNORE INTO schema_migrations(version) VALUES(3);
INSERT OR IGNORE INTO schema_migrations(version) VALUES(4);
`

type User struct {
	ID        string `json:"id"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Active    bool   `json:"active"`
	CreatedAt int64  `json:"createdAt"`
}

// Session is the account view; authorization is the unlock cookie's grant, not a stored flag.
type Session struct {
	ID               string `json:"id"`
	Username         string `json:"username"`
	Name             string `json:"name"`
	Role             string `json:"role"`
	LockEnabled      bool   `json:"lockEnabled"`
	HasPasskey       bool   `json:"hasPasskey"`
	PasskeyAvailable bool   `json:"passkeyAvailable"`
}

func randomID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func OpenDB(dir string) (*sql.DB, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "mediaex.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, p := range []string{"PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err = db.Exec(p); err != nil {
			db.Close()
			return nil, err
		}
	}
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err = tx.Exec(schema); err != nil {
		tx.Rollback()
		db.Close()
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func Bootstrap(ctx context.Context, db *sql.DB, name, password string) error {
	var n int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if err := validateUsername(name); err != nil {
		return fmt.Errorf("ADMIN_NAME: %w", err)
	}
	if err := validatePassword(password); err != nil {
		return fmt.Errorf("ADMIN_PASSWORD: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO users VALUES(?,?,?,?,?,1,?)", randomID(), name, name, string(hash), "admin", time.Now().Unix())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Username, &u.Name, &u.Role, &u.Active, &u.CreatedAt)
	return u, err
}

const userCols = "id,username,name,role,active,created_at"

var errFinalAdmin = errors.New("The last active administrator cannot be disabled, demoted, or deleted.")

func finalAdminGuard(ctx context.Context, tx *sql.Tx, id string) error {
	var role string
	var active bool
	if err := tx.QueryRowContext(ctx, "SELECT role,active FROM users WHERE id=?", id).Scan(&role, &active); err != nil {
		return err
	}
	if role == "admin" && active {
		var others int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE role='admin' AND active=1 AND id<>?", id).Scan(&others); err != nil {
			return err
		}
		if others == 0 {
			return errFinalAdmin
		}
	}
	return nil
}

// purgeExpiredLockRows reuses the auth purge point so no scheduler is needed.
func purgeExpiredLockRows(ctx context.Context, tx *sql.Tx, now int64) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM unlock_grants WHERE expires_at<=?", now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM lock_challenges WHERE expires_at<=?", now)
	return err
}
