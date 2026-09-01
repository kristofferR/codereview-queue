package dialect

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	codexBlobLinkRE   = regexp.MustCompile(`(?m)^https://github\.com/[^/\s]+/[^/\s]+/blob/([0-9a-fA-F]{7,64})/(.+?)#L([0-9]+)(?:-L([0-9]+))?\s*$`)
	codexRunningRowRE = regexp.MustCompile("(?im)^\\|[^\\n]*\\*\\*running\\*\\*[^\\n]*\\|\\s*`([0-9a-f]{7,40})`\\s*\\|")
	markdownImageRE   = regexp.MustCompile(`!\[[^]]*\]\([^)]+\)`)
	subTagRE          = regexp.MustCompile(`(?i)</?sub>`)
	// codexReviewedCommitRE matches Codex's "**Reviewed commit:** `4d9e8bca82`"
	// line in the newer clean-summary format.
	codexReviewedCommitRE = regexp.MustCompile("(?i)reviewed commit[:*\\s]*`([0-9a-fA-F]{7,40})`")
)

const codexReviewSummaryMarker = "<!-- codex-pull-request-review-summary -->"

// CodexBotLogin is the canonical Codex GitHub app login. It is the one place
// this literal may appear; engine/state/crq consume this constant rather than
// repeating the wording.
const CodexBotLogin = "chatgpt-codex-connector[bot]"

// CodexReviewCommand uses Codex's documented focused-review suffix so every
// automated round carries the fleet's scope boundary even before a repository
// adds its own AGENTS.md review rules.
const CodexReviewCommand = "@codex review for consequential defects introduced by this PR; do not expand its original scope with unrelated refactors, features, or speculative hardening"

func IsCodexBot(login string) bool {
	return NormalizeBotName(login) == NormalizeBotName(CodexBotLogin)
}

func HasCodexBot(bots []string) bool {
	for _, bot := range bots {
		if IsCodexBot(bot) {
			return true
		}
	}
	return false
}

func IsCodexNoActionReviewCompletion(text string) bool {
	text = NormalizeReviewText(text)
	if !strings.Contains(text, "didn't find any major issues") {
		return false
	}
	// Codex has shipped several clean-summary tails: the original
	// "Keep them coming!", and the newer ":tada:" flourish with a
	// "**Reviewed commit:** `sha`" line.
	return strings.Contains(text, "keep them coming") ||
		strings.Contains(text, ":tada:") ||
		strings.Contains(text, "🎉") ||
		CodexReviewedCommitSHA(text) != ""
}

// IsCodexEnvironmentNotice reports whether a Codex comment is its platform
// boilerplate asking the repo owner to create a Codex cloud environment. It is
// posted as a thread reply and must never read as a finding or a rebuttal.
func IsCodexEnvironmentNotice(text string) bool {
	t := NormalizeReviewText(text)
	return strings.Contains(t, "create an environment for this repo") ||
		strings.Contains(t, "create a codex account and connect to github")
}

// IsCodexUsageLimit reports whether a Codex comment is its usage-limit
// exhaustion notice ("You have reached your Codex usage limits for code
// reviews"). It is non-actionable like Codex's other acks, but distinct: it
// means Codex cannot produce a review this round, which the dynamic completion
// gate uses to avoid waiting on a Codex that will never finish.
func IsCodexUsageLimit(text string) bool {
	return strings.Contains(NormalizeReviewText(text), "usage limits for code reviews")
}

// IsCodexReviewSummary reports whether text is Codex's editable PR activity
// summary. The whole comment is operational status, never review feedback.
func IsCodexReviewSummary(text string) bool {
	return strings.Contains(text, codexReviewSummaryMarker)
}

// CodexReviewInProgressSHA extracts the commit from a running row in Codex's
// editable PR activity summary. A marked summary with no running row is still
// informational, but it is not evidence that a review remains in flight.
func CodexReviewInProgressSHA(text string) string {
	if !IsCodexReviewSummary(text) {
		return ""
	}
	match := codexRunningRowRE.FindStringSubmatch(text)
	if len(match) == 2 {
		return strings.ToLower(match[1])
	}
	return ""
}

// CodexReviewedCommitSHA extracts the commit hash Codex says it reviewed,
// or "" when the comment carries no such line.
func CodexReviewedCommitSHA(text string) string {
	match := codexReviewedCommitRE.FindStringSubmatch(text)
	if len(match) == 2 {
		return strings.ToLower(match[1])
	}
	return ""
}

// SHAPrefixMatch reports whether two commit-hash abbreviations refer to the
// same commit: both are prefixes of the full SHA, so the shorter must prefix
// the longer (crq truncates heads to 9 chars; Codex abbreviates to 10).
func SHAPrefixMatch(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == "" || b == "" {
		return false
	}
	if len(a) <= len(b) {
		return strings.HasPrefix(b, a)
	}
	return strings.HasPrefix(a, b)
}

func CleanCodexReviewText(text string) string {
	text = markdownImageRE.ReplaceAllString(text, "")
	text = subTagRE.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

func CodexPrioritySeverity(text string) string {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "![p0 badge]"):
		return "critical"
	case strings.Contains(lower, "![p1 badge]"):
		return "major"
	case strings.Contains(lower, "![p2 badge]"), strings.Contains(lower, "![p3 badge]"):
		return "minor"
	default:
		return SeverityOf(text)
	}
}

// ParseCodexReviewFindings extracts findings that Codex can only place in the
// review body when the referenced line is outside GitHub's current diff. Codex
// anchors each item with a blob URL followed by a bold priority/title line.
func ParseCodexReviewFindings(body string, review ReviewMeta, bot string) []Finding {
	if !IsCodexBot(bot) {
		return nil
	}
	matches := codexBlobLinkRE.FindAllStringSubmatchIndex(body, -1)
	out := make([]Finding, 0, len(matches))
	for index, match := range matches {
		blockEnd := len(body)
		if index+1 < len(matches) {
			blockEnd = matches[index+1][0]
		}
		block := strings.TrimSpace(body[match[1]:blockEnd])
		if details := strings.Index(strings.ToLower(block), "<details"); details >= 0 {
			block = strings.TrimSpace(block[:details])
		}
		if block == "" {
			continue
		}

		path, err := url.PathUnescape(body[match[4]:match[5]])
		if err != nil {
			path = body[match[4]:match[5]]
		}
		line, _ := strconv.Atoi(body[match[6]:match[7]])
		title := ""
		if titleMatch := boldTitleRE.FindStringSubmatch(block); titleMatch != nil {
			title = CleanCodexReviewText(titleMatch[1])
		}
		if title == "" {
			title = TitleOf(block)
		}
		labels := ReviewLabelsFor(bot, block)
		severity := labels.Severity
		if severity == "" {
			severity = CodexPrioritySeverity(block)
		}
		finding := Finding{
			Bot:       bot,
			Severity:  severity,
			Scale:     labels.Scale,
			Path:      path,
			Line:      line,
			Title:     title,
			Body:      CleanCodexReviewText(CompactReviewBody(block)),
			ReviewID:  review.ID,
			Commit:    ShortOID(body[match[2]:match[3]]),
			URL:       review.HTMLURL,
			Source:    "review_body",
			CreatedAt: review.SubmittedAt,
		}
		if IsActionableFinding(finding) {
			out = append(out, finding)
		}
	}
	return out
}
