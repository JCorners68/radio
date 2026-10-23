package youtube

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// R2Config configures the Cloudflare R2 REST object storage client.
type R2Config struct {
	AccountID string
	APIToken  string
	Bucket    string
	BaseURL   string // optional override for unit tests
}

// R2Store uploads and checks cached audio tracks in Cloudflare R2 so the
// radio-edge Worker can stream them directly from the Cloudflare edge.
type R2Store struct {
	accountID string
	apiToken  string
	bucket    string
	baseURL   string
	httpCli   *http.Client
}

// NewR2Store returns an R2Store if AccountID, APIToken, and Bucket are non-empty, or nil otherwise.
func NewR2Store(cfg R2Config) *R2Store {
	acc := strings.TrimSpace(cfg.AccountID)
	tok := strings.TrimSpace(cfg.APIToken)
	bkt := strings.TrimSpace(cfg.Bucket)
	if acc == "" || tok == "" || bkt == "" {
		return nil
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	return &R2Store{
		accountID: acc,
		apiToken:  tok,
		bucket:    bkt,
		baseURL:   base,
		httpCli: &http.Client{
			Timeout: 10 * time.Minute,
		},
	}
}

func (r *R2Store) objectURL(key string) string {
	return fmt.Sprintf("%s/accounts/%s/r2/buckets/%s/objects/%s",
		r.baseURL, r.accountID, r.bucket, strings.TrimLeft(key, "/"))
}

// TrackKey returns the canonical R2 object key for a videoID.
func TrackKey(videoID string) string {
	return "tracks/" + videoID + ".m4a"
}

const maxR2SinglePutBytes = 300 << 20 // 300 MiB Cloudflare v4 REST API single-PUT limit

// UploadFile streams a local M4A file to Cloudflare R2 under TrackKey(videoID).
func (r *R2Store) UploadFile(ctx context.Context, videoID, filePath string) error {
	if r == nil {
		return nil
	}
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() <= 0 {
		return fmt.Errorf("refusing to upload empty file for %s", videoID)
	}
	if st.Size() > maxR2SinglePutBytes {
		// Oversized multi-hour mixes stay in local NVMe cache and skip v4 single-PUT
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, r.objectURL(TrackKey(videoID)), f)
	if err != nil {
		return err
	}
	req.ContentLength = st.Size()
	req.Header.Set("Authorization", "Bearer "+r.apiToken)
	req.Header.Set("Content-Type", "audio/mp4")

	resp, err := r.httpCli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("r2 put %s status %d: %s", videoID, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
