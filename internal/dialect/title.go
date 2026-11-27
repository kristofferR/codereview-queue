package dialect

import (
	"regexp"
	"strings"
	"unicode"
)

// markup matches the wrappers a bot puts AROUND its title: badge images, the
// specific HTML tags the bots use, and emphasis runs that delimit a span.
//
// Both halves are deliberately narrow. A generic <[^>]+> would eat `Map<string,
// User>` from a title, and stripping every # would turn "#123" into "123", so
// only known tags and boundary emphasis go. Heading markers are handled by
// headingOf, which is where they mean something.
//
// A boundary run is one delimiter KIND, never a mixture: in "`__init__`" the
// run is the backtick alone, so the underscores stay part of the identifier.
// Merging them would report the finding as being about "init".
var markup = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)` +
	`|</?(?i:sub|sup|details|summary|b|i|em|strong|br|img|div|span|p|a|code)(?:\s[^>]*)?/?>` +
	"|(^|\\s)(?:_+|[*`]+)|(?:_+|[*`]+)(\\s|$)")

// rubric matches CodeRabbit's fixed severity header — "🎯 Functional
// Correctness | 🟡 Minor | ⚡ Quick win" — which is the same on every finding
// and displaces the part that differs. It matches the SHAPE rather than any
// pipe, because a title may contain one: "Support A | B configuration" must not
// become "B configuration".
const codeRabbitCategoryPattern = `Correctness|Maintainability|Security|Performance|Reliability|Quality|Data Integrity`

var (
	rubric             = regexp.MustCompile(`^[^|]*\b(` + codeRabbitCategoryPattern + `)\b[^|]*\|[^|]*\|[^|]*\|?\s*`)
	codeRabbitCategory = regexp.MustCompile(`(?i)\b(` + codeRabbitCategoryPattern + `)\b`)
)

// severityWord is the vocabulary Bugbot and Macroscope lead with.
const severityWord = `potential issue|critical|high|medium|low|major|minor|blocker|trivial|info`

// severityOnly matches a title that is nothing but a severity label — "High
// Severity", "Critical" — which carries nothing a severity field does not.
var severityOnly = regexp.MustCompile(`(?i)^\W*(` + severityWord + `)(\s+severity)?\W*$`)

// severityPrefix matches a leading severity label, which Macroscope puts in
// front of the path on its first line.
var severityPrefix = regexp.MustCompile(`(?i)^\W*(` + severityWord + `)(\s+severity)?\b[\s:—-]*`)

var (
	codexPriority   = regexp.MustCompile(`(?i)(?:P([0-3])\s+Badge|badge/P([0-3])(?:-|$))`)
	bugbotScale     = regexp.MustCompile(`(?im)^\s*\*\*(Critical|High|Medium|Low)\s+Severity\*\*`)
	detailsBlock    = regexp.MustCompile(`(?is)<details(?:\s[^>]*)?>.*?</details>`)
	macroscopeScale = regexp.MustCompile(
		`(?im)^\s*\W*\*\*(Critical|High|Medium|Low)\*\*`,
	)
	// greptilePriority reads the badge Greptile leads a finding with:
	// <img alt="P1" src=".../badges/p1.svg">.
	greptilePriority = regexp.MustCompile(`(?i)<img\b[^>]*\balt="P([0-3])"`)
	// greptileLegacyLabel is the category earlier Greptile findings opened with
	// instead of a badge: "**logic:** …", "**style:** …", "**syntax:** …".
	greptileLegacyLabel = regexp.MustCompile(`(?i)^\s*\*\*(logic|style|syntax):\*\*\s*`)
	// greptilePrompt is the collapsed "Prompt To Fix With AI" block, which
	// repeats the finding and must not supply its title.
	greptilePrompt = regexp.MustCompile(`(?is)<details>\s*<summary>\s*Prompt To Fix With AI\s*</summary>.*?</details>`)
	// greptileWrapper is a <details>/<summary> line around out-of-diff
	// findings ("Additional Comments (1)"), which describes none of them.
	greptileWrapper = regexp.MustCompile(`(?i)^\s*</?(?:details|summary)\b`)
)

// ReviewLabels are the independent labels in a reviewer's rubric header.
// Requiredness and actionability live elsewhere; these are presentation facts
// the reviewer explicitly wrote, such as "Functional Correctness | Minor |
// Quick win".
type ReviewLabels struct {
	Category string
	Severity string
	Scale    string
	Effort   string
}

// ReviewLabelsFor reads the labels in one reviewer's own dialect. Do not merge
// these formats: CodeRabbit's "Minor" and Codex's "P2" mean similar urgency but
// are different scales the UI should preserve.
func ReviewLabelsFor(bot, body string) ReviewLabels {
	switch NormalizeBotName(bot) {
	case "coderabbitai":
		return codeRabbitLabels(body)
	case "chatgpt-codex-connector":
		return codexLabels(body)
	case "cursor":
		return singleScaleLabels(body, bugbotScale)
	case "macroscopeapp":
		return singleScaleLabels(body, macroscopeScale)
	case "greptile-apps":
		return greptileLabels(body)
	default:
		return ReviewLabels{}
	}
}

// codeRabbitLabels reads the three-part rubric without letting words deep in
// the explanation override it. CodeRabbit routinely writes "Minor" in the
// header and later mentions a "major" code path; scanning the whole body made
// every such finding render as Major.
func codeRabbitLabels(body string) ReviewLabels {
	for _, line := range strings.Split(body, "\n") {
		parts := strings.Split(line, "|")
		if len(parts) != 3 {
			continue
		}
		category := labelText(parts[0])
		scale := labelText(parts[1])
		effort := labelText(parts[2])
		severity := severityFromLabel(scale)
		if !isCodeRabbitCategory(category) || severity == "" {
			continue
		}
		return ReviewLabels{Category: category, Severity: severity, Scale: scale, Effort: effort}
	}
	return ReviewLabels{}
}

func isCodeRabbitCategory(category string) bool {
	return codeRabbitCategory.MatchString(category)
}

func codexLabels(body string) ReviewLabels {
	match := codexPriority.FindStringSubmatch(body)
	if match == nil {
		return ReviewLabels{}
	}
	priority := match[1]
	if priority == "" {
		priority = match[2]
	}
	severity := map[string]string{"0": "critical", "1": "major", "2": "potential", "3": "minor"}[priority]
	return ReviewLabels{Severity: severity, Scale: "P" + priority}
}

// greptileLabels maps Greptile's documented badges onto the shared scale:
// P0 "must fix before merging", P1 "should fix", P2 "consider fixing". The
// legacy categories carry no urgency of their own; style is the only one that
// was ever cosmetic.
func greptileLabels(body string) ReviewLabels {
	if match := greptilePriority.FindStringSubmatch(body); match != nil {
		severity := map[string]string{
			"0": SeverityCritical, "1": SeverityMajor, "2": SeverityPotential, "3": SeverityMinor,
		}[match[1]]
		return ReviewLabels{Severity: severity, Scale: "P" + match[1]}
	}
	if match := greptileLegacyLabel.FindStringSubmatch(body); match != nil {
		category := strings.ToLower(match[1])
		severity := SeverityPotential
		if category == "style" {
			severity = SeverityMinor
		}
		return ReviewLabels{Category: category, Severity: severity, Scale: category}
	}
	return ReviewLabels{}
}

func singleScaleLabels(body string, pattern *regexp.Regexp) ReviewLabels {
	match := pattern.FindStringSubmatch(body)
	if match == nil {
		return ReviewLabels{}
	}
	scale := match[1]
	return ReviewLabels{Severity: severityFromLabel(scale), Scale: scale}
}

func labelText(label string) string {
	label = strings.Trim(strings.TrimSpace(label), "_*` ")
	runes := []rune(label)
	for len(runes) > 0 && !unicode.IsLetter(runes[0]) && !unicode.IsNumber(runes[0]) {
		runes = runes[1:]
	}
	return strings.TrimSpace(string(runes))
}

func severityFromLabel(label string) string {
	lower := strings.ToLower(label)
	switch {
	case strings.Contains(lower, "critical"), strings.Contains(lower, "blocker"):
		return "critical"
	case strings.Contains(lower, "major"), strings.Contains(lower, "high"):
		return "major"
	case strings.Contains(lower, "potential"), strings.Contains(lower, "medium"):
		return "potential"
	case strings.Contains(lower, "minor"), strings.Contains(lower, "low"), strings.Contains(lower, "trivial"):
		return "minor"
	default:
		return ""
	}
}

// ReviewTitleFor reads a title in one reviewer's dialect. In particular,
// CodeRabbit may put an Analysis chain before the actual finding; shell
// comments and bold labels inside that collapsed block are implementation
// detail, never title candidates.
func ReviewTitleFor(bot, body string) string {
	switch NormalizeBotName(bot) {
	case "coderabbitai":
		return ThreadTitle(true, detailsBlock.ReplaceAllString(body, ""))
	case "chatgpt-codex-connector", "cursor", "macroscopeapp":
		return ThreadTitle(true, body)
	case "greptile-apps":
		if title := greptileTitle(body); title != "" {
			return title
		}
		return ThreadTitle(true, body)
	default:
		return ThreadTitle(true, body)
	}
}

// greptileTitle is the first line of a Greptile finding that says something.
// The title is the bold span after the badge on the first line; a legacy
// finding has no title, so its first sentence (minus the category label) is
// the best one. The generic order would prefer a later bold line such as
// "**Context Used:**" over that sentence.
func greptileTitle(body string) string {
	for _, line := range strings.Split(greptilePrompt.ReplaceAllString(body, ""), "\n") {
		if greptileWrapper.MatchString(line) {
			continue
		}
		if title := usableTitle(greptileLegacyLabel.ReplaceAllString(line, "")); title != "" {
			return title
		}
	}
	return ""
}

// ThreadTitle reduces a review comment to one readable line, per bot.
//
// Every bot wraps its summary differently: CodeRabbit prefixes a fixed severity
// rubric, Codex leads with a badge image, Bugbot opens with a `###` heading
// followed by a bold severity, and Macroscope starts with an emoji severity and
// the path. Taking the first bold span works for two of them and reports "High
// Severity" for the others. It returns "" when nothing in the comment describes
// the finding, since a non-descriptive title is worse than none.
//
// It lives here because this is where bot wording lives; a listing in the
// orchestration layer must not have to know how a bot writes a heading. Whether
// the author IS a reviewer is the caller's question, not this package's: it
// depends on the configured primary and on matching logins rather than config
// names, and a person whose handle happens to be "codex" is not the bot.
func ThreadTitle(bot bool, body string) string {
	var candidates []string
	if bot {
		// A heading first, for the bots that lead with a severity label and put
		// the summary in one; then the bold span, then the first prose line,
		// which is where Macroscope's summary lives.
		candidates = append(candidates, headingOf(body), TitleFromDetailedBlock(body), firstProse(body))
	} else {
		// A person's first line is what they meant to say. Preferring a bold
		// span picks up a label they wrote later — "Suggestion:" instead of the
		// question they asked.
		candidates = append(candidates, firstProse(body), TitleOf(body))
	}

	for _, candidate := range candidates {
		if title := usableTitle(candidate); title != "" {
			return title
		}
	}
	// Everything reduced to a severity label or a path. Empty is the honest
	// answer: the listing omits the title and still shows path, line and URL,
	// which is strictly more than "High Severity" would have said.
	return ""
}

// usableTitle is a cleaned candidate that actually says something: not a bare
// severity label, and not just the file it points at.
func usableTitle(candidate string) string {
	title := cleanTitle(candidate)
	if title == "" || severityOnly.MatchString(title) {
		return ""
	}
	// A line that is only the rubric describes every finding equally, so it
	// describes none of them.
	if rubric.MatchString(candidate) && strings.TrimSpace(rubric.ReplaceAllString(cleanTitle(candidate), "")) == "" {
		return ""
	}
	if rest := strings.TrimSpace(severityPrefix.ReplaceAllString(title, "")); rest == "" || pathLike(rest) {
		return ""
	}
	return title
}

// pathLike reports whether text is a bare file reference, which is a location
// rather than a description of what is wrong.
func pathLike(text string) bool {
	if strings.ContainsAny(text, " \t") {
		return false
	}
	return strings.ContainsAny(text, "/.:")
}

// firstProse returns the first line that reads like a sentence rather than a
// severity label or a path. Macroscope's summary is its third line.
func firstProse(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		_, heading := headingText(line)
		if line == "" || strings.HasPrefix(line, "<") || heading {
			continue
		}
		if usableTitle(line) != "" {
			return line
		}
	}
	return ""
}

// headingOf returns the first Markdown heading's text.
func headingOf(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if heading, ok := headingText(line); ok {
			return heading
		}
	}
	return ""
}

// headingText recognizes an ATX heading marker only when its hashes are
// followed by whitespace or the end of the line. An issue reference such as
// "#123 breaks parsing" is prose and must keep its hash.
func headingText(line string) (string, bool) {
	line = strings.TrimSpace(line)
	hashes := 0
	for hashes < len(line) && hashes < 6 && line[hashes] == '#' {
		hashes++
	}
	if hashes == 0 || (hashes < len(line) && line[hashes] == '#') {
		return "", false
	}
	if hashes == len(line) {
		return "", true
	}
	if line[hashes] != ' ' && line[hashes] != '\t' {
		return "", false
	}
	return strings.TrimSpace(line[hashes:]), true
}

func cleanTitle(title string) string {
	// TitleOf caps at 180 BYTES, which can leave a half-written rune; JSON then
	// renders it as a replacement character.
	title = strings.ToValidUTF8(title, "")
	title = strings.Join(strings.Fields(markup.ReplaceAllString(title, " ")), " ")
	// A title is already rendered separately from its Markdown body. Backticks
	// that survive beside punctuation show up literally (and an unmatched one
	// can consume the rest of the line in a Markdown renderer), so retain the
	// identifier but not its source-format delimiter.
	title = strings.ReplaceAll(title, "`", "")
	if trimmed := strings.TrimSpace(rubric.ReplaceAllString(title, "")); trimmed != "" {
		title = trimmed
	}
	// By runes: cutting bytes can split a multi-byte character, and the invalid
	// UTF-8 that leaves becomes a replacement character in the JSON.
	if runes := []rune(title); len(runes) > 120 {
		title = string(runes[:117]) + "..."
	}
	return strings.TrimSpace(title)
}
