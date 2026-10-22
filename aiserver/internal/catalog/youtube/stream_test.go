package youtube

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseByteRange(t *testing.T) {
	const total = int64(179192009)

	cases := []struct {
		rng       string
		wantStart int64
		wantEnd   int64
		wantPart  bool
		wantOK    bool
	}{
		{"", 0, total - 1, false, true},
		{"bytes=0-1", 0, 1, true, true},
		{"bytes=0-179192008", 0, total - 1, true, true},
		{"bytes=0-", 0, total - 1, true, true},
		{"bytes=524288-1048575", 524288, 1048575, true, true},
		{"bytes=-1000", total - 1000, total - 1, true, true},
		{"bytes=179192009-179192100", 0, 0, false, false},
		{"items=0-5", 0, 0, false, false},
	}

	for _, tc := range cases {
		s, e, part, ok := parseByteRange(tc.rng, total)
		if ok != tc.wantOK || s != tc.wantStart || e != tc.wantEnd || part != tc.wantPart {
			t.Errorf("parseByteRange(%q, %d) = (%d, %d, %v, %v), want (%d, %d, %v, %v)",
				tc.rng, total, s, e, part, ok, tc.wantStart, tc.wantEnd, tc.wantPart, tc.wantOK)
		}
	}
}

func TestProxyStreamChunkedAndHeadCache(t *testing.T) {
	// Create a synthetic 5 MiB payload so ProxyStream must serve the first 512 KiB
	// from Head and fetch the remaining 4.5 MiB across multiple bounded 2 MiB chunks.
	const totalSize = 5 << 20 // 5 MiB
	payload := make([]byte, totalSize)
	for i := range payload {
		payload[i] = byte((i * 31) & 0xff)
	}

	var upstreamReqs int32
	var maxRequestedSpan int64

	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamReqs, 1)
		rng := r.Header.Get("Range")
		if !strings.HasPrefix(rng, "bytes=") {
			t.Errorf("upstream got non-range request: %q", rng)
			http.Error(w, "range required", http.StatusBadRequest)
			return
		}
		parts := strings.SplitN(strings.TrimPrefix(rng, "bytes="), "-", 2)
		start, err1 := strconv.ParseInt(parts[0], 10, 64)
		end, err2 := strconv.ParseInt(parts[1], 10, 64)
		if err1 != nil || err2 != nil || start < 0 || end < start || end >= totalSize {
			t.Errorf("invalid range %q", rng)
			http.Error(w, "bad range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		span := end - start + 1
		for {
			prev := atomic.LoadInt64(&maxRequestedSpan)
			if span <= prev || atomic.CompareAndSwapInt64(&maxRequestedSpan, prev, span) {
				break
			}
		}

		w.Header().Set("Content-Type", "audio/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, totalSize))
		w.Header().Set("Content-Length", strconv.FormatInt(span, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	defer upSrv.Close()

	sc := NewStreamCache(NewClient("yt-dlp"))
	vid := "dtGTNcS_1sI"
	sc.entries[vid] = cachedStream{
		URL:         upSrv.URL + "/videoplayback?clen=" + strconv.Itoa(totalSize),
		Expires:     time.Now().Add(30 * time.Minute),
		TotalLen:    totalSize,
		ContentType: "audio/mp4",
		Head:        append([]byte(nil), payload[:headPrefetchBytes]...),
	}

	// 1. iOS Safari 2-byte probe (Range: bytes=0-1) should be served 100% from RAM (0 upstream calls).
	reqProbe := httptest.NewRequest(http.MethodGet, "/api/stream/"+vid, nil)
	reqProbe.Header.Set("Range", "bytes=0-1")
	recProbe := httptest.NewRecorder()
	sc.ProxyStream(recProbe, reqProbe, vid)

	if recProbe.Code != http.StatusPartialContent {
		t.Fatalf("probe status = %d, want 206", recProbe.Code)
	}
	if got := atomic.LoadInt32(&upstreamReqs); got != 0 {
		t.Fatalf("probe triggered %d upstream requests, want 0 (served from RAM Head)", got)
	}
	if !bytes.Equal(recProbe.Body.Bytes(), payload[:2]) {
		t.Fatalf("probe body mismatch")
	}
	if gotCR := recProbe.Header().Get("Content-Range"); gotCR != fmt.Sprintf("bytes 0-1/%d", totalSize) {
		t.Fatalf("probe Content-Range = %q", gotCR)
	}

	// 2. iOS Safari full-file Range request (Range: bytes=0-5242879) should serve the first
	// 512 KiB from RAM and fetch the remaining 4.5 MiB in 3 bounded chunks (<= 2 MiB each).
	reqFull := httptest.NewRequest(http.MethodGet, "/api/stream/"+vid, nil)
	reqFull.Header.Set("Range", fmt.Sprintf("bytes=0-%d", totalSize-1))
	recFull := httptest.NewRecorder()
	sc.ProxyStream(recFull, reqFull, vid)

	if recFull.Code != http.StatusPartialContent {
		t.Fatalf("full range status = %d, want 206", recFull.Code)
	}
	if got := atomic.LoadInt32(&upstreamReqs); got != 3 {
		t.Fatalf("full range triggered %d upstream chunk requests, want 3", got)
	}
	if maxSpan := atomic.LoadInt64(&maxRequestedSpan); maxSpan > upstreamChunkBytes {
		t.Fatalf("max upstream chunk span = %d, exceeds %d", maxSpan, upstreamChunkBytes)
	}
	if !bytes.Equal(recFull.Body.Bytes(), payload) {
		t.Fatalf("full range body did not match 5 MiB payload (got %d bytes)", recFull.Body.Len())
	}
}
