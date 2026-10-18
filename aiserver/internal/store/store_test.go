package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	rev, st := s.Get()
	if rev != 0 || string(st) != "null" {
		t.Fatalf("initial store = rev %d state %s, want 0 null", rev, st)
	}

	// Reject invalid state shape
	if _, err := s.Put(0, json.RawMessage(`{"playlists":[],"tracks":{}}`)); err == nil {
		t.Errorf("expected empty playlists to fail validation")
	}

	validState := json.RawMessage(`{"playlists":[{"id":"p1","name":"Main","trackIds":[]}],"tracks":{}}`)
	rev, err = s.Put(0, validState)
	if err != nil || rev != 1 {
		t.Fatalf("first Put: rev=%d err=%v, want rev=1", rev, err)
	}

	// Stale revision must return ErrStaleRev
	if _, err := s.Put(0, validState); !errors.Is(err, ErrStaleRev) {
		t.Errorf("stale Put err = %v, want ErrStaleRev", err)
	}

	// Second Put creates a daily backup of rev 1
	rev, err = s.Put(1, validState)
	if err != nil || rev != 2 {
		t.Fatalf("second Put: rev=%d err=%v, want rev=2", rev, err)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "backups", "station-*.json"))
	if len(backups) != 1 {
		t.Errorf("expected 1 daily backup file, found %d", len(backups))
	}

	// HTML injection replaces <!--STATION--> and escapes </script>
	page := []byte("<html><body><!--STATION--></body></html>")
	injected := s.InjectState(page)
	if !bytes.Contains(injected, []byte("window.__STATION__=")) || bytes.Contains(injected, []byte("<!--STATION-->")) {
		t.Errorf("InjectState unexpected output: %s", injected)
	}

	// Re-open from disk and check persistence
	s2, err := OpenFileStore(dir)
	if err != nil {
		t.Fatalf("re-open store: %v", err)
	}
	if r2, _ := s2.Get(); r2 != 2 {
		t.Errorf("re-opened rev = %d, want 2", r2)
	}
	_ = os.RemoveAll(dir)
}
