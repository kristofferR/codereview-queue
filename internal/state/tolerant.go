package state

import (
	"encoding/json"
	"reflect"
	"strings"
)

// Version tolerance: the fleet shares ONE state ref across binaries that may not
// be the same build.
//
// Go's encoding/json silently drops members it has no field for, so an older
// binary that merely reads and rewrites state erased every field a newer one had
// added. That is why Codex's per-round bookkeeping had to be dual-written into
// legacy fields, and why adding anything to a Round was a rollout problem rather
// than a change.
//
// Unknown members are therefore carried by default. A load keeps whatever it
// did not recognise, a save puts it back, and a field this binary has never
// heard of survives a foreign write untouched. A schema bump is reserved for a
// compatibility fence: newer state is refused by older binaries, as v4 requires
// so v3 pumping clients cannot ignore administrative holds.

// unknownFields holds JSON members this binary has no field for, verbatim.
type unknownFields map[string]json.RawMessage

var (
	fireSlotFields      = jsonFieldNames(reflect.TypeOf(FireSlot{}))
	roundFields         = jsonFieldNames(reflect.TypeOf(Round{}))
	stateFields         = jsonFieldNames(reflect.TypeOf(State{}))
	fleetFields         = jsonFieldNames(reflect.TypeOf(FleetDefaults{}))
	solverFields        = jsonFieldNames(reflect.TypeOf(SolverSettings{}))
	repoReviewersFields = jsonFieldNames(reflect.TypeOf(RepoReviewers{}))
	repoAutofixFields   = jsonFieldNames(reflect.TypeOf(RepoAutofixSwitch{}))
	enrollmentFields    = jsonFieldNames(reflect.TypeOf(RepoEnrollment{}))
	accountFields       = jsonFieldNames(reflect.TypeOf(AccountQuota{}))
	coBotFields         = jsonFieldNames(reflect.TypeOf(CoBotRound{}))
	dispatchFields      = jsonFieldNames(reflect.TypeOf(DispatchClaim{}))
	workClaimFields     = jsonFieldNames(reflect.TypeOf(WorkClaim{}))
	hostReportFields    = jsonFieldNames(reflect.TypeOf(HostReport{}))
	toolReportFields    = jsonFieldNames(reflect.TypeOf(ToolReport{}))
)

// UnmarshalJSON decodes an interactive work claim and preserves fields written
// by a newer binary. State recognises the surrounding map, so the record needs
// its own tolerant carrier just like dispatch and repository settings do.
func (c *WorkClaim) UnmarshalJSON(raw []byte) error {
	type plain WorkClaim
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, workClaimFields)
	if err != nil {
		return err
	}
	*c = WorkClaim(decoded)
	c.unknown = unknown
	return nil
}

// MarshalJSON writes a work claim with unknown members intact.
func (c WorkClaim) MarshalJSON() ([]byte, error) {
	type plain WorkClaim
	return mergeUnknown(plain(c), c.unknown)
}

// UnmarshalJSON decodes a fire slot and remembers anything it did not recognise.
func (s *FireSlot) UnmarshalJSON(raw []byte) error {
	type plain FireSlot
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, fireSlotFields)
	if err != nil {
		return err
	}
	*s = FireSlot(decoded)
	s.unknown = unknown
	return nil
}

// MarshalJSON writes a fire slot back with the members it did not recognise intact.
func (s FireSlot) MarshalJSON() ([]byte, error) {
	type plain FireSlot
	return mergeUnknown(plain(s), s.unknown)
}

// UnmarshalJSON decodes a round and remembers anything it did not recognise.
func (r *Round) UnmarshalJSON(raw []byte) error {
	// A distinct type with the same layout: without it, json would call this
	// method again for the inner decode and recurse forever.
	type plain Round
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, roundFields)
	if err != nil {
		return err
	}
	*r = Round(decoded)
	r.unknown = unknown
	return nil
}

// MarshalJSON writes a round back with the members it did not recognise intact.
func (r Round) MarshalJSON() ([]byte, error) {
	type plain Round
	return mergeUnknown(plain(r), r.unknown)
}

// UnmarshalJSON decodes state and remembers anything it did not recognise.
func (s *State) UnmarshalJSON(raw []byte) error {
	type plain State
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, stateFields)
	if err != nil {
		return err
	}
	*s = State(decoded)
	s.unknown = unknown
	return nil
}

// MarshalJSON writes state back with the members it did not recognise intact.
func (s State) MarshalJSON() ([]byte, error) {
	type plain State
	return mergeUnknown(plain(s), s.unknown)
}

// Nesting matters: State recognises "fleet" and hands the whole object to an
// ordinary decoder, so its top-level carrier never sees a member added INSIDE
// the record. Without their own round trip, a newer binary's fleet or solver
// setting is dropped the first time an older one rewrites state — which is the
// rolling-upgrade guarantee the record's own documentation makes.

// UnmarshalJSON decodes fleet defaults and remembers anything it did not recognise.
func (f *FleetDefaults) UnmarshalJSON(raw []byte) error {
	type plain FleetDefaults
	decodable := raw
	var object map[string]json.RawMessage
	var envUnknown unknownFields
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	if envRaw, ok := object["env"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(envRaw, &nested); err != nil {
			return err
		}
		known := make(map[string]string)
		for key, valueRaw := range nested {
			var value string
			if err := json.Unmarshal(valueRaw, &value); err != nil {
				if envUnknown == nil {
					envUnknown = unknownFields{}
				}
				envUnknown[key] = valueRaw
				continue
			}
			known[key] = value
		}
		knownRaw, err := json.Marshal(known)
		if err != nil {
			return err
		}
		object["env"] = knownRaw
		decodable, err = json.Marshal(object)
		if err != nil {
			return err
		}
	}
	var decoded plain
	if err := json.Unmarshal(decodable, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, fleetFields)
	if err != nil {
		return err
	}
	*f = FleetDefaults(decoded)
	f.envUnknown = envUnknown
	f.unknown = unknown
	return nil
}

// MarshalJSON writes fleet defaults back with the members it did not recognise intact.
func (f FleetDefaults) MarshalJSON() ([]byte, error) {
	type plain FleetDefaults
	raw, err := mergeUnknown(plain(f), f.unknown)
	if err != nil || len(f.envUnknown) == 0 {
		return raw, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	nested := make(map[string]json.RawMessage, len(f.Env)+len(f.envUnknown))
	for key, valueRaw := range f.envUnknown {
		nested[key] = valueRaw
	}
	for key, value := range f.Env {
		valueRaw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		nested[key] = valueRaw
	}
	envRaw, err := json.Marshal(nested)
	if err != nil {
		return nil, err
	}
	object["env"] = envRaw
	return json.Marshal(object)
}

// UnmarshalJSON decodes solver settings and remembers anything it did not recognise.
func (s *SolverSettings) UnmarshalJSON(raw []byte) error {
	type plain SolverSettings
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, solverFields)
	if err != nil {
		return err
	}
	*s = SolverSettings(decoded)
	s.unknown = unknown
	return nil
}

// MarshalJSON writes solver settings back with the members it did not recognise intact.
func (s SolverSettings) MarshalJSON() ([]byte, error) {
	type plain SolverSettings
	return mergeUnknown(plain(s), s.unknown)
}

// UnmarshalJSON decodes one repository's reviewer override and remembers
// anything it did not recognise. The map around it is not enough: State
// recognises "repos" by name, so only the record itself can carry a member a
// newer binary added inside it.
func (r *RepoReviewers) UnmarshalJSON(raw []byte) error {
	type plain RepoReviewers
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, repoReviewersFields)
	if err != nil {
		return err
	}
	*r = RepoReviewers(decoded)
	r.unknown = unknown
	return nil
}

// MarshalJSON writes a reviewer override back with the members it did not recognise intact.
func (r RepoReviewers) MarshalJSON() ([]byte, error) {
	type plain RepoReviewers
	return mergeUnknown(plain(r), r.unknown)
}

// UnmarshalJSON decodes one repository's enrollment record and remembers
// anything it did not recognise. Same nesting argument as the reviewer
// override: "enrolled" is a member every binary that has the map recognises,
// so only the record itself can carry a member a newer one added inside it.
func (e *RepoEnrollment) UnmarshalJSON(raw []byte) error {
	type plain RepoEnrollment
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, enrollmentFields)
	if err != nil {
		return err
	}
	*e = RepoEnrollment(decoded)
	e.unknown = unknown
	return nil
}

// MarshalJSON writes an enrollment record back with the members it did not recognise intact.
func (e RepoEnrollment) MarshalJSON() ([]byte, error) {
	type plain RepoEnrollment
	return mergeUnknown(plain(e), e.unknown)
}

// UnmarshalJSON decodes a repository autofix switch and remembers anything it
// did not recognise.
func (s *RepoAutofixSwitch) UnmarshalJSON(raw []byte) error {
	type plain RepoAutofixSwitch
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, repoAutofixFields)
	if err != nil {
		return err
	}
	*s = RepoAutofixSwitch(decoded)
	s.unknown = unknown
	return nil
}

// MarshalJSON writes an autofix switch back with unknown members intact.
func (s RepoAutofixSwitch) MarshalJSON() ([]byte, error) {
	type plain RepoAutofixSwitch
	return mergeUnknown(plain(s), s.unknown)
}

// The same nesting argument applies to records that have been recognised for
// longer, and it is sharper there: "account", "cobots" and "dispatches" are
// members every schema-v4 binary knows, so an older one hands each to an
// ordinary decoder and drops whatever a newer binary added INSIDE it — the
// weekly fire log, a co-reviewer's answer, a session's findings count. The
// containing map being new is what makes a record safe to leave without one;
// none of these is new.

// UnmarshalJSON decodes the account quota and remembers anything it did not recognise.
func (a *AccountQuota) UnmarshalJSON(raw []byte) error {
	type plain AccountQuota
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, accountFields)
	if err != nil {
		return err
	}
	*a = AccountQuota(decoded)
	a.unknown = unknown
	return nil
}

// MarshalJSON writes the account quota back with the members it did not recognise intact.
func (a AccountQuota) MarshalJSON() ([]byte, error) {
	type plain AccountQuota
	return mergeUnknown(plain(a), a.unknown)
}

// UnmarshalJSON decodes one co-reviewer's round bookkeeping and remembers
// anything it did not recognise.
func (c *CoBotRound) UnmarshalJSON(raw []byte) error {
	type plain CoBotRound
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, coBotFields)
	if err != nil {
		return err
	}
	*c = CoBotRound(decoded)
	c.unknown = unknown
	return nil
}

// MarshalJSON writes a co-reviewer's bookkeeping back with the members it did not recognise intact.
func (c CoBotRound) MarshalJSON() ([]byte, error) {
	type plain CoBotRound
	return mergeUnknown(plain(c), c.unknown)
}

// UnmarshalJSON decodes one dispatch claim and remembers anything it did not recognise.
func (c *DispatchClaim) UnmarshalJSON(raw []byte) error {
	type plain DispatchClaim
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, dispatchFields)
	if err != nil {
		return err
	}
	*c = DispatchClaim(decoded)
	c.unknown = unknown
	return nil
}

// MarshalJSON writes a dispatch claim back with the members it did not recognise intact.
func (c DispatchClaim) MarshalJSON() ([]byte, error) {
	type plain DispatchClaim
	return mergeUnknown(plain(c), c.unknown)
}

// UnmarshalJSON decodes one host's self-report and remembers anything it did
// not recognise. Same nesting argument as every record above: "host_reports" is
// a member this binary knows, so it hands each report to an ordinary decoder
// and only the report itself can carry a member a newer binary added inside it.
func (r *HostReport) UnmarshalJSON(raw []byte) error {
	type plain HostReport
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, hostReportFields)
	if err != nil {
		return err
	}
	*r = HostReport(decoded)
	r.unknown = unknown
	return nil
}

// MarshalJSON writes a host report back with the members it did not recognise intact.
func (r HostReport) MarshalJSON() ([]byte, error) {
	type plain HostReport
	return mergeUnknown(plain(r), r.unknown)
}

// UnmarshalJSON decodes one tool probe and remembers anything it did not
// recognise. Nested one level deeper again — inside "tools" and "role_tools" —
// so neither the report's carrier nor the map around it can speak for it.
func (t *ToolReport) UnmarshalJSON(raw []byte) error {
	type plain ToolReport
	var decoded plain
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	unknown, err := captureUnknown(raw, toolReportFields)
	if err != nil {
		return err
	}
	*t = ToolReport(decoded)
	t.unknown = unknown
	return nil
}

// MarshalJSON writes a tool probe back with the members it did not recognise intact.
func (t ToolReport) MarshalJSON() ([]byte, error) {
	type plain ToolReport
	return mergeUnknown(plain(t), t.unknown)
}

// captureUnknown returns the members of raw that known does not name.
func captureUnknown(raw []byte, known map[string]bool) (unknownFields, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	var out unknownFields
	for name, value := range all {
		if known[name] {
			continue
		}
		if out == nil {
			out = unknownFields{}
		}
		out[name] = value
	}
	return out, nil
}

// carryUnknown folds prev's carried members into next's, for a record that is
// REBUILT rather than edited.
//
// Round-tripping is only half the guarantee: a value a writer constructs from
// scratch — a host's self-report, a repository's enrollment — starts with an
// empty carrier however carefully the loaded one preserved its members, so the
// next save erased them. next wins where both carry a member: it is the newer
// read of the two.
func carryUnknown(next, prev unknownFields) unknownFields {
	if len(prev) == 0 {
		return next
	}
	out := make(unknownFields, len(prev)+len(next))
	for name, value := range prev {
		out[name] = value
	}
	for name, value := range next {
		out[name] = value
	}
	return out
}

// mergeUnknown marshals value and adds the carried members back.
//
// A carried member never shadows a field this binary owns: if a later build
// dropped a field that a foreign write still sends, what this binary computes
// now is the current truth, and the stale copy would silently win.
func mergeUnknown(value any, unknown unknownFields) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(unknown) == 0 {
		return encoded, nil
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &members); err != nil {
		return nil, err
	}
	for name, value := range unknown {
		if _, owned := members[name]; owned {
			continue
		}
		members[name] = value
	}
	// Marshalling a map sorts its keys, so the output stays byte-stable for the
	// same content — the dashboard hash and the CAS blob both depend on that.
	return json.Marshal(members)
}

// jsonFieldNames is the set of JSON member names a struct type owns.
func jsonFieldNames(t reflect.Type) map[string]bool {
	names := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue // unexported: never a JSON member
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = field.Name
		}
		names[name] = true
	}
	return names
}
