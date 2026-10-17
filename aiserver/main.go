// Command radio-ai-server is a tiny personal-use backend for the radio-station AI
// playlist builder. It calls Gemini on Vertex AI (gemini-3.6-flash) for music taste —
// picking real songs (title/artist) that fit a seed keyword/mood/artist/YouTube link —
// then resolves each pick to an actual playable YouTube video via yt-dlp's real search,
// rather than trusting Gemini to invent a video ID/URL itself.
//
// Why: Gemini's own "youtube_url" guesses looked plausible but were frequently
// hallucinated (even with Google Search grounding enabled) — the song/artist picks
// were consistently good, but the specific video ID was often fabricated and failed
// validation, making results feel sparse/random. Splitting "what song" (Gemini, which
// it's good at) from "which exact video" (yt-dlp, which actually searches YouTube)
// fixes that at the root instead of just filtering harder after the fact.
//
// Auth: reads GOOGLE_SERVICE_ACCOUNT_JSON / VERTEX_LOCATION from a dotenv file
// (read-only, never logged, never copied elsewhere) using line-scan parsing so files
// with characters that break `source` still work. Defaults to <root>/.env, or override
// the path with RADIO_ENV_FILE.
//
// Serving: one process serves the static pages (index.html, ai-builder.html, find.html)
// and the /api/* endpoints from the same origin, so there is no separate static server
// and no CORS. The app root is the parent of this binary's directory (override: RADIO_ROOT).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	geminiModel   = "gemini-3.6-flash"
	defaultListen = "127.0.0.1:8420"
)

// ytDlpPath is resolved at startup: YTDLP env, else <root>/bin/yt-dlp, else $PATH.
var ytDlpPath string

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// appRoot is the radio-station directory: RADIO_ROOT, else the parent of the
// directory holding this binary (it is built into <root>/aiserver/).
func appRoot() string {
	if v := os.Getenv("RADIO_ROOT"); v != "" {
		return v
	}
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("locate executable: %v", err)
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return filepath.Dir(filepath.Dir(exe))
}

func resolveYtDlp(root string) string {
	if v := os.Getenv("YTDLP"); v != "" {
		return v
	}
	if p := filepath.Join(root, "bin", "yt-dlp"); fileExists(p) {
		return p
	}
	if p, err := exec.LookPath("yt-dlp"); err == nil {
		return p
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ---------------- dotenv parsing (never logs values) ----------------

// parseDotEnvLine parses a single KEY=VALUE line from a dotenv file.
func parseDotEnvLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	eq := strings.IndexByte(line, '=')
	if eq < 0 {
		return "", "", false
	}
	k := strings.TrimSpace(line[:eq])
	v := strings.TrimSpace(line[eq+1:])
	if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	return k, v, k != ""
}

// loadDotEnvFile loads any unset environment variables from a dotenv file.
func loadDotEnvFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := parseDotEnvLine(line); ok && os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

// loadEnvValue reads a single KEY from a dotenv file without sourcing it.
func loadEnvValue(path, key string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := parseDotEnvLine(line); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// ---------------- Gemini request/response shapes ----------------

type suggestion struct {
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Reason string `json:"reason"`
}

type validatedTrack struct {
	VideoID string `json:"videoId"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	Reason  string `json:"reason"`
	Thumb   string `json:"thumb"`
}

const systemInstruction = `You are an expert music-recommendation DJ for a personal internet radio app.
Given a seed — which may be a genre, mood, artist name, or a YouTube URL/video the listener already likes —
recommend real songs that fit. Every song must be a real, existing recording by a real artist — never invent
a song or artist. You do NOT need to provide a video URL or ID; a separate system resolves each pick to the
actual YouTube video, so focus entirely on making great, specific, real picks.

Output STRICT JSON ONLY: a JSON array (not wrapped in an object) of objects with exactly this shape:
[{"title": "<song title>", "artist": "<artist name>", "reason": "<why it fits, 8 words max>"}, ...]

Rules:
- Return exactly the requested count of suggestions when possible.
- Every title/artist pair must be a real, specific, existing song — no invented songs, no vague placeholders.
- Prefer well-known official releases over obscure/unreleased material so the video is easy to find.
- Do not repeat the seed track itself as a suggestion.
- No prose, no markdown, no code fences — JSON array only.`

func buildPrompt(seed string, count int) string {
	return fmt.Sprintf("Seed: %s\nDesired number of suggestions: %d\n\nSuggest %d songs that fit this seed.", seed, count, count)
}

func callGemini(ctx context.Context, vc *VertexClient, seed string, count int) ([]suggestion, error) {
	payload := map[string]any{
		"systemInstruction": map[string]any{
			"parts": []map[string]string{{"text": systemInstruction}},
		},
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]string{{"text": buildPrompt(seed, count)}}},
		},
		// No "tools"/grounding here on purpose — we only need Gemini's taste (real
		// title/artist picks), not a video URL, so responseMimeType JSON mode (which
		// conflicts with grounding tools on Vertex) is safe to use again for reliable
		// strict-JSON output.
		"generationConfig": map[string]any{
			"temperature":      0.9,
			"maxOutputTokens":  4096,
			"thinkingConfig":   map[string]any{"thinkingBudget": 0},
			"responseMimeType": "application/json",
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	tok, err := vc.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("vertex token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", vc.GenerateContentURL(geminiModel), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 100 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gemini HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode gemini envelope: %w (raw=%s)", err, truncate(string(raw), 500))
	}
	if len(parsed.Candidates) == 0 || len(parsed.Candidates[0].Content.Parts) == 0 {
		return nil, fmt.Errorf("empty gemini candidate (raw=%s)", truncate(string(raw), 3000))
	}

	// concatenate all parts (grounding can split text across multiple parts)
	var sb strings.Builder
	for _, p := range parsed.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	text := extractJSONArray(sb.String())

	var suggestions []suggestion
	if err := json.Unmarshal([]byte(text), &suggestions); err != nil {
		// The model's output may have been cut off mid-object by maxOutputTokens.
		// Salvage whatever complete {...} entries exist before the truncation point
		// rather than throwing the whole batch away.
		if repaired := repairTruncatedArray(text); repaired != "" {
			if err2 := json.Unmarshal([]byte(repaired), &suggestions); err2 == nil && len(suggestions) > 0 {
				return suggestions, nil
			}
		}
		return nil, fmt.Errorf("decode suggestions JSON: %w (text=%s)", err, truncate(text, 800))
	}
	return suggestions, nil
}

// repairTruncatedArray takes a JSON array string that may have been cut off mid-object
// (e.g. by hitting maxOutputTokens) and returns a valid, closed array containing only
// the complete top-level objects that appear before the truncation point. Returns ""
// if no complete object could be salvaged.
func repairTruncatedArray(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	lastCompleteEnd := -1
	for i, r := range s {
		if inString {
			if escaped {
				escaped = false
			} else if r == '\\' {
				escaped = true
			} else if r == '"' {
				inString = false
			}
			continue
		}
		switch r {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				lastCompleteEnd = i
			}
		}
	}
	if lastCompleteEnd < 0 {
		return ""
	}
	return s[:lastCompleteEnd+1] + "]"
}

// extractJSONArray pulls a JSON array out of model output that may be wrapped in
// markdown code fences or preceded/followed by stray prose.
func extractJSONArray(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '[')
	end := strings.LastIndexByte(s, ']')
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// resolveSuggestions resolves each Gemini pick to a real YouTube video concurrently,
// dropping anything that yt-dlp's real search can't find a hit for.
func resolveSuggestions(ctx context.Context, raw []suggestion) []validatedTrack {
	type result struct {
		idx int
		vt  *validatedTrack
	}
	results := make(chan result, len(raw))
	for i, s := range raw {
		go func(i int, s suggestion) {
			vt, ok := resolveViaYtDlp(ctx, s.Title, s.Artist)
			if !ok {
				results <- result{i, nil}
				return
			}
			vt.Reason = s.Reason
			results <- result{i, vt}
		}(i, s)
	}

	ordered := make([]*validatedTrack, len(raw))
	for range raw {
		r := <-results
		ordered[r.idx] = r.vt
	}
	out := make([]validatedTrack, 0, len(raw))
	seen := map[string]bool{}
	for _, vt := range ordered {
		if vt == nil || seen[vt.VideoID] {
			continue
		}
		seen[vt.VideoID] = true
		out = append(out, *vt)
	}
	return out
}

// gatherSuggestions calls Gemini in a top-up loop until it has req.Count tracks that
// matched a real YouTube video (or gives up after a few attempts). Each retry asks for
// roughly double the remaining shortfall to absorb picks that found no matching video,
// and tells the model what's already been picked so it doesn't repeat itself.
func gatherSuggestions(ctx context.Context, vc *VertexClient, seed string, count int) ([]validatedTrack, error) {
	seed = resolveSeed(ctx, seed)
	validated := make([]validatedTrack, 0, count)
	seen := map[string]bool{}
	var titlesSoFar []string
	var lastErr error

	const maxAttempts = 4
	for attempt := 1; attempt <= maxAttempts && len(validated) < count; attempt++ {
		remaining := count - len(validated)
		ask := remaining * 2
		if ask < 6 {
			ask = 6
		}
		if ask > 30 {
			ask = 30
		}

		callSeed := seed
		if len(titlesSoFar) > 0 {
			callSeed = fmt.Sprintf("%s (give %d MORE songs, do not repeat any of these already-picked titles: %s)",
				seed, ask, strings.Join(titlesSoFar, "; "))
		}

		raw, err := callGemini(ctx, vc, callSeed, ask)
		if err != nil {
			log.Printf("gatherSuggestions: attempt %d failed: %v", attempt, err)
			lastErr = err
			continue // a single bad/truncated response shouldn't sink the whole request
		}

		extra := resolveSuggestions(ctx, raw)
		log.Printf("gatherSuggestions: attempt %d asked=%d raw=%d resolved=%d (seed=%q)", attempt, ask, len(raw), len(extra), callSeed)
		for _, v := range extra {
			if seen[v.VideoID] {
				continue
			}
			seen[v.VideoID] = true
			validated = append(validated, v)
			titlesSoFar = append(titlesSoFar, v.Title)
			if len(validated) >= count {
				break
			}
		}
	}

	if len(validated) > count {
		validated = validated[:count]
	}
	if len(validated) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return validated, nil
}

// ---------------- HTTP handlers ----------------

type suggestRequest struct {
	Seed  string `json:"seed"`
	Count int    `json:"count"`
}

type suggestResponse struct {
	Suggestions []validatedTrack `json:"suggestions"`
	Error       string           `json:"error,omitempty"`
}

// servePage serves one named page from the app root with the current station state
// injected. Only the pages are exposed — never the directory (it holds the server
// source, data/, logs and bin/).
func servePage(store *Store, root, name string) http.HandlerFunc {
	path := filepath.Join(root, name)
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile(path)
		if err != nil {
			http.Error(w, "page missing", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(store.injectState(b))
	}
}

func main() {
	root := appRoot()
	loadDotEnvFile(filepath.Join(root, ".env"))
	envPath := envOr("RADIO_ENV_FILE", filepath.Join(root, ".env"))
	listenAddr := envOr("RADIO_LISTEN", defaultListen)

	ytDlpPath = resolveYtDlp(root)
	if ytDlpPath == "" {
		log.Fatalf("yt-dlp not found: set YTDLP, put it at %s, or install it on $PATH", filepath.Join(root, "bin", "yt-dlp"))
	}
	for _, page := range []string{"index.html", "ai-builder.html", "find.html", "station-store.js"} {
		if !fileExists(filepath.Join(root, page)) {
			log.Fatalf("%s not found under app root %s (set RADIO_ROOT)", page, root)
		}
	}
	log.Printf("app root=%s yt-dlp=%s env=%s", root, ytDlpPath, envPath)

	saJSON, ok := loadEnvValue(envPath, "GOOGLE_SERVICE_ACCOUNT_JSON")
	if !ok || saJSON == "" {
		log.Fatalf("GOOGLE_SERVICE_ACCOUNT_JSON not found in %s", envPath)
	}
	location, _ := loadEnvValue(envPath, "VERTEX_LOCATION")
	if location == "" {
		location = "global"
	}

	vc, err := NewVertexClient([]byte(saJSON), location)
	if err != nil {
		log.Fatalf("vertex client init failed: %v", err)
	}
	log.Printf("vertex client OK: project=%s location=%s model=%s", vc.projectID, vc.location, geminiModel)

	store, err := OpenStore(envOr("RADIO_DATA", filepath.Join(root, "data")))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	rev, _ := store.Get()
	log.Printf("station store: %s rev=%d", store.path, rev)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", servePage(store, root, "index.html"))
	for _, page := range []string{"index.html", "ai-builder.html", "find.html"} {
		mux.HandleFunc("GET /"+page, servePage(store, root, page))
	}
	mux.HandleFunc("GET /station-store.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(root, "station-store.js"))
	})

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		rev, state := store.Get()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{"rev": rev, "state": state})
	})
	mux.HandleFunc("PUT /api/state", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Rev   int64           `json:"rev"`
			State json.RawMessage `json:"state"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, stationBodyLimit)).Decode(&req); err != nil {
			http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		rev, err := store.Put(req.Rev, req.State)
		switch {
		case errors.Is(err, errStaleRev):
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"rev": rev, "error": "stale revision"})
		case err != nil:
			log.Printf("store put: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		default:
			json.NewEncoder(w).Encode(map[string]any{"rev": rev})
		}
	})

	mux.HandleFunc("POST /api/find", handleFind)

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"project":%q,"location":%q,"model":%q}`, vc.projectID, vc.location, geminiModel)
	})

	mux.HandleFunc("/api/suggest", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
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

		validated, err := gatherSuggestions(ctx, vc, req.Seed, req.Count)
		if err != nil {
			log.Printf("gemini error: %v", err)
			writeJSONErr(w, http.StatusBadGateway, "gemini call failed: "+err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(suggestResponse{Suggestions: validated})
	})

	log.Printf("radio-ai-server listening on http://%s", listenAddr)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}

func writeJSONErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(suggestResponse{Suggestions: []validatedTrack{}, Error: msg})
}
