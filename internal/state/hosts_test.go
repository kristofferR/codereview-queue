package state

import (
	"testing"
	"time"
)

// Every crq service on a host writes the SAME record and reports only its own
// role, which is why roles merge. The merge is also what made a stopped service
// immortal: the survivor refreshed the record's single timestamp on every pass,
// so the role it carried forward never aged out and the dashboard reported a
// dead daemon running for ever.
func TestStoppedHostRoleExpiresWhileTheOtherKeepsReporting(t *testing.T) {
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	st := New()

	st.SetHostReport(HostReport{Host: "mac", Roles: []string{"autoreview"}}, base)
	st.SetHostReport(HostReport{Host: "mac", Roles: []string{"autofix"}}, base)
	if got := st.HostReports["mac"].Roles; len(got) != 2 {
		t.Fatalf("roles = %v, want both services merged", got)
	}

	// autofix stops here. autoreview keeps reporting, well inside the TTL.
	for at := base.Add(10 * time.Minute); !at.After(base.Add(25 * time.Minute)); at = at.Add(10 * time.Minute) {
		st.SetHostReport(HostReport{Host: "mac", Roles: []string{"autoreview"}}, at)
	}
	if got := st.HostReports["mac"].Roles; len(got) != 2 {
		t.Fatalf("roles = %v, want autofix still listed inside its own TTL", got)
	}

	// Past autofix's OWN last sighting it is dropped, even though the record
	// itself is fresh.
	st.SetHostReport(HostReport{Host: "mac", Roles: []string{"autoreview"}}, base.Add(HostReportTTL+time.Minute))
	got := st.HostReports["mac"].Roles
	if len(got) != 1 || got[0] != "autoreview" {
		t.Errorf("roles = %v, want only the service still reporting", got)
	}
}

func TestFixAgentIgnoresAHostWhoseAutofixRoleExpired(t *testing.T) {
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	st := New()
	st.SetHostReport(HostReport{Host: "stale", Roles: []string{"autofix"}, Agent: "claude"}, base)
	st.SetHostReport(HostReport{Host: "live", Roles: []string{"autofix"}, Agent: "codex"}, base.Add(10*time.Minute))

	// The stale host keeps reporting serve, so its record is freshest even
	// after its autofix role has expired.
	now := base.Add(HostReportTTL + time.Minute)
	st.SetHostReport(HostReport{Host: "stale", Roles: []string{"serve"}}, now)
	if got := st.FixAgent(now); got != "codex" {
		t.Fatalf("fix agent = %q, want the host whose autofix service is still live", got)
	}

	// Freshness is also enforced without another report getting a chance to
	// prune the stored role.
	unpruned := New()
	unpruned.SetHostReport(HostReport{Host: "stale", Roles: []string{"autofix"}, Agent: "claude"}, base)
	unpruned.SetHostReport(HostReport{Host: "live", Roles: []string{"autofix"}, Agent: "codex"}, base.Add(10*time.Minute))
	if got := unpruned.FixAgent(now); got != "codex" {
		t.Fatalf("fix agent with unpruned stale role = %q, want the still-fresh host", got)
	}
}

func TestAutofixCanClearTheHostAgentWhileOtherRolesPreserveIt(t *testing.T) {
	now := time.Date(2026, 7, 29, 19, 0, 0, 0, time.UTC)
	st := New()
	st.SetHostReport(HostReport{Host: "mac", Roles: []string{"autofix"}, Agent: "claude"}, now)
	st.SetHostReport(HostReport{Host: "mac", Roles: []string{"serve"}}, now.Add(time.Minute))
	if got := st.HostReports["mac"].Agent; got != "claude" {
		t.Fatalf("serve cleared agent to %q, want it preserved", got)
	}
	st.SetHostReport(HostReport{Host: "mac", Roles: []string{"autofix"}}, now.Add(2*time.Minute))
	if got := st.HostReports["mac"].Agent; got != "" {
		t.Fatalf("autofix cleared agent to %q, want empty", got)
	}
}

func TestHostVersionsStayWithTheirReportingRoles(t *testing.T) {
	now := time.Date(2026, 7, 29, 19, 0, 0, 0, time.UTC)
	st := New()
	st.SetHostReport(HostReport{Host: "mac", Version: "2.0.0", Roles: []string{"autofix"}}, now)
	st.SetHostReport(HostReport{Host: "mac", Version: "2.1.0", Roles: []string{"serve"}}, now.Add(time.Minute))

	got := st.HostReports["mac"]
	if got.VersionFor("autofix") != "2.0.0" || got.VersionFor("serve") != "2.1.0" {
		t.Fatalf("role versions = %v, want each service's own build", got.RoleVersions)
	}
	if got.Version != "2.1.0" {
		t.Fatalf("host version = %q, want the newest active service", got.Version)
	}

	st.SetHostReport(HostReport{Host: "mac", Version: "2.0.0", Roles: []string{"autofix"}}, now.Add(2*time.Minute))
	if got := st.HostReports["mac"].Version; got != "2.1.0" {
		t.Fatalf("older role heartbeat regressed host version to %q", got)
	}
}

func TestHostVersionComparisonUnderstandsPrefixesAndPrereleases(t *testing.T) {
	for _, tc := range []struct {
		name, newer, older string
	}{
		{name: "leading v", newer: "2.1.0", older: "v2.0.0"},
		{name: "release after prerelease", newer: "2.1.0", older: "2.1.0-rc1"},
		{name: "later prerelease", newer: "2.1.0-rc.2", older: "2.1.0-rc.1"},
		{name: "longer prerelease", newer: "2.1.0-rc.1.1", older: "2.1.0-rc.1"},
		{name: "text after numeric prerelease", newer: "2.1.0-rc.alpha", older: "2.1.0-rc.999999999999999999999999"},
		{name: "oversized core", newer: "999999999999999999999999.0.0", older: "888888888888888888888888.9.9"},
		{name: "oversized prerelease", newer: "2.1.0-rc.999999999999999999999999", older: "2.1.0-rc.888888888888888888888888"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !dottedVersionAfter(tc.newer, tc.older) {
				t.Fatalf("%q was not ranked after %q", tc.newer, tc.older)
			}
			if dottedVersionAfter(tc.older, tc.newer) {
				t.Fatalf("%q was incorrectly ranked after %q", tc.older, tc.newer)
			}
		})
	}
	if dottedVersionAfter("2.1.0+new-build", "2.1.0+old-build") ||
		dottedVersionAfter("2.1.0+old-build", "2.1.0+new-build") {
		t.Fatal("build metadata changed version precedence")
	}
}

// A record an older binary wrote carries roles and no dates. They are exactly
// as old as the record, which is the honest date to expire them from — anything
// else either drops a live role or keeps a dead one.
func TestUndatedRolesExpireFromTheRecordTheyCameWith(t *testing.T) {
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	st := New()
	st.HostReports = map[string]HostReport{
		"linux": {Host: "linux", Roles: []string{"serve"}, At: base},
	}

	st.SetHostReport(HostReport{Host: "linux", Roles: []string{"autofix"}}, base.Add(time.Minute))
	if got := st.HostReports["linux"].Roles; len(got) != 2 {
		t.Fatalf("roles = %v, want the undated role carried while it is still fresh", got)
	}

	st.SetHostReport(HostReport{Host: "linux", Roles: []string{"autofix"}}, base.Add(HostReportTTL+time.Minute))
	got := st.HostReports["linux"].Roles
	if len(got) != 1 || got[0] != "autofix" {
		t.Errorf("roles = %v, want the undated role expired against the record it came with", got)
	}
}

// StaleRole is what a caller skipping a no-change write has to ask: the record
// is fresh because another service keeps writing it, and skipping is precisely
// what would leave the dead role listed.
func TestStaleRoleSeesPastAFreshRecord(t *testing.T) {
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	st := New()
	st.SetHostReport(HostReport{Host: "mac", Roles: []string{"autofix"}}, base)
	st.SetHostReport(HostReport{Host: "mac", Roles: []string{"autoreview"}}, base.Add(20*time.Minute))

	rec := st.HostReports["mac"]
	if rec.StaleRole(base.Add(20 * time.Minute)) {
		t.Error("no role has aged out yet")
	}
	if !rec.StaleRole(base.Add(HostReportTTL + time.Minute)) {
		t.Error("autofix has not been heard from in a TTL and the record still names it")
	}
}

// Tool probes describe the PATH of the service that took them, and those PATHs
// differ: the autofix unit adds the selected agent's directory and serve does
// not. Replacing the list with whichever service wrote last made the dashboard
// alternate between "the fix agent is here" and "it is missing" while the
// service that actually runs sessions never changed.
func TestToolReportsStayWithTheRoleThatProbedThem(t *testing.T) {
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	st := New()

	st.SetHostReport(HostReport{Host: "mac", Roles: []string{"autofix"}, Tools: []ToolReport{
		{Name: "git", Path: "/usr/bin/git"},
		{Name: "claude", Path: "/opt/agents/claude"},
	}}, base)
	// serve runs on the same machine with a plainer PATH and cannot see the agent.
	st.SetHostReport(HostReport{Host: "mac", Roles: []string{"serve"}, Tools: []ToolReport{
		{Name: "git", Path: "/usr/bin/git"},
		{Name: "claude"},
	}}, base.Add(time.Minute))

	agent, ok := st.ToolOn("mac", "claude")
	if !ok || !agent.Found() || agent.Path != "/opt/agents/claude" {
		t.Errorf("claude = %+v, want the answer from the role that runs fix sessions", agent)
	}
	if probed := st.HostReports["mac"].RoleTools["serve"]; len(probed) != 2 || probed[1].Found() {
		t.Errorf("serve's own probe = %+v, want it kept as reported", probed)
	}

	// Once autofix stops reporting there is nobody left to outrank serve, so its
	// answer is the host's answer.
	st.SetHostReport(HostReport{Host: "mac", Roles: []string{"serve"}, Tools: []ToolReport{
		{Name: "git", Path: "/usr/bin/git"},
		{Name: "claude"},
	}}, base.Add(HostReportTTL+time.Minute))
	if agent, _ := st.ToolOn("mac", "claude"); agent.Found() {
		t.Errorf("claude = %+v, want serve's answer once autofix has aged out", agent)
	}
}

func TestForgetHostReport(t *testing.T) {
	now := time.Now().UTC()
	st := &State{}
	st.SetHostReport(HostReport{Host: "K-Mac.local", Roles: []string{"autofix"}}, now)
	st.SetHostReport(HostReport{Host: "omarchy", Roles: []string{"serve"}}, now)

	// A name reads off the dashboard as the machine spells it; matching it back
	// should not depend on reproducing that spelling exactly.
	if !st.ForgetHostReport("k-mac.LOCAL") {
		t.Fatal("forgetting a recorded host reported no record")
	}
	if _, ok := st.HostReports["K-Mac.local"]; ok {
		t.Fatal("the record survived being forgotten")
	}
	if _, ok := st.HostReports["omarchy"]; !ok {
		t.Fatal("forgetting one host dropped another")
	}
	if st.ForgetHostReport("K-Mac.local") {
		t.Fatal("forgetting an absent host claimed it removed one")
	}
	if st.ForgetHostReport("  ") {
		t.Fatal("a blank name matched a record")
	}
}
