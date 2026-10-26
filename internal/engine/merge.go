package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/kristofferR/codereview-queue/internal/dialect"
)

// The merge gate's synthetic findings. Each has no thread and only a push can
// clear it, which is why NextAction stops repeating them once the caller holds
// local work, and why none of them ever stops a review from being requested.
const (
	SourceCICheck       = "ci_check"
	SourceMergeConflict = "merge_conflict"
	SourceBranchBehind  = "branch_behind"
)

// IsGateFinding reports whether a finding came from the merge gate rather than
// from a reviewer.
func IsGateFinding(f dialect.Finding) bool {
	switch f.Source {
	case SourceCICheck, SourceMergeConflict, SourceBranchBehind:
		return true
	}
	return false
}

// WithoutGateFindings drops the merge gate's findings.
func WithoutGateFindings(findings []dialect.Finding) []dialect.Finding {
	out := make([]dialect.Finding, 0, len(findings))
	for _, f := range findings {
		if !IsGateFinding(f) {
			out = append(out, f)
		}
	}
	return out
}

// CIState is a CI check's outcome, reduced to what gates a merge.
type CIState string

const (
	CIPass    CIState = "pass"
	CIPending CIState = "pending"
	CIFail    CIState = "fail"
)

func (s CIState) rank() int {
	switch s {
	case CIFail:
		return 2
	case CIPending:
		return 1
	}
	return 0
}

// CISeen is one CI check run or commit status on the observed head. Review
// bots' own checks never reach here: crq waits for those bots through their
// reviews.
type CISeen struct {
	Name  string
	State CIState
	URL   string
}

// MergeSeen is what the merge gate decides from.
type MergeSeen struct {
	// CIUnknown means the head's checks or the base's required checks could
	// not be read, so CI cannot be called green.
	CIUnknown bool
	CI        []CISeen
	// Required names the checks branch protection requires. One that has not
	// reported yet is pending, not absent.
	Required []string
	// State is GitHub's mergeable_state; "" when it was not observed.
	State string
	Base  string
	// ChangesRequested lists people whose latest review requests changes.
	ChangesRequested []string
	// PersonThreads holds the URLs of unresolved review threads a person started.
	// ThreadsUnknown means they could not be read, so nobody can be ruled out.
	PersonThreads  []string
	ThreadsUnknown bool
}

// Gate is the merge gate's verdict. Failures, conflicts and a stale branch are
// work and travel as findings (see GateFindings); Wait and Blocked are what
// remain once every finding is handled.
type Gate struct {
	Failed   []CISeen
	Pending  []string
	Conflict bool
	Behind   bool
	// Wait says why `done` must wait (CI still running, GitHub still computing
	// mergeability). Blocked says why only a person can make it mergeable.
	Wait    string
	Blocked string
	base    string
}

// MergeGate reduces a PR's CI, mergeability and review state to the gate.
//
// Every CI check gates, not only the required ones: a repository whose only
// required check is a description lint would otherwise call a PR with failing
// tests ready. Required checks add the ones that have not reported yet.
func MergeGate(m MergeSeen) Gate {
	g := Gate{base: m.Base}
	worst := map[string]CISeen{}
	for _, c := range m.CI {
		if cur, ok := worst[c.Name]; !ok || c.State.rank() > cur.State.rank() {
			worst[c.Name] = c
		}
	}
	for _, name := range m.Required {
		if _, ok := worst[name]; !ok {
			worst[name] = CISeen{Name: name, State: CIPending}
		}
	}
	names := make([]string, 0, len(worst))
	for name := range worst {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		switch c := worst[name]; c.State {
		case CIFail:
			g.Failed = append(g.Failed, c)
		case CIPending:
			g.Pending = append(g.Pending, name)
		}
	}

	state := strings.ToLower(m.State)
	g.Conflict = state == "dirty"
	g.Behind = state == "behind"

	switch {
	case m.CIUnknown:
		g.Wait = "could not read CI for this head"
	case len(g.Pending) > 0:
		g.Wait = "waiting for CI: " + strings.Join(g.Pending, ", ")
	case state == "unknown":
		g.Wait = "waiting for GitHub to compute mergeability"
	case m.ThreadsUnknown:
		g.Wait = "could not read review threads, so a person's open thread cannot be ruled out"
	}

	switch {
	case len(m.ChangesRequested) > 0:
		g.Blocked = "changes requested by " + strings.Join(m.ChangesRequested, ", ")
	case len(m.PersonThreads) > 0:
		g.Blocked = fmt.Sprintf("%d unresolved review thread(s) from people: %s",
			len(m.PersonThreads), strings.Join(m.PersonThreads, " "))
	case state == "draft":
		g.Blocked = "the pull request is a draft"
	case state == "blocked" && g.Wait == "":
		// Nothing is still running, so GitHub is enforcing something crq cannot
		// clear: a required approval, a rule such as signed commits, or a
		// required check that failed. Failures are findings first; once one is
		// dismissed, this keeps it from reading as done.
		g.Blocked = "branch protection still blocks merging (a required check, a required approval, or another rule)"
	}
	return g
}

// GateFindings turns the gate's work into head-bound findings, so the fix
// flow, autofix and `crq dismiss` handle a red check like any other finding.
func GateFindings(g Gate, head string) []dialect.Finding {
	var out []dialect.Finding
	add := func(source, key, title, body, url string) {
		sum := sha256.Sum256([]byte(source + "|" + key + "|" + head))
		out = append(out, dialect.Finding{
			ID:       hex.EncodeToString(sum[:]),
			Bot:      "ci",
			Severity: "major",
			Title:    title,
			Body:     body,
			Commit:   head,
			URL:      url,
			Source:   source,
		})
	}
	for _, c := range g.Failed {
		add(SourceCICheck, c.Name, "CI check failed: "+c.Name,
			"Fix the failure. If it is unrelated to this PR (a flaky test or an infrastructure outage), "+
				"re-run the check instead, or set it aside with `crq dismiss` and a reason.", c.URL)
	}
	base := g.base
	if base == "" {
		base = "the base branch"
	}
	if g.Conflict {
		add(SourceMergeConflict, base, "Merge conflict with "+base,
			"Merge or rebase the latest "+base+" into this branch, resolve the conflicts, and run the checks.", "")
	}
	if g.Behind {
		add(SourceBranchBehind, base, "Branch is behind "+base,
			"This repository requires an up-to-date branch: merge or rebase the latest "+base+" and run the checks.", "")
	}
	return out
}
