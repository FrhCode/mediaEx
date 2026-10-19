package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// issueMediaCols is the shared projection for every admin issue tab.
const issueMediaCols = "m.id,m.path,m.name,m.kind,m.format,m.size_bytes,m.modified_ns,m.width,m.height,m.duration"

type issueItem struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Path      string  `json:"path"`
	Kind      string  `json:"kind"`
	Format    string  `json:"format"`
	Size      int64   `json:"size"`
	Modified  int64   `json:"modifiedAt"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Duration  float64 `json:"duration"`
	Stage     string  `json:"stage"`
	Attempt   int     `json:"attempt"`
	Max       *int    `json:"maxAttempts"`
	NextRetry *int64  `json:"nextRetryAt"`
	Reason    string  `json:"reason"`
	Strategy  string  `json:"strategy,omitempty"`
	Status    string  `json:"status,omitempty"`
	UpdatedAt *int64  `json:"updatedAt"`
	Actor     string  `json:"skippedBy,omitempty"`
}

type issueCounts struct {
	Probe       int `json:"probe"`
	Sprite      int `json:"sprite"`
	Conversions int `json:"conversions"`
	Queue       int `json:"queue"`
	Skipped     int `json:"skipped"`
	Processing  int `json:"processing"`
}

func issueMaxAttempts(stage string) *int {
	n := derivMaxAttempts
	switch stage {
	case "probe":
		return nil
	case "sprite", "conversion":
		return &n
	}
	return nil
}

// issueSearch appends the shared name/path filter; the union repeats it per branch.
func issueSearch(q string) (string, []any) {
	if q == "" {
		return "", nil
	}
	return " AND (instr(lower(m.name), lower(?)) > 0 OR instr(lower(m.path), lower(?)) > 0)", []any{q, q}
}

func failedIssuesSQL(q string) (string, []any) {
	where, args := issueSearch(q)
	query := `SELECT ` + issueMediaCols + `,r.stage,r.attempt,r.next_ns,r.reason,'','',0,''
		FROM media_retries r JOIN media m ON m.id=r.media_id
		WHERE NOT EXISTS(SELECT 1 FROM media_skips k WHERE k.media_id=m.id)` + where + `
		UNION ALL
		SELECT ` + issueMediaCols + `,'conversion',d.attempt,0,d.reason,d.strategy,d.status,d.updated_ns,''
		FROM playback_derivations d JOIN media m ON m.id=d.media_id
		WHERE d.status='failed' AND NOT EXISTS(SELECT 1 FROM media_skips k WHERE k.media_id=m.id)` + where
	return query, append(append([]any{}, args...), args...)
}

func queueIssuesSQL(q string) (string, []any) {
	where, args := issueSearch(q)
	query := `SELECT ` + issueMediaCols + `,'queue',d.attempt,0,d.reason,d.strategy,d.status,d.updated_ns,''
		FROM playback_derivations d JOIN media m ON m.id=d.media_id
		WHERE d.status IN ('pending','processing','waiting_source') AND NOT EXISTS(SELECT 1 FROM media_skips k WHERE k.media_id=m.id)` + where + `
		ORDER BY CASE d.status WHEN 'processing' THEN 0 ELSE 1 END, d.updated_ns, m.name COLLATE NOCASE`
	return query, args
}

func skippedIssuesSQL(q string) (string, []any) {
	where, args := issueSearch(q)
	query := `SELECT ` + issueMediaCols + `,'skipped',0,0,k.reason,'','',k.created_at,COALESCE(u.name,u.username,'')
		FROM media_skips k JOIN media m ON m.id=k.media_id LEFT JOIN users u ON u.id=k.created_by
		WHERE 1=1` + where + `
		ORDER BY k.created_at DESC, m.name COLLATE NOCASE`
	return query, args
}

// issueListSQL returns the tab query plus its stable outer ordering, because the
// pagination wrapper turns it into a subquery.
func issueListSQL(tab, q string) (string, string, []any) {
	switch tab {
	case "queue":
		query, args := queueIssuesSQL(q)
		return query, "ORDER BY CASE status WHEN 'processing' THEN 0 ELSE 1 END, updated_ns, name COLLATE NOCASE", args
	case "skipped":
		query, args := skippedIssuesSQL(q)
		return query, "ORDER BY created_at DESC, name COLLATE NOCASE", args
	}
	query, args := failedIssuesSQL(q)
	return query, "ORDER BY name COLLATE NOCASE, id", args
}

func (s *Server) issueCounts(ctx context.Context) (issueCounts, error) {
	var c issueCounts
	err := s.DB.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM media_retries r WHERE r.stage='probe' AND NOT EXISTS(SELECT 1 FROM media_skips k WHERE k.media_id=r.media_id)),
		(SELECT COUNT(*) FROM media_retries r WHERE r.stage='sprite' AND NOT EXISTS(SELECT 1 FROM media_skips k WHERE k.media_id=r.media_id)),
		(SELECT COUNT(*) FROM playback_derivations d WHERE d.status='failed' AND NOT EXISTS(SELECT 1 FROM media_skips k WHERE k.media_id=d.media_id)),
		(SELECT COUNT(*) FROM playback_derivations d WHERE d.status IN ('pending','processing','waiting_source') AND NOT EXISTS(SELECT 1 FROM media_skips k WHERE k.media_id=d.media_id)),
		(SELECT COUNT(*) FROM media_skips),
		(SELECT COUNT(*) FROM playback_derivations d WHERE d.status='processing' AND NOT EXISTS(SELECT 1 FROM media_skips k WHERE k.media_id=d.media_id))`).
		Scan(&c.Probe, &c.Sprite, &c.Conversions, &c.Queue, &c.Skipped, &c.Processing)
	return c, err
}

func (s *Server) issues(w http.ResponseWriter, r *http.Request) {
	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "failed"
	}
	if tab != "failed" && tab != "queue" && tab != "skipped" {
		fail(w, 400, "Invalid issue tab.")
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(q) > 200 {
		fail(w, 400, "Search text is too long.")
		return
	}
	limit, offset := 50, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			fail(w, 400, "Invalid page size.")
			return
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fail(w, 400, "Invalid page offset.")
			return
		}
		offset = n
	}
	counts, err := s.issueCounts(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	query, order, args := issueListSQL(tab, q)
	var total int
	if err = s.DB.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM ("+query+")", args...).Scan(&total); err != nil {
		s.internal(w, err)
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), "SELECT * FROM ("+query+") "+order+" LIMIT ? OFFSET ?", append(args, limit, offset)...)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer rows.Close()
	items := []issueItem{}
	for rows.Next() {
		var it issueItem
		var modified, next int64
		var updated int64
		if err = rows.Scan(&it.ID, &it.Path, &it.Name, &it.Kind, &it.Format, &it.Size, &modified, &it.Width, &it.Height, &it.Duration,
			&it.Stage, &it.Attempt, &next, &it.Reason, &it.Strategy, &it.Status, &updated, &it.Actor); err != nil {
			s.internal(w, err)
			return
		}
		it.Modified = modified / int64(time.Second)
		it.Max = issueMaxAttempts(it.Stage)
		if next > 0 {
			v := next / int64(time.Second)
			it.NextRetry = &v
		}
		if updated > 0 {
			v := updated / int64(time.Second)
			it.UpdatedAt = &v
		}
		items = append(items, it)
	}
	if err = rows.Err(); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"tab": tab, "counts": counts, "total": total, "items": items, "limit": limit, "offset": offset})
}

// stopDerivations cancels queued or failing work without touching a playable artifact.
func stopDerivations(ctx context.Context, tx *sql.Tx, mediaID string) error {
	_, err := tx.ExecContext(ctx, "DELETE FROM playback_derivations WHERE media_id=? AND status NOT IN ('ready','unsupported')", mediaID)
	return err
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// mediaByID takes the caller's transaction: the SQLite pool holds one connection.
func mediaByID(ctx context.Context, q queryRower, id string) (MediaRecord, error) {
	return scanMedia(q.QueryRowContext(ctx, "SELECT "+mediaCols+" FROM media WHERE id=?", id))
}

// scheduleProbe asks the next scan to re-examine one media item.
func scheduleProbe(ctx context.Context, tx *sql.Tx, m MediaRecord) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO media_retries(media_id,stage,source_key,attempt,next_ns,reason) VALUES(?,'probe',?,0,?,'')
		ON CONFLICT(media_id,stage) DO UPDATE SET source_key=excluded.source_key,attempt=0,next_ns=excluded.next_ns,reason=''`, m.ID, mediaSourceKey(m), time.Now().UnixNano())
	return err
}

func (s *Server) skipIssue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Reason) > 500 {
		fail(w, 400, "The note is too long.")
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer tx.Rollback()
	if _, err = mediaByID(r.Context(), tx, r.PathValue("id")); errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "This media item could not be found.")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if err = skipMedia(r.Context(), tx, r.PathValue("id"), body.Reason, currentUser(r).ID); err != nil {
		s.internal(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func skipMedia(ctx context.Context, tx *sql.Tx, id, reason, userID string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO media_skips(media_id,reason,created_by,created_at) VALUES(?,?,?,?)
		ON CONFLICT(media_id) DO UPDATE SET reason=excluded.reason,created_by=excluded.created_by,created_at=excluded.created_at`, id, reason, userID, time.Now().UnixNano()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM media_retries WHERE media_id=?", id); err != nil {
		return err
	}
	return stopDerivations(ctx, tx, id)
}

func (s *Server) unskipIssue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer tx.Rollback()
	m, err := mediaByID(r.Context(), tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "This media item could not be found.")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if _, err = tx.ExecContext(r.Context(), "DELETE FROM media_skips WHERE media_id=?", id); err != nil {
		s.internal(w, err)
		return
	}
	if err = scheduleProbe(r.Context(), tx, m); err != nil {
		s.internal(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) retryIssue(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer tx.Rollback()
	m, err := mediaByID(r.Context(), tx, id)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "This media item could not be found.")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	var skipped int
	if err = tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM media_skips WHERE media_id=?", id).Scan(&skipped); err != nil {
		s.internal(w, err)
		return
	}
	if skipped > 0 {
		fail(w, 409, "This item is skipped. Restore it before trying again.")
		return
	}
	now := time.Now().UnixNano()
	if _, err = tx.ExecContext(r.Context(), "UPDATE playback_derivations SET status='pending',attempt=0,updated_ns=?,reason='' WHERE media_id=? AND status='failed'", now, id); err != nil {
		s.internal(w, err)
		return
	}
	// A probe retry makes the next scan rebuild the metadata, thumbnail, sprite,
	// and playback plan for this item in the usual pipeline.
	if err = scheduleProbe(r.Context(), tx, m); err != nil {
		s.internal(w, err)
		return
	}
	if err = tx.Commit(); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) skipIssues(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs    []string `json:"ids"`
		Reason string   `json:"reason"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.IDs) == 0 || len(body.IDs) > 200 {
		fail(w, 400, "Select between 1 and 200 items.")
		return
	}
	if len(body.Reason) > 500 {
		fail(w, 400, "The note is too long.")
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.internal(w, err)
		return
	}
	defer tx.Rollback()
	skipped := 0
	for _, id := range body.IDs {
		var exists int
		if err = tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM media WHERE id=?", id).Scan(&exists); err != nil {
			s.internal(w, err)
			return
		}
		if exists == 0 {
			continue
		}
		if err = skipMedia(r.Context(), tx, id, body.Reason, currentUser(r).ID); err != nil {
			s.internal(w, err)
			return
		}
		skipped++
	}
	if err = tx.Commit(); err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "skipped": skipped})
}
