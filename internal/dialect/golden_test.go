package dialect

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goldenCR mirrors the marker defaults in Config, so the corpus classifies
// exactly as production does.
var goldenCR = CodeRabbit{
	CompletionMarker:  "Review finished",
	RateLimitMarker:   "rate limited by coderabbit.ai",
	CalibrationMarker: "auto-generated reply by CodeRabbit",
}

func readGolden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read corpus file: %v", err)
	}
	return string(data)
}

// TestGoldenClassification pins one corpus file per known bot-message format.
// When a bot ships a new phrasing, add a file and a row — the row IS the spec.
func TestGoldenClassification(t *testing.T) {
	cases := []struct {
		file            string
		rateLimited     bool
		paused          bool
		inProgress      bool
		failed          bool
		alreadyDone     bool
		reviewSkipped   bool
		completionReply bool
		autoReply       bool
		noAction        bool
		summaryOnly     bool
		codexClean      bool
		codexUsageLimit bool
		codexSummary    bool
		nonActionable   bool
		availableIn     time.Duration // 0 = no window must parse
		reviewedSHA     string
		inProgressSHA   string
		// author + wantKind pin Classifier.Classify's dominant kind for the file;
		// wantKind == EvOther (the zero value) skips the Classify assertion.
		author   string
		wantKind EventKind
		// registryPrimary classifies the fixture with Codex promoted from a
		// co-reviewer to the configured primary.
		registryPrimary bool
	}{
		{file: "coderabbit/rate-limit-fair-usage.md", rateLimited: true, autoReply: true, availableIn: 48 * time.Minute},
		// Contains the "does not re-review" boilerplate in its help section —
		// must still classify as a rate limit, NOT as an already-reviewed ack.
		{file: "coderabbit/rate-limit-bold-window.md", rateLimited: true, autoReply: true, availableIn: 40 * time.Minute},
		{file: "coderabbit/rate-limit-legacy.md", rateLimited: true, availableIn: 3 * time.Minute},
		// No parseable window: the engine must fall back to its conservative fixed block.
		{file: "coderabbit/rate-limit-no-window.md", rateLimited: true, autoReply: true},
		{file: "coderabbit/review-in-progress.md", inProgress: true},
		{file: "coderabbit/review-failed.md", failed: true},
		{file: "coderabbit/reviews-paused.md", paused: true},
		{file: "coderabbit/no-actionable-comments.md", noAction: true},
		// CodeRabbit Free on a private repo: the walkthrough IS the whole
		// output — no review object ever follows, so crq must run the
		// co-reviewers alone rather than firing (and waiting on) CodeRabbit.
		{file: "coderabbit/summary-only-free-plan.md", summaryOnly: true},
		// "Review skipped": CodeRabbit refused this head outright. It ships WITH
		// the rate-limit marker embedded, so it must classify as EvSkipped and
		// NOT as a rate limit — otherwise crq fabricates a window that never
		// clears, re-fires forever, and blocks the whole account's quota.
		{file: "coderabbit/review-skipped-too-many-files.md", reviewSkipped: true, rateLimited: true,
			author: "coderabbitai[bot]", wantKind: EvSkipped},
		// The SAME "Review skipped" heading, meaning the opposite: auto-review is
		// off and CodeRabbit is inviting the explicit trigger. That is crq's
		// REQUIRED prerequisite, so this notice appears on every push crq
		// manages — reading it as a refusal stops crq firing CodeRabbit
		// everywhere. It must NOT be a skip. (This capture is from a Free
		// private repo, so summaryOnly is true for an unrelated reason.)
		{file: "coderabbit/review-skipped-auto-reviews-disabled.md", reviewSkipped: false, summaryOnly: true},
		{file: "coderabbit/already-reviewed.md", alreadyDone: true, autoReply: true},
		{file: "coderabbit/completion-reply.md", completionReply: true, autoReply: true},
		// The standalone trailer is an ack; a real finding CARRYING the trailer
		// must stay actionable (a substring match dropped four real findings).
		{file: "coderabbit/thread-ack-also-applies.md", nonActionable: true},
		{file: "coderabbit/finding-with-also-applies-trailer.md"},
		{file: "codex/clean-summary-legacy.md", codexClean: true, noAction: true, nonActionable: true, author: "chatgpt-codex-connector[bot]", wantKind: EvCoClean},
		{file: "codex/clean-summary-tada.md", codexClean: true, noAction: true, nonActionable: true, reviewedSHA: "4d9e8bca82", author: "chatgpt-codex-connector[bot]", wantKind: EvCoClean},
		{file: "codex/review-summary-running.md", codexSummary: true, nonActionable: true, inProgressSHA: "15d9ec9", author: CodexBotLogin, wantKind: EvCoInProgress},
		{file: "codex/usage-limit.md", codexUsageLimit: true, nonActionable: true, author: "chatgpt-codex-connector[bot]", wantKind: EvCoUnable},
		{file: "codex/clean-summary-tada.md", codexClean: true, noAction: true, nonActionable: true, reviewedSHA: "4d9e8bca82",
			author: "chatgpt-codex-connector[bot]", wantKind: EvNoAction, registryPrimary: true},
		{file: "codex/usage-limit.md", codexUsageLimit: true, nonActionable: true,
			author: "chatgpt-codex-connector[bot]", wantKind: EvFailed, registryPrimary: true},
		{file: "codex/review-summary-running.md", codexSummary: true, nonActionable: true, inProgressSHA: "15d9ec9",
			author: CodexBotLogin, wantKind: EvInProgress, registryPrimary: true},
		// Codex's "create an environment" platform ad, posted as a thread reply —
		// never a finding, never a rebuttal.
		{file: "codex/environment-notice.md", nonActionable: true, author: "chatgpt-codex-connector[bot]", wantKind: EvCoNotice},
		{file: "codex/review-command.md", author: "kristofferR", wantKind: EvCoCommand},
	}
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	classifier := Classifier{CodeRabbit: goldenCR, Bot: "coderabbitai[bot]", ReviewCommand: "@coderabbitai review", CoReviewers: KnownCoReviewers()}
	for _, tc := range cases {
		name := tc.file
		if tc.registryPrimary {
			name += "/registry-primary"
		}
		t.Run(name, func(t *testing.T) {
			body := readGolden(t, tc.file)
			activeClassifier := classifier
			if tc.registryPrimary {
				primary, ok := CoReviewerByName("codex")
				if !ok {
					t.Fatal("Codex registry entry is missing")
				}
				activeClassifier.Bot, activeClassifier.Primary = primary.Login, &primary
			}
			checks := []struct {
				name string
				got  bool
				want bool
			}{
				{"IsRateLimited", goldenCR.IsRateLimited(body), tc.rateLimited},
				{"IsReviewsPaused", goldenCR.IsReviewsPaused(body), tc.paused},
				{"IsReviewInProgress", goldenCR.IsReviewInProgress(body), tc.inProgress},
				{"IsReviewFailure", goldenCR.IsReviewFailure(body), tc.failed},
				{"IsReviewAlreadyDone", goldenCR.IsReviewAlreadyDone(body), tc.alreadyDone},
				{"IsCompletionReply", goldenCR.IsCompletionReply(body), tc.completionReply},
				{"IsAutoReply", goldenCR.IsAutoReply(body), tc.autoReply},
				{"IsNoActionReviewCompletion", IsNoActionReviewCompletion(body), tc.noAction},
				{"IsSummaryOnlyPlan", goldenCR.IsSummaryOnlyPlan(body), tc.summaryOnly},
				{"IsReviewSkipped", goldenCR.IsReviewSkipped(body), tc.reviewSkipped},
				{"IsCodexNoActionReviewCompletion", IsCodexNoActionReviewCompletion(body), tc.codexClean},
				{"IsCodexUsageLimit", IsCodexUsageLimit(body), tc.codexUsageLimit},
				{"IsCodexReviewSummary", IsCodexReviewSummary(body), tc.codexSummary},
				{"IsNonActionableText", IsNonActionableText(body), tc.nonActionable},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
				}
			}
			reset := ParseAvailableIn(body, base)
			if tc.availableIn == 0 {
				if reset != nil {
					t.Errorf("ParseAvailableIn = %v, want none", reset)
				}
			} else if reset == nil || !reset.Equal(base.Add(tc.availableIn)) {
				t.Errorf("ParseAvailableIn = %v, want base+%v", reset, tc.availableIn)
			}
			if got := CodexReviewedCommitSHA(body); got != tc.reviewedSHA {
				t.Errorf("CodexReviewedCommitSHA = %q, want %q", got, tc.reviewedSHA)
			}
			if got := CodexReviewInProgressSHA(body); got != tc.inProgressSHA {
				t.Errorf("CodexReviewInProgressSHA = %q, want %q", got, tc.inProgressSHA)
			}
			if tc.wantKind != EvOther {
				if got := activeClassifier.Classify(tc.author, body, 1, base, base).Kind; got != tc.wantKind {
					t.Errorf("Classify kind = %v, want %v", got, tc.wantKind)
				}
			}
			// SummaryOnly rides alongside the dominant kind, so it is asserted
			// on the classified event rather than through wantKind.
			if strings.HasPrefix(tc.file, "coderabbit/") {
				if got := classifier.Classify("coderabbitai[bot]", body, 1, base, base).SummaryOnly; got != tc.summaryOnly {
					t.Errorf("Classify SummaryOnly = %v, want %v", got, tc.summaryOnly)
				}
			}
		})
	}
}

// TestGoldenFindings pins the review-body finding extractors against real
// review-body markup shapes.
func TestGoldenFindings(t *testing.T) {
	meta := ReviewMeta{
		ID:          99,
		CommitID:    "abcdef1234567890",
		HTMLURL:     "https://example.test/r/99",
		SubmittedAt: time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
	}
	type want struct {
		path     string
		line     int
		severity string // "" = don't check
		scale    string // "" = don't check
		title    string // "" = don't check
		source   string
		commit   string // "" = don't check
	}
	cases := []struct {
		file string
		bot  string
		want []want
	}{
		{
			file: "coderabbit/findings-outside-diff.md",
			bot:  "coderabbitai[bot]",
			want: []want{{path: "internal/foo.go", line: 42, severity: "major", title: "Fix the cancellation path.", source: "review_body"}},
		},
		{
			file: "coderabbit/findings-nested-quotes.md",
			bot:  "coderabbitai[bot]",
			want: []want{
				{path: "internal/deep.go", line: 10, severity: "major", title: "Nested finding one.", source: "review_body"},
				{path: "internal/deeper.go", line: 20, severity: "minor", title: "Nested finding two.", source: "review_body"},
			},
		},
		{
			file: "coderabbit/findings-failed-to-post.md",
			bot:  "coderabbitai[bot]",
			want: []want{{path: "src-tauri/inject/messenger.js", line: 561, severity: "major", title: "Move the hide-names toggle out of `messenger.js` or update the allowlist first.", source: "review_body"}},
		},
		{
			file: "coderabbit/findings-prompt-block.md",
			bot:  "coderabbitai[bot]",
			want: []want{
				{path: "src/app.ts", line: 12, source: "review_prompt"},
				{path: "README.md", line: 7, source: "review_prompt"},
			},
		},
		{
			file: "codex/findings-outside-diff.md",
			bot:  "chatgpt-codex-connector[bot]",
			want: []want{{path: "convex/sections/aiCommands.ts", line: 2170, severity: "potential", scale: "P2", title: "Query learning history by topic before taking", source: "review_body", commit: "347388ffd"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			body := readGolden(t, tc.file)
			got := ParseReviewBodyFindings(body, meta, tc.bot)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d findings, want %d: %#v", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				f := got[i]
				if f.Path != w.path || f.Line != w.line {
					t.Errorf("finding %d location = %s:%d, want %s:%d", i, f.Path, f.Line, w.path, w.line)
				}
				if w.severity != "" && f.Severity != w.severity {
					t.Errorf("finding %d severity = %q, want %q", i, f.Severity, w.severity)
				}
				if w.scale != "" && f.Scale != w.scale {
					t.Errorf("finding %d scale = %q, want %q", i, f.Scale, w.scale)
				}
				if w.title != "" && f.Title != w.title {
					t.Errorf("finding %d title = %q, want %q", i, f.Title, w.title)
				}
				if f.Source != w.source {
					t.Errorf("finding %d source = %q, want %q", i, f.Source, w.source)
				}
				if w.commit != "" && f.Commit != w.commit {
					t.Errorf("finding %d commit = %q, want %q", i, f.Commit, w.commit)
				}
			}
		})
	}
}

// TestGoldenReplyVerdict pins the concede/contest classification of a bot's
// reply to the agent's decline, using CodeRabbit's real replies from PR #30.
func TestGoldenReplyVerdict(t *testing.T) {
	cases := []struct {
		file      string
		withdrawn bool
		retained  bool
	}{
		{file: "coderabbit/reply-withdrawn.md", withdrawn: true},
		{file: "coderabbit/reply-withdrawn-confirmed.md", withdrawn: true},
		// A concession whose PROSE reads like agreement, not like the stock
		// "withdrawing this" phrasing. CodeRabbit ships a machine-readable
		// marker with it; matching that is what keeps a settled finding from
		// re-surfacing as a rebuttal and blocking convergence.
		{file: "coderabbit/reply-withdrawn-marker.md", withdrawn: true},
		{file: "coderabbit/reply-retained.md", retained: true},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			body := readGolden(t, tc.file)
			if got := IsReviewFindingWithdrawn(body); got != tc.withdrawn {
				t.Errorf("IsReviewFindingWithdrawn = %v, want %v", got, tc.withdrawn)
			}
			if got := IsReviewFindingRetained(body); got != tc.retained {
				t.Errorf("IsReviewFindingRetained = %v, want %v", got, tc.retained)
			}
			// The verdict is what callers act on, and its wording travels with it:
			// only a stated rebuttal may claim to contest the decline.
			verdict := ClassifyDeclineReply(body)
			switch {
			case tc.withdrawn && verdict != ReplyWithdrawn:
				t.Errorf("ClassifyDeclineReply = %v, want ReplyWithdrawn", verdict)
			case tc.retained && verdict != ReplyRetained:
				t.Errorf("ClassifyDeclineReply = %v, want ReplyRetained", verdict)
			}
			if contests := strings.Contains(verdict.TitlePrefix(), "contests"); contests != (verdict == ReplyRetained) {
				t.Errorf("TitlePrefix for %v claims contest=%v", verdict, contests)
			}
		})
	}
}

// TestGoldenCoReviewers pins the Bugbot/Macroscope corpus: comment
// classification through the registry-backed Classifier, the reviewed/resolved
// SHA extractors, the BUG_ID dedupe key, and that carrier/summary bodies parse
// as ZERO review-body findings (their findings live in inline threads).
func TestGoldenCoReviewers(t *testing.T) {
	classifier := Classifier{
		CodeRabbit:    goldenCR,
		Bot:           "coderabbitai[bot]",
		ReviewCommand: "@coderabbitai review",
		CoReviewers:   KnownCoReviewers(),
	}
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	truth, falsehood := true, false
	cases := []struct {
		file          string
		author        string
		wantKind      EventKind
		wantFor       string
		wantApproved  *bool  // EvCoVerdict only
		reviewedSHA   string // BugbotReviewedCommitSHA
		resolvedSHA   string // MacroscopeResolvedInSHA
		dedupeKey     string // BugbotFindingDedupeKey ("" = none)
		bodyFindings  int    // ParseReviewBodyFindings count as this bot
		summaryReview bool   // IsBugbotReviewSummary
	}{
		{
			file: "bugbot/review-summary-issues.md", author: BugbotLogin,
			wantKind: EvOther, wantFor: BugbotLogin, summaryReview: true,
			reviewedSHA: "2218b91213dd6303e65cf14faea4af55587342e5",
		},
		{
			file: "bugbot/inline-finding-high.md", author: BugbotLogin,
			wantKind: EvOther, wantFor: BugbotLogin,
			reviewedSHA: "299d961f670337e6c10d020a489380ddcb69ad1e",
			dedupeKey:   "c76cc5f6-52df-4e72-8076-e2535882a772",
		},
		{
			file: "bugbot/inline-finding-medium.md", author: BugbotLogin,
			wantKind: EvOther, wantFor: BugbotLogin,
			reviewedSHA: "f222834e847b66f8389a9b35e1bd0ce1dbb10ba8",
			dedupeKey:   "d228c05b-14a4-4184-81ea-44242ad98ce2",
		},
		{file: "bugbot/trigger-command.md", author: "kristofferR", wantKind: EvCoCommand, wantFor: BugbotLogin},
		{file: "bugbot/trigger-command-alt.md", author: "kristofferR", wantKind: EvCoCommand, wantFor: BugbotLogin},
		{file: "macroscope/trigger-command.md", author: "kristofferR", wantKind: EvCoCommand, wantFor: MacroscopeLogin},
		{
			file: "macroscope/approvability-approved.md", author: MacroscopeLogin,
			wantKind: EvCoVerdict, wantFor: MacroscopeLogin, wantApproved: &truth,
		},
		{
			file: "macroscope/approvability-needs-human.md", author: MacroscopeLogin,
			wantKind: EvCoVerdict, wantFor: MacroscopeLogin, wantApproved: &falsehood,
		},
		// Open findings carry no MURMUR_IGNORE marker and no resolved line.
		{file: "macroscope/inline-finding-high.md", author: MacroscopeLogin, wantKind: EvOther, wantFor: MacroscopeLogin},
		{file: "macroscope/inline-finding-medium.md", author: MacroscopeLogin, wantKind: EvOther, wantFor: MacroscopeLogin},
		// Macroscope EDITS a finding to append its settled marker — the edit IS
		// its resolution, in either wording.
		{
			file: "macroscope/inline-finding-resolved.md", author: MacroscopeLogin,
			wantKind: EvCoNotice, wantFor: MacroscopeLogin,
			resolvedSHA: "148c355df49cc1692434f4d6689f53666523cadc",
		},
		{
			file: "macroscope/inline-finding-no-longer-relevant.md", author: MacroscopeLogin,
			wantKind: EvCoNotice, wantFor: MacroscopeLogin,
			resolvedSHA: "6a06232237270dfc6d1e39af9611ce2ac3349ce5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			body := readGolden(t, tc.file)
			ev := classifier.Classify(tc.author, body, 1, base, base)
			if ev.Kind != tc.wantKind {
				t.Errorf("Classify kind = %v, want %v", ev.Kind, tc.wantKind)
			}
			if ev.For != tc.wantFor {
				t.Errorf("Classify For = %q, want %q", ev.For, tc.wantFor)
			}
			if tc.wantApproved != nil && (ev.Approved == nil || *ev.Approved != *tc.wantApproved) {
				t.Errorf("Classify Approved = %v, want %v", ev.Approved, *tc.wantApproved)
			}
			if got := IsBugbotReviewSummary(body); got != tc.summaryReview {
				t.Errorf("IsBugbotReviewSummary = %v, want %v", got, tc.summaryReview)
			}
			if strings.HasPrefix(tc.file, "bugbot/") {
				if got := BugbotReviewedCommitSHA(body); got != tc.reviewedSHA {
					t.Errorf("BugbotReviewedCommitSHA = %q, want %q", got, tc.reviewedSHA)
				}
				key, ok := BugbotFindingDedupeKey(body)
				if ok != (tc.dedupeKey != "") || key != tc.dedupeKey {
					t.Errorf("BugbotFindingDedupeKey = %q,%v, want %q", key, ok, tc.dedupeKey)
				}
			}
			if strings.HasPrefix(tc.file, "macroscope/") {
				if got := MacroscopeResolvedInSHA(body); got != tc.resolvedSHA {
					t.Errorf("MacroscopeResolvedInSHA = %q, want %q", got, tc.resolvedSHA)
				}
			}
			meta := ReviewMeta{ID: 7, SubmittedAt: base}
			if got := ParseReviewBodyFindings(body, meta, tc.author); len(got) != tc.bodyFindings {
				t.Errorf("ParseReviewBodyFindings = %d findings, want %d: %#v", len(got), tc.bodyFindings, got)
			}
		})
	}

	// The Bugbot footer must strip from surfaced finding bodies.
	high := readGolden(t, "bugbot/inline-finding-high.md")
	if cleaned := CleanBugbotCommentText(high); strings.Contains(cleaned, "Reviewed by [Cursor Bugbot]") {
		t.Errorf("CleanBugbotCommentText left the footer: %q", cleaned)
	}
	// Severity vocabulary maps through the shared SeverityOf.
	if got := SeverityOf("**High Severity**"); got != "major" {
		t.Errorf("SeverityOf(High) = %q", got)
	}
	if got := SeverityOf("🟡 **Medium** `a.ts:1`"); got != "potential" {
		t.Errorf("SeverityOf(Medium) = %q", got)
	}
}

// TestGoldenCheckRuns pins check-run classification against the captured
// check-run objects — the only place Bugbot reports a clean round, and the
// name-prefix-only rule for Macroscope's custom checks (whose output titles
// can be garbled — check-custom.json's is literally "O").
func TestGoldenCheckRuns(t *testing.T) {
	type run struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Concl  string `json:"conclusion"`
		App    struct {
			Slug string `json:"slug"`
		} `json:"app"`
		Output struct {
			Title   string `json:"title"`
			Summary string `json:"summary"`
		} `json:"output"`
	}
	cases := []struct {
		file        string
		wantLogin   string
		wantVerdict CheckVerdict
	}{
		{"bugbot/check-clean.json", BugbotLogin, CheckDoneClean},
		{"bugbot/check-issues.json", BugbotLogin, CheckDone},
		{"bugbot/check-in-progress.json", BugbotLogin, CheckInProgress},
		{"macroscope/check-correctness-clean.json", MacroscopeLogin, CheckDoneClean},
		{"macroscope/check-correctness-issues.json", MacroscopeLogin, CheckDone},
		// `skipped` means two opposite things, so the title decides. Nothing to
		// analyse is a clean round; a billing-blocked workspace never reviewed at
		// all and no re-trigger can change that (CheckUnable, not CheckFailed —
		// nudging it every round would be pure comment spam). The billing capture
		// is trimmed to the classifier-relevant fields: it comes from a private
		// repo, and the envelope would publish its name and commits.
		{"macroscope/check-correctness-skipped-no-code.json", MacroscopeLogin, CheckDoneClean},
		{"macroscope/check-correctness-skipped-billing.json", MacroscopeLogin, CheckUnable},
		// `neutral` is an ordinary completed conclusion — a real review with
		// findings concludes neutral too — so only the error TITLE marks a run
		// that did not deliver.
		{"macroscope/check-correctness-neutral-issues.json", MacroscopeLogin, CheckDone},
		{"macroscope/check-correctness-error.json", MacroscopeLogin, CheckFailed},
		// Only the Correctness Check is Macroscope's REVIEW. Approvability and
		// repo-custom checks routinely complete first, so counting them as a
		// finished review would let the round converge while correctness is
		// still running — they are auxiliary: participation, never completion.
		{"macroscope/check-approvability-approved.json", MacroscopeLogin, CheckAuxiliary},
		{"macroscope/check-approvability-not-eligible.json", MacroscopeLogin, CheckAuxiliary},
		{"macroscope/check-custom.json", MacroscopeLogin, CheckAuxiliary},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			var r run
			if err := json.Unmarshal([]byte(readGolden(t, tc.file)), &r); err != nil {
				t.Fatal(err)
			}
			login, verdict := ClassifyCheckRun(r.App.Slug, r.Name, r.Output.Title, r.Output.Summary, r.Status, r.Concl)
			if login != tc.wantLogin || verdict != tc.wantVerdict {
				t.Errorf("ClassifyCheckRun = %q,%v, want %q,%v", login, verdict, tc.wantLogin, tc.wantVerdict)
			}
		})
	}
	// A check from an unrelated app never binds to a co-reviewer.
	if login, verdict := ClassifyCheckRun("github-actions", "CI", "ok", "", "completed", "success"); login != "" || verdict != CheckUnrelated {
		t.Errorf("unrelated check classified as %q,%v", login, verdict)
	}
	// A skip cause crq has never seen — a new wording, or one Macroscope has yet
	// to ship — must fail closed. Reading it as a delivered review would mark a
	// required Macroscope reviewed on a run that produced no threads at all, and
	// the round would converge having been reviewed by nobody.
	if _, verdict := ClassifyCheckRun("macroscopeapp", macroscopeCorrectnessCheck,
		"Review skipped — some future reason", "", "completed", "skipped"); verdict != CheckFailed {
		t.Errorf("unrecognized skip = %v, want CheckFailed (fail closed)", verdict)
	}
	// Another cursor-app check that is not the Bugbot review stays unrelated.
	if login, verdict := ClassifyCheckRun("cursor", "Cursor Something Else", "", "", "completed", "success"); login != "" || verdict != CheckUnrelated {
		t.Errorf("non-review cursor check classified as %q,%v", login, verdict)
	}
}

// TestGoldenReviewSkipped pins the parts of the "Review skipped" notice crq
// acts on: the head it binds to (so a reworked PR fires again) and the reason
// it surfaces to the agent (so the PR actually gets narrowed).
func TestGoldenReviewSkipped(t *testing.T) {
	body := readGolden(t, "coderabbit/review-skipped-too-many-files.md")

	if got := ReviewSkippedHeadSHA(body); got != "56150a0423a243224b03f355c3a3ba6941011b5b" {
		t.Errorf("ReviewSkippedHeadSHA = %q", got)
	}
	if got := ReviewSkippedReason(body); !strings.Contains(got, "Too many files") {
		t.Errorf("ReviewSkippedReason = %q, want the file-count reason", got)
	}
	// The skip must never be mistaken for a timed block: no window parses, so
	// treating it as a rate limit would invent a fallback that never clears.
	if at := ParseAvailableIn(body, time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)); at != nil {
		t.Errorf("ParseAvailableIn = %v, want none (a skip has no window)", at)
	}
	// A normal rate limit must NOT read as a skip.
	if got := goldenCR.IsReviewSkipped(readGolden(t, "coderabbit/rate-limit-fair-usage.md")); got {
		t.Error("a real rate-limit notice must not classify as a skipped review")
	}
	// Nor may ordinary prose mentioning the words trip it — a false positive
	// stops crq firing CodeRabbit on a PR it could really review.
	for _, prose := range []string{
		"The review skipped nothing important.",
		"> Some files had their review skipped for brevity.",
	} {
		if goldenCR.IsReviewSkipped(prose) {
			t.Errorf("prose must not classify as skipped: %q", prose)
		}
	}
}

// TestCheckRunsThatAreNotReviewEvidence pins the two ways a check run can be
// present and completed without meaning "this bot reviewed the code". Both were
// reported by Codex on the co-reviewer PR: counting either as completion lets a
// round converge with no findings while the real review is still running (or
// never ran at all).
func TestCheckRunsThatAreNotReviewEvidence(t *testing.T) {
	cases := []struct {
		name       string
		check      string
		status     string
		conclusion string
		want       CheckVerdict
	}{
		// A crashed review is still status "completed".
		{"bugbot failure", bugbotCheckName, "completed", "failure", CheckFailed},
		{"bugbot cancelled", bugbotCheckName, "completed", "cancelled", CheckFailed},
		{"bugbot timed out", bugbotCheckName, "completed", "timed_out", CheckFailed},
		// The good paths must keep working.
		{"bugbot findings", bugbotCheckName, "completed", "neutral", CheckDone},
		{"bugbot running", bugbotCheckName, "in_progress", "", CheckInProgress},
		// Macroscope's non-correctness checks are auxiliary whatever they conclude.
		{"macroscope approvability", "Macroscope - Approvability Check", "completed", "success", CheckAuxiliary},
		{"macroscope custom", "Macroscope - Effect Service Conventions", "completed", "failure", CheckAuxiliary},
		{"macroscope correctness failed", macroscopeCorrectnessCheck, "completed", "failure", CheckFailed},
		{"macroscope correctness done", macroscopeCorrectnessCheck, "completed", "neutral", CheckDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slug := "cursor"
			if strings.HasPrefix(tc.check, macroscopeCheckPrefix) {
				slug = "macroscopeapp"
			}
			_, got := ClassifyCheckRun(slug, tc.check, "", "", tc.status, tc.conclusion)
			if got != tc.want {
				t.Fatalf("verdict = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAutoReviewsDisabledIsNotARefusal guards the regression that broke crq's
// core function: "Review skipped — Auto reviews are disabled on this
// repository … To trigger a single review, invoke the `@coderabbitai review`
// command" shares a heading with the real refusals but means the opposite. It
// says a review IS available on request, and because crq REQUIRES auto-review
// to be off it appears on every push crq manages. Classifying it as a refusal
// makes PrimaryReviewUnavailable true everywhere, so crq stops firing
// CodeRabbit on every repo it manages and every loop exits instantly on an
// unresolvable, thread-less finding.
func TestAutoReviewsDisabledIsNotARefusal(t *testing.T) {
	body := readGolden(t, "coderabbit/review-skipped-auto-reviews-disabled.md")
	if goldenCR.IsReviewSkipped(body) {
		t.Fatal("the auto-review-disabled notice must never read as a refusal to review")
	}
	// The classifier must still see it as an ordinary CodeRabbit comment, not
	// as the EvSkipped state.
	classifier := Classifier{CodeRabbit: goldenCR, Bot: "coderabbitai[bot]",
		ReviewCommand: "@coderabbitai review", CoReviewers: KnownCoReviewers()}
	base := time.Date(2026, 7, 24, 21, 15, 52, 0, time.UTC)
	if got := classifier.Classify("coderabbitai[bot]", body, 1, base, base).Kind; got == EvSkipped {
		t.Fatal("Classify must not report EvSkipped for the auto-review-disabled notice")
	}
	// The genuine refusal still classifies, so the fix is a discrimination and
	// not a blanket disable.
	if !goldenCR.IsReviewSkipped(readGolden(t, "coderabbit/review-skipped-too-many-files.md")) {
		t.Fatal("a real refusal must still be recognised")
	}
}

// The CLI's error vocabulary is CodeRabbit's, so it lives here with a verbatim
// fixture like every other wording crq depends on. The fixture is the real
// --agent stream event captured from a blocked account.
func TestGoldenCLIRateLimit(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "coderabbit", "cli-rate-limit.json"))
	if err != nil {
		t.Fatal(err)
	}
	var event struct {
		ErrorType   string `json:"errorType"`
		Recoverable bool   `json:"recoverable"`
		Metadata    struct {
			WaitTime string `json:"waitTime"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	if !IsCLIRateLimit(event.ErrorType) {
		t.Errorf("IsCLIRateLimit(%q) = false, want true", event.ErrorType)
	}
	// The whole event shape is dialect's contract, so parse the captured fixture
	// through the real parser rather than re-reading its keys by hand here.
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatal(err)
	}
	parsed := ParseCLIError(asMap)
	if !parsed.IsAccountBlock() {
		t.Errorf("ParseCLIError(%v).IsAccountBlock() = false, want true", parsed)
	}
	if !parsed.Recoverable || !parsed.OrgAttributed || parsed.WaitTime == "" {
		t.Errorf("ParseCLIError lost fields: %+v", parsed)
	}
	// The window matters as much as the classification: crq records this block
	// for the whole fleet, so parse the fixture's real waitTime rather than a
	// hand-written string.
	base := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	if got := ParseCLIWaitTime(event.Metadata.WaitTime, base); got == nil {
		t.Errorf("ParseCLIWaitTime(%q) = nil, want a window", event.Metadata.WaitTime)
	} else if want := base.Add(32 * time.Minute); !got.Equal(want) {
		t.Errorf("ParseCLIWaitTime(%q) = %s, want %s", event.Metadata.WaitTime, got, want)
	}
	// An unreadable or nonsensical value must not become a window in the past:
	// the caller falls back to a conservative block instead.
	for _, bad := range []string{"", "soon", "-5 minutes", "shortly"} {
		if got := ParseCLIWaitTime(bad, base); got != nil {
			t.Errorf("ParseCLIWaitTime(%q) = %s, want nil", bad, got)
		}
	}
	if !event.Recoverable || event.Metadata.WaitTime == "" {
		t.Errorf("the fixture must carry the recoverable flag and a wait time: %+v", event)
	}
	for _, other := range []string{"", "auth", "network", "rate_limits"} {
		if IsCLIRateLimit(other) {
			t.Errorf("IsCLIRateLimit(%q) = true, want false", other)
		}
	}
}

// TestGoldenPricing pins the money vocabulary against the same corpus every
// other classifier is pinned against. Both payloads were already captured and
// their billing fields discarded; these rows are the spec for reading them.
func TestGoldenPricing(t *testing.T) {
	t.Run("macroscope agent credits", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join("testdata", "macroscope", "check-custom.json"))
		if err != nil {
			t.Fatal(err)
		}
		var run struct {
			Output struct {
				Text string `json:"text"`
			} `json:"output"`
		}
		if err := json.Unmarshal(raw, &run); err != nil {
			t.Fatal(err)
		}
		credits, ok := ParseAgentCredits(run.Output.Text)
		if !ok || credits != 81 {
			t.Fatalf("credits = %d, %v; want 81, true", credits, ok)
		}
		// A body without the line is not zero credits — it is no answer.
		if _, ok := ParseAgentCredits("no credits line here"); ok {
			t.Error("a body with no credit line must report absence, not 0")
		}
	})

	t.Run("coderabbit cli billing metadata", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join("testdata", "coderabbit", "cli-rate-limit.json"))
		if err != nil {
			t.Fatal(err)
		}
		var event map[string]any
		if err := json.Unmarshal(raw, &event); err != nil {
			t.Fatal(err)
		}
		got := ParseCLIError(event)
		if got.ProUser {
			t.Error("isProUser is false in this capture")
		}
		if !strings.Contains(got.PolicyGuidance, "$0.25/file") {
			t.Errorf("policy guidance = %q, want the vendor's own overage price kept verbatim", got.PolicyGuidance)
		}
		// The guidance INVITES enabling usage-based reviews, so they are off —
		// which means this block costs nothing and simply waits.
		if got.UsageBasedEnabled {
			t.Error("guidance offering to enable usage-based reviews means they are not enabled")
		}
	})

	t.Run("estimates", func(t *testing.T) {
		// Under the 10 KB minimum the floor IS the price, so it is exact.
		small := EstimateMacroscope(DiffStat{Additions: 40, Deletions: 20, ChangedFiles: 3})
		if !small.Exact || small.Low != 0.50 || small.High != 0.50 {
			t.Errorf("small diff = %+v, want an exact $0.50 floor", small)
		}
		// A large one is a range, and never above the per-review cap.
		large := EstimateMacroscope(DiffStat{Additions: 40000, Deletions: 20000, ChangedFiles: 300})
		if large.High > 10.0 || large.Low != macroscopeMinKB*macroscopePerKB || large.Low >= large.High || large.Exact {
			t.Errorf("large diff = %+v, want the incremental floor through the capped whole-diff upper bound", large)
		}

		// A co-reviewer on its own subscription is free and says why; an
		// unknown login is Unknown, never a confident $0.00.
		if e := EstimateCost(CodexBotLogin, "coderabbitai[bot]", DiffStat{}, Allowance{}); !e.Exact || e.High != 0 || e.Basis == "" || e.Metered {
			t.Errorf("codex = %+v, want an explained zero", e)
		}
		if e := EstimateCost("sonar[bot]", "coderabbitai[bot]", DiffStat{}, Allowance{}); !e.Unknown {
			t.Errorf("unknown bot = %+v, want Unknown rather than free", e)
		}

		// The VENDOR decides the basis, not the role. CRQ_BOT may name a
		// registry bot, and pricing whichever one is configured on CodeRabbit's
		// allowance model billed a Macroscope primary on the wrong basis
		// entirely and hid a Codex primary's subscription behind an allowance
		// it does not use.
		big := DiffStat{Additions: 40000, Deletions: 20000, ChangedFiles: 300}
		spent := Allowance{RemainingKnown: true, UsageBasedKnown: true, UsageBasedEnabled: true}
		if e := EstimateCost(MacroscopeLogin, MacroscopeLogin, big, spent); e != EstimateMacroscope(big) {
			t.Errorf("macroscope primary = %+v, want its own per-kilobyte price", e)
		}
		if e := EstimateCost(CodexBotLogin, CodexBotLogin, big, spent); !e.Exact || e.High != 0 {
			t.Errorf("codex primary = %+v, want the subscription it is actually covered by", e)
		}

		// The primary is free inside the allowance, unknown without a count,
		// and priced per file only once past it WITH usage-based billing on.
		d := DiffStat{ChangedFiles: 8}
		if e := EstimateCodeRabbit("coderabbitai[bot]", d, Allowance{Remaining: 3, RemainingKnown: true}); !e.Exact || e.High != 0 || !e.Metered {
			t.Errorf("inside allowance = %+v, want free", e)
		}
		if e := EstimateCodeRabbit("coderabbitai[bot]", d, Allowance{}); !e.Unknown {
			t.Errorf("no count = %+v, want Unknown — absent is not exhausted", e)
		}
		if e := EstimateCodeRabbit("coderabbitai[bot]", d, Allowance{RemainingKnown: true}); !e.Unknown {
			t.Errorf("exhausted, billing mode unknown = %+v, want Unknown — unknown is not off", e)
		}
		if e := EstimateCodeRabbit("coderabbitai[bot]", d, Allowance{RemainingKnown: true, UsageBasedKnown: true}); !e.Exact || e.High != 0 {
			t.Errorf("exhausted with billing off = %+v, want free (it waits instead)", e)
		}
		e := EstimateCodeRabbit("coderabbitai[bot]", d, Allowance{RemainingKnown: true, UsageBasedKnown: true, UsageBasedEnabled: true})
		if e.High != 2.0 {
			t.Errorf("exhausted with billing on = %+v, want up to 8 files x $0.25", e)
		}
	})
}
