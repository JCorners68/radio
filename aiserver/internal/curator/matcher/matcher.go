// Package matcher implements the two-tier Copy & Find resolver:
//  1. direct — deterministic yt-dlp search + token/duration scoring
//  2. qwen — local Qwen tool-calling agent bounded to real search results
package matcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"radio-ai-server/internal/catalog/spotify"
	"radio-ai-server/internal/catalog/youtube"
)

const (
	directWorkers   = 6
	qwenWorkers     = 3
	qwenMaxSearches = 5
	searchHits      = 8
)

type Result struct {
	Type       string   `json:"type"` // "result"
	I          int      `json:"i"`
	Found      bool     `json:"found"`
	Method     string   `json:"method"` // "direct" | "qwen"
	VideoID    string   `json:"videoId,omitempty"`
	Title      string   `json:"title,omitempty"`
	Channel    string   `json:"channel,omitempty"`
	Secs       int      `json:"secs,omitempty"`
	Thumb      string   `json:"thumb,omitempty"`
	Confidence string   `json:"confidence,omitempty"` // high | medium | low
	Reason     string   `json:"reason,omitempty"`
	Searches   []string `json:"searches,omitempty"`
}

// Service coordinates direct and Qwen-assisted track matching.
type Service struct {
	YT        *youtube.Client
	QwenURL   string
	QwenModel string
}

func NewService(yt *youtube.Client, qwenURL, qwenModel string) *Service {
	if qwenURL == "" {
		qwenURL = "http://127.0.0.1:8000/v1"
	}
	if qwenModel == "" {
		qwenModel = "qwen3.8-27b"
	}
	return &Service{YT: yt, QwenURL: qwenURL, QwenModel: qwenModel}
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func hitResult(i int, h youtube.Hit, method, confidence, reason string, searches []string) Result {
	return Result{
		Type:       "result",
		I:          i,
		Found:      true,
		Method:     method,
		VideoID:    h.ID,
		Title:      h.Title,
		Channel:    h.Channel,
		Secs:       h.Duration,
		Thumb:      fmt.Sprintf("https://i.ytimg.com/vi/%s/mqdefault.jpg", h.ID),
		Confidence: confidence,
		Reason:     reason,
		Searches:   searches,
	}
}

func mmss(s int) string { return fmt.Sprintf("%d:%02d", s/60, s%60) }

func describeHits(hits []youtube.Hit) string {
	var b strings.Builder
	for _, h := range hits {
		d := "?"
		if h.Duration > 0 {
			d = mmss(h.Duration)
		}
		fmt.Fprintf(&b, "%s | %s | %s | %s\n", h.ID, h.Title, h.Channel, d)
	}
	if b.Len() == 0 {
		return "(no results)\n"
	}
	return b.String()
}

// FindDirect is tier 1. It returns ok=false plus the hits it saw, which seed tier 2.
func (s *Service) FindDirect(ctx context.Context, i int, t spotify.Track) (Result, []youtube.Hit, bool) {
	q := strings.TrimSpace(t.Artist + " " + t.Title)
	hits, err := s.YT.Search(ctx, q, searchHits)
	if err != nil || len(hits) == 0 {
		return Result{}, nil, false
	}
	h, score, ok := youtube.BestHit(hits, t.Title, t.Artist, t.Secs)
	if !ok {
		return Result{}, hits, false
	}
	switch {
	case t.Secs > 0 && h.Duration > 0 && absInt(h.Duration-t.Secs) <= 5:
		return hitResult(i, h, "direct", "high", fmt.Sprintf("title, artist and length match (%s vs %s)", mmss(h.Duration), mmss(t.Secs)), []string{q}), hits, true
	case t.Secs == 0 && score >= 3.0:
		return hitResult(i, h, "direct", "high", "title and artist match", []string{q}), hits, true
	}
	return Result{}, hits, false
}

const qwenSystem = `You locate one specific recording on YouTube for a personal music player.

You have a youtube_search tool. Each result line is: videoId | title | channel | length.

How to work:
- Think about why a plain search might miss: alternate spellings, transliterations, "feat." artists, remaster/radio-edit/extended versions, the artist's own channel or the auto-generated "<Artist> - Topic" channel (those carry the exact studio recording).
- The target length is the strongest evidence: the right recording is usually within a few seconds of it.
- Reject covers, karaoke, live versions, remixes, sped-up/slowed edits, lyric-video re-uploads by random channels and long mixes — unless the target itself is that version.
- Use at most 5 searches. Stop as soon as you are confident.

When done, reply with ONLY a JSON object, no prose:
{"videoId": "<an id that appeared in a search result, or null>", "confidence": "high|medium|low", "reason": "<20 words max>"}`

type chatMsg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

var youtubeSearchTool = map[string]any{
	"type": "function",
	"function": map[string]any{
		"name":        "youtube_search",
		"description": "Search YouTube. Returns up to 8 results as: videoId | title | channel | length.",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
			"required":   []string{"query"},
		},
	},
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (s *Service) qwenChat(ctx context.Context, msgs []chatMsg, think bool) (chatMsg, string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":                s.QwenModel,
		"messages":             msgs,
		"tools":                []any{youtubeSearchTool},
		"max_tokens":           4096,
		"temperature":          0.3,
		"chat_template_kwargs": map[string]any{"enable_thinking": think},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", s.QwenURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatMsg{}, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 240 * time.Second}).Do(req)
	if err != nil {
		return chatMsg{}, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return chatMsg{}, "", fmt.Errorf("qwen HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var parsed struct {
		Choices []struct {
			Message      chatMsg `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Choices) == 0 {
		return chatMsg{}, "", fmt.Errorf("qwen: bad response: %s", truncate(string(raw), 300))
	}
	return parsed.Choices[0].Message, parsed.Choices[0].FinishReason, nil
}

// FindQwen is tier 2: Qwen drives the searches and picks; Go holds it to real results.
func (s *Service) FindQwen(ctx context.Context, i int, t spotify.Track, firstQuery string, firstHits []youtube.Hit) Result {
	seen := map[string]youtube.Hit{}
	for _, h := range firstHits {
		seen[h.ID] = h
	}
	searches := []string{firstQuery}

	target := fmt.Sprintf("Title: %s\nArtist(s): %s\n", t.Title, t.Artist)
	if t.Secs > 0 {
		target += "Length: " + mmss(t.Secs) + "\n"
	} else {
		target += "Length: unknown\n"
	}
	if t.Raw != "" {
		target += fmt.Sprintf("(typed by the user as %q — the artist/title split is a guess)\n", t.Raw)
	}
	msgs := []chatMsg{
		{Role: "system", Content: qwenSystem},
		{Role: "user", Content: fmt.Sprintf("Find this recording:\n%s\nI already searched %q and got:\n%s", target, firstQuery, describeHits(firstHits))},
	}

	notFound := func(reason string) Result {
		return Result{Type: "result", I: i, Found: false, Method: "qwen", Reason: reason, Searches: searches}
	}

	nudged := false
	for turn := 0; turn < qwenMaxSearches+3; turn++ {
		msg, finish, err := s.qwenChat(ctx, msgs, !nudged)
		if err != nil {
			log.Printf("find[%d] qwen: %v", i, err)
			return notFound("Qwen unavailable: " + truncate(err.Error(), 80))
		}
		msgs = append(msgs, chatMsg{Role: "assistant", Content: msg.Content, ToolCalls: msg.ToolCalls})

		if len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				var args struct {
					Query string `json:"query"`
				}
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				var out string
				switch {
				case len(searches) > qwenMaxSearches:
					out = "Search budget used up. Give your final JSON answer now."
				case strings.TrimSpace(args.Query) == "":
					out = "Empty query."
				default:
					searches = append(searches, args.Query)
					hits, err := s.YT.Search(ctx, args.Query, searchHits)
					if err != nil {
						out = "Search failed: " + err.Error()
					} else {
						for _, h := range hits {
							seen[h.ID] = h
						}
						out = describeHits(hits)
					}
				}
				msgs = append(msgs, chatMsg{Role: "tool", ToolCallID: tc.ID, Content: out})
			}
			continue
		}

		var ans struct {
			VideoID    *string `json:"videoId"`
			Confidence string  `json:"confidence"`
			Reason     string  `json:"reason"`
		}
		obj := ExtractJSONObject(msg.Content)
		if err := json.Unmarshal([]byte(obj), &ans); err != nil {
			log.Printf("find[%d] qwen answer not JSON (finish=%s nudged=%v): %q", i, finish, nudged, truncate(msg.Content, 300))
			if nudged {
				return notFound("Qwen gave no usable answer")
			}
			nudged = true
			msgs = msgs[:len(msgs)-1]
			msgs = append(msgs, chatMsg{Role: "user", Content: `Decide now from the results you have. Reply with ONLY the JSON object: {"videoId": <id or null>, "confidence": "high|medium|low", "reason": "<20 words max>"}`})
			continue
		}
		if ans.VideoID == nil || *ans.VideoID == "" {
			return notFound(ans.Reason)
		}
		h, ok := seen[*ans.VideoID]
		if !ok {
			log.Printf("find[%d] qwen picked %q which was never in a search result — rejected", i, *ans.VideoID)
			return notFound("Qwen picked a video that wasn't in any search result (rejected)")
		}
		conf := ans.Confidence
		if conf != "high" && conf != "medium" && conf != "low" {
			conf = "low"
		}
		return hitResult(i, h, "qwen", conf, ans.Reason, searches)
	}
	return notFound("ran out of turns")
}

// ExtractJSONObject pulls the outermost {...} out of model text (fences, stray prose).
func ExtractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

// StreamFind runs the two-tier worker pool across tracks and invokes emit for each NDJSON event.
func (s *Service) StreamFind(ctx context.Context, name, image string, tracks []spotify.Track, emit func(any)) {
	emit(map[string]any{"type": "source", "name": name, "image": image, "tracks": tracks})
	log.Printf("find: %d tracks from %q (image=%v)", len(tracks), name, image != "")

	results := make(chan Result)
	directSem := make(chan struct{}, directWorkers)
	qwenSem := make(chan struct{}, qwenWorkers)
	var wg sync.WaitGroup
	for i, t := range tracks {
		wg.Add(1)
		go func(i int, t spotify.Track) {
			defer wg.Done()
			select {
			case directSem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			res, hits, ok := s.FindDirect(ctx, i, t)
			<-directSem
			if !ok {
				select {
				case qwenSem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				res = s.FindQwen(ctx, i, t, strings.TrimSpace(t.Artist+" "+t.Title), hits)
				<-qwenSem
			}
			select {
			case results <- res:
			case <-ctx.Done():
			}
		}(i, t)
	}
	go func() { wg.Wait(); close(results) }()

	found, viaQwen := 0, 0
	for res := range results {
		if res.Found {
			found++
		}
		if res.Method == "qwen" {
			viaQwen++
		}
		emit(res)
	}
	log.Printf("find: done %d/%d found (%d needed qwen)", found, len(tracks), viaQwen)
	emit(map[string]any{"type": "done", "found": found, "total": len(tracks)})
}
