// Package store persists station playlists and tracks with optimistic concurrency.
//
// data/station.json holds {"rev": N, "savedAt": ..., "state": {...}}. Every write is
// atomic (temp file + fsync + rename), so a crash mid-save can never leave a torn file.
// Writers must send the rev they last read; a stale rev is refused with ErrStaleRev so two
// open tabs can't silently overwrite each other. The first save of each day snapshots the
// previous file into data/backups/ (30 kept).
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	keepBackups = 30
	// BodyLimit caps PUT bodies; a few thousand tracks is well under this.
	BodyLimit = 8 << 20
)

var ErrStaleRev = errors.New("stale revision")

// Store defines the persistence contract for station state.
type Store interface {
	Get() (int64, json.RawMessage)
	Put(baseRev int64, state json.RawMessage) (int64, error)
	InjectState(page []byte) []byte
	Path() string
}

type stationFile struct {
	Rev     int64           `json:"rev"`
	SavedAt time.Time       `json:"savedAt"`
	State   json.RawMessage `json:"state"`
}

// FileStore serializes all reads and writes of data/station.json.
type FileStore struct {
	mu   sync.Mutex
	dir  string
	path string
	cur  stationFile
}

func OpenFileStore(dataDir string) (*FileStore, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "backups"), 0o755); err != nil {
		return nil, err
	}
	s := &FileStore{dir: dataDir, path: filepath.Join(dataDir, "station.json")}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.cur = stationFile{State: json.RawMessage("null")}
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.cur); err != nil {
			return nil, fmt.Errorf("parse %s: %w", s.path, err)
		}
	}
	return s, nil
}

func (s *FileStore) Path() string {
	return s.path
}

// Get returns the current revision and state (state is JSON null before the first save).
func (s *FileStore) Get() (int64, json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur.Rev, s.cur.State
}

// Put replaces the state if baseRev is current, returning the new revision.
func (s *FileStore) Put(baseRev int64, state json.RawMessage) (int64, error) {
	if err := validateState(state); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if baseRev != s.cur.Rev {
		return s.cur.Rev, ErrStaleRev
	}
	s.dailyBackup()

	next := stationFile{Rev: s.cur.Rev + 1, SavedAt: time.Now().UTC(), State: state}
	b, err := json.MarshalIndent(next, "", " ")
	if err != nil {
		return 0, err
	}
	if err := writeAtomic(s.path, b); err != nil {
		return 0, err
	}
	s.cur = next
	return next.Rev, nil
}

// validateState rejects anything that isn't recognisably a station, so a buggy page can't
// replace the library with garbage.
func validateState(state json.RawMessage) error {
	var shape struct {
		Playlists []struct {
			ID       string   `json:"id"`
			TrackIDs []string `json:"trackIds"`
		} `json:"playlists"`
		Tracks map[string]json.RawMessage `json:"tracks"`
	}
	if err := json.Unmarshal(state, &shape); err != nil {
		return fmt.Errorf("state is not a station object: %w", err)
	}
	if len(shape.Playlists) == 0 || shape.Tracks == nil {
		return errors.New("state needs at least one playlist and a tracks map")
	}
	return nil
}

// dailyBackup copies the file as it stood before today's first write into
// backups/station-YYYY-MM-DD.json, then prunes to the newest keepBackups.
func (s *FileStore) dailyBackup() {
	if s.cur.Rev == 0 {
		return
	}
	dst := filepath.Join(s.dir, "backups", "station-"+time.Now().Format("2006-01-02")+".json")
	if _, err := os.Stat(dst); err == nil {
		return
	}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = writeAtomic(dst, b)
	}
	entries, _ := filepath.Glob(filepath.Join(s.dir, "backups", "station-*.json"))
	sort.Strings(entries)
	for len(entries) > keepBackups {
		_ = os.Remove(entries[0])
		entries = entries[1:]
	}
}

func writeAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// InjectState renders the current state into a page at its <!--STATION--> marker, so
// pages start with real data synchronously instead of waiting on a fetch. json.Marshal
// HTML-escapes <, > and &, so the payload can't close the script tag.
func (s *FileStore) InjectState(page []byte) []byte {
	rev, state := s.Get()
	payload, _ := json.Marshal(map[string]any{"rev": rev, "state": state})
	var buf bytes.Buffer
	buf.WriteString("<script>window.__STATION__=")
	buf.Write(payload)
	buf.WriteString(";</script>")
	return bytes.Replace(page, []byte("<!--STATION-->"), buf.Bytes(), 1)
}
