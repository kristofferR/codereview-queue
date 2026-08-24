package crq

import (
	"context"
	"testing"
	"time"
)

func TestSameHostReportComparesCapabilitiesForTheReportingRole(t *testing.T) {
	prev := HostReport{
		Version: "2.1.0", Caps: 3,
		RoleCaps:     map[string]int{"autofix": 3, "serve": 1},
		RoleVersions: map[string]string{"autofix": "2.1.0", "serve": "2.0.0"},
	}
	serve := HostReport{Version: "2.0.0", Caps: 1, Roles: []string{"serve"}}
	if !sameHostReport(prev, serve) {
		t.Fatal("an unchanged older serve role was treated as different from another role's newer report")
	}
	serve.Caps = 2
	if sameHostReport(prev, serve) {
		t.Fatal("a changed serve capability was not detected")
	}
	serve.Caps, serve.Version = 1, "2.1.0"
	if sameHostReport(prev, serve) {
		t.Fatal("an upgraded serve binary was not detected when its capabilities stayed the same")
	}
}

func TestSameHostReportLetsAutofixClearItsAgent(t *testing.T) {
	prev := HostReport{Agent: "claude", Roles: []string{"autofix"}}
	if sameHostReport(prev, HostReport{Roles: []string{"autofix"}}) {
		t.Fatal("autofix removing its agent must be recorded")
	}
	if !sameHostReport(prev, HostReport{Roles: []string{"serve"}}) {
		t.Fatal("a role that does not choose the fix agent must preserve it silently")
	}
}

func TestForgetHostRemovesOneRecord(t *testing.T) {
	ctx := context.Background()
	cfg := Config{GateRepo: "o/gate", Host: "omarchy"}
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	now := time.Now().UTC()
	if _, err := store.Update(ctx, func(st *State) error {
		st.SetHostReport(HostReport{Host: "MacBookAir", Roles: []string{"autoreview"}}, now)
		st.SetHostReport(HostReport{Host: "omarchy", Roles: []string{"serve"}}, now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	result, err := svc.ForgetHost(ctx, "MacBookAir")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Forgot || result.Hosts == nil {
		t.Fatalf("ForgetHost = %+v, want the record dropped and the survivors listed", result)
	}
	if len(result.Hosts) != 1 || result.Hosts[0] != "omarchy" {
		t.Fatalf("remaining hosts = %v, want just omarchy", result.Hosts)
	}

	// A name nobody reported is not an error: it is the answer to "is this gone".
	again, err := svc.ForgetHost(ctx, "MacBookAir")
	if err != nil {
		t.Fatal(err)
	}
	if again.Forgot || again.Reason == "" {
		t.Fatalf("second ForgetHost = %+v, want forgot=false with a reason", again)
	}
	if _, err := svc.ForgetHost(ctx, "  "); err == nil {
		t.Fatal("a blank host name should be refused, not matched against records")
	}
}
