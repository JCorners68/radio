// Package server wires HTTP transport routes to the station store, stream proxy, auth gate, and curator services.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"radio-ai-server/internal/auth"
	"radio-ai-server/internal/catalog/spotify"
	"radio-ai-server/internal/catalog/youtube"
	"radio-ai-server/internal/curator/dj"
	"radio-ai-server/internal/curator/matcher"
	"radio-ai-server/internal/store"
)

type Server struct {
	RootDir    string
	Store      *store.FileStore
	MultiStore *store.MultiStore
	DJ         *dj.Service
	Matcher    *matcher.Service
	Streams    *youtube.StreamCache
	Auth       *auth.Gate
}

type suggestRequest struct {
	Seed  string `json:"seed"`
	Count int    `json:"count"`
}

type suggestResponse struct {
	Suggestions []youtube.ValidatedTrack `json:"suggestions"`
	Error       string                   `json:"error,omitempty"`
}

func (s *Server) fileStoreForRequest(r *http.Request) *store.FileStore {
	if s.MultiStore != nil {
		u := auth.UserFromContext(r.Context())
		if fs, err := s.MultiStore.ForUser(u.Email, u.IsOwner); err == nil && fs != nil {
			return fs
		}
		return s.MultiStore.Owner()
	}
	return s.Store
}

// Handler builds the HTTP ServeMux for all static pages, HTMX UI fragments, client scripts, and API routes,
// wrapped with the hybrid OAuth/Access gate for remote requests.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.servePage("index.html"))
	for _, page := range []string{"index.html", "ai-builder.html", "find.html"} {
		mux.HandleFunc("GET /"+page, s.servePage(page))
	}
	for _, asset := range []string{"htmx.min.js", "station-store.js", "player-adapter.js", "manifest.json", "icon.svg"} {
		file := asset
		mux.HandleFunc("GET /"+file, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=300, stale-while-revalidate=86400")
			http.ServeFile(w, r, filepath.Join(s.RootDir, file))
		})
	}

	// HTMX server-rendered UI endpoints
	mux.HandleFunc("GET /ui/drawer", s.handleUIDrawer)
	mux.HandleFunc("GET /ui/playlists/{id}", s.handleSelectPlaylist)
	mux.HandleFunc("POST /ui/playlists", s.handleCreatePlaylist)
	mux.HandleFunc("POST /ui/playlists/{id}/rename", s.handleRenamePlaylist)
	mux.HandleFunc("DELETE /ui/playlists/{id}", s.handleDeletePlaylist)
	mux.HandleFunc("POST /ui/flags/{flag}", s.handleToggleFlag)
	mux.HandleFunc("POST /ui/tracks", s.handleAddTrack)
	mux.HandleFunc("POST /ui/tracks/{id}/play", s.handlePlayTrack)
	mux.HandleFunc("POST /ui/tracks/{id}/fav", s.handleToggleFav)
	mux.HandleFunc("DELETE /ui/playlists/{pid}/tracks/{tid}", s.handleRemoveTrack)
	mux.HandleFunc("POST /ui/playlists/{pid}/tracks/{tid}/move", s.handleMoveTrack)
	mux.HandleFunc("POST /ui/player/next", s.handlePlayerStep(false))
	mux.HandleFunc("POST /ui/player/prev", s.handlePlayerStep(true))
	mux.HandleFunc("POST /ui/import", s.handleImportStation)

	// Export & JSON API endpoints
	mux.HandleFunc("GET /api/export/station.json", s.handleExportStation)
	mux.HandleFunc("GET /api/export/playlist.m3u", s.handleExportM3U)
	mux.HandleFunc("GET /api/state", s.handleGetState)
	mux.HandleFunc("PUT /api/state", s.handlePutState)
	mux.HandleFunc("POST /api/find", s.handleFind)
	mux.HandleFunc("/api/suggest", s.handleSuggest)
	mux.HandleFunc("GET /api/stream/{videoId}", s.handleStream)
	mux.HandleFunc("HEAD /api/stream/{videoId}", s.handleStream)
	mux.HandleFunc("/healthz", s.handleHealthz)

	var h http.Handler = mux
	if s.Auth != nil {
		h = s.Auth.Wrap(h)
	}
	return compressMiddleware(h)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if s.Streams == nil {
		http.Error(w, "streaming not configured", http.StatusServiceUnavailable)
		return
	}
	videoID := r.PathValue("videoId")
	s.Streams.ProxyStream(w, r, videoID)
}

// servePage serves one named page from the app root. index.html is server-rendered with Go
// html/template; auxiliary pages (ai-builder.html, find.html) get the user's station state injected.
func (s *Server) servePage(name string) http.HandlerFunc {
	path := filepath.Join(s.RootDir, name)
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "127.0.0.1") {
			target := "http://" + strings.Replace(r.Host, "127.0.0.1", "localhost", 1) + r.URL.RequestURI()
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			http.Error(w, "page missing", http.StatusInternalServerError)
			return
		}
		fs := s.fileStoreForRequest(r)
		if name == "index.html" {
			go s.warmInitialStream(fs)
			s.renderIndexPage(w, r, b)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "origin-when-cross-origin")
		w.Write(fs.InjectState(b))
	}
}

func (s *Server) warmInitialStream(fs *store.FileStore) {
	if s.Streams == nil || fs == nil {
		return
	}
	_, st, err := fs.ReadState()
	if err != nil {
		return
	}
	trackID := st.CurrentTrackID
	if trackID == "" {
		if pl := st.ActivePlaylist(); pl != nil && len(pl.TrackIDs) > 0 {
			trackID = pl.TrackIDs[0]
		}
	}
	if t, ok := st.Tracks[trackID]; ok && youtube.ValidVideoID(t.VideoID) {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		_, _ = s.Streams.Resolve(ctx, t.VideoID)
		cancel()
	}
	if pl := st.ActivePlaylist(); pl != nil {
		var ids []string
		for _, tid := range pl.TrackIDs {
			if t, ok := st.Tracks[tid]; ok && youtube.ValidVideoID(t.VideoID) {
				ids = append(ids, t.VideoID)
				if len(ids) >= 20 {
					break
				}
			}
		}
		s.Streams.EnqueueWarm(ids...)
	}
}

func (s *Server) handleGetState(w http.ResponseWriter, r *http.Request) {
	fs := s.fileStoreForRequest(r)
	rev, state := fs.Get()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"rev": rev, "state": state})
}

func (s *Server) handlePutState(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rev   int64           `json:"rev"`
		State json.RawMessage `json:"state"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, store.BodyLimit)).Decode(&req); err != nil {
		http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	fs := s.fileStoreForRequest(r)
	w.Header().Set("Content-Type", "application/json")
	rev, err := fs.Put(req.Rev, req.State)
	switch {
	case errors.Is(err, store.ErrStaleRev):
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"rev": rev, "error": "stale revision"})
	case err != nil:
		log.Printf("store put: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
	default:
		json.NewEncoder(w).Encode(map[string]any{"rev": rev})
	}
}

// handleFind streams NDJSON: one "source" line with the parsed tracks, then one "result"
// line per track as it resolves (out of order), then "done".
func (s *Server) handleFind(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	name, tracks, err := spotify.ParsePaste(ctx, req.Text)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if len(tracks) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "nothing to find — paste a Spotify playlist/album/track link, or Artist - Title lines"})
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	emit := func(v any) {
		enc.Encode(v)
		if flusher != nil {
			flusher.Flush()
		}
	}
	s.Matcher.StreamFind(ctx, name, tracks, emit)
}

func (s *Server) handleSuggest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.Auth != nil {
		u := auth.UserFromContext(r.Context())
		if !s.Auth.AllowAISuggest(u) {
			writeJSONErr(w, http.StatusTooManyRequests, "daily AI Suggest quota reached for this account")
			return
		}
	}
	var req suggestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	req.Seed = strings.TrimSpace(req.Seed)
	if req.Seed == "" {
		writeJSONErr(w, http.StatusBadRequest, "seed is required")
		return
	}
	if req.Count <= 0 {
		req.Count = 12
	}
	if req.Count > 30 {
		req.Count = 30
	}

	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()

	validated, err := s.DJ.GatherSuggestions(ctx, req.Seed, req.Count)
	if err != nil {
		log.Printf("gemini error: %v", err)
		writeJSONErr(w, http.StatusBadGateway, "gemini call failed: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(suggestResponse{Suggestions: validated})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"project":%q,"location":%q,"model":%q}`,
		s.DJ.Vertex.ProjectID(), s.DJ.Vertex.Location(), s.DJ.Model)
}

func writeJSONErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(suggestResponse{Suggestions: []youtube.ValidatedTrack{}, Error: msg})
}
