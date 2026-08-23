package crq

import (
	"context"
	"testing"
)

// The daemon's workspace has to come from the config file, not the process
// environment, and credentials must be resolved when Git runs so token rotation
// does not leave a long-lived checkout with a stale value.
func TestServiceWorkspaceUsesTheConfiguredRootAndCurrentToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_from_the_environment")
	s := &Service{cfg: Config{WorkspaceRoot: "/tmp/crq-configured-root"}}
	ws := s.workspace(context.Background())
	if ws.Root != "/tmp/crq-configured-root" {
		t.Errorf("workspace root = %q, want the configured one", ws.Root)
	}
	if got := ws.TokenSource(context.Background()); got != "ghp_from_the_environment" {
		t.Errorf("workspace token = %q, want the one the API client resolves", got)
	}
}

func TestStateStoreUsesTheCurrentToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_from_the_environment")
	cfg := Config{}.storeConfig()
	if got := cfg.TokenSource(context.Background()); got != "ghp_from_the_environment" {
		t.Errorf("state store token = %q, want the one the API client resolves", got)
	}
}
