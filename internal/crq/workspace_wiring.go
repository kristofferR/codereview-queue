package crq

import (
	"context"

	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
	"github.com/kristofferR/codereview-queue/internal/workspace"
)

// workspace wires crq's configured cache root and GitHub credential resolution
// into the reusable workspace implementation.
func (s *Service) workspace(context.Context) workspace.Workspace {
	return workspace.Workspace{
		Root:        s.cfg.WorkspaceRoot,
		TokenSource: ghapi.LookupToken,
	}
}

// gitDir keeps command-local Git probes in crq while the process and error
// handling live with the rest of the Git implementation.
func gitDir(ctx context.Context, dir string, args ...string) (string, error) {
	return workspace.Git(ctx, dir, args...)
}
