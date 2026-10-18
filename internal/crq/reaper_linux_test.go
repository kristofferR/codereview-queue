//go:build linux

package crq

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunReapedKillsDetachedDescendants runs the reaper in a child test
// process: it makes its caller a subreaper and reaps any child, which would
// steal exit statuses from the rest of this package's tests.
func TestRunReapedKillsDetachedDescendants(t *testing.T) {
	if script := os.Getenv("CRQ_TEST_REAPER_SCRIPT"); script != "" {
		code, err := runReaped("/bin/sh", []string{"sh", "-c", script})
		if err != nil {
			t.Fatal(err)
		}
		os.Exit(code)
	}

	for _, tc := range []struct {
		name     string
		script   string
		stop     bool
		wantCode int
	}{
		// The agent exits, leaving a process it detached into its own session.
		{name: "agent exits", script: `setsid sleep 60 & echo $! > "$PID_FILE"; exit 3`, wantCode: 3},
		// The watcher stops the session while the agent is still running.
		{name: "session stopped", script: `setsid sleep 60 & echo $! > "$PID_FILE"; sleep 60`, stop: true, wantCode: 128 + int(syscall.SIGKILL)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pid")
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunReapedKillsDetachedDescendants$")
			cmd.Env = append(os.Environ(), "CRQ_TEST_REAPER_SCRIPT="+tc.script, "PID_FILE="+pidFile)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			detached := waitForPID(t, pidFile)
			if tc.stop {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			var exitErr *exec.ExitError
			if err := cmd.Wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.wantCode {
				t.Fatalf("reaper exit = %v, want code %d", err, tc.wantCode)
			}
			if err := syscall.Kill(detached, 0); !errors.Is(err, syscall.ESRCH) {
				_ = syscall.Kill(detached, syscall.SIGKILL)
				t.Fatalf("detached process %d outlived the session: %v", detached, err)
			}
		})
	}
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(body), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the session never started its detached process")
	return 0
}
