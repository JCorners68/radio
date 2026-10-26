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
		st.Playlists = []store.Playlist{{
			ID:       "pl-main",
			Name:     "Synthwave",
			Image:    "https://i.scdn.co/image/ab67706f00000002e8ccdd567fff6a597b87586b",
			TrackIDs: []string{},
		}}
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

	// 1. Server-rendered index.html contains current track without inline JSON dump,
	// and /ui/drawer renders the playlist rail, tracks, and prominent Import Spotify CTA
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:55111"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="drawer-inner"`) {
		t.Fatalf("expected server-rendered shell in GET / response")
	}
	if strings.Contains(body, "window.__STATION__=") {
		t.Fatalf("expected index.html to be pure template-rendered without window.__STATION__ blob")
	}

	// Verify Brotli compression on GET /ui/drawer
	req = httptest.NewRequest(http.MethodGet, "/ui/drawer", nil)
	req.RemoteAddr = "127.0.0.1:55111"
	req.Header.Set("Accept-Encoding", "br")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Encoding") != "br" {
		t.Fatalf("expected Content-Encoding: br on /ui/drawer, got status=%d enc=%q", rec.Code, rec.Header().Get("Content-Encoding"))
	}

	// Verify uncompressed /ui/drawer contains playlist and Import Spotify button
	req = httptest.NewRequest(http.MethodGet, "/ui/drawer", nil)
	req.RemoteAddr = "127.0.0.1:55111"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	drawerBody := rec.Body.String()
	if !strings.Contains(drawerBody, "Synthwave") || !strings.Contains(drawerBody, "Nightcall") || !strings.Contains(drawerBody, "Import Spotify") {
		t.Fatalf("expected Synthwave, Nightcall, and Import Spotify in /ui/drawer response")
	}
	if !strings.Contains(drawerBody, `class="plist-thumb" src="https://i.scdn.co/image/ab67706f00000002e8ccdd567fff6a597b87586b"`) ||
		!strings.Contains(drawerBody, `id="panel-playlist-thumb" src="https://i.scdn.co/image/ab67706f00000002e8ccdd567fff6a597b87586b"`) {
		t.Fatalf("expected playlist cover image in rail and panel header")
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
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Nightcall") {
		t.Fatalf("subsequent GET / after fragment execution failed: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 6. Auto-advance with ?alreadyPlaying=<trackId> commits currentTrackId and emits trackActivated (not loadTrack)
	var secondTrackID string
	_, _, err = ownerStore.Mutate(func(st *store.StationState) error {
		_, tr := store.AddTracksToPlaylist(st, "pl-main", []store.NewTrackInput{
			{VideoID: "jNQXAC9IVRw", Title: "Pacific Coast Highway", Thumb: "https://i.ytimg.com/vi/jNQXAC9IVRw/hqdefault.jpg"},
		})
		secondTrackID = tr.ID
		return nil
	})
	if err != nil {
		t.Fatalf("add second track: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/ui/player/next?alreadyPlaying="+secondTrackID, nil)
	req.RemoteAddr = "127.0.0.1:55111"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /ui/player/next?alreadyPlaying status=%d", rec.Code)
	}
	hxTrig := rec.Header().Get("HX-Trigger")
	if !strings.Contains(hxTrig, `"trackActivated"`) || strings.Contains(hxTrig, `"loadTrack"`) {
		t.Fatalf("expected trackActivated without loadTrack on alreadyPlaying handoff, got %s", hxTrig)
	}
	if !strings.Contains(hxTrig, `"nextTrackId":"`+trackID+`"`) {
		t.Fatalf("expected wrapped nextTrackId=%s in HX-Trigger, got %s", trackID, hxTrig)
	}

	// 7. 204 No Content responses must not attach Content-Encoding: br or body bytes
	req = httptest.NewRequest(http.MethodPost, "/ui/flags/showVideo?value=0", nil)
	req.RemoteAddr = "127.0.0.1:55111"
	req.Header.Set("Accept-Encoding", "br")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /ui/flags/showVideo status=%d, want 204", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "" || rec.Body.Len() != 0 {
		t.Fatalf("expected 204 response to have empty Content-Encoding and 0 bytes, got enc=%q len=%d", rec.Header().Get("Content-Encoding"), rec.Body.Len())
	}

	// 8. Pasting a Spotify URL into the quick-add input redirects to /find.html with ?paste=
	req = httptest.NewRequest(http.MethodPost, "/ui/tracks", strings.NewReader("input=https%3A%2F%2Fopen.spotify.com%2Fplaylist%2F37i9dQZF1DXcBWIGoYBM5M"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "127.0.0.1:55111"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || !strings.HasPrefix(rec.Header().Get("HX-Redirect"), "/find.html?") {
		t.Fatalf("expected HX-Redirect to /find.html on Spotify paste in quick-add, got status=%d redirect=%q", rec.Code, rec.Header().Get("HX-Redirect"))
	}
}
