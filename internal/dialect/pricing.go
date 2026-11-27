package dialect

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
)

// PricesCheckedAt is the day every figure in this file was read off the
// vendor's own pricing page. It travels with the estimate and is displayed
// beside it, because a price nobody has re-checked is a number that quietly
// stops being true — CodeRabbit's are already roughly double the figures
// widely cited a year ago.
const PricesCheckedAt = "2026-07-27"

// PricingDisclosure is the shared user-facing explanation of vendor billing.
// It lives beside the prices so changing a vendor term cannot leave the CLI and
// dashboard describing a different model from the estimator.
const PricingDisclosure = "Macroscope bills by diff size with a per-review minimum and charges incrementally after its first review of a pull request, so later rounds can cost less than this whole-head estimate. CodeRabbit uses the plan allowance first and may bill by reviewed file after it; path filters can reduce that figure. Greptile spends prepaid credits per review, set by the repository's review tier; free and open-source plans include some. Codex and Cursor Bugbot are covered by their own subscriptions."

// Published prices, per vendor. Named constants rather than literals in the
// arithmetic so re-checking them is reading one block, not auditing a function.
const (
	// Macroscope bills the diff it reviews: $0.05 per KB with a 10 KB minimum,
	// so every review costs at least $0.50, capped at $10 per review (and $50
	// per pull request, which crq does not track across rounds).
	macroscopePerKB    = 0.05
	macroscopeMinKB    = 10.0
	macroscopeMaxSpend = 10.0

	// CodeRabbit charges nothing until the plan's allowance is spent; past it,
	// usage-based reviews bill $0.25 per reviewed file. Only reached when the
	// account has usage-based reviews enabled at all.
	coderabbitPerFile = 0.25

	// Greptile sells prepaid credits at $1 each, shared across the
	// organization. A Base review spends 1, Plus 3 and Apex 10, whatever the
	// diff's size. Read 2026-11-27, after PricesCheckedAt.
	greptilePerCredit   = 1.0
	greptileBaseCredits = 1.0
	greptileApexCredits = 10.0
)

// DiffStat is what a cost estimate needs to know about a head. Lines rather
// than bytes because that is what GitHub reports on a pull request without
// fetching the diff itself.
type DiffStat struct {
	Additions    int
	Deletions    int
	ChangedFiles int
}

// Lines is the diff's total changed lines.
func (d DiffStat) Lines() int { return d.Additions + d.Deletions }

// bytesPerLineHigh turns a cumulative line count into a conservative whole-diff
// upper bound for Macroscope. Measurements over this repository reached 118
// bytes per line on long-line-heavy diffs; later rounds may instead bill only
// their increment, whose lower bound is the vendor's per-review floor.
const bytesPerLineHigh = 120.0

// Allowance is the account state a CodeRabbit estimate depends on: whether the
// plan's included reviews are exhausted, and whether usage-based billing is
// even switched on to take over when they are.
type Allowance struct {
	// Remaining is included reviews left, as the bot last reported them.
	Remaining int
	// RemainingKnown is false when crq has never seen a count. Absent is not
	// zero: treating "unknown" as "exhausted" would invent a charge.
	RemainingKnown bool
	// UsageBasedEnabled says pay-as-you-go is on. When it is off, an exhausted
	// account does not spend money — it stops, which is a different answer.
	UsageBasedEnabled bool
	// UsageBasedKnown is false when crq has never learned which of those two it
	// is. Absent is not "off", for the same reason absent is not "exhausted":
	// asserting "off" prices an exhausted account at exactly $0.00, and an
	// account that does have overages on would then be told its backlog is free
	// and billed per reviewed file for it.
	UsageBasedKnown bool
}

// CostEstimate is what one reviewer will cost for one head, in US dollars.
//
// Low and High bound the answer rather than pretending to a single figure: the
// only honest output for a per-kilobyte price derived from a line count is a
// range. Exact marks the cases where there genuinely is one number — a bot that
// costs nothing, or a diff small enough that the whole band lands on the
// vendor's minimum charge.
type CostEstimate struct {
	Bot  string
	Low  float64
	High float64
	// Metered says this review spends CodeRabbit's account allowance.
	Metered bool
	// Exact says Low and High are the same number and that number is not a
	// guess.
	Exact bool
	// Unknown says crq has no basis to estimate this reviewer at all. A UI must
	// say so rather than showing $0.00, which reads as "free".
	Unknown bool
	// Basis is the one-sentence explanation shown under the figure. Every
	// estimate carries one; a number without its reasoning is not checkable.
	Basis string
}

// Free is the estimate for a reviewer covered by its own subscription, which is
// every co-reviewer except Macroscope and Greptile.
func freeEstimate(login, why string) CostEstimate {
	return CostEstimate{Bot: login, Exact: true, Basis: why}
}

// EstimateMacroscope prices one Macroscope review of this diff.
//
// The diff basis is deliberately the whole head: Macroscope bills incrementally
// after its first review of a pull request (head vs the previously reviewed
// head), so this is an upper bound on later rounds and exact on the first.
func EstimateMacroscope(d DiffStat) CostEstimate {
	est := CostEstimate{Bot: MacroscopeLogin}
	highKB := math.Max(macroscopeMinKB, float64(d.Lines())*bytesPerLineHigh/1024)
	// The first review sees the whole pull-request diff, but later reviews are
	// incremental. Without the previously reviewed head the only honest lower
	// bound is the per-review floor; a tiny follow-up can cost that little even
	// when the cumulative PR diff is large.
	est.Low = macroscopeMinKB * macroscopePerKB
	est.High = math.Min(macroscopeMaxSpend, highKB*macroscopePerKB)
	switch {
	case est.Low == est.High && est.High == macroscopeMinKB*macroscopePerKB:
		// The entire band fits under the 10 KB minimum, so the minimum IS the
		// price — no estimation left in it.
		est.Exact = true
		est.Basis = fmt.Sprintf("%d changed lines is under the 10 KB minimum, so it is the $%.2f floor",
			d.Lines(), macroscopeMinKB*macroscopePerKB)
	default:
		est.Basis = fmt.Sprintf(
			"$%.2f incremental-review floor to a $%.2f whole-diff upper bound (%d changed lines at %.0f bytes/line)",
			est.Low, est.High, d.Lines(), bytesPerLineHigh,
		)
	}
	return est
}

// EstimateGreptile prices one Greptile review. The tier is a repository setting
// crq cannot read, so the band runs up to an Apex review. Credits a free or
// open-source plan includes cost nothing, and crq cannot see those either, so
// the band starts at zero.
func EstimateGreptile(DiffStat) CostEstimate {
	return CostEstimate{
		Bot:  GreptileLogin,
		High: greptileApexCredits * greptilePerCredit,
		Basis: fmt.Sprintf("%.0f credit for a Base review to %.0f for Apex at $%.2f each, whatever the diff size; included plan credits cost nothing",
			greptileBaseCredits, greptileApexCredits, greptilePerCredit),
	}
}

// EstimateCodeRabbit prices one CodeRabbit review of this diff against the
// account's remaining allowance.
func EstimateCodeRabbit(login string, d DiffStat, a Allowance) CostEstimate {
	est := CostEstimate{Bot: login, Metered: true}
	switch {
	case !a.RemainingKnown:
		est.Unknown = true
		est.Basis = "crq has not seen a remaining-review count, so whether this one is included or billed is unknown"
	case a.Remaining > 0:
		est.Exact = true
		est.Basis = fmt.Sprintf("included: %d review(s) left in the plan allowance", a.Remaining)
	case !a.UsageBasedKnown:
		est.Unknown = true
		est.Basis = "the allowance is spent and crq has not learned whether usage-based reviews are on, so whether this waits or bills is unknown"
	case !a.UsageBasedEnabled:
		est.Exact = true
		est.Basis = "the allowance is spent and usage-based reviews are off, so this waits for the window rather than costing anything"
	case d.ChangedFiles <= 0:
		est.Unknown = true
		est.Basis = "past the allowance, billed per reviewed file — and the file count is not known here"
	default:
		// Every changed file is not necessarily a REVIEWED file: path filters
		// and the plan's per-review file cap both cut it down. So this is an
		// upper bound, and says so.
		est.High = float64(d.ChangedFiles) * coderabbitPerFile
		est.Basis = fmt.Sprintf("past the allowance: up to %d file(s) at $%.2f, less whatever path filters exclude",
			d.ChangedFiles, coderabbitPerFile)
	}
	return est
}

// EstimateCost prices one reviewer's review of one head. login may be the
// configured primary or any registry co-reviewer; an unrecognised one is
// Unknown rather than free, because crq cannot know what somebody else's bot
// charges.
//
// The VENDOR decides the basis, not the role. CRQ_BOT may name a registry bot —
// Macroscope, Codex — and asking about the primary first billed whichever one
// was configured on CodeRabbit's allowance-then-per-file model: a Macroscope
// primary was quoted the wrong basis outright, and a Codex primary read as
// billable or unknown instead of covered by its subscription. The registry is
// therefore consulted first; CodeRabbit has no entry there, so a CodeRabbit
// primary still lands on its own estimator.
func EstimateCost(login, primary string, d DiffStat, a Allowance) CostEstimate {
	if co, ok := CoReviewerByName(login); ok {
		var estimate CostEstimate
		if co.Price != nil {
			estimate = co.Price(d)
		} else {
			estimate = freeEstimate(co.Login, "covered by its own subscription — it spends no per-review money")
		}
		estimate.Metered = co.Budget == BudgetAccount
		return estimate
	}
	if primary != "" && NormalizeBotName(login) == NormalizeBotName(primary) {
		return EstimateCodeRabbit(login, d, a)
	}
	return CostEstimate{Bot: login, Unknown: true, Basis: "crq has no pricing for this reviewer"}
}

// agentCredits matches the credit line Macroscope appends to a check run's
// body: "**Agent Credits:** 81 credits". Undocumented, observed in the wild —
// which is why it is one regexp with a corpus file behind it.
var agentCredits = regexp.MustCompile(`(?i)\*\*Agent Credits:\*\*\s*([0-9][0-9,]*)\s*credits?`)

// ParseAgentCredits reads the credits a Macroscope check run reports spending.
// The second return is false when the body carries no such line, which is the
// normal case for its other check types.
func ParseAgentCredits(body string) (int, bool) {
	m := agentCredits.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	digits := make([]byte, 0, len(m[1]))
	for i := 0; i < len(m[1]); i++ {
		if m[1][i] != ',' {
			digits = append(digits, m[1][i])
		}
	}
	n, err := strconv.Atoi(string(digits))
	if err != nil {
		return 0, false
	}
	return n, true
}
