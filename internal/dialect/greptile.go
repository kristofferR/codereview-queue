package dialect

import (
	"regexp"
	"strings"
)

// Greptile wording. Verified against the live corpus in testdata/greptile/
// (kristofferR/ha-adjustable-bed and siblings): by default it reviews a pull
// request once, when it opens, and again only when someone comments
// `@greptile`, which it acknowledges with a 👍 reaction rather than a reply.
// Each review spends prepaid credits.
//
// A review leaves three traces. One `<!-- greptile_summary -->` issue comment
// is created on the first review and edited in place on every later one; it
// names the commit it covers ("Last reviewed commit") and lists the
// still-unresolved findings under a Findings heading. Findings themselves are
// inline comments on an empty-body COMMENTED review, each led by a P0–P2
// badge. A "Greptile Review" check run (app slug "greptile-apps") reports the
// outcome. A clean review submits no review object at all, so the summary and
// the check run are its only evidence. Greptile never resolves or edits its own
// threads.

// GreptileLogin is the canonical Greptile GitHub app login.
const GreptileLogin = "greptile-apps[bot]"

const greptileCheckName = "Greptile Review"

var (
	// greptileSummaryRE matches the hidden marker the current summary opens with.
	greptileSummaryRE = regexp.MustCompile(`<!--\s*greptile_summary\s*-->`)
	// greptileLegacySummaryRE matches the headings earlier summaries opened
	// with. They name no reviewed commit and say nothing about cleanliness, so
	// they are only ever notices.
	greptileLegacySummaryRE = regexp.MustCompile(`(?i)\A\s*(?:<h[23]>\s*Greptile (?:Summary|Overview)\s*</h[23]>|#{2,3}\s+Greptile Summary)`)
	// greptileFindingsRE matches the summary's list of unresolved findings: its
	// heading, or any severity badge, so a reworded heading still reads as
	// findings rather than as clean.
	greptileFindingsRE = regexp.MustCompile(`(?im)^\s*(?:<h[1-6]>\s*Findings\s*</h[1-6]>|#{1,6}\s+Findings\s*$)|badges/p[0-3]\.svg`)
	// greptileReviewedRE captures the SHA from the summary's footer:
	// `Last reviewed commit: ["subject"](https://github.com/o/r/commit/<sha>)`.
	greptileReviewedRE = regexp.MustCompile(`(?i)Last reviewed commit:\s*\[[^\]]*\]\([^)]*/commit/([0-9a-f]{7,40})\)`)
	// greptileCreditPauseRE matches the notice posted when the organization's
	// prepaid credits are spent. Reviews stop until someone buys more.
	greptileCreditPauseRE = regexp.MustCompile(`<!--\s*greptile-credit-pause:|(?i)run out of review credits`)
	// greptileBehaviorNoticeRE matches a one-off product announcement.
	greptileBehaviorNoticeRE = regexp.MustCompile(`(?i)Greptile's behavior is changing`)
	// greptileCleanCheckRE matches the check-run summary of a review that left
	// no comments. The count includes the out-of-diff comments Greptile posts
	// on the timeline, so zero really means nothing was found.
	greptileCleanCheckRE = regexp.MustCompile(`(?i)\b0 comments added\b`)
)

// GreptileReviewedCommitSHA extracts the commit the summary says the latest
// review covered, or "" when the text names none.
func GreptileReviewedCommitSHA(text string) string {
	if match := greptileReviewedRE.FindStringSubmatch(text); len(match) == 2 {
		return strings.ToLower(match[1])
	}
	return ""
}

// ClassifyGreptileComment classifies one of Greptile's issue comments.
//
// The summary is clean only when it lists no findings and names the commit it
// covers; otherwise it is a notice, because its findings already arrive as
// inline threads and surfacing the summary too would report each one twice.
// An "Additional Comments" block (findings outside the diff) is deliberately
// left as EvOther: that comment is the only record of those findings.
func ClassifyGreptileComment(body string) CoEvent {
	switch {
	case greptileSummaryRE.MatchString(body):
		sha := GreptileReviewedCommitSHA(body)
		if sha == "" || greptileFindingsRE.MatchString(body) {
			return CoEvent{Kind: EvCoNotice}
		}
		return CoEvent{Kind: EvCoClean, SHA: sha}
	case greptileLegacySummaryRE.MatchString(body), greptileBehaviorNoticeRE.MatchString(body):
		return CoEvent{Kind: EvCoNotice}
	case greptileCreditPauseRE.MatchString(body):
		// No trigger can buy credits, so this is the bot saying it cannot finish.
		return CoEvent{Kind: EvCoUnable}
	}
	return CoEvent{Kind: EvOther}
}

// ClassifyGreptileCheck classifies one check run owned by the "greptile-apps"
// app. Only "Greptile Review" is its review. Completed runs conclude `success`
// whether or not they found anything, so the summary's comment count decides
// clean; a changed wording degrades to CheckDone, where findings still gate
// through their threads.
func ClassifyGreptileCheck(name, title, summary, status, conclusion string) CheckVerdict {
	if name != greptileCheckName {
		return CheckUnrelated
	}
	if !strings.EqualFold(status, "completed") {
		return CheckInProgress
	}
	// No skip Greptile reports means "reviewed", so treat one as non-delivery.
	if checkRunFailed(conclusion) || strings.EqualFold(strings.TrimSpace(conclusion), "skipped") {
		return CheckFailed
	}
	if greptileCleanCheckRE.MatchString(summary) {
		return CheckDoneClean
	}
	return CheckDone
}
