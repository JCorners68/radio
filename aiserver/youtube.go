// youtube.go — YouTube lookups via yt-dlp, the ground-truth half of the AI flow.
//
// Two jobs:
//  1. resolveSeed: a YouTube URL seed is turned into "title — channel" BEFORE it reaches
//     Gemini. Gemini has no tools on this path, so a bare URL is just an opaque video ID to
//     it; given AC/DC "Thunderstruck" as a URL it confidently returned post-hardcore.
//  2. resolveViaYtDlp: a Gemini pick (title/artist) is matched against the top few real
//     search hits, and a hit is only accepted if its title/channel actually contain the
//     song and the artist. Taking hit #1 blindly let "Anatomy" resolve to a Grey's
//     Anatomy clip and "Âme" to an A-Wall video.
package main

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

// ---------------- seed resolution ----------------

var bareVideoID = regexp.MustCompile(`^[\w-]{11}$`)

// youTubeID returns the video ID if s is a YouTube watch/short/embed URL, else "".
func youTubeID(s string) string {
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

// resolveSeed replaces a YouTube URL seed with the video's real title and channel. Any
// other seed (genre, mood, artist) is returned unchanged.
func resolveSeed(ctx context.Context, seed string) string {
	id := youTubeID(seed)
	if id == "" {
		return seed
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, ytDlpPath, "https://www.youtube.com/watch?v="+id,
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

// ---------------- pick -> video matching ----------------

// ytHit is one yt-dlp search result.
type ytHit struct {
	ID       string
	Title    string
	Channel  string
	Duration int // seconds, 0 if unknown
}

// ytSearch runs a real YouTube search and returns up to n hits.
func ytSearch(ctx context.Context, query string, n int) ([]ytHit, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, ytDlpPath, fmt.Sprintf("ytsearch%d:%s", n, query),
		"--skip-download", "--no-warnings", "--quiet", "--flat-playlist",
		"--print", "%(id)s\t%(title)s\t%(channel)s\t%(duration)s").Output()
	if err != nil {
		return nil, err
	}
	var hits []ytHit
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p := strings.SplitN(line, "\t", 4)
		if len(p) < 4 || p[0] == "" {
			continue
		}
		d, _ := strconv.ParseFloat(p[3], 64)
		hits = append(hits, ytHit{ID: p[0], Title: p[1], Channel: p[2], Duration: int(d)})
	}
	return hits, nil
}

// normTokens lowercases, strips accents and punctuation, and splits into words.
// Note: transform.Chain is stateful and not safe for concurrent use across goroutines,
// so construct it per call.
func normTokens(s string) []string {
	fold := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	s, _, _ = transform.String(fold, strings.ToLower(s))
	s = strings.ReplaceAll(s, "&", " and ")
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// coreTitle drops bracketed qualifiers ("(feat. X)", "[Remastered 2011]", "- 2009 Remaster")
// so matching is on the song name itself.
var bracketed = regexp.MustCompile(`\s*[\(\[][^\)\]]*[\)\]]`)

func coreTitle(t string) string {
	t = bracketed.ReplaceAllString(t, "")
	if i := strings.Index(t, " - "); i > 0 {
		low := strings.ToLower(t[i:])
		if strings.Contains(low, "remaster") || strings.Contains(low, "version") || strings.Contains(low, "edit") || strings.Contains(low, "mix") {
			t = t[:i]
		}
	}
	return t
}

// coverage is the fraction of want's tokens present in have (as tokens, or — for
// run-together channel names like "ACDCVEVO" — as a substring of have with spaces removed).
func coverage(want []string, have string) float64 {
	if len(want) == 0 {
		return 0
	}
	hv := normTokens(have)
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

// Words that mark a different recording than the one asked for — penalized unless the
// requested title itself contains them.
var offRecording = []string{"cover", "karaoke", "reaction", "tutorial", "lesson", "instrumental", "8d", "slowed", "sped", "nightcore", "1 hour", "10 hours", "loop"}

// scoreHit returns how well hit matches the wanted song, and whether it clears the bar.
// A hit must contain most of the song title and most of the artist (in title or channel).
func scoreHit(h ytHit, title, artist string, wantSecs int) (float64, bool) {
	tt := normTokens(coreTitle(title))
	at := normTokens(artist)
	ts := coverage(tt, h.Title)
	as := 1.0
	if len(at) > 0 {
		as = coverage(at, h.Title+" "+h.Channel)
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
		// a known track length is the strongest signal there is
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
		score -= 1.0 // a mix/compilation, not a track
	}
	return score, score > 1.0
}

// bestHit returns the highest-scoring acceptable hit, or ok=false.
func bestHit(hits []ytHit, title, artist string, wantSecs int) (ytHit, float64, bool) {
	var best ytHit
	bestScore := -1.0
	for _, h := range hits {
		if s, ok := scoreHit(h, title, artist, wantSecs); ok && s > bestScore {
			best, bestScore = h, s
		}
	}
	return best, bestScore, bestScore >= 0
}

// resolveViaYtDlp searches YouTube for "<artist> <title>" and returns the best hit that
// actually matches the pick, so a wrong or unrelated video never reaches the user.
func resolveViaYtDlp(ctx context.Context, title, artist string) (*validatedTrack, bool) {
	query := strings.TrimSpace(artist + " " + title)
	if query == "" {
		return nil, false
	}
	hits, err := ytSearch(ctx, query, 5)
	if err != nil || len(hits) == 0 {
		return nil, false
	}
	h, _, ok := bestHit(hits, title, artist, 0)
	if !ok {
		log.Printf("resolve: no matching hit for %q / %q (top was %q by %q)", title, artist, hits[0].Title, hits[0].Channel)
		return nil, false
	}
	resolvedArtist := artist
	if resolvedArtist == "" {
		resolvedArtist = h.Channel
	}
	return &validatedTrack{
		VideoID: h.ID,
		Title:   h.Title,
		Artist:  resolvedArtist,
		Thumb:   fmt.Sprintf("https://i.ytimg.com/vi/%s/mqdefault.jpg", h.ID),
	}, true
}
