package dialect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// A listing exists to be read. Both bots wrap their title in badge images and
// HTML, and CodeRabbit prefixes a rubric that is identical on every finding, so
// the raw first line shows everything except what the finding says.
func TestThreadTitleReadsAsText(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			"codex badge",
			"**<sub><sub>![P2 Badge](https://img.shields.io/badge/P2-yellow?style=flat)</sub></sub>  Remove the primary from the legacy view**\n\nWhen CRQ_BOT names a registry bot...",
			"Remove the primary from the legacy view",
		},
		{
			"coderabbit rubric",
			"_🎯 Functional Correctness_ | _🟡 Minor_ | _⚡ Quick win_\n\n**Reject flags supplied as option values.**\n\nDetail follows.",
			"Reject flags supplied as option values.",
		},
		{"plain", "Just a sentence about the bug.", "Just a sentence about the bug."},
		{
			// A pipe is allowed in a title; only the fixed rubric header goes.
			"pipe in the title",
			"**Support A | B configuration**\n\nDetail.",
			"Support A | B configuration",
		},
		{
			// Underscores inside an identifier are content, not emphasis.
			"identifier with underscores",
			"**Handle user_id and api_key**\n\nDetail.",
			"Handle user_id and api_key",
		},
		{
			// The code span goes, the identifier stays whole: a run of markup is
			// one delimiter kind, so the backtick does not carry the dunder with
			// it and leave the title talking about "init".
			"identifier with leading underscores in a code span",
			"**Preserve `__init__` ordering**\n\nDetail.",
			"Preserve __init__ ordering",
		},
		{
			"code span before punctuation",
			"**Handle no-op changes in `SetFleetSolver`.**\n\nDetail.",
			"Handle no-op changes in SetFleetSolver.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ThreadTitle(true, tc.body); got != tc.want {
				t.Errorf("threadTitle = %q, want %q", got, tc.want)
			}
		})
	}

	long := ThreadTitle(true, strings.Repeat("x", 400))
	if len([]rune(long)) > 120 {
		t.Errorf("title is %d chars; a listing line must stay short", len([]rune(long)))
	}

	// Truncation cuts runes, not bytes: splitting a multi-byte character leaves
	// invalid UTF-8, which JSON renders as a replacement character.
	wide := ThreadTitle(true, strings.Repeat("é", 400))
	if !utf8.ValidString(wide) {
		t.Errorf("truncated title is not valid UTF-8: %q", wide)
	}
	if strings.Contains(wide, "\uFFFD") {
		t.Errorf("truncation split a character: %q", wide)
	}
}

func TestReviewLabelsComeFromTheRubricNotTheExplanation(t *testing.T) {
	body := `_🎯 Functional Correctness_ | _🟡 Minor_ | _⚡ Quick win_

**Preserve the setting.**

This touches a major code path, but the finding itself is minor.`
	labels := ReviewLabelsFor("coderabbitai[bot]", body)
	if labels.Category != "Functional Correctness" || labels.Severity != "minor" ||
		labels.Scale != "Minor" || labels.Effort != "Quick win" {
		t.Fatalf("labels = %+v", labels)
	}
	if got := SeverityFor("coderabbitai[bot]", body); got != "minor" {
		t.Fatalf("SeverityFor = %q, want rubric severity minor", got)
	}
}

func TestDataIntegrityRubricDoesNotBecomeTheFindingTitle(t *testing.T) {
	body := `_🗄️ Data Integrity & Integration_ | _🟠 Major_ | _⚡ Quick win_

**Keep archived dispatch state intact.**

The rubric belongs in chips, not in the finding title.`
	if got := ReviewTitleFor("coderabbitai[bot]", body); got != "Keep archived dispatch state intact." {
		t.Fatalf("ReviewTitleFor = %q", got)
	}
}

func TestCodeRabbitTitleIgnoresAnalysisBeforeFinding(t *testing.T) {
	body := `_🔒 Security & Privacy_ | _🟠 Major_ | _⚡ Quick win_

<details>
<summary>🧩 Analysis chain</summary>

` + "```shell" + `
# How is the token resolved, and is it re-read anywhere else?
rg LookupToken
` + "```" + `
</details>

**Refresh the GitHub token before using cached icon misses.**

The startup token can expire.`
	if got := ReviewTitleFor("coderabbitai[bot]", body); got != "Refresh the GitHub token before using cached icon misses." {
		t.Fatalf("ReviewTitleFor = %q", got)
	}
}

func TestCodeRabbitDataIntegrityRubricIsNotATitle(t *testing.T) {
	body := `_🗄️ Data Integrity & Integration_ | _🟡 Minor_ | _⚡ Quick win_

**Keep an empty model list as an array.**`
	if got := ReviewTitleFor("coderabbitai[bot]", body); got != "Keep an empty model list as an array." {
		t.Fatalf("ReviewTitleFor = %q", got)
	}
}

func TestReviewLabelsKeepEachBotsNativeScale(t *testing.T) {
	codex := ReviewLabelsFor("chatgpt-codex-connector[bot]",
		`**<sub><sub>![P2 Badge](https://img.shields.io/badge/P2-yellow)</sub></sub> Preserve fallback ordering**`)
	if codex.Scale != "P2" || codex.Severity != "potential" || codex.Category != "" {
		t.Fatalf("codex labels = %+v", codex)
	}
	if got := SeverityFor("chatgpt-codex-connector[bot]",
		`![P2 Badge](https://img.shields.io/badge/P2-yellow) this mentions a major path`); got != "potential" {
		t.Fatalf("Codex SeverityFor = %q, want P2/potential", got)
	}

	bugbot := ReviewLabelsFor("cursor[bot]", "**High Severity**\n\nA nil map write.")
	if bugbot.Scale != "High" || bugbot.Severity != "major" {
		t.Fatalf("bugbot labels = %+v", bugbot)
	}
}

// Every bot wraps its summary differently, and the first bold span is the
// SEVERITY for two of them — a listing that says "High Severity" for every
// Bugbot finding has told the reader nothing.
func TestThreadTitleSkipsASeverityLabel(t *testing.T) {
	for _, tc := range []struct {
		name, author, body, want string
	}{
		{
			"bugbot heading then severity",
			"cursor[bot]",
			"### Nil map write in the claim path\n\n**High Severity**\n\nDetail follows.",
			"Nil map write in the claim path",
		},
		{
			"macroscope severity first",
			"macroscopeapp[bot]",
			"🟠 **High** internal/crq/service.go\n\n### Slot released twice\n\nDetail.",
			"Slot released twice",
		},
		{
			// Nothing left to say: no title beats one that repeats the severity
			// field. The listing still carries path, line and URL.
			"nothing but the severity",
			"cursor[bot]",
			"**High Severity**\n",
			"",
		},
		{
			"nothing but the severity and the path",
			"macroscopeapp[bot]",
			"🟠 **High** internal/crq/service.go\n",
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ThreadTitle(true, tc.body); got != tc.want {
				t.Errorf("ThreadTitle = %q, want %q", got, tc.want)
			}
		})
	}
}

// Against the real captured comments, which is what the corpus is for. Each of
// these formats produced a title that said nothing: "High Severity" for Bugbot,
// the severity-and-path line for Macroscope.
func TestThreadTitleAgainstTheCorpus(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{"bugbot/inline-finding-high.md", "Queue cap desyncs stores"},
		{"bugbot/inline-finding-medium.md", "Composer acks unrelated queue changes"},
		{"macroscope/inline-finding-high.md", "writeScreenshotFile writes directly to absolutePath after WorkspacePaths.resolveRelativePathWithinRoot validates it, ..."},
	} {
		t.Run(tc.file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatalf("read corpus fixture: %v", err)
			}
			got := ThreadTitle(true, string(body))
			if got != tc.want {
				t.Errorf("ThreadTitle = %q, want %q", got, tc.want)
			}
			if severityOnly.MatchString(got) {
				t.Errorf("ThreadTitle = %q — a severity label is not a summary", got)
			}
		})
	}
}

// Greptile leads with a badge, so its title is the bold span after it — the
// generic order would pick the first prose line instead. Legacy findings
// opened with a category label and had no title at all.
func TestGreptileFindingTitlesAndSeverity(t *testing.T) {
	for _, tc := range []struct{ file, title, severity, scale string }{
		{"greptile/inline-finding-p1.md", "Light stays off after turn-on", SeverityMajor, "P1"},
		{"greptile/inline-finding-p2.md", "Refresh lacks required diagnostic flag", SeverityPotential, "P2"},
		{"greptile/inline-finding-legacy-logic.md",
			"Critical architectural issue: calling move functions directly bypasses async_execute_controller_command(), which viol...",
			SeverityPotential, "logic"},
		// The <summary> wrapper and the bold path are not a description.
		{"greptile/additional-comments.md", "Missing required imports timedelta and dt_util for the brew_now service", "", ""},
	} {
		t.Run(tc.file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatalf("read corpus fixture: %v", err)
			}
			if got := ReviewTitleFor(GreptileLogin, string(body)); got != tc.title {
				t.Errorf("ReviewTitleFor = %q, want %q", got, tc.title)
			}
			labels := ReviewLabelsFor(GreptileLogin, string(body))
			if labels.Severity != tc.severity || labels.Scale != tc.scale {
				t.Errorf("ReviewLabelsFor = %+v, want severity %q scale %q", labels, tc.severity, tc.scale)
			}
		})
	}
}

// Titles carry code and issue references. A generic tag or heading strip eats
// them: `Map<string, User>` becomes `Map`, and `#123` becomes `123`.
func TestThreadTitleKeepsCodeAndReferences(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"**Handle Map<string, User> correctly**\n\ndetail", "Handle Map<string, User> correctly"},
		{"**Fix the leak from #123**\n\ndetail", "Fix the leak from #123"},
		{"#123 breaks parsing\n\ndetail", "#123 breaks parsing"},
	} {
		if got := ThreadTitle(true, tc.body); got != tc.want {
			t.Errorf("ThreadTitle = %q, want %q", got, tc.want)
		}
	}
}

// A person's first line is what they meant to say; a label they bolded later is
// not a better summary of it.
func TestThreadTitlePrefersAHumansFirstLine(t *testing.T) {
	body := "Could we simplify this?\n\n**Suggestion:** use a map.\n"
	if got := ThreadTitle(false, body); got != "Could we simplify this?" {
		t.Errorf("ThreadTitle = %q, want the question they asked", got)
	}
}
