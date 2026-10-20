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

func TestMultiStoreIsolation(t *testing.T) {
	dir := t.TempDir()
	owner, err := OpenFileStore(dir)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	_, _, err = owner.Mutate(func(st *StationState) error {
		st.Playlists = []Playlist{{ID: "pl1", Name: "JC Mix", TrackIDs: []string{}}}
		st.ActivePlaylistID = "pl1"
		AddTracksToPlaylist(st, "pl1", []NewTrackInput{
			{VideoID: "dQw4w9WgXcQ", Title: "Owner Song"},
		})
		return nil
	})
	if err != nil {
		t.Fatalf("seed owner: %v", err)
	}

	ms := NewMultiStore(owner)

	// Owner gets primary store
	fsOwner, err := ms.ForUser("jonathan.corners@gmail.com", true)
	if err != nil || fsOwner != owner {
		t.Fatalf("expected primary owner store")
	}

	// Family member gets isolated store pre-seeded with owner's library
	fsRiki, err := ms.ForUser("riki.corners@gmail.com", false)
	if err != nil || fsRiki == owner {
		t.Fatalf("expected isolated store for riki: %v", err)
	}
	_, rikiState, err := fsRiki.ReadState()
	if err != nil {
		t.Fatalf("read riki state: %v", err)
	}
	if len(rikiState.Playlists) != 1 || rikiState.Playlists[0].Name != "JC Mix" || len(rikiState.Tracks) != 1 {
		t.Fatalf("expected riki store pre-seeded with JC Mix, got %+v", rikiState)
	}

	// Mutating Riki's store does not mutate owner's store
	_, _, err = fsRiki.Mutate(func(st *StationState) error {
		st.Playlists[0].Name = "Riki Mix"
		st.Playlists[0].TrackIDs = []string{}
		return nil
	})
	if err != nil {
		t.Fatalf("mutate riki: %v", err)
	}

	_, ownerAfter, _ := owner.ReadState()
	if ownerAfter.Playlists[0].Name != "JC Mix" || len(ownerAfter.Playlists[0].TrackIDs) != 1 {
		t.Fatalf("owner store was mutated by non-owner! %+v", ownerAfter)
	}
}

