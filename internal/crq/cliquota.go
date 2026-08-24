package crq

import (
	"context"
	"strings"
	"time"

	"github.com/kristofferR/codereview-queue/internal/dialect"
)

// CLIQuotaResult reports what RecordCLIQuota did, so the caller can say so
// instead of failing silently.
type CLIQuotaResult struct {
	Applied bool       `json:"applied"`
	Reason  string     `json:"reason,omitempty"`
	Until   *time.Time `json:"blocked_until,omitempty"`
}

// IsCLIAccountBlock reports whether a preflight run ended in an account-quota
// block, so a caller can decide whether sharing it is even relevant.
func IsCLIAccountBlock(report PreflightReport) bool {
	return dialect.IsCLIRateLimit(report.ErrorType)
}

// RecordCLIQuota folds an account block the LOCAL CodeRabbit CLI reported into
// the shared AccountQuota.
//
// The CLI spends the same account-wide review budget the queue exists to
// serialize, so a local refusal is direct evidence about the very thing crq
// otherwise discovers by asking on the calibration PR and reading the reply. It
// costs no probe comment and no calibration-thread growth — only the state write
// itself — and it is fresher, because the CLI answers immediately.
//
// It is deliberately best-effort and never fatal: preflight is a local command
// that must keep working with no crq config and no GitHub token at all.
//
// Two guards matter:
//
//   - **The account must match.** The CLI can be authenticated to a different
//     CodeRabbit organisation than the one crq queues for, and applying that
//     block would stall reviews for an account that is not limited.
//   - **A block is never shortened** (engine.AcceptAccountBlock). A standing
//     window from a PR comment is authoritative about that account; a local
//     reading may be a different, narrower limit, so it can only ever extend.
func (s *Service) RecordCLIQuota(ctx context.Context, report PreflightReport, cliOrg string) (CLIQuotaResult, error) {
	if !dialect.IsCLIRateLimit(report.ErrorType) {
		return CLIQuotaResult{Reason: "the cli reported no account block"}, nil
	}
	// A limit the CLI attributes to this USER is not a fleet-wide block, and the
	// captured event distinguishes the two. Sharing a personal one would stall
	// every repository in the scope.
	if !report.OrgAttributed {
		return CLIQuotaResult{Reason: "the cli did not attribute this block to the organisation"}, nil
	}
	// Credentials passed on the command line cannot be reproduced by the identity
	// probe, so the login it reports may belong to a different account than the
	// one that hit the limit. Refuse rather than guess whose block this is.
	if report.CredentialsOverridden {
		return CLIQuotaResult{Reason: "this run used credentials passed on the command line, so the account behind the block cannot be confirmed"}, nil
	}
	if err := s.cfg.RequireState(); err != nil {
		return CLIQuotaResult{Reason: "no crq state configured, so there is no shared quota to update"}, nil
	}
	state, _, err := s.store.Load(ctx)
	if err != nil {
		return CLIQuotaResult{Reason: "shared quota state could not be read, so the account behind the block cannot be confirmed"}, nil
	}
	cfg := s.cfg.WithFleet(state.Fleet)
	if !cliOrgMatches(cfg, cliOrg) {
		return CLIQuotaResult{
			Reason: "the coderabbit cli is authenticated to " + orDash(cliOrg) +
				", which is not the account crq queues for (" + strings.Join(cfg.Scope, ",") + ")",
			Until: state.Account.BlockedUntil,
		}, nil
	}

	now := s.clock()
	until := dialect.ParseCLIWaitTime(report.RetryAfter, now)
	if until == nil {
		// The CLI said "blocked" without a window crq could read. Waiting the
		// conservative fallback is right: treating an unreadable window as "not
		// blocked" is what let the daemon re-fire every couple of minutes against
		// a limit measured in tens of minutes.
		//
		// The FLEET's window, when one is recorded. CRQ_RL_FALLBACK is a fleet
		// setting, and reading this host's startup value recorded a shorter block
		// than the settings page said was in force — resuming metered fires early
		// against the number the dashboard was showing.
		fallback := now.Add(cliQuotaFallback(cfg.RateLimitFallback))
		until = &fallback
	}

	result := CLIQuotaResult{Until: until}
	if s.cfg.DryRun {
		result.Reason = "dry run: would record the block"
		return result, nil
	}

	applied, standing, matched, err := s.applyAccountBlock(ctx, *until, "coderabbit-cli", cfg, cliOrg)
	if err != nil {
		return result, err
	}
	if !matched {
		result.Reason = "the fleet account changed before the block could be recorded"
		result.Until = standing
		return result, nil
	}
	result.Applied = applied
	if !applied {
		result.Reason = "a longer account block is already recorded"
		result.Until = standing
	}
	return result, nil
}

// cliOrgMatches reports whether the CLI's current organisation is the account
// crq queues for. An empty org fails closed: without knowing whose limit this is,
// applying it fleet-wide is the more expensive mistake.
func cliOrgMatches(cfg Config, cliOrg string) bool {
	org := strings.ToLower(strings.TrimSpace(cliOrg))
	if org == "" {
		return false
	}
	// ONLY the configured scope counts. Accepting the gate repo's owner as well
	// let a personal CodeRabbit org stall an unrelated scope: with
	// CRQ_REPO=alice/crq-state and CRQ_SCOPE=acme, Alice's local limit would have
	// blocked every review for acme.
	for _, scope := range cfg.Scope {
		if strings.EqualFold(strings.TrimSpace(scope), org) {
			return true
		}
	}
	return false
}

// cliQuotaFallback is the window to record when the CLI reports a block without a
// readable duration. A zero or negative CRQ_RL_FALLBACK would place the block at
// or before now, which reads as "not blocked" and re-fires immediately — the
// opposite of what an explicit block means. Mirrors the engine's own floor.
func cliQuotaFallback(configured time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return 15 * time.Minute
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "an unknown organisation"
	}
	return value
}
