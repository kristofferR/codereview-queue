package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// CheckRun is one check run on a commit, reduced to what crq consumes. The
// bot-agnostic transport fetches ALL runs for a ref; matching them to a
// co-reviewer (by app slug / name) is dialect's job — server-side filtering
// cannot express Macroscope's repo-custom check names.
type CheckRun struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	HeadSHA     string    `json:"head_sha"`
	Status      string    `json:"status"`     // queued | in_progress | completed
	Conclusion  string    `json:"conclusion"` // success | neutral | failure | skipped | … ("" while running)
	HTMLURL     string    `json:"html_url"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"` // zero while running
	Output      struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
	App struct {
		Slug string `json:"slug"`
	} `json:"app"`
}

// ListCheckRuns fetches every check run for a commit ref. The endpoint wraps
// its pages in a {total_count, check_runs} envelope, which requestPaged's
// bare-slice decoding cannot express — hence this envelope-aware page loop
// over the same send/nextPage plumbing (so it rides the ETag cache and
// throttle handling for free; repeat polls of an unchanged ref are 304s).
func (g *GitHub) ListCheckRuns(ctx context.Context, repo, ref string) ([]CheckRun, error) {
	next := g.apiBase + fmt.Sprintf("/repos/%s/commits/%s/check-runs?per_page=100", repoPath(repo), url.PathEscape(ref))
	var out []CheckRun
	for next != "" {
		resp, err := g.send(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotFound {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return nil, ErrNotFound
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return nil, &APIError{Method: http.MethodGet, URL: next, Status: resp.StatusCode, Body: string(b)}
		}
		var envelope struct {
			CheckRuns []CheckRun `json:"check_runs"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&envelope)
		link := resp.Header.Get("Link")
		_ = resp.Body.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}
		out = append(out, envelope.CheckRuns...)
		next = nextPage(link)
	}
	return out, nil
}

// CommitStatus is one legacy commit status (the Statuses API, used by
// deployment previews and by bots that predate check runs).
type CommitStatus struct {
	Context   string `json:"context"`
	State     string `json:"state"` // error | failure | pending | success
	TargetURL string `json:"target_url"`
}

// ListCommitStatuses returns the latest status per context for a commit — the
// combined-status view, so a context that went pending → success appears once.
func (g *GitHub) ListCommitStatuses(ctx context.Context, repo, ref string) ([]CommitStatus, error) {
	var out struct {
		Statuses []CommitStatus `json:"statuses"`
	}
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/commits/%s/status?per_page=100", repoPath(repo), url.PathEscape(ref)), nil, &out)
	return out.Statuses, err
}

// RequiredChecks names the status checks a branch requires before merging,
// from classic branch protection and repository rulesets alike. A source the
// token cannot read (404 when unprotected, 403 for rulesets on a plan without
// them) contributes nothing rather than failing the lookup.
func (g *GitHub) RequiredChecks(ctx context.Context, repo, branch string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	var protected struct {
		Protection struct {
			RequiredStatusChecks struct {
				Contexts []string `json:"contexts"`
				Checks   []struct {
					Context string `json:"context"`
				} `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	err := g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/branches/%s", repoPath(repo), refPath(branch)), nil, &protected)
	if err != nil && !unreadable(err) {
		return nil, err
	}
	for _, name := range protected.Protection.RequiredStatusChecks.Contexts {
		add(name)
	}
	for _, check := range protected.Protection.RequiredStatusChecks.Checks {
		add(check.Context)
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	err = g.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/rules/branches/%s", repoPath(repo), refPath(branch)), nil, &rules)
	if err != nil && !unreadable(err) {
		return nil, err
	}
	for _, rule := range rules {
		if rule.Type != "required_status_checks" {
			continue
		}
		for _, check := range rule.Parameters.RequiredStatusChecks {
			add(check.Context)
		}
	}
	return out, nil
}

// unreadable reports a resource the token is not allowed to see, as opposed to
// a transport failure worth retrying.
func unreadable(err error) bool {
	if errors.Is(err, ErrNotFound) {
		return true
	}
	var api *APIError
	return errors.As(err, &api) && api.Status == http.StatusForbidden
}
