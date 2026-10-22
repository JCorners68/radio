// Package youtube provides YouTube lookups and deterministic matching via yt-dlp.
package youtube

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Client wraps yt-dlp invocations against YouTube.
type Client struct {
	YtDlpPath string
}

func NewClient(ytDlpPath string) *Client {
	return &Client{YtDlpPath: ytDlpPath}
}

// Hit is one yt-dlp search result.
type Hit struct {
	ID       string
	Title    string
	Channel  string
	Duration int // seconds, 0 if unknown
}

// ValidatedTrack is a resolved YouTube track returned by AI suggest.
type ValidatedTrack struct {
	VideoID string `json:"videoId"`
	Title   string `json:"title"`
	Artist  string `json:"artist"`
	Reason  string `json:"reason"`
	Thumb   string `json:"thumb"`
}

var bareVideoID = regexp.MustCompile(`^[\w-]{11}$`)

// YouTubeID returns the video ID if s is a YouTube watch/short/embed URL, else "".
func YouTubeID(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	host = strings.TrimPrefix(host, "m.")
	switch host {
	case "youtu.be":
		if id := strings.Trim(u.Path, "/"); bareVideoID.MatchString(id) {
			return id
		}
	case "youtube.com", "music.youtube.com":
		if id := u.Query().Get("v"); bareVideoID.MatchString(id) {
			return id
		}
		for _, p := range []string{"/shorts/", "/embed/", "/live/"} {
			if rest, ok := strings.CutPrefix(u.Path, p); ok {
				if id := strings.SplitN(rest, "/", 2)[0]; bareVideoID.MatchString(id) {
					return id
				}
			}
		}
	}
	return ""
}

// ResolveSeed replaces a YouTube URL seed with the video's real title and channel. Any
// other seed (genre, mood, artist) is returned unchanged.
func (c *Client) ResolveSeed(ctx context.Context, seed string) string {
	id := YouTubeID(seed)
	if id == "" {
		return seed
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, c.YtDlpPath, "https://www.youtube.com/watch?v="+id,
		"--js-runtimes", "node",
		"--skip-download", "--no-warnings", "--quiet", "--print", "%(title)s\t%(channel)s").Output()
	if err != nil {
		log.Printf("resolveSeed: yt-dlp failed for %s: %v", id, err)
		return seed
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(parts) == 0 || parts[0] == "" {
		return seed
	}
	resolved := "the song in this YouTube video: \"" + parts[0] + "\""
	if len(parts) == 2 && parts[1] != "" {
		resolved += " (channel: " + parts[1] + ")"
	}
	log.Printf("resolveSeed: %s -> %s", id, resolved)
	return resolved
}

// Search runs a real YouTube search and returns up to n hits.
func (c *Client) Search(ctx context.Context, query string, n int) ([]Hit, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, c.YtDlpPath, fmt.Sprintf("ytsearch%d:%s", n, query),
		"--js-runtimes", "node",
		"--skip-download", "--no-warnings", "--quiet", "--flat-playlist",
		"--print", "%(id)s\t%(title)s\t%(channel)s\t%(duration)s").Output()
	if err != nil {
		return nil, err
	}
	var hits []Hit
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p := strings.SplitN(line, "\t", 4)
		if len(p) < 4 || p[0] == "" {
			continue
		}
		d, _ := strconv.ParseFloat(p[3], 64)
		hits = append(hits, Hit{ID: p[0], Title: p[1], Channel: p[2], Duration: int(d)})
	}
	return hits, nil
}

// ResolvePick searches YouTube for "<artist> <title>" and returns the best hit that
// actually matches the pick, so a wrong or unrelated video never reaches the user.
func (c *Client) ResolvePick(ctx context.Context, title, artist string) (*ValidatedTrack, bool) {
	query := strings.TrimSpace(artist + " " + title)
	if query == "" {
		return nil, false
	}
	hits, err := c.Search(ctx, query, 5)
	if err != nil || len(hits) == 0 {
		return nil, false
	}
	h, _, ok := BestHit(hits, title, artist, 0)
	if !ok {
		log.Printf("resolve: no matching hit for %q / %q (top was %q by %q)", title, artist, hits[0].Title, hits[0].Channel)
		return nil, false
	}
	resolvedArtist := artist
	if resolvedArtist == "" {
		resolvedArtist = h.Channel
	}
	return &ValidatedTrack{
		VideoID: h.ID,
		Title:   h.Title,
		Artist:  resolvedArtist,
		Thumb:   fmt.Sprintf("https://i.ytimg.com/vi/%s/mqdefault.jpg", h.ID),
	}, true
}

// NormTokens lowercases, strips accents and punctuation, and splits into words.
// Note: transform.Chain is stateful and not safe for concurrent use across goroutines,
// so construct it per call.
func NormTokens(s string) []string {
	fold := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	s, _, _ = transform.String(fold, strings.ToLower(s))
	s = strings.ReplaceAll(s, "&", " and ")
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

var bracketed = regexp.MustCompile(`\s*[\(\[][^\)\]]*[\)\]]`)

// CoreTitle drops bracketed qualifiers ("(feat. X)", "[Remastered 2011]", "- 2009 Remaster")
// so matching is on the song name itself.
func CoreTitle(t string) string {
	t = bracketed.ReplaceAllString(t, "")
	if i := strings.Index(t, " - "); i > 0 {
		low := strings.ToLower(t[i:])
		if strings.Contains(low, "remaster") || strings.Contains(low, "version") || strings.Contains(low, "edit") || strings.Contains(low, "mix") {
			t = t[:i]
		}
	}
	return t
}

// Coverage is the fraction of want's tokens present in have (as tokens, or for
// run-together channel names like "ACDCVEVO", as a substring of have with spaces removed).
func Coverage(want []string, have string) float64 {
	if len(want) == 0 {
		return 0
	}
	hv := NormTokens(have)
	set := make(map[string]bool, len(hv))
	for _, t := range hv {
		set[t] = true
	}
	joined := strings.Join(hv, "")
	n := 0
	for _, w := range want {
		if set[w] || (len(w) >= 3 && strings.Contains(joined, w)) {
			n++
		}
	}
	return float64(n) / float64(len(want))
}

var offRecording = []string{"cover", "karaoke", "reaction", "tutorial", "lesson", "instrumental", "8d", "slowed", "sped", "nightcore", "1 hour", "10 hours", "loop"}

// ScoreHit returns how well hit matches the wanted song, and whether it clears the bar.
func ScoreHit(h Hit, title, artist string, wantSecs int) (float64, bool) {
	tt := NormTokens(CoreTitle(title))
	at := NormTokens(artist)
	ts := Coverage(tt, h.Title)
	as := 1.0
	if len(at) > 0 {
		as = Coverage(at, h.Title+" "+h.Channel)
	}
	if ts < 0.6 || as < 0.5 {
		return 0, false
	}
	score := 2*ts + as

	low := strings.ToLower(h.Title)
	wantLow := strings.ToLower(title)
	for _, w := range offRecording {
		if strings.Contains(low, w) && !strings.Contains(wantLow, w) {
			score -= 1.5
		}
	}
	if strings.Contains(low, "official") || strings.HasSuffix(h.Channel, " - Topic") || strings.Contains(strings.ToLower(h.Channel), "vevo") {
		score += 0.3
	}
	switch {
	case wantSecs > 0 && h.Duration > 0:
		diff := h.Duration - wantSecs
		if diff < 0 {
			diff = -diff
		}
		switch {
		case diff <= 5:
			score += 1.0
		case diff <= 20:
			score += 0.4
		case diff > 90:
			score -= 1.0
		}
	case h.Duration > 15*60:
		score -= 1.0
	}
	return score, score > 1.0
}

// BestHit returns the highest-scoring acceptable hit, or ok=false.
func BestHit(hits []Hit, title, artist string, wantSecs int) (Hit, float64, bool) {
	var best Hit
	bestScore := -1.0
	for _, h := range hits {
		if s, ok := ScoreHit(h, title, artist, wantSecs); ok && s > bestScore {
			best, bestScore = h, s
		}
	}
	return best, bestScore, bestScore >= 0
}
