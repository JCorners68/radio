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
// under <dataDir>/users/<email>/station.json, initialized with a clean empty default library on first login.
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

// ForUser returns the primary FileStore for owners (or empty/local emails) and an isolated
// FileStore initialized with DefaultStationState() for family/guest emails.
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

	// Initialize new non-owner user with a clean empty default state (do not copy owner playlists).
	rev, raw := fs.Get()
	if rev == 0 || len(raw) == 0 || string(raw) == "null" {
		def := DefaultStationState()
		if seeded, err := json.Marshal(def); err == nil {
			_, _ = fs.Put(0, seeded)
		}
	}

	m.users[email] = fs
	return fs, nil
}
