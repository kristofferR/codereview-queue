package dialect

import (
	"testing"
	"time"
)

func TestCodexReviewCommandsClassify(t *testing.T) {
	classifier := Classifier{CoReviewers: KnownCoReviewers()}
	for _, command := range []string{CodexReviewCommand, "@codex review"} {
		event := classifier.Classify("kristofferR", command, 0, time.Time{}, time.Time{})
		if event.Kind != EvCoCommand || event.For != CodexBotLogin {
			t.Fatalf("command %q classified as %+v", command, event)
		}
	}
}
