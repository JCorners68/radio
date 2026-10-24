package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	mrand "math/rand"
	"strings"
	"time"
)

type Playlist struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	TrackIDs []string `json:"trackIds"`
}

type Track struct {
	ID        string `json:"id"`
	VideoID   string `json:"videoId"`
	Title     string `json:"title"`
	Thumb     string `json:"thumb"`
	PlayCount int    `json:"playCount"`
	Favorite  bool   `json:"favorite"`
	Autopilot bool   `json:"autopilot,omitempty"`
	AddedAt   int64  `json:"addedAt,omitempty"`
}

type StationState struct {
	Playlists        []Playlist       `json:"playlists"`
	Tracks           map[string]Track `json:"tracks"`
	ActivePlaylistID string           `json:"activePlaylistId"`
	CurrentTrackID   string           `json:"currentTrackId"`
	Shuffle          bool             `json:"shuffle"`
	Repeat           bool             `json:"repeat"`
	Autopilot        bool             `json:"autopilot"`
	Volume           int              `json:"volume"`
	ShowVideo        *bool            `json:"showVideo,omitempty"`
	VideoMaximized   bool             `json:"videoMaximized"`
}

type NewTrackInput struct {
	VideoID   string
	Title     string
	Thumb     string
	Autopilot bool
}

func GenerateUID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func DefaultStationState() StationState {
	plID := GenerateUID()
	showVid := true
	return StationState{
		Playlists: []Playlist{
			{ID: plID, Name: "Favorites Mix", TrackIDs: []string{}},
		},
		Tracks:            make(map[string]Track),
		ActivePlaylistID: plID,
		CurrentTrackID:   "",
		Shuffle:          true,
		Repeat:           true,
		Autopilot:        false,
		Volume:           70,
		ShowVideo:        &showVid,
		VideoMaximized:   false,
	}
}

func (st *StationState) normalize() {
	if st.Tracks == nil {
		st.Tracks = make(map[string]Track)
	}
	if len(st.Playlists) == 0 {
		plID := GenerateUID()
		st.Playlists = []Playlist{{ID: plID, Name: "Favorites Mix", TrackIDs: []string{}}}
		st.ActivePlaylistID = plID
	}
	for i := range st.Playlists {
		if st.Playlists[i].TrackIDs == nil {
			st.Playlists[i].TrackIDs = []string{}
		}
	}
	found := false
	for _, p := range st.Playlists {
		if p.ID == st.ActivePlaylistID {
			found = true
			break
		}
	}
	if !found {
		st.ActivePlaylistID = st.Playlists[0].ID
	}
	if st.Volume <= 0 || st.Volume > 100 {
		st.Volume = 70
	}
	if st.ShowVideo == nil {
		v := true
		st.ShowVideo = &v
	}
	if pl := st.ActivePlaylist(); pl != nil && len(pl.TrackIDs) > 0 {
		if st.CurrentTrackID == "" {
			st.CurrentTrackID = pl.TrackIDs[0]
		} else if _, ok := st.Tracks[st.CurrentTrackID]; !ok {
			st.CurrentTrackID = pl.TrackIDs[0]
		}
	}
}

func (st *StationState) ActivePlaylist() *Playlist {
	for i := range st.Playlists {
		if st.Playlists[i].ID == st.ActivePlaylistID {
			return &st.Playlists[i]
		}
	}
	if len(st.Playlists) > 0 {
		return &st.Playlists[0]
	}
	return nil
}

func (st *StationState) PlaylistByID(id string) *Playlist {
	for i := range st.Playlists {
		if st.Playlists[i].ID == id {
			return &st.Playlists[i]
		}
	}
	return nil
}

func (st *StationState) ActiveTracks() []Track {
	pl := st.ActivePlaylist()
	if pl == nil {
		return nil
	}
	out := make([]Track, 0, len(pl.TrackIDs))
	for _, id := range pl.TrackIDs {
		if t, ok := st.Tracks[id]; ok {
			out = append(out, t)
		}
	}
	return out
}

func (st *StationState) CurrentTrack() *Track {
	if st.CurrentTrackID == "" {
		return nil
	}
	if t, ok := st.Tracks[st.CurrentTrackID]; ok {
		return &t
	}
	return nil
}

func (st *StationState) PickNextTrackID(prev bool) (string, bool) {
	pl := st.ActivePlaylist()
	if pl == nil || len(pl.TrackIDs) == 0 {
		return "", false
	}
	ids := pl.TrackIDs
	if prev {
		idx := indexOf(ids, st.CurrentTrackID)
		prevIdx := idx - 1
		if prevIdx < 0 {
			prevIdx = len(ids) - 1
		}
		return ids[prevIdx], false
	}

	if st.Shuffle {
		return st.weightedPick(ids, st.CurrentTrackID), false
	}
	idx := indexOf(ids, st.CurrentTrackID)
	nextIdx := idx + 1
	if nextIdx >= len(ids) {
		if st.Autopilot {
			return "", true // signals caller that playlist reached end with autopilot enabled
		}
		nextIdx = 0
	}
	return ids[nextIdx], false
}

func (st *StationState) weightedPick(ids []string, excludeID string) string {
	pool := ids
	if excludeID != "" && len(ids) > 1 {
		filtered := make([]string, 0, len(ids)-1)
		for _, id := range ids {
			if id != excludeID {
				filtered = append(filtered, id)
			}
		}
		if len(filtered) > 0 {
			pool = filtered
		}
	}
	weights := make([]float64, len(pool))
	total := 0.0
	for i, id := range pool {
		t := st.Tracks[id]
		w := 1.0 + math.Log(float64(t.PlayCount)+1.0)
		if t.Favorite {
			w *= 2.5
		}
		weights[i] = w
		total += w
	}
	r := mrand.Float64() * total
	for i := range pool {
		r -= weights[i]
		if r <= 0 {
			return pool[i]
		}
	}
	return pool[len(pool)-1]
}

func indexOf(slice []string, item string) int {
	for i, v := range slice {
		if v == item {
			return i
		}
	}
	return -1
}

// ReadState returns the current parsed StationState without holding the lock after return.
func (s *FileStore) ReadState() (int64, StationState, error) {
	rev, raw := s.Get()
	if len(raw) == 0 || string(raw) == "null" {
		st := DefaultStationState()
		return rev, st, nil
	}
	var st StationState
	if err := json.Unmarshal(raw, &st); err != nil {
		return rev, StationState{}, err
	}
	st.normalize()
	return rev, st, nil
}

// Mutate atomically reads, transforms, and persists the StationState under FileStore's mutex.
func (s *FileStore) Mutate(fn func(*StationState) error) (int64, StationState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var st StationState
	if len(s.cur.State) == 0 || string(s.cur.State) == "null" {
		st = DefaultStationState()
	} else if err := json.Unmarshal(s.cur.State, &st); err != nil {
		return s.cur.Rev, StationState{}, fmt.Errorf("unmarshal station state: %w", err)
	}
	st.normalize()

	if err := fn(&st); err != nil {
		return s.cur.Rev, st, err
	}
	st.normalize()

	raw, err := json.Marshal(st)
	if err != nil {
		return s.cur.Rev, st, err
	}
	s.dailyBackup()

	next := stationFile{
		Rev:     s.cur.Rev + 1,
		SavedAt: time.Now().UTC(),
		State:   raw,
	}
	b, err := json.MarshalIndent(next, "", " ")
	if err != nil {
		return s.cur.Rev, st, err
	}
	if err := writeAtomic(s.path, b); err != nil {
		return s.cur.Rev, st, err
	}
	s.cur = next
	return next.Rev, st, nil
}

// AddTracksToPlaylist adds any tracks whose VideoID isn't already in the library or playlist.
func AddTracksToPlaylist(st *StationState, playlistID string, items []NewTrackInput) (int, *Track) {
	pl := st.PlaylistByID(playlistID)
	if pl == nil {
		pl = st.ActivePlaylist()
	}
	if pl == nil {
		return 0, nil
	}

	byVideo := make(map[string]string, len(st.Tracks))
	for id, t := range st.Tracks {
		byVideo[t.VideoID] = id
	}
	inPl := make(map[string]bool, len(pl.TrackIDs))
	for _, id := range pl.TrackIDs {
		inPl[id] = true
	}

	added := 0
	var firstTrack *Track
	nowMs := time.Now().UnixMilli()
	for _, it := range items {
		vid := strings.TrimSpace(it.VideoID)
		if vid == "" {
			continue
		}
		tid, exists := byVideo[vid]
		if !exists {
			tid = GenerateUID()
			title := strings.TrimSpace(it.Title)
			if title == "" {
				title = vid
			}
			thumb := strings.TrimSpace(it.Thumb)
			if thumb == "" {
				thumb = "https://i.ytimg.com/vi/" + vid + "/hqdefault.jpg"
			}
			tr := Track{
				ID:        tid,
				VideoID:   vid,
				Title:     title,
				Thumb:     thumb,
				PlayCount: 0,
				Favorite:  false,
				Autopilot: it.Autopilot,
				AddedAt:   nowMs,
			}
			st.Tracks[tid] = tr
			byVideo[vid] = tid
		}
		if !inPl[tid] {
			pl.TrackIDs = append(pl.TrackIDs, tid)
			inPl[tid] = true
			added++
		}
		if firstTrack == nil {
			t := st.Tracks[tid]
			firstTrack = &t
		}
	}
	if added == 0 && len(items) == 0 {
		return 0, nil
	}
	return added, firstTrack
}

var ErrNotFound = errors.New("not found")
