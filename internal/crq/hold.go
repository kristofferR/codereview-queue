package crq

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
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

func holdComment(repo string, pr int, reason string) string {
	return fmt.Sprintf("<!-- crq:hold -->\n⏸️ crq will not request further automated reviews for this pull request.\n\n**Reason:** %s\n\nResume with `crq unhold %s %d`.", reason, repo, pr)
}

func unholdComment() string {
	return "<!-- crq:unhold -->\n▶️ The administrative hold has been released. crq may request automated reviews again."
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
