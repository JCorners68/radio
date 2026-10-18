package matcher

import "testing"

func TestExtractJSONObject(t *testing.T) {
	proseObj := "Sure! Here is the answer:\n{\"videoId\": \"abc\", \"confidence\": \"high\"}\n"
	if got := ExtractJSONObject(proseObj); got != `{"videoId": "abc", "confidence": "high"}` {
		t.Errorf("ExtractJSONObject = %q", got)
	}
}
