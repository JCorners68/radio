// Command radio-ai-server is the backend for the radio-station app.
//
// It wires the isolated domain services:
//   - internal/config: environment, root paths, and yt-dlp discovery
//   - internal/auth: remote passcode gate with 90-day session cookie + localhost bypass
//   - internal/store: atomic file-backed station state with optimistic concurrency
//   - internal/catalog/youtube: yt-dlp search, seed resolution, deterministic scoring, and m4a stream proxy
//   - internal/catalog/spotify: Spotify public embed + pathfinder v2 playlist parser
//   - internal/curator/dj: Vertex AI Gemini recommendation engine
//   - internal/curator/matcher: two-tier direct + Qwen tool-calling track matcher
//   - internal/server: HTTP transport for pages, client scripts, and /api/* endpoints
package main

import (
	"log"
	"net/http"
	"path/filepath"

	"radio-ai-server/internal/auth"
	"radio-ai-server/internal/catalog/youtube"
	"radio-ai-server/internal/config"
	"radio-ai-server/internal/curator/dj"
	"radio-ai-server/internal/curator/matcher"
	"radio-ai-server/internal/server"
	"radio-ai-server/internal/store"
)

func main() {
	root := config.AppRoot()
	config.LoadDotEnvFile(filepath.Join(root, ".env"))
	envPath := config.EnvOr("RADIO_ENV_FILE", filepath.Join(root, ".env"))
	listenAddr := config.EnvOr("RADIO_LISTEN", config.DefaultListen)

	ytDlpPath := config.ResolveYtDlp(root)
	if ytDlpPath == "" {
		log.Fatalf("yt-dlp not found: set YTDLP, put it at %s, or install it on $PATH", filepath.Join(root, "bin", "yt-dlp"))
	}
	for _, asset := range []string{"index.html", "ai-builder.html", "find.html", "station-store.js", "player-adapter.js", "manifest.json", "icon.svg"} {
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

	st, err := store.OpenFileStore(config.EnvOr("RADIO_DATA", filepath.Join(root, "data")))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	rev, _ := st.Get()
	log.Printf("station store: %s rev=%d", st.Path(), rev)

	ytClient := youtube.NewClient(ytDlpPath)
	streamCache := youtube.NewStreamCache(ytClient)
	djSvc := dj.NewService(vc, ytClient, dj.DefaultGeminiModel)
	matcherSvc := matcher.NewService(
		ytClient,
		config.EnvOr("QWEN_URL", "http://127.0.0.1:8000/v1"),
		config.EnvOr("QWEN_MODEL", "qwen3.8-27b"),
	)

	passcode := config.EnvOr("RADIO_PASSCODE", "")
	authGate := auth.NewGate(passcode)
	if authGate.Enabled() {
		log.Printf("auth gate enabled for non-localhost requests")
	}

	srv := &server.Server{
		RootDir: root,
		Store:   st,
		DJ:      djSvc,
		Matcher: matcherSvc,
		Streams: streamCache,
		Auth:    authGate,
	}

	log.Printf("radio-ai-server listening on http://%s", listenAddr)
	if err := http.ListenAndServe(listenAddr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}
