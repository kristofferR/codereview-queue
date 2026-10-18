package crq

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// FixSession runs one fix session: it is what `crq watch --dispatch` executes
// per pull request.
//
// It exists so crq is ONE binary. The install used to write two bash scripts —
// a wrapper that started the watcher, and a session script that assembled the
// agent's command line — which meant three things had to agree about the
// configuration, two of them generated text on disk that no test ever ran. A
// setting added here reached a fleet only after every host reinstalled, and
// nothing said which hosts had not.
//
// The environment it reads is set by the dispatcher (watch.go):
//
//	CRQ_FIX_AGENT   the agent binary, chosen at install time on this machine
//	CRQ_FIX_ARGS    extra arguments the operator configured
//	CRQ_FIX_PROMPT_FILE  the instruction file
//	CRQ_FIX_MODEL / CRQ_FIX_EFFORT / CRQ_FIX_PROMPT  the repository's settings
//
// On Linux it stays as the agent's parent and exits with the agent's status
// once everything the agent started is gone (see runReaped). Elsewhere it execs
// the agent, so the process the watcher supervises IS the agent. Either way
// this call does not return on success.
func FixSession(ctx context.Context, cfg Config) error {
	agent := cfg.fixAgent()
	if agent == "" {
		return errors.New("no fix agent configured (CRQ_FIX_AGENT); run crq autofix install")
	}
	resolved, err := exec.LookPath(agent)
	if err != nil {
		return fmt.Errorf("fix agent %q: %w", agent, err)
	}

	prompt, err := fixSessionPrompt()
	if err != nil {
		return err
	}
	argv := fixSessionArgv(resolved, prompt, SplitArgv(os.Getenv("CRQ_FIX_ARGS")),
		os.Getenv("CRQ_FIX_MODEL"), os.Getenv("CRQ_FIX_EFFORT"))

	code, err := runReaped(resolved, argv)
	if errors.Is(err, errors.ErrUnsupported) {
		return syscall.Exec(resolved, argv, os.Environ())
	}
	if err != nil {
		return fmt.Errorf("fix agent %q: %w", agent, err)
	}
	os.Exit(code)
	return nil
}

// fixSessionPrompt is the instruction file with the repository's standing
// addition, when it has one.
func fixSessionPrompt() (string, error) {
	path := strings.TrimSpace(os.Getenv("CRQ_FIX_PROMPT_FILE"))
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, ".local", "share", "crq", "fix-prompt.txt")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading the fix prompt %q: %w", path, err)
	}
	prompt := string(body)
	if extra := strings.TrimSpace(os.Getenv("CRQ_FIX_PROMPT")); extra != "" {
		prompt += "\n\nAdditional instructions for this repository:\n" + extra
	}
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("fix prompt %q is empty", path)
	}
	return prompt, nil
}

// fixSessionArgv builds the agent's command line.
//
// An empty model or effort adds no flag at all rather than an empty one: every
// agent rejects an empty value differently and none ignores it, so a session
// would die on its first argument and the fix would silently never happen.
func fixSessionArgv(agent, prompt string, extra []string, model, effort string) []string {
	model, effort = strings.TrimSpace(model), strings.TrimSpace(effort)
	argv := []string{agent}

	switch filepath.Base(agent) {
	case "claude":
		// stream-json so the session log fills as it works rather than only at
		// the end, where a hung session would leave an empty file.
		argv = append(argv, "-p", prompt,
			"--permission-mode", "bypassPermissions",
			"--output-format", "stream-json", "--verbose")
		if model != "" {
			argv = append(argv, "--model", model)
		}
		if effort != "" {
			argv = append(argv, "--effort", effort)
		}
		argv = append(argv, extra...)
	case "codex":
		// exec is codex's non-interactive form; the prompt is its final
		// positional argument. --skip-git-repo-check because the session runs
		// in a detached worktree crq created.
		argv = append(argv, "exec", "--json", "--skip-git-repo-check",
			"--dangerously-bypass-approvals-and-sandbox")
		if model != "" {
			argv = append(argv, "--model", model)
		}
		if effort != "" {
			argv = append(argv, "-c", `model_reasoning_effort="`+effort+`"`)
		}
		argv = append(argv, extra...)
		argv = append(argv, prompt)
	default:
		// An unknown executable is a self-contained prompt-taking wrapper. It
		// gets no flags invented for it; the environment still reaches it.
		argv = append(argv, extra...)
		argv = append(argv, prompt)
	}
	return argv
}
