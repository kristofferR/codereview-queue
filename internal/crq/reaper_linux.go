//go:build linux

package crq

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// prSetChildSubreaper is PR_SET_CHILD_SUBREAPER from <linux/prctl.h>.
const prSetChildSubreaper = 36

// runReaped runs the agent as a child of this process and returns its exit
// status once the agent and everything it started are gone.
//
// Exec'ing the agent was not enough. A process the session detached (a
// browser launched with setsid, a daemonized dev server) was reparented to the
// service manager, out of reach of the watcher's process-group kill, so every
// session could leave one more behind until systemd-oomd killed the whole
// autofix unit. As a child subreaper this process inherits those orphans
// instead, and kills them all when the agent exits or the watcher stops the
// session with SIGTERM.
func runReaped(path string, argv []string) (int, error) {
	// Without /proc the descendants cannot be found, and a reaper that cannot
	// see them would only claim a cleanup it never did.
	if _, err := os.ReadDir("/proc"); err != nil {
		return 0, fmt.Errorf("%w: %v", errors.ErrUnsupported, err)
	}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		return 0, fmt.Errorf("%w: PR_SET_CHILD_SUBREAPER: %v", errors.ErrUnsupported, errno)
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	agent, err := os.StartProcess(path, argv, &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
	if err != nil {
		return 0, err
	}
	go func() {
		for range stop {
			killDescendants()
		}
	}()
	code := waitAgent(agent.Pid)
	reapDescendants(5 * time.Second)
	return code, nil
}

// waitAgent reaps children until the agent itself exits, so orphans that end
// while it runs do not linger as zombies.
func waitAgent(pid int) int {
	for {
		var status syscall.WaitStatus
		reaped, err := syscall.Wait4(-1, &status, 0, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return 1
		}
		if reaped != pid {
			continue
		}
		if status.Signaled() {
			return 128 + int(status.Signal())
		}
		return status.ExitStatus()
	}
}

// reapDescendants kills what is left of the session and reaps it. The kill is
// repeated because a process can fork between the scan and its death, leaving
// a new orphan for this process to inherit.
func reapDescendants(limit time.Duration) {
	deadline := time.Now().Add(limit)
	for {
		killDescendants()
		for {
			pid, err := syscall.Wait4(-1, nil, syscall.WNOHANG, nil)
			if errors.Is(err, syscall.ECHILD) {
				return
			}
			if err != nil || pid <= 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// killDescendants SIGKILLs every process below this one. As a subreaper, this
// process is an ancestor of everything the session started, detached or not.
func killDescendants() {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	children := map[int][]int{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		// The command name is parenthesised and may contain ") ", so the
		// fields "state ppid ..." start after the LAST parenthesis.
		end := bytes.LastIndexByte(stat, ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(string(stat[end+1:]))
		if len(fields) < 2 {
			continue
		}
		if ppid, err := strconv.Atoi(fields[1]); err == nil {
			children[ppid] = append(children[ppid], pid)
		}
	}
	queue := children[os.Getpid()]
	for len(queue) > 0 {
		pid := queue[0]
		queue = append(queue[1:], children[pid]...)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}
