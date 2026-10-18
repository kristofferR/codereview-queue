//go:build !linux

package crq

import "errors"

// runReaped needs PR_SET_CHILD_SUBREAPER, which only Linux has. Elsewhere the
// session execs the agent and relies on the watcher's process-group sweep.
func runReaped(string, []string) (int, error) {
	return 0, errors.ErrUnsupported
}
