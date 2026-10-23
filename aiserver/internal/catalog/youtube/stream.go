package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

// StreamCache caches resolved direct audio stream URLs, their initial 512 KiB
// header in RAM, persistent full-file M4A audio on local NVMe disk, and mirrors
// completed tracks to Cloudflare R2 so the Cloudflare edge Worker serves them
// with zero residential origin traversal.
type StreamCache struct {
	client   *Client
	cacheDir string
	r2       *R2Store

	mu           sync.Mutex
	entries      map[string]cachedStream
	inflight     map[string]*inflightCall
	archiving    map[string]bool
	r2Uploaded   map[string]bool
	r2IndexPath  string
	warmQueue    chan string
	warmEnqueued map[string]bool
	httpCli      *http.Client
}

func NewStreamCache(c *Client) *StreamCache {
	return &StreamCache{
		client:       c,
		entries:      make(map[string]cachedStream),
		inflight:     make(map[string]*inflightCall),
		archiving:    make(map[string]bool),
		r2Uploaded:   make(map[string]bool),
		warmQueue:    make(chan string, 1024),
		warmEnqueued: make(map[string]bool),
		httpCli: &http.Client{
			Timeout: 0, // streaming responses have no fixed total timeout
		},
	}
}

// ConfigurePersistence enables local NVMe disk caching under cacheDir and optional
// Cloudflare R2 mirroring, and starts background warmers.
func (sc *StreamCache) ConfigurePersistence(cacheDir string, r2 *R2Store) {
	sc.mu.Lock()
	if cacheDir != "" {
		_ = os.MkdirAll(cacheDir, 0o755)
		sc.cacheDir = cacheDir
		sc.r2IndexPath = filepath.Join(cacheDir, "r2_uploaded.json")
		if b, err := os.ReadFile(sc.r2IndexPath); err == nil {
			var list []string
			if json.Unmarshal(b, &list) == nil {
				for _, id := range list {
					sc.r2Uploaded[id] = true
				}
			}
		}
	}
	sc.r2 = r2
	sc.mu.Unlock()

	for i := 0; i < 2; i++ {
		go sc.warmWorker()
	}
}

// EnqueueWarm schedules videoIDs for background download to local NVMe and Cloudflare R2.
func (sc *StreamCache) EnqueueWarm(videoIDs ...string) {
	for _, id := range videoIDs {
		id = strings.TrimSpace(id)
		if !ValidVideoID(id) {
			continue
		}
		sc.mu.Lock()
		if sc.warmEnqueued[id] {
			sc.mu.Unlock()
			continue
		}
		sc.warmEnqueued[id] = true
		sc.mu.Unlock()

		select {
		case sc.warmQueue <- id:
		default:
			sc.mu.Lock()
			delete(sc.warmEnqueued, id)
			sc.mu.Unlock()
		}
	}
}

func (sc *StreamCache) warmWorker() {
	for videoID := range sc.warmQueue {
		sc.warmOne(videoID)
	}
}

func (sc *StreamCache) warmOne(videoID string) {
	defer func() {
		sc.mu.Lock()
		delete(sc.warmEnqueued, videoID)
		sc.mu.Unlock()
	}()

	if p := sc.localDiskPath(videoID); p != "" {
		sc.ensureR2Uploaded(videoID, p)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	entry, err := sc.resolveEntry(ctx, videoID)
	cancel()
	if err != nil {
		return
	}
	sc.archiveTrackSync(videoID, entry)
}

func (sc *StreamCache) markR2Uploaded(videoID string) {
	sc.mu.Lock()
	sc.r2Uploaded[videoID] = true
	indexPath := sc.r2IndexPath
	var list []string
	if indexPath != "" {
		list = make([]string, 0, len(sc.r2Uploaded))
		for id := range sc.r2Uploaded {
			list = append(list, id)
		}
	}
	sc.mu.Unlock()

	if indexPath != "" {
		if b, err := json.Marshal(list); err == nil {
			_ = os.WriteFile(indexPath, b, 0o644)
		}
	}
}

func (sc *StreamCache) isR2Uploaded(videoID string) bool {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.r2Uploaded[videoID]
}

func (sc *StreamCache) ensureR2Uploaded(videoID, diskPath string) {
	if sc.r2 == nil || sc.isR2Uploaded(videoID) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := sc.r2.UploadFile(ctx, videoID, diskPath); err != nil {
		log.Printf("r2 upload %s failed: %v", videoID, err)
		return
	}
	sc.markR2Uploaded(videoID)
	log.Printf("r2 uploaded %s (%s)", videoID, TrackKey(videoID))
}

func (sc *StreamCache) localDiskPath(videoID string) string {
	sc.mu.Lock()
	dir := sc.cacheDir
	sc.mu.Unlock()
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, videoID+".m4a")
	st, err := os.Stat(p)
	if err != nil || st.IsDir() || st.Size() <= 0 {
		return ""
	}
	return p
}

func (sc *StreamCache) triggerBackgroundArchive(videoID string, entry cachedStream) {
	sc.mu.Lock()
	dir := sc.cacheDir
	if dir == "" || sc.archiving[videoID] {
		sc.mu.Unlock()
		return
	}
	sc.archiving[videoID] = true
	sc.mu.Unlock()

	go func() {
		defer func() {
			sc.mu.Lock()
			delete(sc.archiving, videoID)
			sc.mu.Unlock()
		}()
		sc.archiveTrackSync(videoID, entry)
	}()
}

func (sc *StreamCache) archiveTrackSync(videoID string, entry cachedStream) {
	if p := sc.localDiskPath(videoID); p != "" {
		sc.ensureR2Uploaded(videoID, p)
		return
	}
	sc.mu.Lock()
	dir := sc.cacheDir
	sc.mu.Unlock()
	if dir == "" || entry.URL == "" {
		return
	}

	if entry.TotalLen <= 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		sc.populateHead(ctx, &entry)
		cancel()
	}
	if entry.TotalLen <= 0 {
		return
	}

	tmpPath := filepath.Join(dir, fmt.Sprintf(".%s.%d.tmp", videoID, time.Now().UnixNano()))
	finalPath := filepath.Join(dir, videoID+".m4a")

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	defer func() {
		f.Close()
		_ = os.Remove(tmpPath)
	}()

	cur := int64(0)
	if len(entry.Head) > 0 {
		n, werr := f.Write(entry.Head)
		if werr != nil {
			return
		}
		cur = int64(n)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	for cur < entry.TotalLen {
		chunkEnd := cur + upstreamChunkBytes - 1
		if chunkEnd >= entry.TotalLen {
			chunkEnd = entry.TotalLen - 1
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.URL, nil)
		if err != nil {
			return
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", cur, chunkEnd))
		req.Header.Set("User-Agent", streamUserAgent)

		resp, err := sc.httpCli.Do(req)
		if err != nil {
			return
		}
		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return
		}
		expected := chunkEnd - cur + 1
		n, copyErr := io.CopyN(f, resp.Body, expected)
		resp.Body.Close()
		cur += n
		if copyErr != nil && !errors.Is(copyErr, io.EOF) {
			return
		}
		if n == 0 {
			return
		}
	}

	if err := f.Close(); err != nil {
		return
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return
	}
	log.Printf("disk cache stored %s (%d bytes)", videoID, entry.TotalLen)
	sc.ensureR2Uploaded(videoID, finalPath)
}

// ValidVideoID reports whether id is a valid 11-character YouTube video ID.
func ValidVideoID(id string) bool {
	return bareVideoID.MatchString(id)
}

// Resolve returns a direct audio stream URL (m4a preferred for iOS Safari) for videoID,
// and triggers background disk + R2 caching if not already cached.
func (sc *StreamCache) Resolve(ctx context.Context, videoID string) (string, error) {
	if p := sc.localDiskPath(videoID); p != "" {
		go sc.ensureR2Uploaded(videoID, p)
		return "file://" + p, nil
	}
	e, err := sc.resolveEntry(ctx, videoID)
	if err != nil {
		return "", err
	}
	sc.triggerBackgroundArchive(videoID, e)
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

func (sc *StreamCache) serveFromDisk(w http.ResponseWriter, r *http.Request, videoID, diskPath string) bool {
	f, err := os.Open(diskPath)
	if err != nil {
		return false
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil || st.Size() <= 0 {
		return false
	}
	totalLen := st.Size()

	start, end, isPartial, ok := parseByteRange(r.Header.Get("Range"), totalLen)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", totalLen))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return true
	}

	contentLen := end - start + 1
	writeStreamHeaders(w, "audio/mp4", contentLen, start, end, totalLen, isPartial)
	w.Header().Set("X-Radio-Cache", "DISK-HIT")
	if r.Method == http.MethodHead {
		return true
	}

	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return true
	}
	_, _ = io.CopyN(w, f, contentLen)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	go sc.ensureR2Uploaded(videoID, diskPath)
	return true
}

// ProxyStream streams audio for videoID to w, serving from local NVMe disk cache first,
// then cached RAM header bytes, and fetching remaining ranges in bounded 2 MiB chunks
// while archiving the full track to local NVMe and Cloudflare R2 in the background.
func (sc *StreamCache) ProxyStream(w http.ResponseWriter, r *http.Request, videoID string) {
	if !ValidVideoID(videoID) {
		http.Error(w, "invalid videoId", http.StatusBadRequest)
		return
	}

	// Prefetch-only request warms the cache and triggers background disk + R2 archive.
	if r.URL.Query().Get("prefetch") == "1" {
		if p := sc.localDiskPath(videoID); p != "" {
			go sc.ensureR2Uploaded(videoID, p)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		entry, err := sc.resolveEntry(r.Context(), videoID)
		if err != nil {
			log.Printf("stream prefetch %s failed: %v", videoID, err)
			http.Error(w, "prefetch failed", http.StatusBadGateway)
			return
		}
		sc.triggerBackgroundArchive(videoID, entry)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Fast path 0: if the complete M4A is already on local NVMe, serve directly with zero YouTube I/O.
	if diskPath := sc.localDiskPath(videoID); diskPath != "" {
		if sc.serveFromDisk(w, r, videoID, diskPath) {
			return
		}
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

		// Kick off full-speed background download to NVMe + R2 so subsequent requests and
		// edge cache misses finish locally and populate Cloudflare R2.
		sc.triggerBackgroundArchive(videoID, entry)

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

		// Fast path 1: if the entire requested range is already in entry.Head (e.g., iOS Safari's
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
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("CDN-Cache-Control", "public, max-age=31536000, immutable")
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
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("CDN-Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(resp.StatusCode)
	if method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
	return false
}
