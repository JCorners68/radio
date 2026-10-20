package youtube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type cachedStream struct {
	URL     string
	Expires time.Time
}

type inflightCall struct {
	done chan struct{}
	url  string
	err  error
}

// StreamCache caches resolved direct audio stream URLs from yt-dlp so iOS Safari
// byte-range probes (Range: bytes=0-1 followed by full range) don't re-run yt-dlp.
type StreamCache struct {
	client   *Client
	mu       sync.Mutex
	entries  map[string]cachedStream
	inflight map[string]*inflightCall
	httpCli  *http.Client
}

func NewStreamCache(c *Client) *StreamCache {
	return &StreamCache{
		client:   c,
		entries:  make(map[string]cachedStream),
		inflight: make(map[string]*inflightCall),
		httpCli: &http.Client{
			Timeout: 0, // streaming responses have no fixed total timeout
		},
	}
}

// ValidVideoID reports whether id is a valid 11-character YouTube video ID.
func ValidVideoID(id string) bool {
	return bareVideoID.MatchString(id)
}

// Resolve returns a direct audio stream URL (m4a preferred for iOS Safari) for videoID.
func (sc *StreamCache) Resolve(ctx context.Context, videoID string) (string, error) {
	if !ValidVideoID(videoID) {
		return "", errors.New("invalid videoId")
	}

	sc.mu.Lock()
	if e, ok := sc.entries[videoID]; ok && time.Now().Before(e.Expires) {
		sc.mu.Unlock()
		return e.URL, nil
	}
	if call, ok := sc.inflight[videoID]; ok {
		sc.mu.Unlock()
		select {
		case <-call.done:
			return call.url, call.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	call := &inflightCall{done: make(chan struct{})}
	sc.inflight[videoID] = call
	sc.mu.Unlock()

	u, exp, err := sc.resolveFresh(ctx, videoID)

	sc.mu.Lock()
	call.url = u
	call.err = err
	close(call.done)
	delete(sc.inflight, videoID)
	if err == nil && u != "" {
		sc.entries[videoID] = cachedStream{URL: u, Expires: exp}
	}
	sc.mu.Unlock()

	return u, err
}

// Evict removes a cached URL for videoID.
func (sc *StreamCache) Evict(videoID string) {
	sc.mu.Lock()
	delete(sc.entries, videoID)
	sc.mu.Unlock()
}

func (sc *StreamCache) resolveFresh(ctx context.Context, videoID string) (string, time.Time, error) {
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	// Prefer m4a (itag 140, AAC in MP4 container) for native iOS/iPadOS Safari <audio> support.
	out, err := exec.CommandContext(cctx, sc.client.YtDlpPath,
		"--no-playlist",
		"-f", "bestaudio[ext=m4a]/bestaudio",
		"--no-warnings", "--quiet", "-g",
		"https://www.youtube.com/watch?v="+videoID,
	).Output()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("yt-dlp stream resolve %s: %w", videoID, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return "", time.Time{}, fmt.Errorf("yt-dlp returned empty stream URL for %s", videoID)
	}
	streamURL := strings.TrimSpace(lines[0])
	exp := parseStreamExpiry(streamURL)
	return streamURL, exp, nil
}

func parseStreamExpiry(rawURL string) time.Time {
	defaultExp := time.Now().Add(45 * time.Minute)
	u, err := url.Parse(rawURL)
	if err != nil {
		return defaultExp
	}
	if expStr := u.Query().Get("expire"); expStr != "" {
		if unixSec, err := strconv.ParseInt(expStr, 10, 64); err == nil && unixSec > 0 {
			exp := time.Unix(unixSec, 0).Add(-5 * time.Minute)
			if exp.After(time.Now()) {
				return exp
			}
		}
	}
	return defaultExp
}

// ProxyStream streams audio for videoID to w, forwarding HTTP Range headers for iOS Safari.
func (sc *StreamCache) ProxyStream(w http.ResponseWriter, r *http.Request, videoID string) {
	if !ValidVideoID(videoID) {
		http.Error(w, "invalid videoId", http.StatusBadRequest)
		return
	}

	// Prefetch-only request warms the cache and returns 204 immediately.
	if r.URL.Query().Get("prefetch") == "1" {
		if _, err := sc.Resolve(r.Context(), videoID); err != nil {
			log.Printf("stream prefetch %s failed: %v", videoID, err)
			http.Error(w, "prefetch failed", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	for attempt := 0; attempt < 2; attempt++ {
		streamURL, err := sc.Resolve(r.Context(), videoID)
		if err != nil {
			log.Printf("stream resolve %s failed: %v", videoID, err)
			http.Error(w, "stream unavailable", http.StatusBadGateway)
			return
		}

		method := "GET"
		if r.Method == http.MethodHead {
			method = "HEAD"
		}
		upReq, err := http.NewRequestWithContext(r.Context(), method, streamURL, nil)
		if err != nil {
			http.Error(w, "bad upstream request", http.StatusInternalServerError)
			return
		}
		if rng := r.Header.Get("Range"); rng != "" {
			upReq.Header.Set("Range", rng)
		}
		upReq.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)")

		resp, err := sc.httpCli.Do(upReq)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("stream upstream %s failed: %v", videoID, err)
			}
			return
		}

		if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone) && attempt == 0 {
			resp.Body.Close()
			sc.Evict(videoID)
			continue
		}
		defer resp.Body.Close()

		for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "audio/mp4")
		}
		if w.Header().Get("Accept-Ranges") == "" {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		w.Header().Set("Cache-Control", "private, max-age=3600")
		w.WriteHeader(resp.StatusCode)
		if method != "HEAD" {
			_, _ = io.Copy(w, resp.Body)
		}
		return
	}
}
