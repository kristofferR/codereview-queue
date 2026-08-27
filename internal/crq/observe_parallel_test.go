package crq

import (
	"context"
	"testing"
	"time"

	ghapi "github.com/kristofferR/codereview-queue/internal/gh"
)

type gatedObserveReads struct {
	GitHubAPI
	started chan string
	release chan struct{}
}

func (g *gatedObserveReads) gate(ctx context.Context, name string) error {
	select {
	case g.started <- name:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-g.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *gatedObserveReads) GetCommit(ctx context.Context, repo, sha string) (ghapi.Commit, error) {
	if err := g.gate(ctx, "commit"); err != nil {
		return ghapi.Commit{}, err
	}
	return g.GitHubAPI.GetCommit(ctx, repo, sha)
}

func (g *gatedObserveReads) ListReviews(ctx context.Context, repo string, pr int) ([]ghapi.Review, error) {
	if err := g.gate(ctx, "reviews"); err != nil {
		return nil, err
	}
	return g.GitHubAPI.ListReviews(ctx, repo, pr)
}

func (g *gatedObserveReads) ListIssueComments(ctx context.Context, repo string, pr int) ([]ghapi.IssueComment, error) {
	if err := g.gate(ctx, "comments"); err != nil {
		return nil, err
	}
	return g.GitHubAPI.ListIssueComments(ctx, repo, pr)
}

func TestObserveFetchesHeadResourcesConcurrently(t *testing.T) {
	cfg := firingConfig()
	base := newFakeGitHub()
	sha := "abcdef1234567890"
	var pull ghapi.Pull
	pull.State = "open"
	pull.Head.SHA = sha
	base.pulls[fakeKey("o/r", 1)] = pull
	base.commits[sha] = ghapi.Commit{SHA: sha}

	github := &gatedObserveReads{
		GitHubAPI: base,
		started:   make(chan string, 3),
		release:   make(chan struct{}),
	}
	service := NewService(cfg, github, NewMemoryStore(cfg), nil)
	done := make(chan error, 1)
	go func() {
		_, err := service.observe(t.Context(), cfg, "o/r", 1, nil, nil, time.Now().UTC())
		done <- err
	}()

	seen := map[string]bool{}
	for range 3 {
		select {
		case name := <-github.started:
			seen[name] = true
		case <-time.After(time.Second):
			t.Fatal("head-dependent reads did not start concurrently")
		}
	}
	close(github.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"commit", "reviews", "comments"} {
		if !seen[name] {
			t.Fatalf("%s read did not start", name)
		}
	}
}
