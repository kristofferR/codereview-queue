package crq

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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

func TestAutoreviewInstallRequiresAWritableGateway(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		response  string
		wantError string
	}{
		{name: "writable", status: http.StatusOK, response: `{"ok":true}`},
		{name: "read only", status: http.StatusForbidden, response: `{"ok":false,"error":"the GitHub gateway is read-only"}`, wantError: "read-only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/gateway/health" || r.URL.Query().Get("write") != "1" {
					http.NotFound(w, r)
					return
				}
				if r.Header.Get("Authorization") != "Bearer secret" {
					t.Error("gateway probe did not carry the configured token")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.response))
			}))
			defer server.Close()

			cfg := firingConfig()
			cfg.ServerURL, cfg.ServerToken = server.URL, "secret"
			svc := NewService(cfg, newFakeGitHub(), NewMemoryStore(cfg), nil)
			err := svc.serviceCanUseGateway(t.Context(), "autoreview")
			if tc.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("gateway validation error = %v, want %q", err, tc.wantError)
			}
		})
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
