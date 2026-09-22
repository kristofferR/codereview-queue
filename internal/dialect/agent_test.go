package dialect

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestClassifyAgentFailure(t *testing.T) {
	now := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	log := []byte(fmt.Sprintf(`{"type":"result","error": "rate_limit","resetsAt":%d}`, reset.Unix()))
	got := ClassifyAgentFailure(log, now)
	if !got.Unavailable {
		t.Fatal("machine-readable provider outage was not classified")
	}
	if got.RetryAt.Unix() != reset.Unix() {
		t.Fatalf("retry = %s, want timestamp from the response", got.RetryAt)
	}
	if ordinary := ClassifyAgentFailure([]byte("tests failed after editing"), reset); ordinary.Unavailable {
		t.Fatal("an ordinary bad fix was refunded as a provider outage")
	}
	repositoryOutput := []byte(`The review says "service unavailable".
The test printed: rate limit exceeded.
source := ` + "`\"type\":\"overloaded_error\"`" + `
`)
	if ordinary := ClassifyAgentFailure(repositoryOutput, reset); ordinary.Unavailable {
		t.Fatal("repository-controlled text was treated as a provider outage")
	}
	repositoryJSON := []byte(
		`{"type":"assistant","message":{"content":[{"type":"tool_result","content":{"error":"rate_limit","resetsAt":4102444800}}]}}`,
	)
	if ordinary := ClassifyAgentFailure(repositoryJSON, reset); ordinary.Unavailable {
		t.Fatal("repository-controlled JSON was treated as a provider outage")
	}
	apiError := []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	if outage := ClassifyAgentFailure(apiError, reset); !outage.Unavailable {
		t.Fatal("the provider's top-level error envelope was not classified")
	}
}

func TestCodexUsageReset(t *testing.T) {
	location, err := time.LoadLocation("Europe/Oslo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 22, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		stamp string
		want  time.Time
	}{
		{"Sep 27th, 2026 5:21 PM", time.Date(2026, 9, 27, 15, 22, 0, 0, time.UTC)},
		{"5:21 PM", time.Date(2026, 9, 22, 15, 22, 0, 0, time.UTC)},
		{"Oct 1st, 2026 12:00 AM", time.Date(2026, 9, 30, 22, 1, 0, 0, time.UTC)},
		{"Oct 2nd, 2026 12:00 PM", time.Date(2026, 10, 2, 10, 1, 0, 0, time.UTC)},
		{"Oct 23rd, 2026 5:21 PM", time.Date(2026, 10, 23, 15, 22, 0, 0, time.UTC)},
		{"Oct 27th, 2026 5:21 PM", time.Date(2026, 10, 27, 16, 22, 0, 0, time.UTC)},
	} {
		t.Run(tc.stamp, func(t *testing.T) {
			got, ok := codexUsageReset("You’ve hit your usage limit. Try again at "+tc.stamp+".", now, location)
			if !ok || got != tc.want.Unix() {
				t.Fatalf("reset = %s, %v; want %s", time.Unix(got, 0), ok, tc.want)
			}
		})
	}
}

func TestCodexUsageLimitEnvelopeAndFallback(t *testing.T) {
	now := time.Date(2026, 9, 22, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		log  string
		wait time.Duration
	}{
		{"no reset", `{"type":"error","message":"You’ve hit your usage limit. Try again later."}`, 15 * time.Minute},
		{"invalid reset", `{"type":"turn.failed","error":{"message":"You've hit your usage limit. Try again at invalid."}}`, 15 * time.Minute},
		{"past reset", `{"type":"error","message":"You’ve hit your usage limit. Try again at Sep 1st, 2026 5:21 PM."}`, 5 * time.Minute},
		{"distant reset", `{"type":"error","message":"You’ve hit your usage limit. Try again at Sep 1st, 2099 5:21 PM."}`, 15 * time.Minute},
		{"model limit", `{"type":"error","message":"You've hit your usage limit for gpt-6-sol. Switch to another model now, or try again at Sep 27th, 2026 5:21 PM."}`, 15 * time.Minute},
		{"model limit failed turn", `{"type":"turn.failed","error":{"message":"You've hit your usage limit for gpt-6-sol. Switch to another model now, or try again at Sep 27th, 2026 5:21 PM."}}`, 15 * time.Minute},
		{"ordinary failure", `{"type":"turn.failed","error":{"message":"tests failed"}}`, 0},
		{"similar unrelated error", `{"type":"error","message":"You've hit your usage limit forecast."}`, 0},
		{"quoted error", `{"type":"error","message":"Test printed: You’ve hit your usage limit."}`, 0},
		{"assistant message", `{"type":"item.completed","item":{"type":"agent_message","text":"You’ve hit your usage limit."}}`, 0},
		{"tool output", `{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"{\"type\":\"error\",\"message\":\"You’ve hit your usage limit.\"}"}}`, 0},
		{"result text", `{"type":"result","result":"You’ve hit your usage limit."}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !json.Valid([]byte(tc.log)) {
				t.Fatal("invalid test event")
			}
			got := ClassifyAgentFailure([]byte(tc.log), now)
			if got.Unavailable != (tc.wait != 0) || tc.wait != 0 && !got.RetryAt.Equal(now.Add(tc.wait)) {
				t.Fatalf("failure = %+v, want wait %s", got, tc.wait)
			}
		})
	}
}
