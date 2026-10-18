package dj

import (
	"encoding/json"
	"testing"
)

func TestRepairTruncatedArray(t *testing.T) {
	truncated := `[{"title":"One","artist":"A","reason":"good"},{"title":"Two","artist":"B","reason":"also [good]"},{"title":"Three","art`
	repaired := RepairTruncatedArray(truncated)
	var got []Suggestion
	if err := json.Unmarshal([]byte(repaired), &got); err != nil {
		t.Fatalf("unmarshal repaired array (%q): %v", repaired, err)
	}
	if len(got) != 2 || got[0].Title != "One" || got[1].Title != "Two" {
		t.Errorf("repaired suggestions = %+v, want 2 items (One, Two)", got)
	}
}

func TestExtractJSONArray(t *testing.T) {
	fencedArr := "```json\n[{\"title\":\"X\"}]\n```"
	if got := ExtractJSONArray(fencedArr); got != `[{"title":"X"}]` {
		t.Errorf("ExtractJSONArray = %q", got)
	}
}
