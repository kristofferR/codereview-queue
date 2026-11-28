package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// formatRequests renders the gateway's request accounting as the section
// `crq status` appends: the allowances as GitHub last reported them, then the
// busiest endpoints. limit bounds the rows; the JSON from `crq requests` has
// them all.
func formatRequests(report ghapi.RequestReport, now time.Time, limit int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n## GitHub requests through crq serve (last %s)\n\n", report.Window)
	for _, quota := range report.Quota {
		reset := "reset passed"
		if until := quota.Reset.Sub(now); until > 0 {
			reset = "resets in " + until.Round(time.Minute).String()
		}
		fmt.Fprintf(&b, "- %s: %d of %d used, %s\n", quota.Resource, quota.Used, quota.Limit, reset)
	}
	fmt.Fprintf(&b, "- %d counted against the quota, %d free (304), %d answered without a request\n",
		report.Counted, report.Free, report.Local)
	if since := now.Sub(report.Since); !report.Since.IsZero() && since < time.Hour {
		fmt.Fprintf(&b, "- counting began %s ago, when the server started\n", since.Round(time.Minute))
	}
	if len(report.Rows) == 0 {
		return b.String()
	}
	b.WriteString("\n| counted | free | local | caller | request | outcomes |\n|---:|---:|---:|---|---|---|\n")
	for i, row := range report.Rows {
		if i == limit {
			fmt.Fprintf(&b, "\nThe busiest %d of %d rows; `crq requests` prints all of them as JSON.\n", limit, len(report.Rows))
			break
		}
		fmt.Fprintf(&b, "| %d | %d | %d | %s | %s %s | %s |\n",
			row.Counted, row.Free, row.Local, row.Caller, row.Method, row.Route, formatOutcomes(row.Outcomes))
	}
	return b.String()
}

func formatOutcomes(outcomes map[string]int) string {
	names := make([]string, 0, len(outcomes))
	for name := range outcomes {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s×%d", name, outcomes[name]))
	}
	return strings.Join(parts, " ")
}
