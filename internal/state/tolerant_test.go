package state

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The fleet shares one state ref across binaries that may not be the same build,
// and encoding/json drops members it has no field for — so an older binary that
// merely read and rewrote state erased whatever a newer one had added. This is the
// property that makes adding a field safe: what this binary does not understand,
// it carries.
func TestUnknownRoundFieldsSurviveARewrite(t *testing.T) {
	// State as a NEWER binary would write it: a round carrying a field this build
	// has never heard of, and a top-level member likewise.
	foreign := `{
	  "v": 3, "rev": 7, "next_seq": 9,
	  "rounds": {
	    "owner/repo#1": {
	      "repo": "owner/repo", "pr": 1, "head": "abcdef123", "seq": 1,
	      "phase": "queued", "enqueued_at": "2026-07-26T12:00:00Z",
	      "dispatch": {"claimed_at": "2026-07-26T12:01:00Z", "attempts": 2},
	      "some_future_flag": true
	    }
	  },
	  "account": {"scope": "owner"},
	  "future_top_level": {"nested": ["a", "b"]}
	}`

	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	// The known fields still decode normally.
	round, ok := st.Rounds["owner/repo#1"]
	if !ok || round.PR != 1 || round.Phase != PhaseQueued {
		t.Fatalf("known fields must decode: %+v", round)
	}

	// Now rewrite it, exactly as a CAS would.
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if _, ok := back["future_top_level"]; !ok {
		t.Errorf("a top-level member this binary does not know was dropped:\n%s", out)
	}
	rounds, _ := back["rounds"].(map[string]any)
	written, _ := rounds["owner/repo#1"].(map[string]any)
	if written == nil {
		t.Fatalf("the round vanished:\n%s", out)
	}
	for _, key := range []string{"dispatch", "some_future_flag"} {
		if _, ok := written[key]; !ok {
			t.Errorf("round member %q was dropped — this is the erasure the carrier exists to prevent:\n%s", key, out)
		}
	}
	// And the carried members keep their shape, not just their names.
	dispatch, _ := written["dispatch"].(map[string]any)
	if dispatch == nil || dispatch["attempts"] != float64(2) {
		t.Errorf("carried member lost its content: %#v", written["dispatch"])
	}
}

func TestOnePassHandOffPreservesUnknownFields(t *testing.T) {
	var st State
	if err := json.Unmarshal([]byte(`{
	  "v":6,
	  "one_pass":{"owner/repo#7":{
	    "ready_head":"abcdef1234567890",
	    "future_merge_receipt":{"id":42}
	  }}
	}`), &st); err != nil {
		t.Fatal(err)
	}
	if !st.OnePassReady("owner/repo", 7, "abcdef123") {
		t.Fatal("known one-pass hand-off did not decode")
	}
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"future_merge_receipt":{"id":42}`) {
		t.Fatalf("one-pass hand-off dropped its future field: %s", out)
	}
}

func TestOnePassEvidencePreservesUnknownFields(t *testing.T) {
	var st State
	if err := json.Unmarshal([]byte(`{
	  "v":6,
	  "one_pass_evidence":{"owner/repo":{
	    "campaign":"campaign-1",
	    "reviewers":["cursor"],
	    "future_audit":{"required":true}
	  }}
	}`), &st); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if !st.MarkOnePassReviewed("owner/repo", 7, "campaign-1", now) {
		t.Fatal("known one-pass review evidence did not decode")
	}
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"future_audit":{"required":true}`) {
		t.Fatalf("one-pass review evidence dropped its future field: %s", out)
	}
}

func TestRenewedDispatchClaimPreservesUnknownFields(t *testing.T) {
	var round Round
	if err := json.Unmarshal([]byte(`{
	  "repo":"owner/repo","pr":1,"head":"abcdef123","phase":"queued",
	  "dispatch":{
	    "host":"old","token":"old","at":"2026-07-26T10:00:00Z",
	    "heartbeat":"2026-07-26T10:00:00Z",
	    "future_dispatch_policy":{"mode":"audit"}
	  }
	}`), &round); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	if ok, why := round.ClaimDispatch("new", "new", now, 3); !ok {
		t.Fatal(why)
	}
	raw, err := json.Marshal(round.Dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"future_dispatch_policy":{"mode":"audit"}`) {
		t.Fatalf("renewed claim dropped its future field: %s", raw)
	}
}

func TestSetEnrollmentCarriesUnknownFields(t *testing.T) {
	var st State
	if err := json.Unmarshal([]byte(`{
	  "v": 4,
	  "enrolled": {
	    "owner/repo": {"enabled": true, "future_enrollment_policy": {"mode": "audit"}}
	  }
	}`), &st); err != nil {
		t.Fatal(err)
	}

	st.SetEnrollment("owner/repo", RepoEnrollment{Enabled: false, Reason: "paused"})
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	enrolled := back["enrolled"].(map[string]any)["owner/repo"].(map[string]any)
	if _, ok := enrolled["future_enrollment_policy"]; !ok {
		t.Fatalf("SetEnrollment dropped a future field: %s", out)
	}
}

// FireSlot is nested beneath a known State field, so top-level tolerance cannot
// carry additions made to the slot itself.
func TestUnknownFireSlotFieldsSurviveARewrite(t *testing.T) {
	foreign := `{
	  "v": 3, "rev": 7, "next_seq": 9,
	  "rounds": {
	    "owner/repo#1": {
	      "repo": "owner/repo", "pr": 1, "head": "abcdef123", "seq": 1,
	      "phase": "reserved", "enqueued_at": "2026-07-26T12:00:00Z",
	      "token": "slot-token"
	    }
	  },
	  "fire_slot": {
	    "key": "owner/repo#1", "token": "slot-token",
	    "since": "2026-07-26T12:01:00Z",
	    "future_hold": {"until": "2026-07-26T12:05:00Z"}
	  },
	  "account": {"scope": "owner"}
	}`

	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	slot, _ := back["fire_slot"].(map[string]any)
	if slot == nil {
		t.Fatalf("the fire slot vanished:\n%s", out)
	}
	hold, _ := slot["future_hold"].(map[string]any)
	if hold == nil || hold["until"] != "2026-07-26T12:05:00Z" {
		t.Errorf("carried fire-slot member lost its content: %#v", slot["future_hold"])
	}
}

// A binary from before FireSlot had its own tolerant marshal drops nested
// hold_until and then clears the orphaned slot in Normalize. The top-level
// mirror is unknown to that binary, so State's existing tolerance carries it
// and a current reader must still honor the in-flight command window.
func TestOrphanedHoldSurvivesALegacyRewrite(t *testing.T) {
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	until := now.Add(15 * time.Minute)
	st := State{
		Version: SchemaVersion,
		Rounds:  map[string]Round{},
		FireSlot: &FireSlot{
			Key: "owner/repo#1", Token: "slot-token", Since: now,
		},
	}
	st.HoldSlotUntil(until)
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}

	// Model the legacy rewrite: its Normalize removes fire_slot because no live
	// round owns it, while its top-level unknown-field carrier leaves the mirror.
	var legacy map[string]any
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy["last_fired"] != until.Format(time.RFC3339) {
		t.Fatalf("legacy pacing anchor = %v, want hold deadline %s", legacy["last_fired"], until)
	}
	delete(legacy, "fire_slot")
	rewritten, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}

	var back State
	if err := json.Unmarshal(rewritten, &back); err != nil {
		t.Fatal(err)
	}
	back.Normalize(now)
	if !back.SlotHeld(now) {
		t.Fatalf("legacy rewrite lost the orphaned hold: %+v", back)
	}
	back.Normalize(until.Add(time.Second))
	if back.SlotHeld(until.Add(time.Second)) || back.FireSlotHoldUntil != nil {
		t.Fatalf("the recovered compatibility hold did not expire: %+v", back)
	}
	if back.LastFired != nil {
		t.Fatalf("current writer did not restore the pre-hold pacing anchor: %s", back.LastFired)
	}
}

// A carried member must never win over a field this binary owns: what this build
// computes now is the current truth, and a stale copy from a foreign write would
// silently override it.
func TestCarriedMembersNeverShadowOwnedFields(t *testing.T) {
	// "note" is a field this binary owns; pretend a foreign payload also sent it
	// (it would normally decode, so force the carrier directly).
	round := Round{Repo: "owner/repo", PR: 1, Head: "abcdef123", Note: "current"}
	round.unknown = unknownFields{"note": json.RawMessage(`"stale"`)}

	out, err := json.Marshal(round)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if back["note"] != "current" {
		t.Errorf("note = %v, want the value this binary computed", back["note"])
	}
}

// The CAS blob and the dashboard hash both depend on the same content producing
// the same bytes, so carrying members must not make the output order vary.
func TestRewriteIsByteStable(t *testing.T) {
	foreign := `{"v":3,"rev":1,"next_seq":1,"rounds":{},"account":{},"zzz":1,"aaa":2,"mmm":3}`
	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	first, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("output is not stable across marshals:\n%s\n%s", first, again)
		}
	}
}

// Round-tripping must not disturb the invariants Normalize repairs, nor the
// legacy Codex folding that shares the same struct.
func TestRewritePreservesNormalizeAndLegacyFolding(t *testing.T) {
	now := time.Now().UTC()
	commanded := now.Add(-time.Minute)
	foreign := `{
	  "v": 3, "rev": 1, "next_seq": 2,
	  "rounds": {
	    "owner/repo#1": {
	      "repo": "owner/repo", "pr": 1, "head": "abcdef123", "seq": 1,
	      "phase": "fired", "enqueued_at": "` + now.Format(time.RFC3339) + `",
	      "codex_command_id": 4242,
	      "codex_commanded_at": "` + commanded.Format(time.RFC3339) + `",
	      "unheard_of": "keep me"
	    }
	  },
	  "account": {}
	}`
	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	st.Normalize(now)

	round := st.Rounds["owner/repo#1"]
	if co := round.Co(codexCoBotKey); co.CommandID != 4242 {
		t.Errorf("legacy Codex fields must still fold into CoBots, got %+v", co)
	}
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "unheard_of") {
		t.Errorf("the carried member did not survive Normalize:\n%s", out)
	}
	if !strings.Contains(string(out), "codex_command_id") {
		t.Errorf("the legacy dual-write must still be emitted:\n%s", out)
	}
}

// The fleet record and the solver record nested inside it are extensible by
// design — their own documentation promises a field a newer binary adds
// survives an older one reading and rewriting state. They are nested beneath a
// member State recognises, so the top-level carrier never sees them and only
// their own round trip can keep that promise.
func TestUnknownFleetFieldsSurviveARewrite(t *testing.T) {
	foreign := `{
	  "v": 3, "rev": 7, "next_seq": 9,
	  "fleet": {
	    "min_interval": "90s",
	    "future_pacing": {"burst": 3},
	    "solver": {"model": "opus", "future_solver_flag": "sandbox"}
	  },
	  "repo_solver": {
	    "owner/repo": {"effort": "high", "future_repo_flag": true}
	  },
	  "account": {"scope": "owner"}
	}`

	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	if st.Fleet.MinInterval != "90s" || st.Fleet.Solver.Model != "opus" {
		t.Fatalf("known fields must still decode: %+v", st.Fleet)
	}

	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	fleet, _ := back["fleet"].(map[string]any)
	if fleet == nil {
		t.Fatalf("the fleet record vanished:\n%s", out)
	}
	pacing, _ := fleet["future_pacing"].(map[string]any)
	if pacing == nil || pacing["burst"] != float64(3) {
		t.Errorf("carried fleet member lost its content: %#v", fleet["future_pacing"])
	}
	solver, _ := fleet["solver"].(map[string]any)
	if solver == nil || solver["future_solver_flag"] != "sandbox" {
		t.Errorf("carried solver member was dropped: %#v", fleet["solver"])
	}
	repos, _ := back["repo_solver"].(map[string]any)
	own, _ := repos["owner/repo"].(map[string]any)
	if own == nil || own["future_repo_flag"] != true {
		t.Errorf("a repository's own solver record dropped a member: %#v", own)
	}
}

// The records that have been recognised the LONGEST are the ones this matters
// most for: every schema-v4 binary knows "account", "cobots" and "dispatches",
// so each hands the value to an ordinary decoder and drops whatever a newer
// binary put inside it — the weekly fire log, a co-reviewer's answer, a
// session's own detail. Being old is not the same as being closed.
func TestUnknownMembersOfLongKnownRecordsSurviveARewrite(t *testing.T) {
	foreign := `{
	  "v": 4, "rev": 3, "next_seq": 2,
	  "rounds": {
	    "owner/repo#1": {
	      "repo": "owner/repo", "pr": 1, "head": "abcdef123", "seq": 1,
	      "phase": "reviewing", "enqueued_at": "2026-07-26T12:00:00Z",
	      "cobots": {
	        "chatgpt-codex-connector": {
	          "command_id": 7, "answered_at": "2026-07-26T12:03:00Z",
	          "future_bot_detail": {"verdict": "clean"}
	        }
	      }
	    }
	  },
	  "dispatches": {
	    "owner/repo#1": {"host": "mac", "attempts": 1, "future_session_detail": "pid=42"}
	  },
	  "account": {
	    "scope": "owner",
	    "fires": ["2026-07-26T11:00:00Z"],
	    "future_quota_detail": {"window": "week"}
	  }
	}`

	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	round := st.Rounds["owner/repo#1"]
	if len(st.Account.Fires) != 1 || round.Co(codexCoBotKey).CommandID != 7 {
		t.Fatalf("known fields must still decode: %+v", st.Account)
	}

	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	account, _ := back["account"].(map[string]any)
	quota, _ := account["future_quota_detail"].(map[string]any)
	if quota == nil || quota["window"] != "week" {
		t.Errorf("the account quota dropped a member: %#v", account)
	}
	rounds, _ := back["rounds"].(map[string]any)
	written, _ := rounds["owner/repo#1"].(map[string]any)
	cobots, _ := written["cobots"].(map[string]any)
	codex, _ := cobots["chatgpt-codex-connector"].(map[string]any)
	detail, _ := codex["future_bot_detail"].(map[string]any)
	if detail == nil || detail["verdict"] != "clean" {
		t.Errorf("a co-reviewer's entry dropped a member: %#v", codex)
	}
	dispatches, _ := back["dispatches"].(map[string]any)
	claim, _ := dispatches["owner/repo#1"].(map[string]any)
	if claim == nil || claim["future_session_detail"] != "pid=42" {
		t.Errorf("a dispatch claim dropped a member: %#v", claim)
	}
}

// A repository's reviewer override is nested the same way the fleet record is:
// State recognises "repos" by name and hands each value to an ordinary decoder,
// so only the record itself can carry a member a newer binary added inside it.
// Without this, an old binary erased PrimaryOff on its next write and the
// repository silently resumed metered primary reviews.
func TestUnknownRepoOverrideFieldsSurviveARewrite(t *testing.T) {
	foreign := `{
	  "v": 4, "rev": 3, "next_seq": 1,
	  "repos": {
	    "owner/repo": {
	      "cobots": ["codex"], "set_cobots": true,
	      "primary_off": true,
	      "future_reviewer_flag": {"mode": "strict"}
	    }
	  },
	  "account": {"scope": "owner"}
	}`

	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	ov, ok := st.RepoOverride("owner/repo")
	if !ok || !ov.PrimaryOff || !ov.SetCoBots {
		t.Fatalf("known fields must still decode: %+v", ov)
	}

	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	repos, _ := back["repos"].(map[string]any)
	own, _ := repos["owner/repo"].(map[string]any)
	if own == nil {
		t.Fatalf("the override vanished:\n%s", out)
	}
	if own["primary_off"] != true {
		t.Errorf("primary_off was not written back: %#v", own)
	}
	carried, _ := own["future_reviewer_flag"].(map[string]any)
	if carried == nil || carried["mode"] != "strict" {
		t.Errorf("a member this binary does not know was dropped: %#v", own["future_reviewer_flag"])
	}
}

func TestUnknownEnrollmentFieldsSurviveARewrite(t *testing.T) {
	foreign := `{
	  "v": 4, "rev": 3, "next_seq": 1,
	  "enrolled": {
	    "owner/repo": {
	      "enabled": false, "reason": "paused for the quarter",
	      "future_enroll_flag": {"until": "2030-01-01"}
	    }
	  },
	  "account": {"scope": "owner"}
	}`

	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	rec, ok := st.Enrollment("owner/repo")
	if !ok || rec.Enabled || rec.Reason != "paused for the quarter" {
		t.Fatalf("known fields must still decode: %+v", rec)
	}

	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	enrolled, _ := back["enrolled"].(map[string]any)
	own, _ := enrolled["owner/repo"].(map[string]any)
	if own == nil {
		t.Fatalf("the enrollment record vanished:\n%s", out)
	}
	if own["reason"] != "paused for the quarter" {
		t.Errorf("reason was not written back: %#v", own)
	}
	carried, _ := own["future_enroll_flag"].(map[string]any)
	if carried == nil || carried["until"] != "2030-01-01" {
		t.Errorf("a member this binary does not know was dropped: %#v", own["future_enroll_flag"])
	}
}

func TestUnknownAutofixSwitchFieldsSurviveReplacement(t *testing.T) {
	foreign := `{
	  "v": 6, "rev": 3, "next_seq": 1,
	  "repo_autofix": {
	    "owner/repo": {
	      "enabled": true,
	      "future_fix_policy": {"sandbox": "strict"}
	    }
	  }
	}`
	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	st.SetAutofixSwitch("owner/repo", RepoAutofixSwitch{Enabled: false, Reason: "paused"})
	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	switches, _ := back["repo_autofix"].(map[string]any)
	sw, _ := switches["owner/repo"].(map[string]any)
	future, _ := sw["future_fix_policy"].(map[string]any)
	if sw["enabled"] != false || future["sandbox"] != "strict" {
		t.Fatalf("replaced switch lost known or future fields: %#v", sw)
	}
}

// A host report is nested twice over: State recognises "host_reports" and hands
// each record to an ordinary decoder, which in turn hands each tool probe to
// another. So neither the map nor the report above it can carry a member a
// newer binary added inside them — only the records themselves can.
func TestUnknownHostReportFieldsSurviveARewrite(t *testing.T) {
	foreign := `{
	  "v": 4, "rev": 3, "next_seq": 1,
	  "host_reports": {
	    "atlas": {
	      "host": "atlas", "version": "1.2.3", "caps": 6,
	      "roles": ["autofix"],
	      "tools": [{"name": "claude", "path": "/usr/bin/claude", "future_tool_flag": "sandboxed"}],
	      "at": "2026-07-26T12:00:00Z",
	      "future_host_flag": {"gpu": true}
	    }
	  },
	  "account": {"scope": "owner"}
	}`

	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	rec := st.HostReports["atlas"]
	if rec.Version != "1.2.3" || rec.Caps != 6 || len(rec.Tools) != 1 || rec.Tools[0].Path != "/usr/bin/claude" {
		t.Fatalf("known fields must still decode: %+v", rec)
	}

	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	reports, _ := back["host_reports"].(map[string]any)
	atlas, _ := reports["atlas"].(map[string]any)
	if atlas == nil {
		t.Fatalf("the host report vanished:\n%s", out)
	}
	carried, _ := atlas["future_host_flag"].(map[string]any)
	if carried == nil || carried["gpu"] != true {
		t.Errorf("a report member this binary does not know was dropped: %#v", atlas["future_host_flag"])
	}
	tools, _ := atlas["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("the tool probes vanished: %#v", atlas["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["future_tool_flag"] != "sandboxed" {
		t.Errorf("a tool member this binary does not know was dropped: %#v", tool)
	}
}

// Round-tripping is only half of it. A host's self-report is CONSTRUCTED fresh
// on every heartbeat, so however carefully the load preserved a newer binary's
// members, the next report by an older service on the same machine replaced the
// record with a value carrying none — erasing them on a timer, at both levels of
// nesting.
func TestAnOlderHeartbeatKeepsANewerBinarysHostMembers(t *testing.T) {
	foreign := `{
	  "v": 4, "rev": 3, "next_seq": 1,
	  "host_reports": {
	    "atlas": {
	      "host": "atlas", "version": "9.9.9", "caps": 99,
	      "roles": ["autofix"],
	      "role_seen": {"autofix": "2026-07-26T11:55:00Z"},
	      "role_tools": {"autofix": [{"name": "claude", "path": "/new/claude", "future_tool_flag": "sandboxed"}]},
	      "at": "2026-07-26T11:55:00Z",
	      "future_host_flag": {"gpu": true}
	    }
	  },
	  "account": {"scope": "owner"}
	}`

	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	// The same machine's older service, reporting what IT can see.
	st.SetHostReport(HostReport{
		Host: "atlas", Version: "1.0.0", Caps: 1, Roles: []string{"autofix"},
		Tools: []ToolReport{{Name: "claude", Path: "/old/claude"}},
	}, now)

	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	reports, _ := back["host_reports"].(map[string]any)
	atlas, _ := reports["atlas"].(map[string]any)
	if atlas == nil {
		t.Fatalf("the host report vanished:\n%s", out)
	}
	// The probe itself is the reporter's to state — a path that changed changed.
	if atlas["version"] != "1.0.0" {
		t.Errorf("version = %#v, want the reporting binary's own", atlas["version"])
	}
	carried, _ := atlas["future_host_flag"].(map[string]any)
	if carried == nil || carried["gpu"] != true {
		t.Errorf("a report member this binary does not know was erased by a heartbeat: %#v", atlas)
	}
	roleTools, _ := atlas["role_tools"].(map[string]any)
	probed, _ := roleTools["autofix"].([]any)
	if len(probed) != 1 {
		t.Fatalf("the role's probes vanished: %#v", roleTools)
	}
	tool, _ := probed[0].(map[string]any)
	if tool["path"] != "/old/claude" {
		t.Errorf("path = %#v, want the fresh probe's answer", tool["path"])
	}
	if tool["future_tool_flag"] != "sandboxed" {
		t.Errorf("a tool member this binary does not know was erased by a re-probe: %#v", tool)
	}
}

// Carrying a member is not enough on its own: a record whose last RECOGNISED
// field is cleared must still read as recorded, or the writer that treats
// "empty" as "delete this" erases the newer binary's setting on the one path
// the round trip exists to survive.
func TestClearingTheLastKnownFieldKeepsACarriedMember(t *testing.T) {
	foreign := `{
	  "v": 4, "rev": 2, "next_seq": 1,
	  "fleet": {
	    "min_interval": "90s",
	    "future_pacing": {"burst": 3},
	    "solver": {"model": "opus", "future_solver_flag": "sandbox"}
	  }
	}`

	var st State
	if err := json.Unmarshal([]byte(foreign), &st); err != nil {
		t.Fatal(err)
	}
	if st.Fleet.Solver.Empty() {
		t.Error("a solver record carrying a newer binary's member is not empty")
	}
	if st.Fleet.Empty() {
		t.Error("fleet defaults carrying a newer binary's member are not empty")
	}

	// An operator clearing every setting THIS build knows, field by field.
	fd := st.Fleet
	fd.MinInterval = ""
	fd.Solver.Model = ""
	if fd.Solver.Empty() || fd.Empty() {
		t.Fatalf("clearing the known fields must not empty a carrying record: %+v", fd)
	}
	st.SetFleetDefaults(fd, "atlas", time.Now())

	out, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "future_pacing") || !strings.Contains(string(out), "future_solver_flag") {
		t.Errorf("a carried member was erased by clearing the fields around it:\n%s", out)
	}
}
