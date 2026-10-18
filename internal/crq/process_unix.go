//go:build darwin || linux

package crq

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// dispatchStopGrace is how long a stopped session's leader has to exit after
// SIGTERM before it is killed outright.
const dispatchStopGrace = 10 * time.Second

// configureDispatchProcess gives one fix session its own process group. A
// canceled watcher must stop the agent and every shell, test, or git process it
// started before the dispatch claim and worktree are released. The group gets
// SIGTERM rather than SIGKILL so `crq fix-session` can first kill the detached
// processes only it can reach (see runReaped).
func configureDispatchProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return signalDispatchGroup(cmd, syscall.SIGTERM) }
	cmd.WaitDelay = dispatchStopGrace
}

// sweepDispatchProcess kills what is left of a finished session's process
// group and waits briefly for it to go. Every session ends with it, not only a
// canceled one: a background job the agent never stopped would otherwise
// outlive its worktree in the daemon, or still be writing while it is removed.
func sweepDispatchProcess(cmd *exec.Cmd) {
	if signalDispatchGroup(cmd, syscall.SIGKILL) != nil {
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && signalDispatchGroup(cmd, 0) == nil {
		time.Sleep(20 * time.Millisecond)
	}
}

func signalDispatchGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}
