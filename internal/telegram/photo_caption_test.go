package telegram

import (
	"encoding/json"
	"testing"
)

// A photo's caption is its instruction. The update struct used to drop it, so
// a captioned photo behaved exactly like an uncaptioned one and the run that
// now starts on every photo would have had nothing to go on but the default.
func TestPhotoCaptionSurvivesUnmarshal(t *testing.T) {
	var u update
	raw := `{"update_id":1,"message":{"message_id":2,"chat":{"id":3},"from":{"id":3},"caption":"what breed is this dog?","photo":[{"file_id":"abc"}]}}`
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatal(err)
	}
	if u.Message == nil || len(u.Message.Photo) == 0 {
		t.Fatal("photo did not parse")
	}
	if u.Message.Caption != "what breed is this dog?" {
		t.Errorf("caption = %q, want the photo's instruction", u.Message.Caption)
	}
}
