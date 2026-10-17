package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestYouTubeID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://www.youtube.com/watch?v=v2AC41dglnM", "v2AC41dglnM"},
		{"https://youtu.be/v2AC41dglnM", "v2AC41dglnM"},
		{"https://music.youtube.com/watch?v=v2AC41dglnM&list=RD", "v2AC41dglnM"},
		{"https://www.youtube.com/embed/v2AC41dglnM", "v2AC41dglnM"},
		{"https://m.youtube.com/shorts/v2AC41dglnM", "v2AC41dglnM"},
		{"https://open.spotify.com/track/1234567890123456789012", ""},
		{"chill synthwave for coding", ""},
		{"v2AC41dglnM", ""}, // bare strings are treated as text seeds, not URLs
	}
	for _, tc := range cases {
		if got := youTubeID(tc.in); got != tc.want {
			t.Errorf("youTubeID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCoreTitleAndNormTokens(t *testing.T) {
	if got := coreTitle("Get Lucky (feat. Pharrell Williams) [Radio Edit]"); got != "Get Lucky" {
		t.Errorf("coreTitle bracketed = %q, want %q", got, "Get Lucky")
	}
	if got := coreTitle("Come Together - 2019 Mix"); got != "Come Together" {
		t.Errorf("coreTitle dash remaster = %q, want %q", got, "Come Together")
	}
	gotTokens := normTokens("Âme & Dixon — Rej!")
	wantTokens := []string{"ame", "and", "dixon", "rej"}
	if !reflect.DeepEqual(gotTokens, wantTokens) {
		t.Errorf("normTokens = %v, want %v", gotTokens, wantTokens)
	}
}

func TestScoreHitAndBestHit(t *testing.T) {
	// 1. Official / VEVO run-together channel match
	acdc := ytHit{ID: "v2AC41dglnM", Title: "AC/DC - Thunderstruck (Official Video)", Channel: "ACDCVEVO", Duration: 292}
	if score, ok := scoreHit(acdc, "Thunderstruck", "AC/DC", 293); !ok || score < 3.5 {
		t.Errorf("expected high score for official AC/DC hit, got score=%v ok=%v", score, ok)
	}

	// 2. Reject unrelated video with partial word overlap ("Anatomy" -> Grey's Anatomy clip)
	greys := ytHit{ID: "bad11111111", Title: "Grey's Anatomy - Best Moments", Channel: "ABC", Duration: 240}
	if _, ok := scoreHit(greys, "Anatomy", "Kerala Dust", 0); ok {
		t.Errorf("expected Grey's Anatomy clip to be rejected for Kerala Dust - Anatomy")
	}

	// 3. Penalize cover/nightcore/karaoke over studio recording
	hits := []ytHit{
		{ID: "cover111111", Title: "Radiohead - Paranoid Android (Acoustic Cover)", Channel: "Some Guy", Duration: 380},
		{ID: "night111111", Title: "Paranoid Android (Nightcore Sped Up)", Channel: "Radiohead fan", Duration: 300},
		{ID: "real1111111", Title: "Paranoid Android", Channel: "Radiohead - Topic", Duration: 387},
	}
	best, _, ok := bestHit(hits, "Paranoid Android", "Radiohead", 387)
	if !ok || best.ID != "real1111111" {
		t.Errorf("bestHit picked %+v (ok=%v), want real1111111", best, ok)
	}

	// 4. Accent folding ("Âme" matches "Ame")
	ame := ytHit{ID: "ame11111111", Title: "Ame - Rej (Original Mix)", Channel: "Innervisions", Duration: 480}
	if _, ok := scoreHit(ame, "Rej", "Âme", 0); !ok {
		t.Errorf("expected accent-folded artist Âme to match Ame")
	}
}

func TestRepairTruncatedArray(t *testing.T) {
	truncated := `[{"title":"One","artist":"A","reason":"good"},{"title":"Two","artist":"B","reason":"also [good]"},{"title":"Three","art`
	repaired := repairTruncatedArray(truncated)
	var got []suggestion
	if err := json.Unmarshal([]byte(repaired), &got); err != nil {
		t.Fatalf("unmarshal repaired array (%q): %v", repaired, err)
	}
	if len(got) != 2 || got[0].Title != "One" || got[1].Title != "Two" {
		t.Errorf("repaired suggestions = %+v, want 2 items (One, Two)", got)
	}
}

func TestExtractJSONHelpers(t *testing.T) {
	fencedArr := "```json\n[{\"title\":\"X\"}]\n```"
	if got := extractJSONArray(fencedArr); got != `[{"title":"X"}]` {
		t.Errorf("extractJSONArray = %q", got)
	}
	proseObj := "Sure! Here is the answer:\n{\"videoId\": \"abc\", \"confidence\": \"high\"}\n"
	if got := extractJSONObject(proseObj); got != `{"videoId": "abc", "confidence": "high"}` {
		t.Errorf("extractJSONObject = %q", got)
	}
}

func TestStoreLifecycle(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
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

	// Stale revision must return errStaleRev
	if _, err := s.Put(0, validState); !errors.Is(err, errStaleRev) {
		t.Errorf("stale Put err = %v, want errStaleRev", err)
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
	injected := s.injectState(page)
	if !bytes.Contains(injected, []byte("window.__STATION__=")) || bytes.Contains(injected, []byte("<!--STATION-->")) {
		t.Errorf("injectState unexpected output: %s", injected)
	}

	// Re-open from disk and check persistence
	s2, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("re-open store: %v", err)
	}
	if r2, _ := s2.Get(); r2 != 2 {
		t.Errorf("re-opened rev = %d, want 2", r2)
	}
	_ = os.RemoveAll(dir)
}
