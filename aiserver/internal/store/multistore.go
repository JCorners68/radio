package store

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var safeEmailChars = regexp.MustCompile(`[^a-z0-9._@-]+`)

// MultiStore manages the primary owner FileStore alongside isolated per-user FileStores
// under <dataDir>/users/<email>/station.json, pre-seeded from the owner's library on first login.
type MultiStore struct {
	dataDir string
	owner   *FileStore

	mu    sync.Mutex
	users map[string]*FileStore
}

func NewMultiStore(owner *FileStore) *MultiStore {
	return &MultiStore{
		dataDir: owner.dir,
		owner:   owner,
		users:   make(map[string]*FileStore),
	}
}

func (m *MultiStore) Owner() *FileStore {
	return m.owner
}

// ForUser returns the primary FileStore for owners (or empty/local emails) and an isolated,
// pre-seeded FileStore for family/guest emails.
func (m *MultiStore) ForUser(email string, isOwner bool) (*FileStore, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if isOwner || email == "" {
		return m.owner, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if fs, ok := m.users[email]; ok {
		return fs, nil
	}

	safeDir := safeEmailChars.ReplaceAllString(email, "_")
	userDir := filepath.Join(m.dataDir, "users", safeDir)
	fs, err := OpenFileStore(userDir)
	if err != nil {
		return nil, err
	}

	// Pre-seed with a copy of the owner's playlists/tracks on first creation (rev == 0).
	rev, raw := fs.Get()
	if rev == 0 || len(raw) == 0 || string(raw) == "null" {
		_, ownerState, err := m.owner.ReadState()
		if err == nil && len(ownerState.Playlists) > 0 {
			ownerState.CurrentTrackID = ""
			if seeded, err := json.Marshal(ownerState); err == nil {
				_, _ = fs.Put(0, seeded)
			}
		}
	}

	m.users[email] = fs
	return fs, nil
}
