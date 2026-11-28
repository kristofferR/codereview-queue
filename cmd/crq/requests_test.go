package main

import (
	"strings"
	"testing"
	"time"

	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

func TestFormatRequestsListsTheBusiestRowsAndSaysWhatWasLeftOut(t *testing.T) {
	now := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	report := ghapi.RequestReport{
		Since: now.Add(-20 * time.Minute), Window: "1h0m0s", Counted: 9, Free: 4, Local: 30,
		Quota: []ghapi.QuotaSeen{{Resource: "core", Limit: 5000, Used: 1200, Reset: now.Add(25 * time.Minute)}},
		Rows: []ghapi.RequestRow{
			{Caller: "autofix@host", Method: "PATCH", Route: "/repos/*/*/git/refs/heads/*", Counted: 7, Outcomes: map[string]int{"200": 5, "422": 2}},
			{Caller: "serve", Method: "GET", Route: "/repos/*/*/git/blobs/*", Counted: 2, Local: 30, Outcomes: map[string]int{"200": 2, "cache": 30}},
		},
	}
	got := formatRequests(report, now, 1)
	for _, want := range []string{
		"core: 1200 of 5000 used, resets in 25m",
		"9 counted against the quota, 4 free (304), 30 answered without a request",
		"counting began 20m0s ago",
		"| 7 | 0 | 0 | autofix@host | PATCH /repos/*/*/git/refs/heads/* | 200×5 422×2 |",
		"The busiest 1 of 2 rows",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "git/blobs") {
		t.Errorf("the row past the limit was printed:\n%s", got)
	}
}
