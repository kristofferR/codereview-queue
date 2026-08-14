package crq

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/kristofferR/coderabbit-queue/internal/dialect"
)

type PreflightOptions struct {
	Binary     string
	ReviewType string
	Base       string
	BaseCommit string
	Dir        string
	Light      bool
	Timeout    time.Duration
	ExtraArgs  []string
}

type PreflightReport struct {
	Status        string             `json:"status"`
	Tool          string             `json:"tool"`
	Command       []string           `json:"command"`
	ReviewContext map[string]any     `json:"review_context,omitempty"`
	Statuses      []PreflightStatus  `json:"statuses"`
	Complete      map[string]any     `json:"complete,omitempty"`
	Findings      []PreflightFinding `json:"findings"`
	Stderr        string             `json:"stderr,omitempty"`
	Error         string             `json:"error,omitempty"`
	// ErrorType is the CLI's own classification of a failure ("rate_limit",
	// ...). Recoverable and RetryAfter come with it. crq keeps them because the
	// local CLI shares the SAME account quota as the PR reviews the queue
	// serializes: a local rate limit is direct evidence about that quota, given
	// for free, with no probe comment and no GitHub round trip.
	ErrorType   string `json:"error_type,omitempty"`
	Recoverable bool   `json:"recoverable,omitempty"`
	RetryAfter  string `json:"retry_after,omitempty"`
	// OrgAttributed is the CLI's own statement that the limit belongs to the
	// organisation rather than to this user. The captured event distinguishes the
	// two, and a personal limit must never become a fleet-wide block.
	OrgAttributed bool `json:"org_attributed,omitempty"`
	// CredentialsOverridden records that this run authenticated with credentials
	// passed on the command line. The identity probe afterwards cannot reproduce
	// them, so it would report the executable's STORED login instead — possibly a
	// different account than the one that produced the block.
	CredentialsOverridden bool `json:"credentials_overridden,omitempty"`
	// Quota reports whether the account block this run observed was shared with
	// crq's queue, and why not when it was not.
	Quota *CLIQuotaResult `json:"quota,omitempty"`
	// SkipReason/BlockedUntil explain a successful no-op when the shared queue
	// already knows the CodeRabbit account cannot accept another review.
	SkipReason   string     `json:"skip_reason,omitempty"`
	BlockedUntil *time.Time `json:"blocked_until,omitempty"`
	ExitCode     int        `json:"exit_code"`
	CheckedAt    time.Time  `json:"checked_at"`
	DurationMS   int64      `json:"duration_ms"`
}

type PreflightStatus struct {
	Phase   string `json:"phase,omitempty"`
	Status  string `json:"status,omitempty"`
	Message string `json:"message,omitempty"`
}

type PreflightFinding struct {
	ID                  string   `json:"id"`
	Bot                 string   `json:"bot"`
	Severity            string   `json:"severity"`
	Path                string   `json:"path,omitempty"`
	Line                int      `json:"line,omitempty"`
	EndLine             int      `json:"end_line,omitempty"`
	Title               string   `json:"title"`
	Body                string   `json:"body"`
	CodegenInstructions string   `json:"codegen_instructions,omitempty"`
	Suggestions         []string `json:"suggestions,omitempty"`
	Fingerprint         string   `json:"fingerprint,omitempty"`
	Source              string   `json:"source"`
}

// SkipBlockedPreflight reports whether a local review should be skipped because
// crq's shared state already holds a live CodeRabbit account-quota block.
//
// Reading the state is deliberately separate from Preflight: the local command
// must still work without crq configuration or GitHub access, so callers can
// treat a state-read failure as a cache miss and run the CLI normally. Explicit
// credentials bypass the shared block because they may select another account.
func (s *Service) SkipBlockedPreflight(ctx context.Context, opts PreflightOptions, cliOrg func() string) (*PreflightReport, error) {
	if carriesCredentials(opts.ExtraArgs) {
		return nil, nil
	}
	start := time.Now()
	state, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	if !s.cfg.WithFleet(state.Fleet).PreflightSkipBlocked {
		return nil, nil
	}
	now := s.clock()
	if state.Account.BlockedUntil == nil || !state.Account.BlockedUntil.After(now) {
		return nil, nil
	}
	if cliOrg == nil || !cliOrgMatches(s.cfg.WithFleet(state.Fleet), cliOrg()) {
		return nil, nil
	}
	until := state.Account.BlockedUntil.UTC()
	return &PreflightReport{
		Status:       "skipped",
		Tool:         "coderabbit-cli",
		Command:      redactSecrets(append([]string{"coderabbit-cli"}, coderabbitArgs(opts)...)),
		Statuses:     []PreflightStatus{},
		Findings:     []PreflightFinding{},
		SkipReason:   "shared CodeRabbit account quota is blocked",
		BlockedUntil: &until,
		ExitCode:     0,
		CheckedAt:    now,
		DurationMS:   time.Since(start).Milliseconds(),
	}, nil
}

func Preflight(ctx context.Context, opts PreflightOptions) (PreflightReport, int, error) {
	start := time.Now()
	report := PreflightReport{
		Status:    "preflight",
		Statuses:  []PreflightStatus{},
		Findings:  []PreflightFinding{},
		CheckedAt: start.UTC(),
	}
	binary, err := coderabbitBinary(opts.Binary)
	if err != nil {
		report.Status = "error"
		report.Error = err.Error()
		report.ExitCode = 1
		report.DurationMS = time.Since(start).Milliseconds()
		return report, 1, err
	}
	args := coderabbitArgs(opts)
	report.Tool = binary
	report.Command = redactSecrets(append([]string{binary}, args...))
	report.CredentialsOverridden = carriesCredentials(opts.ExtraArgs)

	runCtx, cancel := context.WithCancel(ctx)
	if opts.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
	}
	defer cancel()

	cmd := exec.CommandContext(runCtx, binary, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		report.Status = "error"
		report.Error = err.Error()
		report.ExitCode = 1
		report.DurationMS = time.Since(start).Milliseconds()
		return report, 1, err
	}
	if err := cmd.Start(); err != nil {
		report.Status = "error"
		report.Error = err.Error()
		report.ExitCode = 1
		report.DurationMS = time.Since(start).Milliseconds()
		return report, 1, err
	}
	parseErr := parsePreflightStream(stdout, &report)
	if parseErr != nil {
		// Stop the child and drain remaining stdout so cmd.Wait can't block on a
		// full pipe when parsing bailed early (e.g. on a malformed JSON line).
		cancel()
		io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()
	report.Stderr = trimForReport(stderr.String(), 4000)
	report.DurationMS = time.Since(start).Milliseconds()
	// A genuine parse failure must surface as exit 1 even though we cancel() the
	// child to unblock Wait — our own cancellation would otherwise read back as
	// runCtx.Err() and get misreported as a timeout (exit 2). Distinguish the
	// three cases by their actual cause: caller cancellation (parent ctx), a real
	// timeout (DeadlineExceeded), then a parse failure.
	switch {
	case ctx.Err() != nil:
		report.Status = "error"
		report.Error = ctx.Err().Error()
		report.ExitCode = 2
		return report, 2, ctx.Err()
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		report.Status = "error"
		report.Error = runCtx.Err().Error()
		report.ExitCode = 2
		return report, 2, runCtx.Err()
	case parseErr != nil:
		report.Status = "error"
		report.Error = parseErr.Error()
		report.ExitCode = 1
		return report, 1, parseErr
	}
	if waitErr != nil {
		code := 1
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			code = exitErr.ExitCode()
		}
		report.Status = "error"
		// Keep the CLI's own message. Overwriting it with "exit status 1" threw
		// away the only useful part — a caller was told the command failed but
		// not that the account was rate-limited, nor for how long.
		if report.Error == "" {
			report.Error = waitErr.Error()
		}
		// A rate limit is not a broken setup: nothing is misconfigured and the
		// answer is to come back later, which is what exit 2 already means here.
		if dialect.IsCLIRateLimit(report.ErrorType) {
			report.Status = "rate_limited"
			code = 2
		}
		report.ExitCode = code
		return report, code, waitErr
	}
	if len(report.Findings) == 0 {
		report.Status = "clean"
		report.ExitCode = 0
		return report, 0, nil
	}
	report.Status = "feedback"
	report.ExitCode = 10
	return report, 10, nil
}

func coderabbitBinary(explicit string) (string, error) {
	candidates := []string{explicit, os.Getenv("CRQ_CODERABBIT_BIN"), "cr", "coderabbit"}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		path, err := exec.LookPath(candidate)
		if err == nil {
			return path, nil
		}
	}
	return "", errors.New("CodeRabbit CLI not found (install cr/coderabbit or set CRQ_CODERABBIT_BIN)")
}

func coderabbitArgs(opts PreflightOptions) []string {
	args := []string{"review", "--agent"}
	if opts.Light {
		args = append(args, "--light")
	}
	if opts.ReviewType != "" {
		args = append(args, "--type", opts.ReviewType)
	}
	if opts.Base != "" {
		args = append(args, "--base", opts.Base)
	}
	if opts.BaseCommit != "" {
		args = append(args, "--base-commit", opts.BaseCommit)
	}
	if opts.Dir != "" {
		args = append(args, "--dir", opts.Dir)
	}
	args = append(args, opts.ExtraArgs...)
	return args
}

func parsePreflightStream(r io.Reader, report *PreflightReport) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return fmt.Errorf("failed to parse CodeRabbit --agent JSON line: %w", err)
		}
		applyPreflightEvent(report, event)
	}
	return scanner.Err()
}

func applyPreflightEvent(report *PreflightReport, event map[string]any) {
	switch stringField(event, "type") {
	case "review_context":
		report.ReviewContext = event
	case "status", "heartbeat":
		report.Statuses = append(report.Statuses, PreflightStatus{
			Phase:   stringField(event, "phase"),
			Status:  stringField(event, "status"),
			Message: stringField(event, "message"),
		})
	case "complete":
		report.Complete = event
	case "finding":
		if finding := preflightFinding(event); finding.Title != "" || finding.Body != "" || finding.Path != "" {
			report.Findings = append(report.Findings, finding)
		}
	case "error":
		if msg := firstNonEmpty(stringField(event, "message"), stringField(event, "error")); msg != "" {
			report.Error = msg
		}
		// The event's shape is CodeRabbit's contract, so dialect reads it and this
		// only records what it means.
		cliErr := dialect.ParseCLIError(event)
		report.ErrorType = cliErr.Type
		report.Recoverable = cliErr.Recoverable
		report.RetryAfter = cliErr.WaitTime
		report.OrgAttributed = cliErr.OrgAttributed
	}
}

func preflightFinding(event map[string]any) PreflightFinding {
	codegen := stringField(event, "codegenInstructions")
	comment := stringField(event, "comment")
	body := firstNonEmpty(codegen, comment)
	title := firstNonEmpty(stringField(event, "title"), dialect.TitleOf(body))
	severity := strings.ToLower(firstNonEmpty(stringField(event, "severity"), dialect.SeverityOf(title+"\n"+body)))
	finding := PreflightFinding{
		ID:                  firstNonEmpty(stringField(event, "id"), stringField(event, "fingerprint")),
		Bot:                 "coderabbit-cli",
		Severity:            severity,
		Path:                firstNonEmpty(stringField(event, "fileName"), stringField(event, "path")),
		Line:                intField(event, "startLine", "line"),
		EndLine:             intField(event, "endLine"),
		Title:               title,
		Body:                body,
		CodegenInstructions: codegen,
		Suggestions:         stringSliceField(event, "suggestions"),
		Fingerprint:         stringField(event, "fingerprint"),
		Source:              "coderabbit_cli",
	}
	if finding.ID == "" {
		sum := sha256.Sum256([]byte(finding.Path + "|" + strconv.Itoa(finding.Line) + "|" + finding.Title + "|" + finding.Body))
		finding.ID = hex.EncodeToString(sum[:])
	}
	return finding
}

// carriesCredentials reports whether these arguments authenticate the run
// themselves. If they do, the login the CLI would report afterwards is not
// necessarily the account that produced the block.
func carriesCredentials(args []string) bool {
	for _, arg := range args {
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--api-key", "--apikey", "--token":
			return true
		}
	}
	return false
}

// redactSecrets masks the values of secret-bearing flags (e.g. --api-key) so the
// command recorded in the preflight report never leaks credentials.
func redactSecrets(args []string) []string {
	secret := func(flag string) bool {
		switch flag {
		case "--api-key", "--apikey", "--token":
			return true
		}
		return false
	}
	out := make([]string, len(args))
	redactNext := false
	for i, a := range args {
		switch {
		case redactNext:
			out[i] = "***"
			redactNext = false
		case secret(strings.ToLower(a)):
			out[i] = a
			redactNext = true
		case strings.HasPrefix(a, "--") && strings.Contains(a, "="):
			if eq := strings.IndexByte(a, '='); eq > 0 && secret(strings.ToLower(a[:eq])) {
				out[i] = a[:eq+1] + "***"
			} else {
				out[i] = a
			}
		default:
			out[i] = a
		}
	}
	return out
}

func stringField(value map[string]any, key string) string {
	raw, ok := value[key]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func intField(value map[string]any, keys ...string) int {
	for _, key := range keys {
		raw, ok := value[key]
		if !ok || raw == nil {
			continue
		}
		switch v := raw.(type) {
		case float64:
			return int(v)
		case int:
			return v
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
		}
	}
	return 0
}

func stringSliceField(value map[string]any, key string) []string {
	raw, ok := value[key]
	if !ok || raw == nil {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		if s := stringField(value, key); s != "" {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s := strings.TrimSpace(fmt.Sprint(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func trimForReport(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max] + "...(truncated)"
}
