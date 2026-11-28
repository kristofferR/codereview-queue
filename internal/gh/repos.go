package gh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RepoState is what a batched lookup learned about one repository.
type RepoState struct {
	// Name is the repository's current owner/name. GitHub follows a rename or a
	// transfer, so it differs from the name asked for when the repository moved.
	Name string
	// Missing means GitHub does not resolve the name for this credential: the
	// repository was deleted, or it is no longer visible. The two look the same.
	Missing  bool
	Archived bool
	// PRs are the open pull requests, most recently updated first, for a
	// repository that was listed with them.
	PRs []SearchPR
	// Next continues PRs past the first page through MoreOpenPRs; "" when the
	// list is complete.
	Next string
	// Err is a failure GitHub reported for this repository alone. The others in
	// the lookup are unaffected.
	Err error
}

// Lookups are batched: one query answers for many repositories, where the
// search API needs one request each from an allowance of thirty a minute.
const (
	// repoPRChunk bounds the repositories listed with pull requests per query,
	// since each can return a hundred bodies.
	repoPRChunk = 10
	// repoNameChunk bounds the repositories per query altogether.
	repoNameChunk = 50
)

const repoPRFields = ` pullRequests(states:OPEN,first:100,after:$c%d,orderBy:{field:UPDATED_AT,direction:DESC}){pageInfo{hasNextPage endCursor} nodes{number title body author{login}}}`

type repoLookup struct {
	repo   string
	prs    bool
	cursor string
}

type repoNode struct {
	NameWithOwner string `json:"nameWithOwner"`
	IsArchived    bool   `json:"isArchived"`
	PullRequests  *struct {
		PageInfo struct {
			HasNextPage bool   `json:"hasNextPage"`
			EndCursor   string `json:"endCursor"`
		} `json:"pageInfo"`
		Nodes []struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			Body   string `json:"body"`
			Author *struct {
				Login string `json:"login"`
			} `json:"author"`
		} `json:"nodes"`
	} `json:"pullRequests"`
}

// LookupRepos resolves repositories by name in as few requests as it can, and
// lists the open pull requests of the ones in withPRs. The result is keyed by
// the names as given.
//
// A repository that was renamed answers under its new name, and one that no
// longer exists answers Missing — neither fails the lookup, which is the point:
// searching by name fails outright for both, and one stale name then costs
// every other repository its scan. A query that fails for another reason marks
// its own repositories with the error and the rest are still asked; only a
// throttle or an unreachable gateway fails the call.
func (g *GitHub) LookupRepos(ctx context.Context, withPRs, namesOnly []string) (map[string]RepoState, error) {
	lookups := make([]repoLookup, 0, len(withPRs)+len(namesOnly))
	for _, repo := range withPRs {
		lookups = append(lookups, repoLookup{repo: repo, prs: true})
	}
	for _, repo := range namesOnly {
		lookups = append(lookups, repoLookup{repo: repo})
	}
	out := make(map[string]RepoState, len(lookups))
	for len(lookups) > 0 {
		n, listed := 0, 0
		for n < len(lookups) && n < repoNameChunk && (!lookups[n].prs || listed < repoPRChunk) {
			if lookups[n].prs {
				listed++
			}
			n++
		}
		if err := g.lookupRepoChunk(ctx, lookups[:n], out); err != nil {
			// A throttle or a lost gateway is everyone's: stop. Anything else is
			// this query's failure, and the answers already in hand — and the
			// queries still to come — are worth more than a clean error.
			if IsThrottled(err) || IsServerUnreachable(err) || ctx.Err() != nil {
				return nil, err
			}
			for _, lookup := range lookups[:n] {
				if _, answered := out[lookup.repo]; !answered {
					out[lookup.repo] = RepoState{Err: err}
				}
			}
		}
		lookups = lookups[n:]
	}
	return out, nil
}

// MoreOpenPRs returns the next page of a repository's open pull requests, and
// the cursor for the one after it.
func (g *GitHub) MoreOpenPRs(ctx context.Context, repo, cursor string) ([]SearchPR, string, error) {
	out := map[string]RepoState{}
	if err := g.lookupRepoChunk(ctx, []repoLookup{{repo: repo, prs: true, cursor: cursor}}, out); err != nil {
		return nil, "", err
	}
	state := out[repo]
	if state.Err != nil {
		return nil, "", state.Err
	}
	if state.Missing {
		return nil, "", ErrNotFound
	}
	return state.PRs, state.Next, nil
}

func (g *GitHub) lookupRepoChunk(ctx context.Context, lookups []repoLookup, out map[string]RepoState) error {
	// A name that is not owner/name is that entry's problem, like every other
	// way one repository can be wrong: it must not cost the rest their answer.
	wellFormed := lookups[:0:0]
	for _, lookup := range lookups {
		owner, name, ok := strings.Cut(lookup.repo, "/")
		if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			out[lookup.repo] = RepoState{Err: fmt.Errorf("repository %q is not owner/name", lookup.repo)}
			continue
		}
		wellFormed = append(wellFormed, lookup)
	}
	if lookups = wellFormed; len(lookups) == 0 {
		return nil
	}
	var declared, selected strings.Builder
	variables := map[string]any{}
	for i, lookup := range lookups {
		owner, name, _ := strings.Cut(lookup.repo, "/")
		fmt.Fprintf(&declared, "$o%d:String!,$n%d:String!,", i, i)
		variables[fmt.Sprintf("o%d", i)], variables[fmt.Sprintf("n%d", i)] = owner, name
		fields := ""
		if lookup.prs {
			fmt.Fprintf(&declared, "$c%d:String,", i)
			fields = fmt.Sprintf(repoPRFields, i)
			if lookup.cursor != "" {
				variables[fmt.Sprintf("c%d", i)] = lookup.cursor
			}
		}
		fmt.Fprintf(&selected, "r%d:repository(owner:$o%d,name:$n%d){nameWithOwner isArchived%s}\n", i, i, i, fields)
	}
	query := "query(" + strings.TrimSuffix(declared.String(), ",") + "){\n" + selected.String() + "}"
	body, err := marshalBody(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	resp, _, err := g.graphQLResponse(ctx, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Method: http.MethodPost, URL: g.graphBase, Status: resp.StatusCode, Body: string(b)}
	}
	// Decoded here rather than through GraphQL: that treats any error as the
	// whole query failing, and a repository that is gone arrives as an error
	// beside the data for every other one.
	var envelope struct {
		Data   map[string]*repoNode `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Path    []any  `json:"path"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return err
	}
	failed := map[string]RepoState{}
	for _, problem := range envelope.Errors {
		alias := ""
		if len(problem.Path) > 0 {
			alias, _ = problem.Path[0].(string)
		}
		var index int
		if _, err := fmt.Sscanf(alias, "r%d", &index); err != nil || index < 0 || index >= len(lookups) {
			// Not about one repository: the query itself failed.
			return errors.New(problem.Message)
		}
		if problem.Type == "NOT_FOUND" && len(problem.Path) == 1 {
			failed[alias] = RepoState{Missing: true}
		} else if _, seen := failed[alias]; !seen {
			failed[alias] = RepoState{Err: errors.New(problem.Message)}
		}
	}
	for i, lookup := range lookups {
		alias := fmt.Sprintf("r%d", i)
		if state, bad := failed[alias]; bad {
			out[lookup.repo] = state
			continue
		}
		node := envelope.Data[alias]
		if node == nil {
			out[lookup.repo] = RepoState{Err: fmt.Errorf("github returned nothing for %s", lookup.repo)}
			continue
		}
		state := RepoState{Name: node.NameWithOwner, Archived: node.IsArchived}
		if node.PullRequests != nil {
			for _, pr := range node.PullRequests.Nodes {
				author := ""
				if pr.Author != nil {
					author = pr.Author.Login
				}
				state.PRs = append(state.PRs, SearchPR{Repo: node.NameWithOwner, Number: pr.Number, Author: author, Title: pr.Title, Body: pr.Body})
			}
			if node.PullRequests.PageInfo.HasNextPage {
				state.Next = node.PullRequests.PageInfo.EndCursor
			}
		}
		out[lookup.repo] = state
	}
	return nil
}

// mergedChunk bounds the pull requests one MergedPRs query asks about.
const mergedChunk = 100

// MergedPRs reports which of a repository's pull requests have merged, a
// hundred to a query. A number GitHub does not know is absent from the answer.
func (g *GitHub) MergedPRs(ctx context.Context, repo string, prs []int) (map[int]bool, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" {
		return nil, fmt.Errorf("repository %q is not owner/name", repo)
	}
	out := make(map[int]bool, len(prs))
	for len(prs) > 0 {
		chunk := prs[:min(len(prs), mergedChunk)]
		prs = prs[len(chunk):]
		var selected strings.Builder
		for _, pr := range chunk {
			fmt.Fprintf(&selected, "p%d:pullRequest(number:%d){merged}\n", pr, pr)
		}
		body, err := marshalBody(map[string]any{
			"query":     "query($o:String!,$n:String!){repository(owner:$o,name:$n){\n" + selected.String() + "}}",
			"variables": map[string]any{"o": owner, "n": name},
		})
		if err != nil {
			return nil, err
		}
		resp, _, err := g.graphQLResponse(ctx, body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			return nil, &APIError{Method: http.MethodPost, URL: g.graphBase, Status: resp.StatusCode, Body: string(b)}
		}
		// A pull request number GitHub does not know arrives as an error beside
		// the answers for the rest, so the envelope is read here.
		var envelope struct {
			Data struct {
				Repository map[string]*struct {
					Merged bool `json:"merged"`
				} `json:"repository"`
			} `json:"data"`
			Errors []struct {
				Type    string `json:"type"`
				Path    []any  `json:"path"`
				Message string `json:"message"`
			} `json:"errors"`
		}
		err = json.NewDecoder(resp.Body).Decode(&envelope)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		// Not found is an answer: the repository is gone, or that number was
		// never a pull request. Anything else means some of this was not
		// answered at all, and reading the gaps as "not merged" would be a guess.
		for _, problem := range envelope.Errors {
			if problem.Type != "NOT_FOUND" {
				return nil, errors.New(problem.Message)
			}
		}
		if envelope.Data.Repository == nil {
			return nil, ErrNotFound
		}
		for _, pr := range chunk {
			if node := envelope.Data.Repository[fmt.Sprintf("p%d", pr)]; node != nil {
				out[pr] = node.Merged
			}
		}
	}
	return out, nil
}
