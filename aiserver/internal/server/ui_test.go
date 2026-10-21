package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"radio-ai-server/internal/auth"
	"radio-ai-server/internal/store"
)

func testRootDir(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

func TestHTMXUIEndpointsAndUserIsolation(t *testing.T) {
	dir := t.TempDir()
	ownerStore, err := store.OpenFileStore(dir)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	var trackID string
	_, _, err = ownerStore.Mutate(func(st *store.StationState) error {
		st.Playlists = []store.Playlist{{ID: "pl-main", Name: "Synthwave", TrackIDs: []string{}}}
		st.ActivePlaylistID = "pl-main"
		_, tr := store.AddTracksToPlaylist(st, "pl-main", []store.NewTrackInput{
			{VideoID: "dQw4w9WgXcQ", Title: "Nightcall", Thumb: "https://i.ytimg.com/vi/dQw4w9WgXcQ/hqdefault.jpg"},
		})
		trackID = tr.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed ownerStore: %v", err)
	}

	ms := store.NewMultiStore(ownerStore)
	srv := &Server{
		RootDir:    testRootDir(t),
		Store:      ownerStore,
		MultiStore: ms,
	}
	h := srv.Handler()

	// 1. Server-rendered index.html contains playlist and track without inline JSON dump
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:55111"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Synthwave") || !strings.Contains(body, "Nightcall") {
		t.Fatalf("expected server-rendered playlist and track in GET / response")
	}
	if strings.Contains(body, "window.__STATION__=") {
		t.Fatalf("expected index.html to be pure template-rendered without window.__STATION__ blob")
	}

	// 2. Play track via HTMX returns OOB swaps and HX-Trigger
	req = httptest.NewRequest(http.MethodPost, "/ui/tracks/"+trackID+"/play", nil)
	req.RemoteAddr = "127.0.0.1:55111"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /ui/tracks/%s/play status=%d", trackID, rec.Code)
	}
	if !strings.Contains(rec.Header().Get("HX-Trigger"), "trackActivated") {
		t.Fatalf("expected HX-Trigger trackActivated, got %q", rec.Header().Get("HX-Trigger"))
	}
	if !strings.Contains(rec.Body.String(), "▶ 1 plays") {
		t.Fatalf("expected incremented play count in OOB response, got %s", rec.Body.String())
	}

	// 3. Toggle favorite via HTMX returns updated track row
	req = httptest.NewRequest(http.MethodPost, "/ui/tracks/"+trackID+"/fav", nil)
	req.RemoteAddr = "127.0.0.1:55111"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fav active") {
		t.Fatalf("expected active favorite button in response: %d %s", rec.Code, rec.Body.String())
	}

	// 4. Non-owner user context operates on isolated pre-seeded store
	req = httptest.NewRequest(http.MethodPost, "/ui/playlists", nil)
	req.Header.Set("HX-Prompt", "Riki Only Playlist")
	req = req.WithContext(auth.WithUser(req.Context(), auth.User{
		Email:   "riki.corners@gmail.com",
		IsOwner: false,
	}))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Riki Only Playlist") {
		t.Fatalf("expected Riki playlist in response: %d", rec.Code)
	}

	// Verify owner store still has only 1 playlist ("Synthwave")
	_, ownerSt, _ := ownerStore.ReadState()
	if len(ownerSt.Playlists) != 1 || ownerSt.Playlists[0].Name != "Synthwave" {
		t.Fatalf("owner playlists leaked from non-owner mutation: %+v", ownerSt.Playlists)
	}

	// 5. Subsequent GET / after fragment template executions must succeed (no html/template Clone error)
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:55111"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Synthwave") {
		t.Fatalf("subsequent GET / after fragment execution failed: status=%d body=%s", rec.Code, rec.Body.String())
	}
}
