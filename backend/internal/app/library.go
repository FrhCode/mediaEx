package app

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type Folder struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Count    int    `json:"count"`
	ParentID string `json:"parentId"`
}

// Geometry of the preview sheet, published so the player can map a scrub position
// to a tile. Absent for images and for anything without a usable duration.
type SpriteInfo struct {
	URL      string  `json:"url"`
	Cols     int     `json:"cols"`
	Rows     int     `json:"rows"`
	Count    int     `json:"count"`
	Interval float64 `json:"interval"`
}
type Playback struct {
	Status string `json:"status"`
	Src    string `json:"src,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type playbackRecord struct {
	Status, Strategy, SourceKey, Artifact string
}

const playbackCols = "COALESCE(d.status,''),COALESCE(d.strategy,''),COALESCE(d.source_key,''),COALESCE(d.artifact,'')"

func (p playbackRecord) dto(id string) *Playback {
	switch p.Status {
	case "pending", "processing", "waiting_source":
		return &Playback{Status: "processing"}
	case "ready":
		if p.Strategy == "none" {
			return &Playback{Status: "direct"}
		}
		return &Playback{Status: "ready", Src: "/api/items/" + id + "/playback?v=" + p.SourceKey}
	case "failed":
		return &Playback{Status: "failed", Reason: "This video could not be prepared for playback."}
	case "unsupported":
		return &Playback{Status: "unsupported", Reason: "This file has no supported video stream."}
	default:
		return &Playback{Status: "processing"}
	}
}

type Media struct {
	ID         string      `json:"id"`
	FolderID   string      `json:"folderId"`
	Name       string      `json:"name"`
	Kind       string      `json:"kind"`
	Image      string      `json:"image"`
	Src        string      `json:"src"`
	Resolution string      `json:"resolution"`
	Size       string      `json:"size"`
	Format     string      `json:"format"`
	Duration   float64     `json:"duration"`
	Progress   float64     `json:"progress"`
	Sprite     *SpriteInfo `json:"sprite,omitempty"`
	Playback   *Playback   `json:"playback,omitempty"`
}

func mediaDTO(m MediaRecord, progress float64) Media {
	resolution := "Unknown"
	if m.Width > 0 && m.Height > 0 {
		resolution = fmt.Sprintf("%d × %d", m.Width, m.Height)
	}
	if m.Duration > 0 && progress >= m.Duration-1 {
		progress = 0
	}
	var sprite *SpriteInfo
	if m.Kind == "video" {
		if cols, rows, count, interval, ok := spritePlan(m.Duration); ok {
			sprite = &SpriteInfo{"/api/items/" + m.ID + "/sprite?v=" + spriteCacheKey(m.Path, m.Size, m.Modified), cols, rows, count, interval}
		}
	}
	return Media{ID: m.ID, FolderID: m.FolderID, Name: m.Name, Kind: m.Kind, Image: "/api/items/" + m.ID + "/thumbnail", Src: "/api/items/" + m.ID + "/content", Resolution: resolution, Size: formatSize(m.Size), Format: m.Format, Duration: m.Duration, Progress: progress, Sprite: sprite}
}
func formatSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	units := []string{"KB", "MB", "GB", "TB"}
	for _, u := range units {
		v /= 1024
		if v < 1024 || u == "TB" {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return ""
}
func (s *Server) getMedia(r *http.Request) (MediaRecord, error) {
	return scanMedia(s.DB.QueryRowContext(r.Context(), "SELECT "+mediaCols+" FROM media WHERE id=?", r.PathValue("id")))
}
func (s *Server) library(w http.ResponseWriter, r *http.Request) {
	selectedID := r.URL.Query().Get("folder")
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "all"
		if selectedID != "" {
			scope = "folder"
		}
	}
	if (scope != "all" && scope != "folder") || (scope == "all" && selectedID != "") {
		fail(w, 400, "Invalid library scope.")
		return
	}
	rows, e := s.DB.QueryContext(r.Context(), "SELECT id,path,name,parent_id FROM folders ORDER BY name COLLATE NOCASE,path")
	if e != nil {
		s.internal(w, e)
		return
	}
	records := []FolderRecord{}
	for rows.Next() {
		var f FolderRecord
		if e = rows.Scan(&f.ID, &f.Path, &f.Name, &f.ParentID); e != nil {
			rows.Close()
			s.internal(w, e)
			return
		}
		records = append(records, f)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		s.internal(w, e)
		return
	}
	query := strings.ToLower(r.URL.Query().Get("q"))
	var selected *FolderRecord
	byID := map[string]FolderRecord{}
	for _, f := range records {
		byID[f.ID] = f
		if f.ID == selectedID {
			copy := f
			selected = &copy
		}
	}
	if selectedID != "" && selected == nil {
		fail(w, 404, "This folder could not be found.")
		return
	}
	rows, e = s.DB.QueryContext(r.Context(), "SELECT "+prefixedMediaCols()+",COALESCE(p.position,0),"+playbackCols+" FROM media m LEFT JOIN playback_progress p ON p.media_id=m.id AND p.user_id=? LEFT JOIN playback_derivations d ON d.media_id=m.id WHERE NOT EXISTS(SELECT 1 FROM media_skips k WHERE k.media_id=m.id AND (m.width<=0 OR m.height<=0)) ORDER BY m.name COLLATE NOCASE,m.id", currentUser(r).ID)
	if e != nil {
		s.internal(w, e)
		return
	}
	all := []MediaRecord{}
	positions := map[string]float64{}
	playbacks := map[string]playbackRecord{}
	for rows.Next() {
		var m MediaRecord
		var p float64
		var playback playbackRecord
		e = rows.Scan(&m.ID, &m.Path, &m.FolderID, &m.Name, &m.Kind, &m.MIME, &m.Format, &m.Size, &m.Modified, &m.Width, &m.Height, &m.Duration, &m.Thumbnail, &p, &playback.Status, &playback.Strategy, &playback.SourceKey, &playback.Artifact)
		if e != nil {
			rows.Close()
			s.internal(w, e)
			return
		}
		all = append(all, m)
		positions[m.ID] = p
		playbacks[m.ID] = playback
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		s.internal(w, e)
		return
	}
	counts := map[string]int{}
	for _, m := range all {
		for _, f := range records {
			if strings.HasPrefix(m.Path, f.Path+"/") {
				counts[f.ID]++
			}
		}
	}
	dto := func(f FolderRecord) Folder { return Folder{f.ID, f.Name, counts[f.ID], f.ParentID} }
	folders := []Folder{}
	navigation := []Folder{}
	breadcrumbs := []Folder{}
	items := []Media{}
	continuing := []Media{}
	var folder *Folder
	for _, f := range records {
		if f.ParentID == "" {
			navigation = append(navigation, dto(f))
		}
		if f.ParentID == selectedID && query == "" {
			folders = append(folders, dto(f))
		}
	}
	if selected != nil {
		v := dto(*selected)
		folder = &v
		f := *selected
		for {
			breadcrumbs = append(breadcrumbs, dto(f))
			if f.ParentID == "" {
				break
			}
			parent, ok := byID[f.ParentID]
			if !ok {
				break
			}
			f = parent
		}
		for i, j := 0, len(breadcrumbs)-1; i < j; i, j = i+1, j-1 {
			breadcrumbs[i], breadcrumbs[j] = breadcrumbs[j], breadcrumbs[i]
		}
	}
	for _, m := range all {
		p := positions[m.ID]
		media := mediaDTO(m, p)
		if m.Kind == "video" {
			media.Playback = playbacks[m.ID].dto(m.ID)
		}
		if m.Kind == "video" && p > 0 && (m.Duration <= 0 || p < m.Duration-1) {
			continuing = append(continuing, media)
		}
		include := scope == "all" || m.FolderID == selectedID
		if query != "" {
			include = (selected == nil || strings.HasPrefix(m.Path, selected.Path+"/")) && strings.Contains(strings.ToLower(m.Name), query)
		}
		if include {
			items = append(items, media)
		}
	}
	writeJSON(w, 200, map[string]any{"folders": folders, "items": items, "continuing": continuing, "folder": folder, "navigation": navigation, "breadcrumbs": breadcrumbs})
}
func prefixedMediaCols() string {
	parts := strings.Split(mediaCols, ",")
	for i := range parts {
		parts[i] = "m." + parts[i]
	}
	return strings.Join(parts, ",")
}
func (s *Server) item(w http.ResponseWriter, r *http.Request) {
	m, e := s.getMedia(r)
	if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "This file could not be found.")
		return
	}
	if e != nil {
		s.internal(w, e)
		return
	}
	var progress float64
	var playback playbackRecord
	e = s.DB.QueryRowContext(r.Context(), "SELECT COALESCE(p.position,0),"+playbackCols+" FROM media m LEFT JOIN playback_progress p ON p.media_id=m.id AND p.user_id=? LEFT JOIN playback_derivations d ON d.media_id=m.id WHERE m.id=?", currentUser(r).ID, m.ID).Scan(&progress, &playback.Status, &playback.Strategy, &playback.SourceKey, &playback.Artifact)
	if e != nil {
		s.internal(w, e)
		return
	}
	media := mediaDTO(m, progress)
	if m.Kind == "video" {
		media.Playback = playback.dto(m.ID)
	}
	writeJSON(w, 200, media)
}
func (s *Server) progress(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Progress *float64 `json:"progress"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.Progress == nil || math.IsNaN(*b.Progress) || math.IsInf(*b.Progress, 0) {
		fail(w, 400, "Invalid playback progress.")
		return
	}
	m, e := s.getMedia(r)
	if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "This file could not be found.")
		return
	}
	if e != nil {
		s.internal(w, e)
		return
	}
	if m.Kind != "video" {
		fail(w, 400, "Progress is only available for video.")
		return
	}
	p := math.Max(0, *b.Progress)
	if m.Duration > 0 {
		p = math.Min(m.Duration, p)
	}
	_, e = s.DB.ExecContext(r.Context(), "INSERT INTO playback_progress(user_id,media_id,position,updated_at) VALUES(?,?,?,unixepoch()) ON CONFLICT(user_id,media_id) DO UPDATE SET position=excluded.position,updated_at=excluded.updated_at", currentUser(r).ID, m.ID, p)
	if e != nil {
		s.internal(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) playback(w http.ResponseWriter, r *http.Request) {
	var key, artifact, name string
	err := s.DB.QueryRowContext(r.Context(), "SELECT d.source_key,d.artifact,m.name FROM playback_derivations d JOIN media m ON m.id=d.media_id WHERE d.media_id=? AND d.status='ready' AND d.strategy<>'none'", r.PathValue("id")).Scan(&key, &artifact, &name)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "Playback is not available for this file.")
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	if artifact != key+".mp4" || filepath.Base(artifact) != artifact || (r.URL.Query().Get("v") != "" && r.URL.Query().Get("v") != key) {
		fail(w, 404, "Playback is not available for this version.")
		return
	}
	root, err := os.OpenRoot(filepath.Join(s.Config.DataDir, "derivations"))
	if err != nil {
		fail(w, 404, "Playback file is unavailable.")
		return
	}
	defer root.Close()
	f, err := root.Open(artifact)
	if err != nil {
		fail(w, 404, "Playback file is unavailable.")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		fail(w, 404, "Playback file is unavailable.")
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, name+".mp4", st.ModTime(), f)
}

func (s *Server) content(w http.ResponseWriter, r *http.Request) {
	m, e := s.getMedia(r)
	if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "This file could not be found.")
		return
	}
	if e != nil {
		s.internal(w, e)
		return
	}
	root, e := openMediaRoot(s.Config)
	if e != nil {
		fail(w, 503, "Media storage is unavailable.")
		return
	}
	defer root.Close()
	f, e := root.Open(m.Path)
	if e != nil {
		fail(w, 404, "This file is no longer available.")
		return
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		fail(w, 404, "This file is no longer available.")
		return
	}
	w.Header().Set("Content-Type", m.MIME)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, m.Name, st.ModTime(), f)
}

// The sprite is supplementary decoration whose tile geometry the client depends on,
// so a cache miss answers 404 rather than the thumbnail's placeholder image: a
// placeholder would break the tile maths and show one misleading frame everywhere.
func (s *Server) sprite(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	m, e := s.getMedia(r)
	if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "This file could not be found.")
		return
	}
	if e != nil {
		s.internal(w, e)
		return
	}
	key := spriteCacheKey(m.Path, m.Size, m.Modified)
	version := r.URL.Query().Get("v")
	if version != "" && version != key {
		fail(w, 404, "No preview is available for this version.")
		return
	}
	if m.Kind == "video" && m.Duration > 0 {
		root, e := os.OpenRoot(filepath.Join(s.Config.DataDir, "sprites"))
		if e == nil {
			defer root.Close()
			f, e := root.Open(key)
			if e == nil {
				defer f.Close()
				st, e := f.Stat()
				if e == nil && st.Mode().IsRegular() && st.Size() > 0 {
					w.Header().Set("Content-Type", "image/jpeg")
					w.Header().Set("ETag", `"`+key+`"`)
					// Private content gated by the video's grant, so never cacheable.
					http.ServeContent(w, r, m.Name+".jpg", st.ModTime(), f)
					return
				}
			}
		}
	}
	fail(w, 404, "No preview is available for this file.")
}
func (s *Server) thumbnail(w http.ResponseWriter, r *http.Request) {
	m, e := s.getMedia(r)
	if errors.Is(e, sql.ErrNoRows) {
		fail(w, 404, "This file could not be found.")
		return
	}
	if e != nil {
		s.internal(w, e)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	if m.Thumbnail != "" {
		root, e := os.OpenRoot(filepath.Join(s.Config.DataDir, "thumbs"))
		if e == nil {
			defer root.Close()
			f, e := root.Open(m.Thumbnail)
			if e == nil {
				defer f.Close()
				st, e := f.Stat()
				if e == nil && st.Mode().IsRegular() {
					w.Header().Set("Content-Type", "image/jpeg")
					http.ServeContent(w, r, m.Name+".jpg", st.ModTime(), f)
					return
				}
			}
		}
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	if r.Method != "HEAD" {
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="480" height="270" viewBox="0 0 480 270"><rect width="480" height="270" fill="#172a40"/><path d="M210 95v80l70-40z" fill="#85a5c8"/></svg>`))
	}
}
