package crq

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kristofferR/codereview-queue/internal/dialect"
)

// HoldResult reports a PR's hold state after a change.
type HoldResult struct {
	Repo       string `json:"repo"`
	PR         int    `json:"pr"`
	Held       bool   `json:"held"`
	Reason     string `json:"reason,omitempty"`
	By         string `json:"by,omitempty"`
	CommentURL string `json:"comment_url,omitempty"`
	Warning    string `json:"warning,omitempty"`
	// At is a pointer because time.Time is a struct: omitempty never omits one,
	// so an unhold response used to carry "at":"0001-01-01T00:00:00Z".
	At *time.Time `json:"at,omitempty"`
}

// Holds lists every held PR.
func (s *Service) Holds(ctx context.Context) ([]HoldResult, error) {
	st, _, err := s.store.Load(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]HoldResult, 0, len(st.Holds))
	for key, h := range st.Holds {
		repo, pr, ok := parseHoldKey(key)
		if !ok {
			continue
		}
		at := h.At
		out = append(out, HoldResult{Repo: repo, PR: pr, Held: true, Reason: h.Reason, By: h.By, At: &at})
	}
	sortHolds(out)
	return out, nil
}

func parseHoldKey(key string) (repo string, pr int, ok bool) {
	repo, number, ok := strings.Cut(key, "#")
	if !ok || repo == "" {
		return "", 0, false
	}
	pr, err := strconv.Atoi(number)
	if err != nil || pr <= 0 {
		return "", 0, false
	}
	return repo, pr, true
}

func holdComment(repo string, pr int, reason string, cfg Config) string {
	return fmt.Sprintf("<!-- crq:hold -->\n⏸️ crq will not request further automated reviews for this pull request.\n\n**Reason:** %s\n\nResume with `crq unhold %s %d`.", neutralizeReviewCommands(reason, cfg), repo, pr)
}

func unholdComment() string {
	return "<!-- crq:unhold -->\n▶️ The administrative hold has been released. crq may request automated reviews again."
}

func mergedHoldComment() string {
	return "<!-- crq:hold-merged -->\n✅ The administrative hold has been retired because this pull request merged. crq will not request further automated reviews."
}

func neutralizeMentions(text string) string {
	return strings.ReplaceAll(text, "@", "@\u200b")
}

// neutralizeReviewCommands prevents a hold reason from triggering any configured
// or registered review command when rendered in a pull request comment.
func neutralizeReviewCommands(text string, cfg Config) string {
	commands := []string{cfg.ReviewCommand}
	for _, co := range cfg.CoBots {
		commands = append(commands, co.Command)
	}
	for _, co := range cfg.KnownCoBots {
		commands = append(commands, co.Command)
	}
	for _, co := range dialect.KnownCoReviewers() {
		commands = append(commands, co.Command)
		commands = append(commands, co.TriggerAliases...)
	}
	sort.Slice(commands, func(i, j int) bool {
		if len(commands[i]) != len(commands[j]) {
			return len(commands[i]) > len(commands[j])
		}
		return commands[i] < commands[j]
	})
	seen := make(map[string]bool, len(commands))
	for _, command := range commands {
		command = strings.TrimSpace(command)
		if command == "" || seen[command] {
			continue
		}
		seen[command] = true
		_, size := utf8.DecodeRuneInString(command)
		text = strings.ReplaceAll(text, command, command[:size]+"\u200b"+command[size:])
	}
	return neutralizeMentions(text)
}

// sortHolds orders by repo then PR, so a listing is stable rather than however
// the map happened to iterate.
func sortHolds(holds []HoldResult) {
	sort.Slice(holds, func(i, j int) bool {
		if holds[i].Repo != holds[j].Repo {
			return holds[i].Repo < holds[j].Repo
		}
		return holds[i].PR < holds[j].PR
	})
}
