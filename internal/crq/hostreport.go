package crq

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// probedTools is what crq wants to know about a host. The purpose text lives in
// the dashboard; what matters here is the list, and that every host probes the
// same one so two reports can be compared.
var probedTools = []string{"crq", "git", "gh", "claude", "codex", "coderabbit", "macroscope"}

var toolProbeCache struct {
	sync.Mutex
	at    time.Time
	tools []ToolReport
}

// ReportHost records what THIS machine can reach, so the fleet can be read as a
// fleet rather than as whichever host you happened to ask.
//
// The PATH it probes is the reporting process's own, which is the point: a
// daemon reports the PATH its service runs with, and that is the one that
// decides whether a fix session can start. A tool installed for the shell and
// invisible to the service is the failure this exists to make visible, and it
// cannot be seen from the shell.
func (s *Service) ReportHost(ctx context.Context, roles ...string) {
	// By name, not by path: what a reader wants to know is which agent this
	// machine fixes with, and the name is also what the tool probes below answer
	// for. A process with no fix agent says nothing rather than guessing — the
	// record keeps whatever the autofix service on this host reported.
	agent := ""
	if path := s.cfg.fixAgent(); path != "" {
		agent = filepath.Base(path)
	}
	report := HostReport{
		Host:    s.cfg.Host,
		Version: Version,
		Caps:    WriterCaps,
		Roles:   roles,
		Agent:   agent,
	}
	report.Tools = cachedToolReports(ctx, s.clock().UTC())

	now := s.clock().UTC()
	if s.cfg.DryRun {
		return
	}
	if _, err := s.store.Update(ctx, func(st *State) error {
		// Compared against what the merge WOULD produce, not against this
		// process's own view: another service on this host may have added a
		// role since, and treating that as "nothing changed" would leave the
		// merged record un-refreshed until it aged out.
		//
		// Freshness is asked of THIS reporter's own roles, not of the record.
		// A host running two services has the other one refreshing At on every
		// pass, so a record-wide check suppressed this write until the role
		// being reported crossed the TTL — and then whichever service wrote
		// next pruned a service that never stopped running.
		if prev, ok := st.HostReports[report.Host]; ok && sameHostReport(prev, report) &&
			!prev.StaleRole(now) && prev.RolesFresh(report.Roles, now, HostReportTTL/2) {
			// Nothing changed and the record is still fresh. Rewriting it would
			// bump the state revision on every pass and make the dashboard's
			// change feed report a fleet that is constantly doing something.
			return ErrNoChange
		}
		st.SetHostReport(report, now)
		return nil
	}); err != nil && !errors.Is(err, ErrNoChange) && s.log != nil {
		s.log.Printf("warning: reporting host tools: %v", err)
	}
}

func cachedToolReports(ctx context.Context, now time.Time) []ToolReport {
	toolProbeCache.Lock()
	if len(toolProbeCache.tools) > 0 && toolProbeCache.at.After(now.Add(-HostReportTTL/2)) {
		tools := append([]ToolReport(nil), toolProbeCache.tools...)
		toolProbeCache.Unlock()
		return tools
	}
	toolProbeCache.Unlock()

	tools := make([]ToolReport, 0, len(probedTools))
	for _, name := range probedTools {
		t := ToolReport{Name: name}
		if path, err := exec.LookPath(name); err == nil {
			t.Path = path
			t.Version = toolVersion(ctx, path)
		}
		tools = append(tools, t)
	}

	toolProbeCache.Lock()
	defer toolProbeCache.Unlock()
	// Another caller may have refreshed the cache while this one was probing.
	// Keep the first complete result rather than making a slower probe extend
	// the cache lifetime again.
	if len(toolProbeCache.tools) > 0 && toolProbeCache.at.After(now.Add(-HostReportTTL/2)) {
		return append([]ToolReport(nil), toolProbeCache.tools...)
	}
	toolProbeCache.at = now
	toolProbeCache.tools = append(toolProbeCache.tools[:0], tools...)
	return tools
}

// toolVersion asks a tool what it is, briefly. Anything that does not answer in
// two seconds, or answers with something unhelpful, reports no version rather
// than holding up a pass: this is for a person reading a table.
func toolVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	if len(line) > 60 {
		line = line[:60]
	}
	return line
}

// sameHostReport reports whether the stored record already says what this
// reporter is about to say. The tool comparison is per ROLE: the record's own
// Tools list is resolved across every service on the host, so comparing against
// it made a reporter whose PATH another role outranks see a difference on every
// pass and rewrite state for ever.
func sameHostReport(prev, next HostReport) bool {
	// Only when this reporter has an answer: a service that runs no fix sessions
	// says nothing about the agent, and the record keeps what the autofix service
	// on this host said — so its silence is not a difference to write out.
	agentAuthoritative := false
	for _, role := range next.Roles {
		agentAuthoritative = agentAuthoritative || role == "autofix"
	}
	if (next.Agent != "" || agentAuthoritative) && prev.Agent != next.Agent {
		return false
	}
	for _, role := range next.Roles {
		if prev.VersionFor(role) != next.Version {
			return false
		}
		if prev.CapsFor(role) != next.Caps {
			return false
		}
		stored := prev.ToolsReportedBy(role)
		if len(stored) != len(next.Tools) {
			return false
		}
		for i := range stored {
			// Field by field: a ToolReport carries the members a newer binary
			// added, and those are not this reporter's to have an opinion about.
			// Comparing whole values would read a carried member as a difference
			// and rewrite state on every pass to say the same thing.
			if stored[i].Name != next.Tools[i].Name ||
				stored[i].Path != next.Tools[i].Path ||
				stored[i].Version != next.Tools[i].Version {
				return false
			}
		}
	}
	return true
}

// ForgetHostResult reports what a forget-host call removed.
type ForgetHostResult struct {
	Host   string   `json:"host"`
	Forgot bool     `json:"forgot"`
	Reason string   `json:"reason,omitempty"`
	Hosts  []string `json:"hosts"`
	DryRun bool     `json:"dry_run,omitempty"`
}

// ForgetHost removes one host's self-report from the fleet table.
//
// A live service rewrites its record on its next pass, so this is for a name
// that is gone: a renamed machine, a retired one, a hostname that flapped. It
// deliberately does not check whether the record is stale — a host still
// reporting is one an operator can see, and refusing on that basis would only
// send them to edit the state ref by hand.
func (s *Service) ForgetHost(ctx context.Context, host string) (ForgetHostResult, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		return ForgetHostResult{}, errors.New("forget-host needs a host name (see the hosts in crq debug state)")
	}
	result := ForgetHostResult{Host: host, DryRun: s.cfg.DryRun}
	if s.cfg.DryRun {
		st, _, err := s.store.Load(ctx)
		if err != nil {
			return result, err
		}
		result.Forgot, err = st.ForgetHostReport(host)
		if err != nil {
			return result, err
		}
		result.Hosts = hostNames(st)
		if !result.Forgot {
			result.Reason = "no report recorded under that name"
		}
		return result, nil
	}
	// The stores answer ErrNoChange with (state, nil), so whether a record was
	// there is the closure's to report, not the error's.
	forgot := false
	st, err := s.store.Update(ctx, func(st *State) error {
		var err error
		forgot, err = st.ForgetHostReport(host)
		if err != nil {
			return err
		}
		if !forgot {
			return ErrNoChange
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	result.Forgot = forgot
	if !forgot {
		result.Reason = "no report recorded under that name"
	} else {
		s.sync(ctx, st)
	}
	result.Hosts = hostNames(st)
	return result, nil
}

func hostNames(st State) []string {
	names := make([]string, 0, len(st.HostReports))
	for _, r := range st.HostReportList() {
		names = append(names, r.Host)
	}
	return names
}
