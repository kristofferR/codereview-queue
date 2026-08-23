package crq

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type installLoadErrorStore struct{ StateStore }

func (installLoadErrorStore) Load(context.Context) (State, Revision, error) {
	return State{}, Revision{}, errors.New("gateway unavailable")
}

func TestServeUnitCarriesShellProvidedConfiguration(t *testing.T) {
	env := map[string]string{
		"CRQ_REPO": "owner/gate", "CRQ_STATE_REF": "custom-state",
		"CRQ_SCOPE": "owner,second", "CRQ_HOST": "testhost", "CRQ_COBOTS": "",
		"CRQ_DRY_RUN": "1", "CRQ_NO_OPEN": "1",
		"CRQ_DISPATCH_REPO": "owner/pr", "CRQ_DISPATCH_PR": "7",
		"CRQ_DISPATCH_HEAD": "abcdef", "CRQ_DISPATCH_FINDINGS": "/tmp/findings.json",
	}
	for key, value := range env {
		t.Setenv(key, value)
	}
	cfg, err := BuildConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
	plan, err := svc.InstallAutoReview(context.Background(), true, true)
	if err != nil {
		t.Fatal(err)
	}
	unit := serveUnitBody(plan)
	for _, want := range []string{
		"CRQ_REPO=owner/gate",
		"CRQ_STATE_REF=custom-state",
		"CRQ_SCOPE=owner,second",
		"CRQ_HOST=testhost",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit does not carry %q:\n%s", want, unit)
		}
	}
	if strings.Contains(unit, "GITHUB_TOKEN") || strings.Contains(unit, "GH_TOKEN") {
		t.Errorf("unit carries a GitHub credential:\n%s", unit)
	}
	for _, excluded := range []string{
		"CRQ_DRY_RUN", "CRQ_NO_OPEN", "CRQ_DISPATCH_REPO", "CRQ_DISPATCH_PR",
		"CRQ_DISPATCH_HEAD", "CRQ_DISPATCH_FINDINGS",
	} {
		if strings.Contains(unit, excluded) {
			t.Errorf("unit carries excluded %s:\n%s", excluded, unit)
		}
	}
}

func TestServeInstallPreservesPollInterval(t *testing.T) {
	plan := ServeInstall{
		Service: "serve", Binary: "/usr/bin/crq", Addr: "127.0.0.1:7777",
		Poll: (30 * time.Second).String(),
	}
	got := strings.Join(serveArgv(plan), " ")
	if !strings.Contains(got, "--poll 30s") {
		t.Fatalf("serve argv = %q, want the configured poll interval", got)
	}
}

func TestAutoreviewInstallValidatesItsServerBackedStateTransport(t *testing.T) {
	t.Setenv("CRQ_CONFIG", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", t.TempDir())

	cfg := firingConfig()
	store := NewMemoryStore(cfg)
	svc := NewService(cfg, newFakeGitHub(), store, nil)
	if err := svc.serviceCanStart(t.Context(), "autoreview"); err != nil {
		t.Fatalf("server-backed autoreview validation required a local GitHub credential: %v", err)
	}

	svc.store = installLoadErrorStore{StateStore: store}
	err := svc.serviceCanStart(t.Context(), "autoreview")
	if err == nil || !strings.Contains(err.Error(), "gateway unavailable") {
		t.Fatalf("unreadable server-backed state validation error = %v", err)
	}
}

func TestServiceInstallersHonourConfiguredDryRun(t *testing.T) {
	cfg := firingConfig()
	cfg.DryRun = true
	svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)

	serve, err := svc.InstallServe(context.Background(), "", nil, false, 0, false, true)
	if err != nil {
		t.Fatal(err)
	}
	autoreview, err := svc.InstallAutoReview(context.Background(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range []ServeInstall{serve, autoreview} {
		if !plan.DryRun || plan.Started {
			t.Fatalf("configured dry-run %s installation applied its plan: %+v", plan.Service, plan)
		}
	}
}

func TestSystemdEnvironmentPreservesUnicodeAndUsesSupportedEscapes(t *testing.T) {
	value := "CRQ_FIX_PROMPT=blå\u0085\t\"quoted\""
	got := systemdEnvironment(value)
	want := "\"CRQ_FIX_PROMPT=blå\u0085\\t\\\"quoted\\\"\""
	if got != want {
		t.Fatalf("systemd environment = %q, want %q", got, want)
	}
	if got := systemdEnvironment("CRQ_FIX_PROMPT=" + string([]byte{0xff})); got != `"CRQ_FIX_PROMPT=\xff"` {
		t.Fatalf("invalid UTF-8 environment = %q, want a lossless hex escape", got)
	}
}
