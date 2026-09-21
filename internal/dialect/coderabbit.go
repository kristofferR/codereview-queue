package dialect

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CodeRabbit classifies CodeRabbit's comment bodies. The markers come from
// config (CRQ_COMPLETION_MARKER, CRQ_RL_MARKER, CRQ_CAL_REPLY_MARKER); the
// remaining phrasings are CodeRabbit's own current wording, pinned by the
// golden corpus in testdata/coderabbit.
type CodeRabbit struct {
	CompletionMarker  string
	RateLimitMarker   string
	CalibrationMarker string
}

// ConfirmationReviewCommand asks CodeRabbit to re-read an unchanged head.
// The normal review command is incremental and can refuse already-read code.
// Explicit custom commands and other reviewers retain their configured wording.
func ConfirmationReviewCommand(bot, command string) string {
	if NormalizeBotName(bot) == "coderabbitai" && strings.TrimSpace(command) == "@coderabbitai review" {
		return "@coderabbitai full review"
	}
	return command
}

// Default CodeRabbit dialect vocabulary. These literals live here — the one
// package that owns bot-message text — so crq/config and the engine inject them
// without spelling out the "rate limit" wording themselves.
const (
	// DefaultRateLimitCommand is the probe crq posts to read CodeRabbit's
	// account-wide review quota without spending a real review.
	DefaultRateLimitCommand = "@coderabbitai rate limit"
	// DefaultRateLimitMarker is the legacy phrase in CodeRabbit's account-quota
	// notice (the newer Fair Usage format is matched separately).
	DefaultRateLimitMarker = "rate limited by coderabbit.ai"
	// ReasonRateLimited is the requeue reason recorded when a fired review comes
	// back account-blocked. It is the human-readable string users grep in the
	// daemon log (requeue ... reason="rate limited"); the engine and crq
	// reference it so the literal stays in this package.
	ReasonRateLimited = "rate limited"
)

// IsCompletionReply reports whether body is the bot's reply to a processed
// review command (CodeRabbit: "Review finished."). An empty marker disables
// the completion-reply convergence fallback entirely.
func (d CodeRabbit) IsCompletionReply(body string) bool {
	marker := strings.TrimSpace(d.CompletionMarker)
	if marker == "" {
		return false
	}
	return strings.Contains(strings.ToLower(body), strings.ToLower(marker))
}

// IsAutoReply reports whether body is one of the bot's auto-generated replies
// to a command — completion, rate-limit, skip, or progress. The bot posts
// exactly one per command, which is what lets completions be paired to the
// command they answer.
func (d CodeRabbit) IsAutoReply(body string) bool {
	marker := strings.TrimSpace(d.CalibrationMarker)
	if marker == "" {
		return false
	}
	return strings.Contains(strings.ToLower(body), strings.ToLower(marker))
}

// IsRateLimited reports whether a CodeRabbit comment is a rate-limit notice. It
// matches the configured CRQ_RL_MARKER plus CodeRabbit's current phrasings (the
// "Fair Usage Limits Policy" / "currently rate limited" message), which the old
// "rate limited by coderabbit.ai" marker alone misses — so a fired review that
// comes back rate-limited is detected and crq backs off instead of firing on.
func (d CodeRabbit) IsRateLimited(body string) bool {
	l := strings.ToLower(body)
	if m := strings.ToLower(strings.TrimSpace(d.RateLimitMarker)); m != "" && strings.Contains(l, m) {
		return true
	}
	return strings.Contains(l, "currently rate limited") ||
		strings.Contains(l, "rate limited under") ||
		strings.Contains(l, "fair usage limits policy")
}

// IsReviewsPaused reports whether a CodeRabbit comment is the "Reviews paused"
// auto-pause notice. CodeRabbit posts this when a branch is under active
// development (an influx of new commits) and auto_pause_after_reviewed_commits
// kicks in. It acknowledges the branch but is not a review of the fired head, so
// — like a rate-limit notice — it must not be mistaken for a completed review
// round: doing so would falsely converge a loop with zero findings. crq keeps
// triggering reviews explicitly, and "@coderabbitai review" still produces a
// single review while auto-review is paused, so the round completes on the real
// review, not this note.
func (d CodeRabbit) IsReviewsPaused(body string) bool {
	l := strings.ToLower(body)
	return strings.Contains(l, "reviews paused") ||
		strings.Contains(l, "automatically paused this review") ||
		strings.Contains(l, "auto_pause_after_reviewed_commits")
}

// IsReviewAlreadyDone identifies CodeRabbit's "does not re-review already
// reviewed commits" acknowledgement. The text is only a claim, not completion
// evidence: callers require a matching GitHub review before trusting it.
// The same boilerplate can appear inside a rate-limit notice's help section, so
// a comment that is itself a rate limit is excluded.
func (d CodeRabbit) IsReviewAlreadyDone(body string) bool {
	l := strings.ToLower(body)
	if !strings.Contains(l, "does not re-review already reviewed") &&
		!strings.Contains(l, "already reviewed commit") {
		return false
	}
	return !d.IsRateLimited(l)
}

// IsSummaryOnlyPlan reports whether a CodeRabbit comment declares that the
// account's plan produces a high-level summary and walkthrough ONLY, with no
// line-by-line review. CodeRabbit ships this notice on private repositories
// under the Free plan; public repositories get Pro-grade reviews for free and
// carry "**Plan**: Pro" instead, so the notice is exactly the "CodeRabbit will
// never submit a review of this PR" signal — no repo-visibility lookup needed.
//
// Both anchors are statements about THIS account ("Summarized by CodeRabbit
// Free", "on the Free plan"), never generic marketing copy: a false positive
// would stop crq firing CodeRabbit on a PR it could really review.
func (d CodeRabbit) IsSummaryOnlyPlan(body string) bool {
	l := strings.ToLower(body)
	return strings.Contains(l, "summarized by coderabbit free") ||
		(strings.Contains(l, "on the free plan") &&
			strings.Contains(l, "high-level summary and a walkthrough"))
}

var (
	// reviewSkippedRE matches CodeRabbit's "Review skipped" callout heading.
	// Anchored to the heading (optionally inside a blockquote callout) rather
	// than matched as loose prose: a false positive stops crq firing CodeRabbit
	// on a PR it could really review.
	reviewSkippedRE = regexp.MustCompile(`(?mi)^\s*>?\s*#{1,4}\s*review skipped\s*$`)
	// autoReviewsDisabledRE matches the routine "auto-review is off, ask me
	// explicitly" notice that shares the "Review skipped" heading. crq REQUIRES
	// auto-review to be off, so this fires on every push it manages: reading it
	// as a refusal would stop crq firing CodeRabbit on every repo.
	autoReviewsDisabledRE = regexp.MustCompile(`(?i)auto reviews are disabled`)
	// reviewSkippedHeadRE captures the head SHA a skip was evaluated against,
	// from "Reviewing files that changed ... between <base> and <head>.".
	reviewSkippedHeadRE = regexp.MustCompile(`(?i)between\s+[0-9a-f]{7,40}\s+and\s+([0-9a-f]{7,40})`)
	// reviewSkippedReasonRE captures the human-readable reason CodeRabbit gives
	// under the heading (the first non-empty, non-heading callout line).
	reviewSkippedReasonRE = regexp.MustCompile(`(?mi)^\s*>\s*(?:##+\s*)?([A-Z][^\n]*?[.!])\s*$`)
)

// IsReviewSkipped reports whether body is CodeRabbit REFUSING to review this
// head (too many files, no usage credits, an unsupported diff). This is NOT a
// rate limit even though the notice carries the rate-limit marker — there is no
// window after which it clears, so waiting and re-firing produce the same
// refusal forever while poisoning the account-wide quota with a fabricated
// block. Treat it as "no CodeRabbit review is coming for this head" and let the
// co-reviewers decide the round.
//
// CodeRabbit reuses the same "Review skipped" heading for a notice that is the
// exact OPPOSITE of a refusal: "Auto reviews are disabled on this repository …
// To trigger a single review, invoke the `@coderabbitai review` command." That
// one says a review IS available on request — it is what CodeRabbit posts on
// every push once auto-review is off, which is crq's REQUIRED prerequisite and
// therefore its steady state on every repo it manages. Reading it as a refusal
// stops crq firing CodeRabbit anywhere, so it is excluded explicitly.
func (d CodeRabbit) IsReviewSkipped(body string) bool {
	return reviewSkippedRE.MatchString(body) && !autoReviewsDisabledRE.MatchString(body)
}

// ReviewSkippedHeadSHA returns the head commit the skip was evaluated against,
// or "" when the notice names none. The refusal is per-head — splitting the PR
// produces a new head that CodeRabbit may well review — so binding to this SHA
// is what lets the next head fire normally.
func ReviewSkippedHeadSHA(body string) string {
	if match := reviewSkippedHeadRE.FindStringSubmatch(body); len(match) == 2 {
		return strings.ToLower(match[1])
	}
	return ""
}

// ReviewSkippedReason returns the first explanatory sentence CodeRabbit gives
// for skipping ("Too many files!", "This PR contains 119 files, ..."), for
// surfacing to the agent as actionable work. Empty when none parses.
func ReviewSkippedReason(body string) string {
	for _, line := range reviewSkippedReasonRE.FindAllStringSubmatch(body, -1) {
		text := strings.TrimSpace(line[1])
		if text == "" || strings.EqualFold(text, "Review skipped") {
			continue
		}
		return text
	}
	return ""
}

// IsReviewInProgress reports whether body is CodeRabbit's editable top-summary
// state for a review that has started but has not finished. CodeRabbit can post
// a "Review finished" command reply before this summary leaves the processing
// state, so the reply alone is not a terminal signal.
func (d CodeRabbit) IsReviewInProgress(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "currently processing new changes in this pr") ||
		strings.Contains(lower, "review in progress by coderabbit.ai")
}

// IsReviewFailure reports whether body is CodeRabbit's editable top-summary
// failure state. CodeRabbit can still change the command reply to "Review
// finished" after this summary reports that the review itself failed, so the
// reply is not evidence that the current head was reviewed successfully.
func (d CodeRabbit) IsReviewFailure(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "auto-generated comment: failure by coderabbit.ai") ||
		strings.Contains(lower, "## review failed")
}

// ParseAvailableIn extracts CodeRabbit's "next review available in <duration>"
// window from a rate-limit comment and returns base+duration+30s. The safety
// margin covers countdown rounding and timestamp skew before a queued retry.
// It tolerates the markdown and punctuation around the value — the current
// phrasing is "**Next review available in:** **40 minutes**", where a colon and
// bold markers sit between "in" and the number. An unparseable body returns nil;
// the caller then falls back to a conservative fixed window rather than a short
// retry (getting this wrong is exactly what let the daemon re-fire every couple
// of minutes instead of honouring a 40-minute limit).
func ParseAvailableIn(text string, base time.Time) *time.Time {
	lower := strings.ToLower(text)
	idx := strings.Index(lower, "available in")
	if idx < 0 {
		return nil
	}
	frag := lower[idx+len("available in"):]
	// Normalise markdown/punctuation to spaces so "in:** **40 minutes**" scans as
	// "40 minutes". Do this before splitting into fields so bold/colon can't fuse
	// onto the number ("**40") and defeat the numeric parse.
	frag = strings.Map(func(r rune) rune {
		switch r {
		case '*', ':', '`', ',', '_', '(', ')':
			return ' '
		}
		return r
	}, frag)
	// Stop at a sentence boundary so a later number in the body isn't read as part
	// of the window.
	if dot := strings.IndexByte(frag, '.'); dot >= 0 {
		frag = frag[:dot]
	}
	d := scanDurationPhrase(frag)
	if d <= 0 {
		return nil
	}
	t := base.Add(d + 30*time.Second)
	return &t
}

// scanDurationPhrase sums the "<n> <unit>" pairs in an already-normalised
// fragment ("40 minutes", "1 hour 5 minutes"). Shared by every place CodeRabbit
// states a window in prose, so a new phrasing is taught to one scanner.
//
// A non-positive total is not a window. Atoi accepts a leading minus, so a
// mangled body really can scan negative — and every caller adds the result to
// now, which would put a block in the past and read as "not blocked".
func scanDurationPhrase(frag string) time.Duration {
	fields := strings.Fields(frag)
	var d time.Duration
	for i := 0; i+1 < len(fields); i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			continue
		}
		switch unit := fields[i+1]; {
		case strings.HasPrefix(unit, "hour"):
			d += time.Duration(n) * time.Hour
		case strings.HasPrefix(unit, "minute"):
			d += time.Duration(n) * time.Minute
		case strings.HasPrefix(unit, "second"):
			d += time.Duration(n) * time.Second
		}
	}
	return d
}

// ParseCLIWaitTime reads the local CLI's bare wait-time value ("32 minutes",
// from the rate_limit error's metadata) and returns base+duration.
//
// The CLI states the window without the "available in" preamble its PR comments
// use, so it needs its own entry point — but the same scanner, so both formats
// stay in step. An unparseable value returns nil and the caller falls back to its
// conservative window rather than inventing a short one.
func ParseCLIWaitTime(waitTime string, base time.Time) *time.Time {
	d := scanDurationPhrase(strings.ToLower(strings.TrimSpace(waitTime)))
	if d <= 0 {
		return nil
	}
	t := base.Add(d)
	return &t
}

func ParseQuota(text string, base time.Time) (*int, *time.Time) {
	remaining := ParseRemainingReviews(text)
	reset := ParseAvailableIn(text, base)
	return remaining, reset
}

func ParseRemainingReviews(text string) *int {
	lower := strings.ToLower(text)
	words := strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	for i := 0; i < len(words); i++ {
		n, err := strconv.Atoi(words[i])
		if err != nil {
			continue
		}
		if i+2 < len(words) && strings.HasPrefix(words[i+1], "review") && (words[i+2] == "remaining" || words[i+2] == "left") {
			return &n
		}
		if i > 0 && (words[i-1] == "remaining" || words[i-1] == "left") {
			return &n
		}
	}
	return nil
}

var (
	detailSummaryRE = regexp.MustCompile(`(?i)<summary>\s*([^<]+?)\s+\([0-9]+\)\s*</summary>`)
	// Line headers come backticked in "Outside diff range comments" (`12-15`:) and
	// un-backticked in "Comments failed to post" (12-15:) — accept both.
	detailHeaderRE = regexp.MustCompile("^`?([0-9]+)(?:\\s*-\\s*([0-9]+))?`?: *(.*)$")
	// Per-finding summaries carry a full location below the summary instead of
	// grouping line-only headers beneath a "<path> (N)" summary.
	detailLocationRE = regexp.MustCompile("^`([^`]+):([0-9]+)(?:\\s*-\\s*[0-9]+)?`$")
	promptBlockRE    = regexp.MustCompile("(?is)<summary>[^<]*(?:Prompt for all review comments with AI agents|Prompt to fix review comments)[^<]*</summary>.*?```\\s*(.*?)\\s*```")
	promptFileRE     = regexp.MustCompile("^In (?:`@([^`]+)`|@([^:]+)):$")
	promptBulletRE   = regexp.MustCompile("^- (?:Around line|Line)\\s+([0-9]+)(?:\\s*-\\s*([0-9]+))?:\\s*(.*)$")
)

func ParseDetailedReviewFindings(body string, review ReviewMeta, bot string) []Finding {
	lines := strings.Split(body, "\n")
	var out []Finding
	currentPath := ""
	inFence := false
	codeRabbit := NormalizeBotName(bot) == NormalizeBotName(CodeRabbitLogin)
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if match := detailSummaryRE.FindStringSubmatch(line); match != nil {
			summary := strings.TrimSpace(match[1])
			if LooksLikePath(summary) {
				currentPath = summary
			}
			continue
		}
		path, lineNumber, meta := currentPath, "", ""
		if match := detailLocationRE.FindStringSubmatch(line); codeRabbit && match != nil && LooksLikePath(match[1]) {
			path, lineNumber = match[1], match[2]
		} else if match := detailHeaderRE.FindStringSubmatch(line); match != nil {
			lineNumber, meta = match[1], strings.TrimSpace(match[3])
		}
		if lineNumber == "" || path == "" {
			continue
		}
		startLine, _ := strconv.Atoi(lineNumber)
		if IsNonActionableText(meta) {
			continue
		}
		start := i + 1
		end := len(lines)
		depth, blockFence := 0, false
		for j := start; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			if strings.HasPrefix(next, "```") {
				blockFence = !blockFence
				continue
			}
			if blockFence {
				continue
			}
			// Nested AI prompts belong to this finding; the enclosing details
			// close ends it, before later findings or review metadata can leak in.
			depth += strings.Count(next, "<details>") - strings.Count(next, "</details>")
			if depth < 0 || detailHeaderRE.MatchString(next) || (codeRabbit && detailLocationRE.MatchString(next)) || detailSummaryRE.MatchString(next) {
				end = j
				break
			}
		}
		block := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
		title := TitleFromDetailedBlock(block)
		if title == "" {
			title = TitleOf(block)
		}
		bodyText := CompactReviewBody(block)
		finding := Finding{
			Bot:       bot,
			Severity:  SeverityOf(meta + "\n" + block),
			Path:      strings.TrimPrefix(path, "@"),
			Line:      startLine,
			Title:     title,
			Body:      bodyText,
			ReviewID:  review.ID,
			Commit:    ShortOID(review.CommitID),
			URL:       review.HTMLURL,
			Source:    "review_body",
			CreatedAt: review.SubmittedAt,
		}
		if IsActionableFinding(finding) {
			out = append(out, finding)
		}
		i = end - 1
	}
	return out
}

func ParsePromptReviewFindings(body string, review ReviewMeta, bot string) []Finding {
	var out []Finding
	for _, blockMatch := range promptBlockRE.FindAllStringSubmatch(body, -1) {
		block := blockMatch[1]
		lines := strings.Split(block, "\n")
		currentPath := ""
		for i := 0; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if match := promptFileRE.FindStringSubmatch(line); match != nil {
				currentPath = firstNonEmpty(match[1], match[2])
				currentPath = strings.TrimPrefix(currentPath, "@")
				continue
			}
			match := promptBulletRE.FindStringSubmatch(line)
			if match == nil || currentPath == "" {
				continue
			}
			startLine, _ := strconv.Atoi(match[1])
			parts := []string{strings.TrimSpace(match[3])}
			for j := i + 1; j < len(lines); j++ {
				next := strings.TrimSpace(lines[j])
				if next == "" {
					continue
				}
				if strings.HasPrefix(next, "---") || promptFileRE.MatchString(next) || promptBulletRE.MatchString(next) {
					break
				}
				parts = append(parts, next)
				i = j
			}
			bodyText := strings.TrimSpace(strings.Join(parts, " "))
			finding := Finding{
				Bot:       bot,
				Severity:  SeverityOf(bodyText),
				Path:      currentPath,
				Line:      startLine,
				Title:     TitleOf(bodyText),
				Body:      bodyText,
				ReviewID:  review.ID,
				Commit:    ShortOID(review.CommitID),
				URL:       review.HTMLURL,
				Source:    "review_prompt",
				CreatedAt: review.SubmittedAt,
			}
			if IsActionableFinding(finding) {
				out = append(out, finding)
			}
		}
	}
	return out
}

// CLIRateLimitErrorType is the local CodeRabbit CLI's own name for an account
// quota block, as emitted on its --agent JSON stream.
//
// The CLI spends the SAME account quota as the PR reviews crq queues, so this
// string is quota evidence — but it is still CodeRabbit's vocabulary, and this
// package is the only place allowed to know it. Orchestration asks
// IsCLIRateLimit instead of matching the literal, so a renamed or added error
// type is a one-line change here plus a corpus row, not a silent fallback to a
// generic failure.
const CLIRateLimitErrorType = "rate_limit"

// IsCLIRateLimit reports whether a CLI error type means the account is blocked.
func IsCLIRateLimit(errorType string) bool {
	return strings.EqualFold(strings.TrimSpace(errorType), CLIRateLimitErrorType)
}

// CLIError is the local CodeRabbit CLI's structured failure, normalized. The
// --agent stream's key names and their meanings are CodeRabbit's format contract,
// so they are read here rather than in orchestration: a renamed key is then one
// change in this package plus a corpus row, not a silent loss of meaning in a
// caller that never knew the shape.
type CLIError struct {
	// Type is the CLI's own classification, e.g. rate_limit.
	Type string
	// Recoverable is whether waiting can clear it.
	Recoverable bool
	// WaitTime is the CLI's stated window, in its own prose ("32 minutes").
	WaitTime string
	// OrgAttributed distinguishes an organisation-wide limit from this user's
	// own. Only the former says anything about the shared account.
	OrgAttributed bool
	// ProUser is the CLI's own "isProUser". It says which plan's allowance the
	// limit belongs to, which is what decides whether a review past it costs
	// money or simply waits.
	ProUser bool
	// PolicyGuidance is the CLI's prose about what to do, which is the only
	// place it states the overage price. Kept verbatim: it is a vendor string
	// and paraphrasing it would put a price in crq's mouth.
	PolicyGuidance string
	// UsageBasedEnabled is read OFF that prose: when the CLI offers to enable
	// usage-based reviews, they are not enabled, so exhausting the allowance
	// stops work rather than billing for it.
	UsageBasedEnabled bool
}

// usageBasedAlreadyEnabled reports whether the guidance does not invite the
// reader to enable usage-based reviews.
func usageBasedAlreadyEnabled(guidance string) bool {
	if guidance == "" {
		return false
	}
	lower := strings.ToLower(guidance)
	return !strings.Contains(lower, "enable **[usage-based reviews]") &&
		!strings.Contains(lower, "enable usage-based reviews")
}

// ParseCLIError reads an error event from the CLI's --agent JSON stream.
func ParseCLIError(event map[string]any) CLIError {
	out := CLIError{Type: cliStringField(event, "errorType")}
	if v, ok := event["recoverable"].(bool); ok {
		out.Recoverable = v
	}
	meta, _ := event["metadata"].(map[string]any)
	if meta != nil {
		out.WaitTime = cliStringField(meta, "waitTime")
		if v, ok := meta["orgAttributed"].(bool); ok {
			out.OrgAttributed = v
		}
		if v, ok := meta["isProUser"].(bool); ok {
			out.ProUser = v
		}
		out.PolicyGuidance = cliStringField(meta, "policyGuidance")
		out.UsageBasedEnabled = usageBasedAlreadyEnabled(out.PolicyGuidance)
	}
	return out
}

// IsAccountBlock reports whether this failure means the shared account quota is
// exhausted, rather than something local being wrong.
func (e CLIError) IsAccountBlock() bool { return IsCLIRateLimit(e.Type) }

func cliStringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
