package crq

import (
	"testing"

	"github.com/kristofferR/codereview-queue/internal/dialect"
)

// Only a finding that intrinsically cannot have a thread may be dismissed.
// "No thread ID" is not the same question: when the GraphQL thread query fails,
// Feedback falls back to REST, which does not return thread IDs at all — so an
// inline comment with an open thread arrives looking threadless.
func TestOnlyIntrinsicallyThreadlessFindingsAreDismissible(t *testing.T) {
	for _, tc := range []struct {
		source string
		want   bool
	}{
		{"review_body", true},
		{"review_prompt", true},
		{"review_skipped", true},
		{"issue_comment", true},
		{"review_comment", false}, // REST fallback: the thread exists, unread
		{"review_thread", false},
		{"review_reply", false},
	} {
		t.Run(tc.source, func(t *testing.T) {
			err := dismissible(dialect.Finding{ID: "x", Source: tc.source})
			if (err == nil) != tc.want {
				t.Errorf("dismissible(%s) err = %v, want dismissible=%v", tc.source, err, tc.want)
			}
		})
	}

	// A thread ID settles it whatever the source says.
	if err := dismissible(dialect.Finding{ID: "x", Source: "review_body", ThreadID: "PRRT_1"}); err == nil {
		t.Error("a finding with a thread must never be dismissible")
	}
}
