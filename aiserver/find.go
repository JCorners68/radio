// find.go — Copy & Find: paste a Spotify playlist/album/track link (or plain
// "Artist - Title" lines) and get each track located on YouTube.
//
// Reading Spotify needs no account or API key: the public embed page for a playlist,
// album or track carries the track list (title, artists, exact duration) in its
// __NEXT_DATA__ JSON.
//
// Finding runs in two tiers per track:
//  1. direct — one yt-dlp search plus the deterministic matcher in youtube.go. A hit whose
//     title/artist match AND whose length is within 5 s of Spotify's is accepted outright.
//  2. qwen — anything else goes to a local Qwen endpoint with a youtube_search tool. It
//     reasons about spellings, remasters, featured artists, Topic channels etc., runs its
//     own searches, and picks one result. Go only accepts a videoId that appeared in a real
//     search result, so the model can choose but never invent.
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
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxFindTracks   = 1000
	directWorkers   = 6
	qwenWorkers     = 3
	qwenMaxSearches = 5
	searchHits      = 8
)

// ---------------- source parsing ----------------

// srcTrack is one track to find. Secs is 0 when the length is unknown (free text).
type srcTrack struct {
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Secs   int    `json:"secs"`
	Raw    string `json:"raw,omitempty"`
}

var spotifyRef = regexp.MustCompile(`(?:open\.spotify\.com/(?:intl-[a-z]{2}/)?|spotify:)(playlist|album|track)[/:]([A-Za-z0-9]{22})`)
var nextData = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__" type="application/json">(.*?)</script>`)
var dashSplit = regexp.MustCompile(`\s+[-–—]\s+`)

// parsePaste turns pasted text into tracks. Spotify links are expanded via their embed
// pages; any other non-empty line is treated as "Artist - Title" free text.
func parsePaste(ctx context.Context, text string) (string, []srcTrack, error) {
	var tracks []srcTrack
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		refs := spotifyRef.FindAllStringSubmatch(line, -1)
		if len(refs) == 0 {
			if strings.Contains(line, "://") {
				continue // some other link; nothing to search for
			}
			t := srcTrack{Raw: line, Title: line}
			if p := dashSplit.Split(line, 2); len(p) == 2 {
				t.Artist, t.Title = strings.TrimSpace(p[0]), strings.TrimSpace(p[1])
			}
			tracks = append(tracks, t)
			continue
		}
		for _, m := range refs {
			kind, id := m[1], m[2]
			if seen[kind+id] {
				continue
			}
			seen[kind+id] = true
			name, ts, err := fetchSpotifyEmbed(ctx, kind, id)
			if err != nil {
				return "", nil, fmt.Errorf("spotify %s %s: %w", kind, id, err)
			}
			if kind != "track" && name != "" {
				names = append(names, name)
			}
			tracks = append(tracks, ts...)
		}
	}
	if len(tracks) > maxFindTracks {
		tracks = tracks[:maxFindTracks]
	}
	return strings.Join(names, " + "), tracks, nil
}

// fetchSpotifyEmbed reads one playlist/album/track from its public embed page.
// Spotify caps the embed HTML trackList at 100 items, but the same __NEXT_DATA__ payload
// includes an anonymous session accessToken that can query pathfinder v2 for all items.
func fetchSpotifyEmbed(ctx context.Context, kind, id string) (string, []srcTrack, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://open.spotify.com/embed/"+kind+"/"+id, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64)")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("embed page HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	m := nextData.FindSubmatch(body)
	if m == nil {
		return "", nil, errors.New("embed page has no __NEXT_DATA__ (Spotify changed the page?)")
	}
	var nd struct {
		Props struct {
			PageProps struct {
				State struct {
					Settings struct {
						Session struct {
							AccessToken string `json:"accessToken"`
						} `json:"session"`
					} `json:"settings"`
					Data struct {
						Entity struct {
							Name    string `json:"name"`
							Title   string `json:"title"`
							Artists []struct {
								Name string `json:"name"`
							} `json:"artists"`
							Duration  int `json:"duration"`
							TrackList []struct {
								Title    string `json:"title"`
								Subtitle string `json:"subtitle"`
								Duration int    `json:"duration"`
							} `json:"trackList"`
						} `json:"entity"`
					} `json:"data"`
				} `json:"state"`
			} `json:"pageProps"`
		} `json:"props"`
	}
	if err := json.Unmarshal(m[1], &nd); err != nil {
		return "", nil, fmt.Errorf("decode embed data: %w", err)
	}
	e := nd.Props.PageProps.State.Data.Entity
	if kind == "track" {
		var artists []string
		for _, a := range e.Artists {
			artists = append(artists, a.Name)
		}
		title := e.Name
		if title == "" {
			title = e.Title
		}
		return "", []srcTrack{{Title: title, Artist: strings.Join(artists, ", "), Secs: (e.Duration + 500) / 1000}}, nil
	}
	token := nd.Props.PageProps.State.Settings.Session.AccessToken
	if kind == "playlist" && len(e.TrackList) >= 100 && token != "" {
		if full, err := fetchSpotifyPlaylistFull(ctx, id, token); err == nil && len(full) > len(e.TrackList) {
			return e.Name, full, nil
		} else if err != nil {
			log.Printf("find: full playlist fetch fallback to 100 tracks: %v", err)
		}
	}
	out := make([]srcTrack, 0, len(e.TrackList))
	for _, t := range e.TrackList {
		out = append(out, srcTrack{Title: t.Title, Artist: strings.ReplaceAll(t.Subtitle, "\u00a0", " "), Secs: (t.Duration + 500) / 1000})
	}
	return e.Name, out, nil
}

const defaultPlaylistQueryHash = "21ea11a6ac75a5f3e01fd365d29c18e0a4a8b7dc09f89bbb5d73910190c6acaf"

// fetchSpotifyPlaylistFull fetches up to maxFindTracks items from Spotify's partner GraphQL API
// using the anonymous bearer token embedded in the public embed page.
func fetchSpotifyPlaylistFull(ctx context.Context, id, token string) ([]srcTrack, error) {
	payload := map[string]any{
		"variables": map[string]any{
			"uri":                       "spotify:playlist:" + id,
			"offset":                    0,
			"limit":                     maxFindTracks,
			"enableWatchFeedEntrypoint": false,
		},
		"operationName": "fetchPlaylist",
		"extensions": map[string]any{
			"persistedQuery": map[string]any{
				"version":    1,
				"sha256Hash": defaultPlaylistQueryHash,
			},
		},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api-partner.spotify.com/pathfinder/v2/query", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 25 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("pathfinder HTTP %d", resp.StatusCode)
	}
	var res struct {
		Data struct {
			PlaylistV2 struct {
				Content struct {
					Items []struct {
						ItemV2 struct {
							Data struct {
								Name    string `json:"name"`
								Artists struct {
									Items []struct {
										Profile struct {
											Name string `json:"name"`
										} `json:"profile"`
									} `json:"items"`
								} `json:"artists"`
								TrackDuration struct {
									TotalMilliseconds int `json:"totalMilliseconds"`
								} `json:"trackDuration"`
							} `json:"data"`
						} `json:"itemV2"`
					} `json:"items"`
				} `json:"content"`
			} `json:"playlistV2"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	items := res.Data.PlaylistV2.Content.Items
	out := make([]srcTrack, 0, len(items))
	for _, it := range items {
		d := it.ItemV2.Data
		if d.Name == "" {
			continue
		}
		var artists []string
		for _, a := range d.Artists.Items {
			if a.Profile.Name != "" {
				artists = append(artists, a.Profile.Name)
			}
		}
		out = append(out, srcTrack{
			Title:  d.Name,
			Artist: strings.Join(artists, ", "),
			Secs:   (d.TrackDuration.TotalMilliseconds + 500) / 1000,
		})
	}
	return out, nil
}

// ---------------- finding ----------------

type findResult struct {
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

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func hitResult(i int, h ytHit, method, confidence, reason string, searches []string) findResult {
	return findResult{Type: "result", I: i, Found: true, Method: method, VideoID: h.ID, Title: h.Title,
		Channel: h.Channel, Secs: h.Duration, Thumb: fmt.Sprintf("https://i.ytimg.com/vi/%s/mqdefault.jpg", h.ID),
		Confidence: confidence, Reason: reason, Searches: searches}
}

// findDirect is tier 1. It returns ok=false plus the hits it saw, which seed tier 2.
func findDirect(ctx context.Context, i int, t srcTrack) (findResult, []ytHit, bool) {
	q := strings.TrimSpace(t.Artist + " " + t.Title)
	hits, err := ytSearch(ctx, q, searchHits)
	if err != nil || len(hits) == 0 {
		return findResult{}, nil, false
	}
	h, score, ok := bestHit(hits, t.Title, t.Artist, t.Secs)
	if !ok {
		return findResult{}, hits, false
	}
	switch {
	case t.Secs > 0 && h.Duration > 0 && absInt(h.Duration-t.Secs) <= 5:
		return hitResult(i, h, "direct", "high", fmt.Sprintf("title, artist and length match (%s vs %s)", mmss(h.Duration), mmss(t.Secs)), []string{q}), hits, true
	case t.Secs == 0 && score >= 3.0:
		return hitResult(i, h, "direct", "high", "title and artist match", []string{q}), hits, true
	}
	return findResult{}, hits, false
}

func mmss(s int) string { return fmt.Sprintf("%d:%02d", s/60, s%60) }

func describeHits(hits []ytHit) string {
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

// qwenChat sends one turn. think=false switches Qwen's reasoning off for that turn — used
// to force a final answer when a reasoning trace ran out of budget. It also returns the
// finish reason ("length" means the budget ran out before an answer was written).
func qwenChat(ctx context.Context, msgs []chatMsg, think bool) (chatMsg, string, error) {
	qwenURL := envOr("QWEN_URL", "http://127.0.0.1:8000/v1")
	qwenModel := envOr("QWEN_MODEL", "qwen3.8-27b")
	body, _ := json.Marshal(map[string]any{
		"model":                qwenModel,
		"messages":             msgs,
		"tools":                []any{youtubeSearchTool},
		"max_tokens":           4096,
		"temperature":          0.3,
		"chat_template_kwargs": map[string]any{"enable_thinking": think},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", qwenURL+"/chat/completions", bytes.NewReader(body))
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

// findQwen is tier 2: Qwen drives the searches and picks; Go holds it to real results.
func findQwen(ctx context.Context, i int, t srcTrack, firstQuery string, firstHits []ytHit) findResult {
	seen := map[string]ytHit{}
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

	notFound := func(reason string) findResult {
		return findResult{Type: "result", I: i, Found: false, Method: "qwen", Reason: reason, Searches: searches}
	}

	nudged := false
	for turn := 0; turn < qwenMaxSearches+3; turn++ {
		msg, finish, err := qwenChat(ctx, msgs, !nudged)
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
					hits, err := ytSearch(ctx, args.Query, searchHits)
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
		obj := extractJSONObject(msg.Content)
		if err := json.Unmarshal([]byte(obj), &ans); err != nil {
			log.Printf("find[%d] qwen answer not JSON (finish=%s nudged=%v): %q", i, finish, nudged, truncate(msg.Content, 300))
			if nudged {
				return notFound("Qwen gave no usable answer")
			}
			// Usually finish=length: the reasoning trace ate the budget. Ask once more with
			// thinking off, so the reply is the answer itself.
			nudged = true
			msgs = msgs[:len(msgs)-1] // drop the empty assistant turn
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

// extractJSONObject pulls the outermost {...} out of model text (fences, stray prose).
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start >= 0 && end > start {
		return s[start : end+1]
	}
	return s
}

// handleFind streams NDJSON: one "source" line with the parsed tracks, then one "result"
// line per track as it resolves (out of order), then "done".
func handleFind(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	name, tracks, err := parsePaste(ctx, req.Text)
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
	emit(map[string]any{"type": "source", "name": name, "tracks": tracks})
	log.Printf("find: %d tracks from %q", len(tracks), name)

	results := make(chan findResult)
	directSem := make(chan struct{}, directWorkers)
	qwenSem := make(chan struct{}, qwenWorkers)
	var wg sync.WaitGroup
	for i, t := range tracks {
		wg.Add(1)
		go func(i int, t srcTrack) {
			defer wg.Done()
			select {
			case directSem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			res, hits, ok := findDirect(ctx, i, t)
			<-directSem
			if !ok {
				select {
				case qwenSem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				res = findQwen(ctx, i, t, strings.TrimSpace(t.Artist+" "+t.Title), hits)
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
