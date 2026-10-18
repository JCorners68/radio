package dj

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"radio-ai-server/internal/catalog/youtube"
)

const DefaultGeminiModel = "gemini-3.6-flash"

type Suggestion struct {
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Reason string `json:"reason"`
}

// Service coordinates Gemini song recommendations and YouTube video resolution.
type Service struct {
	Vertex *VertexClient
	YT     *youtube.Client
	Model  string
}

func NewService(vc *VertexClient, yt *youtube.Client, model string) *Service {
	if model == "" {
		model = DefaultGeminiModel
	}
	return &Service{Vertex: vc, YT: yt, Model: model}
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

func (s *Service) callGemini(ctx context.Context, seed string, count int) ([]Suggestion, error) {
	payload := map[string]any{
		"systemInstruction": map[string]any{
			"parts": []map[string]string{{"text": systemInstruction}},
		},
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]string{{"text": buildPrompt(seed, count)}}},
		},
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

	tok, err := s.Vertex.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("vertex token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.Vertex.GenerateContentURL(s.Model), bytes.NewReader(body))
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

	var sb strings.Builder
	for _, p := range parsed.Candidates[0].Content.Parts {
		sb.WriteString(p.Text)
	}
	text := ExtractJSONArray(sb.String())

	var suggestions []Suggestion
	if err := json.Unmarshal([]byte(text), &suggestions); err != nil {
		if repaired := RepairTruncatedArray(text); repaired != "" {
			if err2 := json.Unmarshal([]byte(repaired), &suggestions); err2 == nil && len(suggestions) > 0 {
				return suggestions, nil
			}
		}
		return nil, fmt.Errorf("decode suggestions JSON: %w (text=%s)", err, truncate(text, 800))
	}
	return suggestions, nil
}

// RepairTruncatedArray takes a JSON array string that may have been cut off mid-object
// and returns a valid, closed array containing only complete top-level objects.
func RepairTruncatedArray(s string) string {
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

// ExtractJSONArray pulls a JSON array out of model output that may be wrapped in
// markdown code fences or preceded/followed by stray prose.
func ExtractJSONArray(s string) string {
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

func (s *Service) resolveSuggestions(ctx context.Context, raw []Suggestion) []youtube.ValidatedTrack {
	type result struct {
		idx int
		vt  *youtube.ValidatedTrack
	}
	results := make(chan result, len(raw))
	for i, sg := range raw {
		go func(i int, sg Suggestion) {
			vt, ok := s.YT.ResolvePick(ctx, sg.Title, sg.Artist)
			if !ok {
				results <- result{i, nil}
				return
			}
			vt.Reason = sg.Reason
			results <- result{i, vt}
		}(i, sg)
	}

	ordered := make([]*youtube.ValidatedTrack, len(raw))
	for range raw {
		r := <-results
		ordered[r.idx] = r.vt
	}
	out := make([]youtube.ValidatedTrack, 0, len(raw))
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

// GatherSuggestions calls Gemini in a top-up loop until it has count tracks that
// matched a real YouTube video (or gives up after a few attempts).
func (s *Service) GatherSuggestions(ctx context.Context, seed string, count int) ([]youtube.ValidatedTrack, error) {
	seed = s.YT.ResolveSeed(ctx, seed)
	validated := make([]youtube.ValidatedTrack, 0, count)
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

		raw, err := s.callGemini(ctx, callSeed, ask)
		if err != nil {
			log.Printf("gatherSuggestions: attempt %d failed: %v", attempt, err)
			lastErr = err
			continue
		}

		extra := s.resolveSuggestions(ctx, raw)
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
