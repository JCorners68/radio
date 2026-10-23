// Command radio-ai-server is the backend for the radio-station app.
//
// It wires the isolated domain services:
//   - internal/config: environment, root paths, and yt-dlp discovery
//   - internal/auth: hybrid Cloudflare Access + Google OAuth2/OIDC gate with per-email session revocation
//   - internal/store: atomic file-backed station state + per-user MultiStore isolation
//   - internal/catalog/youtube: yt-dlp search, seed resolution, deterministic scoring, and m4a stream proxy
//   - internal/catalog/spotify: Spotify public embed + pathfinder v2 playlist parser
//   - internal/curator/dj: Vertex AI Gemini recommendation engine
//   - internal/curator/matcher: two-tier direct + Qwen tool-calling track matcher
//   - internal/server: HTTP transport for HTMX templates, static pages, client scripts, and /api/* endpoints
package main

import (
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"radio-ai-server/internal/auth"
	"radio-ai-server/internal/catalog/youtube"
	"radio-ai-server/internal/config"
	"radio-ai-server/internal/curator/dj"
	"radio-ai-server/internal/curator/matcher"
	"radio-ai-server/internal/server"
	"radio-ai-server/internal/store"
)

func envOrFile(envPath string, keys ...string) string {
	for _, k := range keys {
		if v := config.EnvOr(k, ""); v != "" {
			return v
		}
		if v, ok := config.LoadEnvValue(envPath, k); ok && v != "" {
			return v
		}
	}
	return ""
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if clean := strings.ToLower(strings.TrimSpace(p)); clean != "" {
			out = append(out, clean)
		}
	}
	return out
}

func main() {
	root := config.AppRoot()
	config.LoadDotEnvFile(filepath.Join(root, ".env"))
	envPath := config.EnvOr("RADIO_ENV_FILE", filepath.Join(root, ".env"))
	listenAddr := config.EnvOr("RADIO_LISTEN", config.DefaultListen)

	ytDlpPath := config.ResolveYtDlp(root)
	if ytDlpPath == "" {
		log.Fatalf("yt-dlp not found: set YTDLP, put it at %s, or install it on $PATH", filepath.Join(root, "bin", "yt-dlp"))
	}
	for _, asset := range []string{"index.html", "ai-builder.html", "find.html", "htmx.min.js", "station-store.js", "player-adapter.js", "manifest.json", "icon.svg"} {
		if !config.FileExists(filepath.Join(root, asset)) {
			log.Fatalf("%s not found under app root %s (set RADIO_ROOT)", asset, root)
		}
	}
	log.Printf("app root=%s yt-dlp=%s env=%s", root, ytDlpPath, envPath)

	saJSON, ok := config.LoadEnvValue(envPath, "GOOGLE_SERVICE_ACCOUNT_JSON")
	if !ok || saJSON == "" {
		log.Fatalf("GOOGLE_SERVICE_ACCOUNT_JSON not found in %s", envPath)
	}
	location, _ := config.LoadEnvValue(envPath, "VERTEX_LOCATION")
	if location == "" {
		location = "global"
	}

	vc, err := dj.NewVertexClient([]byte(saJSON), location)
	if err != nil {
		log.Fatalf("vertex client init failed: %v", err)
	}
	log.Printf("vertex client OK: project=%s location=%s model=%s", vc.ProjectID(), vc.Location(), dj.DefaultGeminiModel)

	dataDir := config.EnvOr("RADIO_DATA", filepath.Join(root, "data"))
	st, err := store.OpenFileStore(dataDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	rev, _ := st.Get()
	log.Printf("station store: %s rev=%d", st.Path(), rev)
	multiStore := store.NewMultiStore(st)

	ytClient := youtube.NewClient(ytDlpPath)
	streamCache := youtube.NewStreamCache(ytClient)
	r2Store := youtube.NewR2Store(youtube.R2Config{
		AccountID: envOrFile(envPath, "CF_ACCOUNT_ID", "R2_ACCOUNT_ID"),
		APIToken:  envOrFile(envPath, "CF_R2_CATALOG_TOKEN", "CF_API_TOKEN"),
		Bucket:    config.EnvOr("RADIO_R2_BUCKET", "radio-station-audio"),
	})
	audioCacheDir := config.EnvOr("RADIO_AUDIO_CACHE", filepath.Join(dataDir, "audio-cache"))
	streamCache.ConfigurePersistence(audioCacheDir, r2Store)
	log.Printf("audio stream cache: dir=%s r2_enabled=%v", audioCacheDir, r2Store != nil)
	go warmCorePlaylists(st, streamCache)

	djSvc := dj.NewService(vc, ytClient, dj.DefaultGeminiModel)
	matcherSvc := matcher.NewService(
		ytClient,
		config.EnvOr("QWEN_URL", "http://127.0.0.1:8000/v1"),
		config.EnvOr("QWEN_MODEL", "qwen3.8-27b"),
	)

	ownerEmails := splitCSV(config.EnvOr("RADIO_OWNER_EMAILS", "jonathan.corners@gmail.com,jonathan.corners@voxell.ai"))
	allowedEmails := splitCSV(config.EnvOr("RADIO_ALLOWED_EMAILS", "jonathan.corners@gmail.com,jonathan.corners@voxell.ai,jelyn.corners@gmail.com,riki.corners@gmail.com,seana.corners@gmail.com"))
	googleClientID := envOrFile(envPath, "GOOGLE_OAUTH_CLIENT_ID", "GF_AUTH_GOOGLE_CLIENT_ID")
	googleClientSecret := envOrFile(envPath, "GOOGLE_OAUTH_CLIENT_SECRET", "GF_AUTH_GOOGLE_CLIENT_SECRET")
	sessionSeed := envOrFile(envPath, "RADIO_SESSION_SECRET", "JWT_SECRET")

	authGate := auth.NewOAuthGate(auth.Config{
		AllowedEmails:      allowedEmails,
		OwnerEmails:        ownerEmails,
		GoogleClientID:     googleClientID,
		GoogleClientSecret: googleClientSecret,
		CFAccessTeamDomain: envOrFile(envPath, "CF_ACCESS_TEAM_DOMAIN"),
		CFAccessAUD:        envOrFile(envPath, "CF_ACCESS_AUD"),
		Passcode:           config.EnvOr("RADIO_PASSCODE", ""),
		SecretSeed:         sessionSeed,
		DataDir:            dataDir,
	})
	if authGate.Enabled() {
		log.Printf("auth gate enabled: google_oauth=%v allowed_users=%d owners=%d",
			authGate.GoogleEnabled(), len(allowedEmails), len(ownerEmails))
	}

	srv := &server.Server{
		RootDir:    root,
		Store:      st,
		MultiStore: multiStore,
		DJ:         djSvc,
		Matcher:    matcherSvc,
		Streams:    streamCache,
		Auth:       authGate,
	}

	log.Printf("radio-ai-server listening on http://%s", listenAddr)
	if err := http.ListenAndServe(listenAddr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

func warmCorePlaylists(st *store.FileStore, sc *youtube.StreamCache) {
	if st == nil || sc == nil {
		return
	}
	_, state, err := st.ReadState()
	if err != nil {
		return
	}
	seen := make(map[string]bool)
	var ids []string
	addVid := func(tid string) {
		if t, ok := state.Tracks[tid]; ok && youtube.ValidVideoID(t.VideoID) && !seen[t.VideoID] {
			seen[t.VideoID] = true
			ids = append(ids, t.VideoID)
		}
	}
	if state.CurrentTrackID != "" {
		addVid(state.CurrentTrackID)
	}
	if pl := state.ActivePlaylist(); pl != nil {
		for _, tid := range pl.TrackIDs {
			addVid(tid)
			if len(ids) >= 30 {
				break
			}
		}
	}
	for _, pl := range state.Playlists {
		if len(pl.TrackIDs) <= 30 {
			for _, tid := range pl.TrackIDs {
				addVid(tid)
			}
		}
	}
	sc.EnqueueWarm(ids...)
}
