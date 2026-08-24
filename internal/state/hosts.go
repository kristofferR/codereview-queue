package state

import (
	"sort"
	"strings"
	"time"
)

// WriterHost reduces a writer key ("host=X pid=… run=…") to its machine name.
// Plain host names pass through unchanged for records written before keys
// carried process identity.
func WriterHost(writer string) string {
	for _, field := range strings.Fields(writer) {
		if host, ok := strings.CutPrefix(field, "host="); ok {
			return host
		}
	}
	return writer
}

// HostReport is what one machine says about itself: which crq it runs, and
// which tools it can actually reach.
//
// It exists because every question about a fleet turns into a per-host
// question, and crq could only answer for whichever machine you happened to be
// asking from. "Is claude installed" has a different answer on each host, and
// the one that matters — is it on the PATH the SERVICE runs with — has a
// different answer again from the shell you are typing in.
//
// Reported rather than inferred. A host writes its own record; nothing else may
// write one for it, because a machine that has stopped reporting is exactly the
// machine whose last claim about itself should stop being trusted.
type HostReport struct {
	Host string `json:"host"`
	// Version is the crq that wrote this. Two hosts on different versions is
	// the single most common cause of "that setting did nothing".
	Version string `json:"version,omitempty"`
	// Caps is what that binary understands, so a reader can see WHY a host
	// ignores a setting rather than only that it does.
	Caps int `json:"caps,omitempty"`
	// Roles are the crq services running here ("autoreview", "autofix",
	// "serve"), so a fleet can be read as who does what.
	Roles []string `json:"roles,omitempty"`
	// RoleSeen dates each role separately, because the record as a whole cannot.
	// Every service on a host writes the SAME record and merges the roles it
	// found there, so the one still running refreshed the stopped one's claim on
	// every pass and the table showed a dead service running for ever.
	RoleSeen map[string]time.Time `json:"role_seen,omitempty"`
	// RoleCaps is what the binary behind each role understands, kept apart for
	// the same reason RoleSeen is. During an ordinary rolling upgrade one
	// machine runs one service on the new build and another on the old, and a
	// single value is whichever of them wrote last: a fresh `serve` heartbeat
	// advertised the newest capabilities while an old `autofix` watcher went on
	// ignoring the very setting LaggingRoleWriters was then told it honoured.
	RoleCaps map[string]int `json:"role_caps,omitempty"`
	// RoleVersions is the crq version behind each role. A host can be halfway
	// through an upgrade, so the record-wide Version alone cannot say whether
	// this particular reporter has changed.
	RoleVersions map[string]string `json:"role_versions,omitempty"`
	// Agent is the fix agent this host is installed to run, by name ("claude",
	// "codex"). Reported rather than inferred, and for the same reason the tool
	// probes are: it is chosen per machine at install time and exported to the
	// autofix unit alone, so no other process can read it. A dashboard that
	// assumed one instead checked the wrong agent's availability on every host
	// and reported a working codex fleet as missing its agent.
	//
	// Kept when a reporter names none, so the serve heartbeat on the same
	// machine does not erase what its autofix service said.
	Agent string `json:"agent,omitempty"`
	// Tools is what this host can run, resolved across the roles reporting here
	// — see RoleTools for why that is not simply the last report.
	Tools []ToolReport `json:"tools,omitempty"`
	// RoleTools is what each role probed on ITS own PATH, kept apart because the
	// PATHs differ: the autofix unit adds the selected agent's directory and
	// serve does not. Merged into one list, the two took turns overwriting each
	// other and the dashboard alternated between "the fix agent is here" and
	// "it is missing" while the service that runs sessions never changed.
	RoleTools map[string][]ToolReport `json:"role_tools,omitempty"`
	At        time.Time               `json:"at"`

	// unknown carries members a newer binary wrote inside this record. State's
	// carrier cannot: it recognises "host_reports" and hands each report to an
	// ordinary decoder. See tolerant.go.
	unknown unknownFields
}

// toolRoleRank orders roles by whose PATH the answer should come from. The
// autofix service is the one that actually starts fix sessions, so its view of
// "is the agent reachable" is the operative one; a dashboard that merely
// renders the answer is the least authoritative.
func toolRoleRank(role string) int {
	switch role {
	case "autofix":
		return 0
	case "autoreview":
		return 1
	case "serve":
		return 3
	default:
		return 2
	}
}

// ToolReport is one executable on one host.
type ToolReport struct {
	Name string `json:"name"`
	// Path is empty when it was not found at all.
	Path string `json:"path,omitempty"`
	// Version is the tool's own, when it will say; some do not.
	Version string `json:"version,omitempty"`

	// unknown carries members a newer binary wrote inside this record, for the
	// same reason HostReport does — it is nested deeper still. See tolerant.go.
	unknown unknownFields
}

// Found reports whether the tool is reachable at all.
func (t ToolReport) Found() bool { return t.Path != "" }

// HostReportTTL is how long a host's self-report is worth showing as current.
// Past it the record is kept and marked stale rather than dropped: "this host
// last said it had claude, two days ago" is more useful than silence.
const HostReportTTL = 30 * time.Minute

// SetHostReport records what a host says about itself.
//
// Roles MERGE rather than replace, because a machine runs more than one crq
// service and each reports only its own: the autofix watcher and the review
// daemon on the same host would otherwise take turns overwriting each other,
// and the table would show whichever wrote last as the only thing running
// there.
//
// Each role therefore expires on its OWN last sighting, not on the record's. A
// shared At cannot answer this: the surviving service refreshes it every pass,
// so a carried-forward role from a service that stopped was renewed for ever
// and the dashboard reported it running indefinitely.
//
// Tool probes merge the same way and for the same reason: they describe the
// PATH of the service that took them, so they are kept per role and resolved
// into Tools rather than replaced wholesale by whoever wrote last. Capabilities
// join them, because a machine mid-upgrade runs its roles on two builds.
//
// The reporter's record is a fresh value every pass, so what a NEWER binary
// wrote is carried onto it — both the report's own unrecognised members and
// each probe's. Without that, an older service's heartbeat erased a newer one's
// additions every time it ran, which is the one thing tolerant.go exists to
// prevent.
func (s *State) SetHostReport(r HostReport, now time.Time) {
	if s.HostReports == nil {
		s.HostReports = map[string]HostReport{}
	}
	now = now.UTC()
	seen := map[string]time.Time{}
	tools := map[string][]ToolReport{}
	caps := map[string]int{}
	versions := map[string]string{}
	reportedVersion := r.Version
	if prev, ok := s.HostReports[r.Host]; ok {
		for role, at := range prev.RoleSeen {
			seen[role] = at
		}
		for role, probed := range prev.RoleTools {
			tools[role] = probed
		}
		for role, c := range prev.RoleCaps {
			caps[role] = c
		}
		for role, version := range prev.RoleVersions {
			versions[role] = version
		}
		for _, role := range prev.Roles {
			// A record written before roles were dated (an older binary, or one
			// whose write dropped the member): its roles are exactly as old as it
			// is, which is the honest date to expire them from.
			if _, dated := seen[role]; !dated {
				seen[role] = prev.At
			}
			// Likewise for a record written before probes were kept per role:
			// its one list is the best any of its roles could say, so it stands
			// in for all of them until each reports again on its own.
			if _, probed := tools[role]; !probed && len(prev.Tools) > 0 {
				tools[role] = prev.Tools
			}
			// And for one written before capabilities were: the record's own
			// value is all it can say for the roles it carries forward.
			if _, known := caps[role]; !known {
				caps[role] = prev.Caps
			}
			if _, known := versions[role]; !known {
				versions[role] = prev.Version
			}
		}
		agentAuthoritative := false
		for _, role := range r.Roles {
			agentAuthoritative = agentAuthoritative || role == "autofix"
		}
		if r.Agent == "" && !agentAuthoritative {
			// Only the autofix service knows this. Every other service on the
			// machine reports nothing, and replacing the answer with silence
			// would make the agent flicker away on the next heartbeat.
			r.Agent = prev.Agent
		}
		r.unknown = carryUnknown(r.unknown, prev.unknown)
	}
	for _, role := range r.Roles {
		seen[role] = now
		caps[role] = r.Caps
		versions[role] = reportedVersion
		if len(r.Tools) > 0 {
			tools[role] = mergeTools(tools[role], r.Tools)
		}
	}
	roles := make([]string, 0, len(seen))
	for role, at := range seen {
		if now.Sub(at) > HostReportTTL {
			delete(seen, role)
			delete(tools, role)
			delete(caps, role)
			delete(versions, role)
			continue
		}
		roles = append(roles, role)
	}
	sort.Strings(roles)
	r.Roles = roles
	if len(seen) == 0 {
		seen = nil
	}
	r.RoleSeen = seen
	r.Tools = resolveTools(tools, roles, r.Tools)
	if len(tools) == 0 {
		tools = nil
	}
	r.RoleTools = tools
	if len(caps) == 0 {
		caps = nil
	}
	r.RoleCaps = caps
	if len(versions) == 0 {
		versions = nil
	}
	r.RoleVersions = versions
	r.Version = newestHostVersion(versions, reportedVersion)
	r.At = now
	s.HostReports[r.Host] = r
}

// VersionFor is the crq version behind role. Records written before versions
// were kept per role fall back to the record-wide answer.
func (r HostReport) VersionFor(role string) string {
	if version, ok := r.RoleVersions[role]; ok {
		return version
	}
	return r.Version
}

func newestHostVersion(versions map[string]string, fallback string) string {
	newest := fallback
	for _, version := range versions {
		if dottedVersionAfter(version, newest) {
			newest = version
		}
	}
	return newest
}

func dottedVersionAfter(a, b string) bool {
	ac, apre, aok := parseDottedVersion(a)
	bc, bpre, bok := parseDottedVersion(b)
	if !aok || !bok {
		return a > b
	}
	for i := 0; i < len(ac) || i < len(bc); i++ {
		av, bv := "0", "0"
		if i < len(ac) {
			av = ac[i]
		}
		if i < len(bc) {
			bv = bc[i]
		}
		if cmp := compareDecimal(av, bv); cmp != 0 {
			return cmp > 0
		}
	}
	switch {
	case apre == "" && bpre != "":
		return true
	case apre != "" && bpre == "":
		return false
	default:
		return prereleaseAfter(apre, bpre)
	}
}

func parseDottedVersion(version string) (core []string, prerelease string, ok bool) {
	version = strings.TrimSpace(version)
	version = strings.TrimPrefix(strings.TrimPrefix(version, "v"), "V")
	version, _, _ = strings.Cut(version, "+")
	coreText, prerelease, _ := strings.Cut(version, "-")
	if coreText == "" {
		return nil, "", false
	}
	for _, part := range strings.Split(coreText, ".") {
		n, numeric := decimalIdentifier(part)
		if !numeric {
			return nil, "", false
		}
		core = append(core, n)
	}
	return core, prerelease, true
}

func prereleaseAfter(a, b string) bool {
	if a == b {
		return false
	}
	ap, bp := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(ap) || i < len(bp); i++ {
		if i >= len(ap) {
			return false
		}
		if i >= len(bp) {
			return true
		}
		if ap[i] == bp[i] {
			continue
		}
		an, aNumeric := decimalIdentifier(ap[i])
		bn, bNumeric := decimalIdentifier(bp[i])
		switch {
		case aNumeric && bNumeric:
			return compareDecimal(an, bn) > 0
		case aNumeric:
			return false // numeric identifiers precede non-numeric ones
		case bNumeric:
			return true
		default:
			return ap[i] > bp[i]
		}
	}
	return false
}

func decimalIdentifier(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return "", false
		}
	}
	value = strings.TrimLeft(value, "0")
	if value == "" {
		return "0", true
	}
	return value, true
}

func compareDecimal(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}

// mergeTools carries what a newer binary recorded about a tool onto this
// reporter's fresh probe of it, matched by name. The probe itself is the
// reporter's to state — a path that has changed has changed — but the members
// it has never heard of are not, and rebuilding the slice from scratch dropped
// them on every pass.
func mergeTools(prev, next []ToolReport) []ToolReport {
	if len(prev) == 0 {
		return next
	}
	byName := make(map[string]ToolReport, len(prev))
	for _, t := range prev {
		byName[t.Name] = t
	}
	out := make([]ToolReport, 0, len(next))
	for _, t := range next {
		if old, ok := byName[t.Name]; ok {
			t.unknown = carryUnknown(t.unknown, old.unknown)
		}
		out = append(out, t)
	}
	return out
}

// CapsFor is what the binary running role understands. It falls back to the
// record's own value for a role that predates per-role capabilities, which is
// the same thing that value meant before they existed.
func (r HostReport) CapsFor(role string) int {
	if c, ok := r.RoleCaps[role]; ok {
		return c
	}
	return r.Caps
}

// resolveTools answers "what can this host run" from the per-role probes, one
// tool at a time: the highest-ranked role that has an opinion about a tool owns
// the answer for it. fallback covers a host whose roles have all aged out —
// there is nobody left to speak for it, so the reporter's own list stands.
func resolveTools(byRole map[string][]ToolReport, roles []string, fallback []ToolReport) []ToolReport {
	ranked := append([]string(nil), roles...)
	sort.SliceStable(ranked, func(i, j int) bool {
		return toolRoleRank(ranked[i]) < toolRoleRank(ranked[j])
	})
	var out []ToolReport
	claimed := map[string]bool{}
	for _, role := range ranked {
		for _, t := range byRole[role] {
			if claimed[t.Name] {
				continue
			}
			claimed[t.Name] = true
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

// ToolsReportedBy is what one role probed, or the record's resolved list when
// that role predates per-role probes. It is what a reporter must compare its
// own probe against: Tools is resolved across every service on the host, so a
// reporter whose answer another role outranks would read the difference as a
// change and rewrite state on every pass.
func (r HostReport) ToolsReportedBy(role string) []ToolReport {
	if probed, ok := r.RoleTools[role]; ok {
		return probed
	}
	return r.Tools
}

// StaleRole reports whether this record still names a role whose own last
// sighting has aged out. The record itself may be fresh — a second service on
// the host keeps writing it — which is why a caller deciding "nothing changed,
// skip the write" has to ask: skipping is what would keep the dead role listed.
func (r HostReport) StaleRole(now time.Time) bool {
	for _, role := range r.Roles {
		at, ok := r.RoleSeen[role]
		if !ok {
			at = r.At
		}
		if now.Sub(at) > HostReportTTL {
			return true
		}
	}
	return false
}

// RolesFresh reports whether every role in want was last seen within the given
// age. It is what a caller deciding "nothing changed, skip the write" must ask
// instead of looking at the record's own At: another service on the same host
// keeps that fresh, so a reporter that trusted it never refreshed its OWN role
// and let it age out from under itself — at which point the next writer of any
// kind prunes the still-running service from the table.
func (r HostReport) RolesFresh(want []string, now time.Time, within time.Duration) bool {
	for _, role := range want {
		at, ok := r.RoleSeen[role]
		if !ok {
			// Undated: only the record's own age can speak for it, and only if
			// the record still lists the role at all.
			if !r.lists(role) {
				return false
			}
			at = r.At
		}
		if now.Sub(at) >= within {
			return false
		}
	}
	return true
}

func (r HostReport) lists(role string) bool {
	for _, have := range r.Roles {
		if have == role {
			return true
		}
	}
	return false
}

// ForgetHostReport drops a host's self-report entirely, returning whether there
// was one. Records are otherwise kept for ever and marked stale, on purpose —
// see HostReportTTL. That is the right default for a host that might come back,
// and the wrong one for a name that never will: a machine renamed, retired, or
// whose hostname flapped leaves a record no heartbeat will ever refresh, and a
// fleet table reading five hosts where two exist is not a fleet table. Deciding
// a name is gone is an operator's call, which is why nothing does this
// automatically.
func (s *State) ForgetHostReport(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" || len(s.HostReports) == 0 {
		return false
	}
	if _, ok := s.HostReports[host]; ok {
		delete(s.HostReports, host)
		return true
	}
	// Hosts report whatever their machine calls itself, so an operator reading a
	// name off the dashboard should not have to reproduce its case exactly.
	for name := range s.HostReports {
		if strings.EqualFold(name, host) {
			delete(s.HostReports, name)
			return true
		}
	}
	return false
}

// HostReportList is every host's self-report, most recently heard from first.
func (s *State) HostReportList() []HostReport {
	out := make([]HostReport, 0, len(s.HostReports))
	for _, r := range s.HostReports {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].Host < out[j].Host
	})
	return out
}

// FixAgent is the agent the fleet's hosts say they run fix sessions with, from
// the most recently heard-from host that has an answer. Empty when none has.
//
// The freshest report wins because hosts CAN disagree — an install is per
// machine — and this answers "what is the fleet fixing with", which is a
// question about the machines actually running. It is not a setting and must
// never be treated as one: a host runs the agent it was installed with,
// whatever this says.
func (s *State) FixAgent(now time.Time) string {
	for _, r := range s.HostReportList() {
		if r.Agent != "" && r.RolesFresh([]string{"autofix"}, now, HostReportTTL) {
			return r.Agent
		}
	}
	return ""
}

// ToolOn reports what a named host says about one tool, and whether that host
// has said anything at all.
func (s *State) ToolOn(host, tool string) (ToolReport, bool) {
	r, ok := s.HostReports[host]
	if !ok {
		return ToolReport{}, false
	}
	for _, t := range r.Tools {
		if t.Name == tool {
			return t, true
		}
	}
	return ToolReport{Name: tool}, true
}
