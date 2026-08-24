package crq

import (
	"context"
	"net/url"
	"testing"

	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

// Inference must ask for THIS branch's pull request, not take whatever the
// repository happens to return first. A fake that ignored the head filter would
// have hidden the difference, so this pins the query itself.
func TestInferTargetAsksForTheBranchesPull(t *testing.T) {
	gh := newFakeGitHub()
	for _, tc := range []struct {
		pr     int
		branch string
	}{{11, "other-work"}, {12, "the-branch"}, {13, "unrelated"}} {
		var p ghapi.Pull
		p.State = "open"
		p.Number = tc.pr
		p.Head.SHA = "aaaaaaaaa"
		p.Head.Ref = tc.branch
		gh.pulls[fakeKey("owner/repo", tc.pr)] = p
	}

	query := url.Values{}
	query.Set("head", "owner:the-branch")
	got, err := gh.ListPulls(context.Background(), "owner/repo", query)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Number != 12 {
		t.Fatalf("head filter returned %+v, want only PR 12 — an unfiltered answer would let a wrong-branch lookup pass", got)
	}

	// No filter still returns everything, deterministically ordered.
	all, err := gh.ListPulls(context.Background(), "owner/repo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Number != 11 || all[2].Number != 13 {
		t.Fatalf("unfiltered = %+v, want 11,12,13 in order", all)
	}
}

// A fork checkout has the branch in one repository and the pull request in
// another: origin is me/app, upstream is owner/app, and the PR is filed against
// upstream with head me:branch. Taking the first remote and its own owner asks
// /repos/me/app/pulls?head=me:branch and reports that no PR exists.
func TestRemoteSlugsCoversForkCheckouts(t *testing.T) {
	repos, owners := remoteSlugs(`origin	git@github.com:me/app.git (fetch)
origin	git@github.com:me/app.git (push)
upstream	https://github.com/owner/app.git (fetch)
upstream	https://github.com/owner/app.git (push)`)

	if len(repos) != 2 || repos[0] != "me/app" || repos[1] != "owner/app" {
		t.Fatalf("repos = %v, want both, origin first", repos)
	}
	if len(owners) != 2 || owners[0] != "me" || owners[1] != "owner" {
		t.Fatalf("owners = %v, want both head owners, origin first", owners)
	}

	// The ordinary single-remote checkout still yields exactly one of each, so
	// inference there still costs exactly one request.
	repos, owners = remoteSlugs("origin\tgit@github.com:owner/app.git (fetch)\norigin\tgit@github.com:owner/app.git (push)")
	if len(repos) != 1 || len(owners) != 1 {
		t.Fatalf("one remote gave repos=%v owners=%v, want one of each", repos, owners)
	}

	// A slug here becomes an API lookup, so a remote that is not GitHub at all
	// must not produce one — repoSlugFromRemote alone would return "code/app".
	if repos, _ := remoteSlugs("origin\t/home/me/code/app (fetch)"); len(repos) != 0 {
		t.Errorf("a non-github remote must not become a repository: %v", repos)
	}

	// An SSH host alias (git@github.com-work:owner/app.git) is how a second
	// account is configured, and is still GitHub.
	if repos, _ := remoteSlugs("origin\tgit@github.com-work:owner/app.git (fetch)"); len(repos) != 1 || repos[0] != "owner/app" {
		t.Errorf("an ssh host alias must still resolve, got %v", repos)
	}
}
