package gh

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// repoGraph answers the batched repository query the way GitHub was seen to:
// a renamed repository under its new name, a missing one as null beside a
// NOT_FOUND error naming its alias.
type repoGraph struct {
	mu      sync.Mutex
	queries [][]string // the repository names asked for, per query
}

func (g *repoGraph) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	data := map[string]any{}
	var problems []map[string]any
	var asked []string
	for i := 0; ; i++ {
		name, ok := in.Variables[fmt.Sprintf("n%d", i)].(string)
		if !ok {
			break
		}
		asked = append(asked, name)
		alias := fmt.Sprintf("r%d", i)
		node := map[string]any{"nameWithOwner": "o/" + name, "isArchived": name == "archived"}
		if strings.Contains(in.Query, fmt.Sprintf("$c%d:", i)) {
			page := map[string]any{
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
				"nodes":    []map[string]any{{"number": 1, "title": "first", "body": "b", "author": map[string]string{"login": "dependabot"}}},
			}
			if name == "many" {
				if in.Variables[fmt.Sprintf("c%d", i)] == "CURSOR" {
					page["nodes"] = []map[string]any{{"number": 2, "title": "second", "author": nil}}
				} else {
					page["pageInfo"] = map[string]any{"hasNextPage": true, "endCursor": "CURSOR"}
				}
			}
			node["pullRequests"] = page
		}
		switch name {
		case "renamed":
			node["nameWithOwner"] = "o/Current"
		case "gone":
			data[alias] = nil
			problems = append(problems, map[string]any{"type": "NOT_FOUND", "path": []string{alias}, "message": "Could not resolve to a Repository with the name 'o/gone'."})
			continue
		case "forbidden":
			data[alias] = nil
			problems = append(problems, map[string]any{"type": "FORBIDDEN", "path": []string{alias}, "message": "Resource protected by organization SAML enforcement."})
			continue
		case "broken":
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]any{{"message": "Something went wrong while executing your query."}}})
			return
		}
		data[alias] = node
	}
	g.mu.Lock()
	g.queries = append(g.queries, asked)
	g.mu.Unlock()
	out := map[string]any{"data": data}
	if len(problems) > 0 {
		out["errors"] = problems
	}
	_ = json.NewEncoder(w).Encode(out)
}

// One renamed or deleted repository used to fail the search for it, and with it
// the pass. The lookup has to answer for each repository on its own.
func TestLookupReposAnswersForEachRepositoryOnItsOwn(t *testing.T) {
	graph := &repoGraph{}
	srv := httptest.NewServer(graph)
	defer srv.Close()
	g := NewTestClient(srv.URL, srv.Client())

	got, err := g.LookupRepos(t.Context(),
		[]string{"o/app", "o/renamed", "o/gone", "o/many", "o/archived"},
		[]string{"o/forbidden", "o/quiet"})
	if err != nil {
		t.Fatal(err)
	}
	if app := got["o/app"]; app.Name != "o/app" || len(app.PRs) != 1 || app.PRs[0].Repo != "o/app" || app.PRs[0].Author != "dependabot" || app.Next != "" {
		t.Errorf("o/app = %+v", app)
	}
	if renamed := got["o/renamed"]; renamed.Name != "o/Current" || len(renamed.PRs) != 1 || renamed.PRs[0].Repo != "o/Current" {
		t.Errorf("a renamed repository must answer under its current name: %+v", renamed)
	}
	if gone := got["o/gone"]; !gone.Missing || gone.Err != nil {
		t.Errorf("a repository that does not resolve = %+v, want Missing", gone)
	}
	if forbidden := got["o/forbidden"]; forbidden.Err == nil || forbidden.Missing {
		t.Errorf("a repository GitHub refuses = %+v, want its own error and not Missing", forbidden)
	}
	if !got["o/archived"].Archived {
		t.Error("archived flag lost")
	}
	if quiet := got["o/quiet"]; quiet.Name != "o/quiet" || quiet.PRs != nil {
		t.Errorf("a name-only lookup = %+v, want the name and no pull requests", quiet)
	}

	many := got["o/many"]
	if many.Next != "CURSOR" || len(many.PRs) != 1 {
		t.Fatalf("o/many = %+v, want a first page and a cursor", many)
	}
	more, next, err := g.MoreOpenPRs(t.Context(), many.Name, many.Next)
	if err != nil || next != "" || len(more) != 1 || more[0].Number != 2 || more[0].Author != "" {
		t.Fatalf("second page = %+v, next = %q, err = %v", more, next, err)
	}
}

func TestLookupReposBatchesAndFailsOnlyWhenTheQueryDoes(t *testing.T) {
	graph := &repoGraph{}
	srv := httptest.NewServer(graph)
	defer srv.Close()
	g := NewTestClient(srv.URL, srv.Client())

	var withPRs, names []string
	for i := range repoPRChunk + 2 {
		withPRs = append(withPRs, fmt.Sprintf("o/listed%d", i))
	}
	for i := range 3 {
		names = append(names, fmt.Sprintf("o/named%d", i))
	}
	got, err := g.LookupRepos(t.Context(), withPRs, names)
	if err != nil || len(got) != len(withPRs)+len(names) {
		t.Fatalf("answers = %d, err = %v", len(got), err)
	}
	// Ten repositories with their pull requests fill a query; the names ride
	// along with the last one instead of costing a request of their own.
	if len(graph.queries) != 2 || len(graph.queries[0]) != repoPRChunk || len(graph.queries[1]) != 5 {
		t.Fatalf("queries = %v, want one of %d and one of 5", graph.queries, repoPRChunk)
	}

	// A query that fails outright marks its own repositories and no others.
	var first []string
	for i := range repoPRChunk {
		first = append(first, fmt.Sprintf("o/ok%d", i))
	}
	split, err := g.LookupRepos(t.Context(), append(first, "o/broken", "o/after"), nil)
	if err != nil {
		t.Fatalf("a failed query failed the whole lookup: %v", err)
	}
	if split["o/ok0"].Name != "o/ok0" || split["o/broken"].Err == nil || split["o/after"].Err == nil {
		t.Fatalf("split lookup = ok0 %+v, broken %+v, after %+v; want the first query's answers kept and the second's marked",
			split["o/ok0"], split["o/broken"], split["o/after"])
	}
	// A malformed name is its own entry's problem, and is never sent.
	before := len(graph.queries)
	mixed, err := g.LookupRepos(t.Context(), []string{"not-a-repository", "o/app"}, nil)
	if err != nil || mixed["not-a-repository"].Err == nil || mixed["o/app"].Name != "o/app" {
		t.Fatalf("mixed lookup = %+v, err = %v", mixed, err)
	}
	if last := graph.queries[len(graph.queries)-1]; len(graph.queries) != before+1 || len(last) != 1 {
		t.Fatalf("queries = %v, want one more, for the well-formed name alone", graph.queries[before:])
	}
}

func TestMergedPRsReadsEachAnswerAndToleratesUnknownNumbers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Variables["n"] == "gone" {
			_, _ = w.Write([]byte(`{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"message":"Could not resolve to a Repository"}]}`))
			return
		}
		if in.Variables["n"] == "partial" {
			_, _ = w.Write([]byte(`{"data":{"repository":{"p1":null}},"errors":[{"type":"SERVICE_UNAVAILABLE","path":["repository","p1"],"message":"timed out"}]}`))
			return
		}
		if !strings.Contains(in.Query, "p1:pullRequest(number:1){merged}") {
			t.Errorf("query = %s", in.Query)
		}
		_, _ = w.Write([]byte(`{"data":{"repository":{"p1":{"merged":true},"p2":{"merged":false},"p3":null}},
			"errors":[{"type":"NOT_FOUND","path":["repository","p3"],"message":"Could not resolve to a PullRequest with the number of 3."}]}`))
	}))
	defer srv.Close()
	g := NewTestClient(srv.URL, srv.Client())

	got, err := g.MergedPRs(t.Context(), "o/app", []int{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if merged, known := got[1]; !merged || !known {
		t.Errorf("#1 = %v, %v, want merged", merged, known)
	}
	if merged, known := got[2]; merged || !known {
		t.Errorf("#2 = %v, %v, want known and unmerged", merged, known)
	}
	if _, known := got[3]; known {
		t.Error("a number GitHub does not know was reported on")
	}
	if _, err := g.MergedPRs(t.Context(), "o/gone", []int{1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for a repository that is gone", err)
	}
	// An answer that is partly missing for any other reason is not an answer:
	// reading the gap as "not merged" would be a guess.
	if _, err := g.MergedPRs(t.Context(), "o/partial", []int{1}); err == nil {
		t.Fatal("a field error other than not-found was read as an answer")
	}
}
