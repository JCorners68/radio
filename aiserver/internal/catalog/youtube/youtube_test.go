package youtube

import (
	"reflect"
	"sync"
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
		if got := YouTubeID(tc.in); got != tc.want {
			t.Errorf("YouTubeID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCoreTitleAndNormTokens(t *testing.T) {
	if got := CoreTitle("Get Lucky (feat. Pharrell Williams) [Radio Edit]"); got != "Get Lucky" {
		t.Errorf("CoreTitle bracketed = %q, want %q", got, "Get Lucky")
	}
	if got := CoreTitle("Come Together - 2019 Mix"); got != "Come Together" {
		t.Errorf("CoreTitle dash remaster = %q, want %q", got, "Come Together")
	}
	gotTokens := NormTokens("Âme & Dixon — Rej!")
	wantTokens := []string{"ame", "and", "dixon", "rej"}
	if !reflect.DeepEqual(gotTokens, wantTokens) {
		t.Errorf("NormTokens = %v, want %v", gotTokens, wantTokens)
	}
}

func TestNormTokensConcurrency(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = NormTokens("Âme & Dixon — Röyksopp / Beyoncé (feat. Jay-Z)")
			}
		}()
	}
	wg.Wait()
}

func TestScoreHitAndBestHit(t *testing.T) {
	// 1. Official / VEVO run-together channel match
	acdc := Hit{ID: "v2AC41dglnM", Title: "AC/DC - Thunderstruck (Official Video)", Channel: "ACDCVEVO", Duration: 292}
	if score, ok := ScoreHit(acdc, "Thunderstruck", "AC/DC", 293); !ok || score < 3.5 {
		t.Errorf("expected high score for official AC/DC hit, got score=%v ok=%v", score, ok)
	}

	// 2. Reject unrelated video with partial word overlap ("Anatomy" -> Grey's Anatomy clip)
	greys := Hit{ID: "bad11111111", Title: "Grey's Anatomy - Best Moments", Channel: "ABC", Duration: 240}
	if _, ok := ScoreHit(greys, "Anatomy", "Kerala Dust", 0); ok {
		t.Errorf("expected Grey's Anatomy clip to be rejected for Kerala Dust - Anatomy")
	}

	// 3. Penalize cover/nightcore/karaoke over studio recording
	hits := []Hit{
		{ID: "cover111111", Title: "Radiohead - Paranoid Android (Acoustic Cover)", Channel: "Some Guy", Duration: 380},
		{ID: "night111111", Title: "Paranoid Android (Nightcore Sped Up)", Channel: "Radiohead fan", Duration: 300},
		{ID: "real1111111", Title: "Paranoid Android", Channel: "Radiohead - Topic", Duration: 387},
	}
	best, _, ok := BestHit(hits, "Paranoid Android", "Radiohead", 387)
	if !ok || best.ID != "real1111111" {
		t.Errorf("BestHit picked %+v (ok=%v), want real1111111", best, ok)
	}

	// 4. Accent folding ("Âme" matches "Ame")
	ame := Hit{ID: "ame11111111", Title: "Ame - Rej (Original Mix)", Channel: "Innervisions", Duration: 480}
	if _, ok := ScoreHit(ame, "Rej", "Âme", 0); !ok {
		t.Errorf("expected accent-folded artist Âme to match Ame")
	}
}
