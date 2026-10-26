// Package spotify extracts track lists from pasted Spotify playlist/album/track links
// or free-form "Artist - Title" lines using Spotify's public embed page and pathfinder v2 API.
package spotify

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
	"time"
)

const MaxFindTracks = 1000

// Track is one source track to find on YouTube. Secs is 0 when the length is unknown (free text).
type Track struct {
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Secs   int    `json:"secs"`
	Raw    string `json:"raw,omitempty"`
}

var (
	spotifyRef    = regexp.MustCompile(`(?:open\.spotify\.com/(?:intl-[a-z]{2}/)?(?:user/[^/\s]+/)?(?:embed/)?|spotify:)(playlist|album|track)[/:]([A-Za-z0-9]{22})`)
	shortLinkRef  = regexp.MustCompile(`https?://(?:spotify\.link|spoti\.fi|spotify\.app\.link)/[A-Za-z0-9_-]+`)
	nextData      = regexp.MustCompile(`(?s)<script id="__NEXT_DATA__" type="application/json">(.*?)</script>`)
	ogTitleRe     = regexp.MustCompile(`<meta property="og:title" content="([^"]+)"`)
	ogImageRe     = regexp.MustCompile(`<meta property="og:image" content="([^"]+)"`)
	dashSplit     = regexp.MustCompile(`\s+[-–—]\s+`)
)

func fetchOpenGraphMeta(ctx context.Context, kind, id string) (title, image string) {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, "https://open.spotify.com/"+kind+"/"+id, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("User-Agent", "facebookexternalhit/1.1")
	resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 24<<10))
	if m := ogTitleRe.FindSubmatch(body); len(m) == 2 {
		title = strings.TrimSpace(string(m[1]))
	}
	if m := ogImageRe.FindSubmatch(body); len(m) == 2 {
		image = strings.TrimSpace(string(m[1]))
	}
	return title, image
}

func expandShortLinks(ctx context.Context, line string) string {
	return shortLinkRef.ReplaceAllStringFunc(line, func(shortURL string) string {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, shortURL, nil)
		if err != nil {
			return shortURL
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64)")
		client := &http.Client{
			Timeout: 8 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if strings.Contains(req.URL.Host, "open.spotify.com") {
					return http.ErrUseLastResponse
				}
				if len(via) >= 5 {
					return http.ErrUseLastResponse
				}
				return nil
			},
		}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("spotify shortlink expand %s error: %v", shortURL, err)
			return shortURL
		}
		defer resp.Body.Close()
		if loc := resp.Header.Get("Location"); loc != "" {
			log.Printf("spotify shortlink %s -> %s", shortURL, loc)
			return loc
		}
		if resp.Request != nil && resp.Request.URL != nil {
			return resp.Request.URL.String()
		}
		return shortURL
	})
}

// ParsePaste turns pasted text into tracks. Spotify links are expanded via their embed
// pages; any other non-empty line is treated as "Artist - Title" free text.
func ParsePaste(ctx context.Context, text string) (string, string, []Track, error) {
	var tracks []Track
	var names []string
	var firstImage string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if shortLinkRef.MatchString(line) {
			line = expandShortLinks(ctx, line)
		}
		refs := spotifyRef.FindAllStringSubmatch(line, -1)
		if len(refs) == 0 {
			if strings.Contains(line, "://") {
				continue // some other link; nothing to search for
			}
			t := Track{Raw: line, Title: line}
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
			name, img, ts, err := FetchEmbed(ctx, kind, id)
			if err != nil {
				if kind == "playlist" && (strings.Contains(line, "pt=") || strings.Contains(err.Error(), "returned 0 tracks")) {
					title, _ := fetchOpenGraphMeta(ctx, kind, id)
					label := id
					if title != "" {
						label = fmt.Sprintf("%q (%s)", title, id)
					}
					return "", "", nil, fmt.Errorf("Spotify playlist %s is set to Private in Spotify (shared with a ?pt= invite token, which Spotify blocks from unauthenticated API reads). Open %s in Spotify -> tap '...' -> tap 'Make public', then click Find on YouTube again", label, label)
				}
				return "", "", nil, fmt.Errorf("spotify %s %s: %w", kind, id, err)
			}
			if kind != "track" && name != "" {
				names = append(names, name)
			}
			if firstImage == "" && img != "" {
				firstImage = img
			}
			tracks = append(tracks, ts...)
		}
	}
	if len(tracks) > MaxFindTracks {
		tracks = tracks[:MaxFindTracks]
	}
	return strings.Join(names, " + "), firstImage, tracks, nil
}

// FetchEmbed reads one playlist/album/track from its public embed page.
// Spotify caps the embed HTML trackList at 100 items, but the same __NEXT_DATA__ payload
// includes an anonymous session accessToken that can query pathfinder v2 for all items.
func FetchEmbed(ctx context.Context, kind, id string) (string, string, []Track, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://open.spotify.com/embed/"+kind+"/"+id, nil)
	if err != nil {
		return "", "", nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64)")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", nil, fmt.Errorf("embed page HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	m := nextData.FindSubmatch(body)
	if m == nil {
		return "", "", nil, errors.New("embed page has no __NEXT_DATA__ (Spotify changed the page?)")
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
							CoverArt struct {
								Sources []struct {
									URL    string `json:"url"`
									Width  int    `json:"width"`
									Height int    `json:"height"`
								} `json:"sources"`
							} `json:"coverArt"`
							VisualIdentity struct {
								Image []struct {
									URL    string `json:"url"`
									Width  int    `json:"width"`
									Height int    `json:"height"`
								} `json:"image"`
							} `json:"visualIdentity"`
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
		return "", "", nil, fmt.Errorf("decode embed data: %w", err)
	}
	e := nd.Props.PageProps.State.Data.Entity
	var imgURL string
	for _, s := range e.CoverArt.Sources {
		if u := strings.TrimSpace(s.URL); u != "" {
			imgURL = u
			if s.Width >= 250 {
				break
			}
		}
	}
	if imgURL == "" {
		for _, s := range e.VisualIdentity.Image {
			if u := strings.TrimSpace(s.URL); u != "" {
				imgURL = u
				if s.Width >= 250 {
					break
				}
			}
		}
	}
	if imgURL == "" {
		_, imgURL = fetchOpenGraphMeta(ctx, kind, id)
	}
	if kind == "track" {
		var artists []string
		for _, a := range e.Artists {
			artists = append(artists, a.Name)
		}
		title := e.Name
		if title == "" {
			title = e.Title
		}
		return "", imgURL, []Track{{Title: title, Artist: strings.Join(artists, ", "), Secs: (e.Duration + 500) / 1000}}, nil
	}
	token := nd.Props.PageProps.State.Settings.Session.AccessToken
	if kind == "playlist" && (len(e.TrackList) == 0 || len(e.TrackList) >= 100) && token != "" {
		if full, err := FetchPlaylistFull(ctx, id, token); err == nil && len(full) > len(e.TrackList) {
			return e.Name, imgURL, full, nil
		} else if err != nil {
			log.Printf("find: full playlist fetch fallback (embed tracks=%d): %v", len(e.TrackList), err)
		}
	}
	out := make([]Track, 0, len(e.TrackList))
	for _, t := range e.TrackList {
		out = append(out, Track{Title: t.Title, Artist: strings.ReplaceAll(t.Subtitle, "\u00a0", " "), Secs: (t.Duration + 500) / 1000})
	}
	if len(out) == 0 {
		return e.Name, imgURL, nil, fmt.Errorf("playlist %s returned 0 tracks (check that the playlist is set to Public in Spotify)", id)
	}
	return e.Name, imgURL, out, nil
}

const defaultPlaylistQueryHash = "21ea11a6ac75a5f3e01fd365d29c18e0a4a8b7dc09f89bbb5d73910190c6acaf"

// FetchPlaylistFull fetches up to MaxFindTracks items from Spotify's partner GraphQL API
// using the anonymous bearer token embedded in the public embed page.
func FetchPlaylistFull(ctx context.Context, id, token string) ([]Track, error) {
	payload := map[string]any{
		"variables": map[string]any{
			"uri":                       "spotify:playlist:" + id,
			"offset":                    0,
			"limit":                     MaxFindTracks,
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
	out := make([]Track, 0, len(items))
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
		out = append(out, Track{
			Title:  d.Name,
			Artist: strings.Join(artists, ", "),
			Secs:   (d.TrackDuration.TotalMilliseconds + 500) / 1000,
		})
	}
	return out, nil
}
