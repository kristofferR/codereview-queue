package state

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/kristofferR/codereview-queue/internal/gh"
)

// refServer serves the four git-data reads Load makes, over one state payload.
func refServer(t *testing.T, payload string) *gh.GitHub {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		switch {
		case strings.Contains(r.URL.Path, "/git/ref/"):
			enc.Encode(map[string]any{"object": map[string]string{"sha": "commitsha"}})
		case strings.Contains(r.URL.Path, "/git/commits/"):
			enc.Encode(map[string]any{"sha": "commitsha", "tree": map[string]string{"sha": "treesha"}})
		case strings.Contains(r.URL.Path, "/git/trees/"):
			enc.Encode(map[string]any{"sha": "treesha", "tree": []map[string]string{
				{"path": statePath, "type": "blob", "sha": "blobsha"},
			}})
		case strings.Contains(r.URL.Path, "/git/blobs/"):
			enc.Encode(map[string]any{"sha": "blobsha", "encoding": "utf-8", "content": payload})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return gh.NewTestClient(srv.URL, srv.Client())
}

func versionStore(t *testing.T, payload string) *GitStateStore {
	t.Helper()
	cfg := StoreConfig{GateRepo: "owner/state", StateRef: "crq-state-v3", DashboardIssue: 1, Scope: []string{"owner"}}
	return NewGitStateStore(cfg, refServer(t, payload), nil)
}

// The fleet runs mixed binary versions during a rolling deploy. An old binary
// meeting state a newer one wrote used to reinitialize — so the first stale
// process to wake up erased every live round in the account, silently, in the
// exact situation the version field exists to detect. It must refuse instead.
func TestLoadRefusesStateFromANewerBinary(t *testing.T) {
	store := versionStore(t, `{"v":99,"rounds":{"owner/repo#7":{"repo":"owner/repo","pr":7,"phase":"fired"}}}`)

	_, _, err := store.Load(context.Background())
	if err == nil {
		t.Fatal("a newer schema must be refused, not reinitialized over")
	}
	// The message has to be actionable: which ref, and how to reset on purpose.
	for _, want := range []string{"crq-state-v3", "v99", "Upgrade crq"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A current or migratable payload this binary cannot decode describes live
// rounds and must not be discarded.
func TestLoadRefusesUndecodableCurrentState(t *testing.T) {
	for _, version := range []int{SchemaVersion - 1, SchemaVersion} {
		payload := fmt.Sprintf(`{"v":%d,"rounds":"not-a-map"}`, version)
		if _, _, err := versionStore(t, payload).Load(context.Background()); err == nil {
			t.Fatalf("an undecodable v%d payload must be refused, not reinitialized over", version)
		}
	}
	if _, _, err := versionStore(t, `not json at all`).Load(context.Background()); err == nil {
		t.Fatal("an unparseable payload must be refused")
	}
}

func TestLoadMigratesV5WithoutLosingLiveRounds(t *testing.T) {
	payload := `{
		"v":5,
		"rev":7,
		"next_seq":2,
		"rounds":{
			"owner/repo#7":{
				"repo":"owner/repo",
				"pr":7,
				"head":"abcdef123",
				"seq":1,
				"phase":"queued",
				"enqueued_at":"2026-07-26T12:00:00Z"
			}
		},
		"account":{"scope":"owner"},
		"fleet":{
			"scope":"owner,friend",
			"repos":"owner/repo,friend/private",
			"exclude":"owner/paused",
			"required-bots":"coderabbitai[bot],cursor[bot]",
			"cobots":"bugbot,codex",
			"min-interval":" 2m ",
			"inflight-timeout":"10m",
			"cobot-bugbot-trigger":"always",
			"future-policy":"keep"
		},
		"future_top_level":{"keep":true}
	}`
	st, _, err := versionStore(t, payload).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != SchemaVersion {
		t.Errorf("version = %d, want migrated v%d", st.Version, SchemaVersion)
	}
	if round := st.Round("owner/repo", 7); round == nil || round.Head != "abcdef123" {
		t.Fatalf("live v4 round was lost during migration: %+v", round)
	}
	if !st.Fleet.SetRequired || strings.Join(st.Fleet.Required, ",") != "coderabbitai[bot],cursor[bot]" {
		t.Errorf("required = %v (set %v), want migrated v5 policy", st.Fleet.Required, st.Fleet.SetRequired)
	}
	if !st.Fleet.SetCoBots || strings.Join(st.Fleet.CoBots, ",") != "bugbot,codex" {
		t.Errorf("cobots = %v (set %v), want migrated v5 policy", st.Fleet.CoBots, st.Fleet.SetCoBots)
	}
	if st.Fleet.MinInterval != "2m" || st.Fleet.Env["CRQ_SCOPE"] != "owner,friend" ||
		st.Fleet.Env["CRQ_REPOS"] != "owner/repo,friend/private" ||
		st.Fleet.Env["CRQ_EXCLUDE"] != "owner/paused" ||
		st.Fleet.Env["CRQ_INFLIGHT_TIMEOUT"] != "10m" ||
		st.Fleet.Env["CRQ_COBOT_BUGBOT_TRIGGER"] != "always" {
		t.Errorf("fleet defaults were not migrated completely: %+v", st.Fleet)
	}
	encoded, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "future_top_level") {
		t.Fatalf("unknown v5 state was lost during migration: %s", encoded)
	}
	if !strings.Contains(string(encoded), "future-policy") {
		t.Fatalf("unknown v5 fleet policy was lost during migration: %s", encoded)
	}
}

func TestLoadMigratesAlreadyNestedV5FleetDefaults(t *testing.T) {
	payload := `{
		"v":5,
		"rounds":{},
		"fleet":{
			"cobots":["codex"],
			"set_cobots":true,
			"required":["coderabbitai[bot]"],
			"set_required":true,
			"min_interval":"3m",
			"env":{"CRQ_SETTLE":"45s"},
			"scope":"owner"
		}
	}`
	st, _, err := versionStore(t, payload).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != SchemaVersion || !st.Fleet.SetCoBots || !st.Fleet.SetRequired ||
		strings.Join(st.Fleet.CoBots, ",") != "codex" ||
		strings.Join(st.Fleet.Required, ",") != "coderabbitai[bot]" ||
		st.Fleet.MinInterval != "3m" || st.Fleet.Env["CRQ_SETTLE"] != "45s" ||
		st.Fleet.Env["CRQ_SCOPE"] != "owner" {
		t.Fatalf("nested v5 fleet defaults were not preserved: %+v", st.Fleet)
	}
}

func TestLoadMergesNestedAndFlatV5FleetEnvironment(t *testing.T) {
	payload := `{
		"v":5,
		"rounds":{},
		"fleet":{
			"env":{
				"CRQ_SETTLE":"45s",
				"CRQ_SCOPE":"nested-owner",
				"FUTURE_SETTING":{"mode":"careful"}
			},
			"scope":"flat-owner",
			"inflight-timeout":"10m"
		}
	}`
	st, _, err := versionStore(t, payload).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Fleet.Env["CRQ_SETTLE"] != "45s" ||
		st.Fleet.Env["CRQ_SCOPE"] != "flat-owner" ||
		st.Fleet.Env["CRQ_INFLIGHT_TIMEOUT"] != "10m" {
		t.Fatalf("mixed-shape v5 environment was not merged: %+v", st.Fleet.Env)
	}
	encoded, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip struct {
		Fleet struct {
			Env map[string]json.RawMessage `json:"env"`
		} `json:"fleet"`
	}
	if err := json.Unmarshal(encoded, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if got := string(roundTrip.Fleet.Env["FUTURE_SETTING"]); got != `{"mode":"careful"}` {
		t.Fatalf("future nested environment member = %s, want it preserved", got)
	}
}

// An OLDER payload is genuinely obsolete: crq is pre-release, there is no
// multi-version migration, and a v3 state describes a world this binary cannot
// act on safely.
func TestLoadReinitializesOlderState(t *testing.T) {
	st, _, err := versionStore(t, `{"v":3,"queue":[{"repo":"owner/repo","pr":7}]}`).Load(context.Background())
	if err != nil {
		t.Fatalf("an older schema must reinitialize, got %v", err)
	}
	if st.Version != SchemaVersion {
		t.Errorf("version = %d, want a fresh v%d", st.Version, SchemaVersion)
	}
	if len(st.Rounds) != 0 {
		t.Errorf("rounds = %v, want none carried from v3", st.Rounds)
	}
}
