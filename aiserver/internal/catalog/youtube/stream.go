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

const (
	// headPrefetchBytes is the initial prefix cached in RAM per resolved video so
	// iOS Safari's 2-byte probe (Range: bytes=0-1) and fMP4 header (ftyp/moov/sidx)
	// return instantaneously without waiting on googlevideo.com.
	headPrefetchBytes = 512 << 10 // 512 KiB

	// upstreamChunkBytes bounds each HTTP Range request to googlevideo.com.
	// YouTube CDN throttles open-ended / full-file Range requests on DASH itag=140
	// streams to ~32 KB/s with 1.5s stalls, which causes iOS AppleCoreMedia to abort
	// and retry in a loop. Bounded 2 MiB Range requests stream at full line rate (~18 MB/s).
	upstreamChunkBytes = 2 << 20 // 2 MiB

	streamUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"
)

type cachedStream struct {
	URL         string
	Expires     time.Time
	TotalLen    int64
	ContentType string
	Head        []byte
}

type inflightCall struct {
	done  chan struct{}
	entry cachedStream
	err   error
}

// StreamCache caches resolved direct audio stream URLs and their initial 512 KiB
// header from yt-dlp so iOS Safari byte-range probes and playback start immediately
// and stream through bounded upstream chunks without YouTube CDN throttling.
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
	e, err := sc.resolveEntry(ctx, videoID)
	if err != nil {
		return "", err
	}
	return e.URL, nil
}

func (sc *StreamCache) resolveEntry(ctx context.Context, videoID string) (cachedStream, error) {
	if !ValidVideoID(videoID) {
		return cachedStream{}, errors.New("invalid videoId")
	}

	sc.mu.Lock()
	if e, ok := sc.entries[videoID]; ok && time.Now().Before(e.Expires) {
		sc.mu.Unlock()
		return e, nil
	}
	if call, ok := sc.inflight[videoID]; ok {
		sc.mu.Unlock()
		select {
		case <-call.done:
			return call.entry, call.err
		case <-ctx.Done():
			return cachedStream{}, ctx.Err()
		}
	}
	call := &inflightCall{done: make(chan struct{})}
	sc.inflight[videoID] = call
	sc.mu.Unlock()

	entry, err := sc.resolveFresh(ctx, videoID)

	sc.mu.Lock()
	call.entry = entry
	call.err = err
	close(call.done)
	delete(sc.inflight, videoID)
	if err == nil && entry.URL != "" {
		sc.entries[videoID] = entry
	}
	sc.mu.Unlock()

	return entry, err
}

// Evict removes a cached URL for videoID.
func (sc *StreamCache) Evict(videoID string) {
	sc.mu.Lock()
	delete(sc.entries, videoID)
	sc.mu.Unlock()
}

func (sc *StreamCache) resolveFresh(ctx context.Context, videoID string) (cachedStream, error) {
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	// Prefer m4a (itag 140, AAC in MP4 container) for native iOS/iPadOS Safari <audio> support.
	out, err := exec.CommandContext(cctx, sc.client.YtDlpPath,
		"--js-runtimes", "node",
		"--no-playlist",
		"-f", "bestaudio[ext=m4a]/bestaudio",
		"--no-warnings", "--quiet", "-g",
		"https://www.youtube.com/watch?v="+videoID,
	).Output()
	if err != nil {
		return cachedStream{}, fmt.Errorf("yt-dlp stream resolve %s: %w", videoID, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return cachedStream{}, fmt.Errorf("yt-dlp returned empty stream URL for %s", videoID)
	}
	streamURL := strings.TrimSpace(lines[0])
	entry := cachedStream{
		URL:         streamURL,
		Expires:     parseStreamExpiry(streamURL),
		TotalLen:    parseStreamClen(streamURL),
		ContentType: "audio/mp4",
	}

	// Pre-fetch the initial 512 KiB prefix so iOS Safari's 2-byte probe and fMP4
	// initialization boxes (ftyp/moov/sidx) are served from RAM in < 1ms.
	sc.populateHead(cctx, &entry)
	return entry, nil
}

func (sc *StreamCache) populateHead(ctx context.Context, entry *cachedStream) {
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(hctx, http.MethodGet, entry.URL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", headPrefetchBytes-1))
	req.Header.Set("User-Agent", streamUserAgent)

	resp, err := sc.httpCli.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return
	}
	if ct := strings.TrimSpace(resp.Header.Get("Content-Type")); ct != "" {
		entry.ContentType = ct
	}
	if total := parseContentRangeTotal(resp.Header.Get("Content-Range")); total > 0 {
		entry.TotalLen = total
	} else if resp.StatusCode == http.StatusOK && resp.ContentLength > 0 {
		entry.TotalLen = resp.ContentLength
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, headPrefetchBytes))
	if err == nil && len(head) > 0 {
		entry.Head = head
	}
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

func parseStreamClen(rawURL string) int64 {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	if clenStr := u.Query().Get("clen"); clenStr != "" {
		if n, err := strconv.ParseInt(clenStr, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func parseContentRangeTotal(cr string) int64 {
	// Format: "bytes start-end/total"
	slash := strings.LastIndexByte(cr, '/')
	if slash < 0 || slash+1 >= len(cr) {
		return 0
	}
	totalStr := strings.TrimSpace(cr[slash+1:])
	if totalStr == "*" {
		return 0
	}
	n, err := strconv.ParseInt(totalStr, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// parseByteRange parses a single HTTP Range header ("bytes=start-end", "bytes=start-", "bytes=-suffix")
// against a known totalLen (> 0). It returns (start, endInclusive, isPartial, ok).
func parseByteRange(rangeHeader string, totalLen int64) (int64, int64, bool, bool) {
	rangeHeader = strings.TrimSpace(rangeHeader)
	if rangeHeader == "" {
		return 0, totalLen - 1, false, true
	}
	if !strings.HasPrefix(rangeHeader, "bytes=") {
		return 0, 0, false, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(rangeHeader, "bytes="))
	if idx := strings.IndexByte(spec, ','); idx >= 0 {
		spec = strings.TrimSpace(spec[:idx])
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false, false
	}
	startPart := strings.TrimSpace(spec[:dash])
	endPart := strings.TrimSpace(spec[dash+1:])

	if startPart == "" {
		// Suffix range: "bytes=-500"
		suffix, err := strconv.ParseInt(endPart, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, false
		}
		if suffix > totalLen {
			suffix = totalLen
		}
		return totalLen - suffix, totalLen - 1, true, true
	}

	start, err := strconv.ParseInt(startPart, 10, 64)
	if err != nil || start < 0 || start >= totalLen {
		return 0, 0, false, false
	}
	end := totalLen - 1
	if endPart != "" {
		parsedEnd, err := strconv.ParseInt(endPart, 10, 64)
		if err != nil || parsedEnd < start {
			return 0, 0, false, false
		}
		if parsedEnd < end {
			end = parsedEnd
		}
	}
	return start, end, true, true
}

// ProxyStream streams audio for videoID to w, serving cached header bytes immediately
// and fetching remaining ranges in bounded 2 MiB chunks to avoid YouTube CDN throttling.
func (sc *StreamCache) ProxyStream(w http.ResponseWriter, r *http.Request, videoID string) {
	if !ValidVideoID(videoID) {
		http.Error(w, "invalid videoId", http.StatusBadRequest)
		return
	}

	// Prefetch-only request warms the cache and returns 204 immediately.
	if r.URL.Query().Get("prefetch") == "1" {
		if _, err := sc.resolveEntry(r.Context(), videoID); err != nil {
			log.Printf("stream prefetch %s failed: %v", videoID, err)
			http.Error(w, "prefetch failed", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	for attempt := 0; attempt < 2; attempt++ {
		entry, err := sc.resolveEntry(r.Context(), videoID)
		if err != nil {
			log.Printf("stream resolve %s failed: %v", videoID, err)
			http.Error(w, "stream unavailable", http.StatusBadGateway)
			return
		}

		// If TotalLen was not in the URL query or head prefetch, fall back to a bounded probe.
		if entry.TotalLen <= 0 {
			sc.populateHead(r.Context(), &entry)
			if entry.TotalLen > 0 {
				sc.mu.Lock()
				sc.entries[videoID] = entry
				sc.mu.Unlock()
			}
		}

		if entry.TotalLen <= 0 {
			// Fallback to direct pass-through if total length is still unknown.
			if retry := sc.proxyPassThrough(w, r, videoID, entry.URL, attempt == 0); retry {
				continue
			}
			return
		}

		start, end, isPartial, ok := parseByteRange(r.Header.Get("Range"), entry.TotalLen)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", entry.TotalLen))
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}

		contentLen := end - start + 1
		ct := entry.ContentType
		if ct == "" {
			ct = "audio/mp4"
		}

		// Fast path: if the entire requested range is already in entry.Head (e.g., iOS Safari's
		// initial Range: bytes=0-1 probe), serve directly from memory with zero upstream latency.
		headLen := int64(len(entry.Head))
		if r.Method == http.MethodHead || (headLen > 0 && end < headLen) {
			writeStreamHeaders(w, ct, contentLen, start, end, entry.TotalLen, isPartial)
			if r.Method != http.MethodHead {
				_, _ = w.Write(entry.Head[start : end+1])
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
			}
			return
		}

		cur := start
		headersWritten := false

		// If the request starts within the cached Head prefix, write that slice first
		// and advance cur so the first upstream chunk starts at headLen.
		if headLen > 0 && cur < headLen {
			writeStreamHeaders(w, ct, contentLen, start, end, entry.TotalLen, isPartial)
			headersWritten = true
			prefixEnd := end + 1
			if prefixEnd > headLen {
				prefixEnd = headLen
			}
			if _, werr := w.Write(entry.Head[cur:prefixEnd]); werr != nil {
				return
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			cur = prefixEnd
		}

		// Stream remaining bytes [cur, end] in bounded 2 MiB Range requests.
		retried := false
		for cur <= end {
			chunkEnd := cur + upstreamChunkBytes - 1
			if chunkEnd > end {
				chunkEnd = end
			}

			upReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, entry.URL, nil)
			if err != nil {
				if !headersWritten {
					http.Error(w, "bad upstream request", http.StatusInternalServerError)
				}
				return
			}
			upReq.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", cur, chunkEnd))
			upReq.Header.Set("User-Agent", streamUserAgent)

			resp, err := sc.httpCli.Do(upReq)
			if err != nil {
				if !errors.Is(err, context.Canceled) {
					log.Printf("stream upstream %s [%d-%d] failed: %v", videoID, cur, chunkEnd, err)
				}
				return
			}

			if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone) && !headersWritten && attempt == 0 {
				resp.Body.Close()
				sc.Evict(videoID)
				retried = true
				break
			}

			if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				if !headersWritten {
					http.Error(w, "upstream stream error", http.StatusBadGateway)
				}
				return
			}

			if !headersWritten {
				writeStreamHeaders(w, ct, contentLen, start, end, entry.TotalLen, isPartial)
				headersWritten = true
			}

			expected := chunkEnd - cur + 1
			n, copyErr := io.CopyN(w, resp.Body, expected)
			resp.Body.Close()
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			cur += n
			if copyErr != nil {
				if !errors.Is(copyErr, io.EOF) && !errors.Is(copyErr, context.Canceled) {
					log.Printf("stream copy %s at %d/%d ended: %v", videoID, cur, end, copyErr)
				}
				return
			}
			if n == 0 {
				return
			}
		}
		if retried {
			continue
		}
		return
	}
}

func writeStreamHeaders(w http.ResponseWriter, contentType string, contentLen, start, end, totalLen int64, isPartial bool) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(contentLen, 10))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if isPartial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, totalLen))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
}

func (sc *StreamCache) proxyPassThrough(w http.ResponseWriter, r *http.Request, videoID, streamURL string, canRetry bool) bool {
	method := http.MethodGet
	if r.Method == http.MethodHead {
		method = http.MethodHead
	}
	upReq, err := http.NewRequestWithContext(r.Context(), method, streamURL, nil)
	if err != nil {
		http.Error(w, "bad upstream request", http.StatusInternalServerError)
		return false
	}
	if rng := r.Header.Get("Range"); rng != "" {
		upReq.Header.Set("Range", rng)
	}
	upReq.Header.Set("User-Agent", streamUserAgent)

	resp, err := sc.httpCli.Do(upReq)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("stream upstream %s failed: %v", videoID, err)
		}
		return false
	}
	if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusGone) && canRetry {
		resp.Body.Close()
		sc.Evict(videoID)
		return true
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
	if method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
	return false
}
