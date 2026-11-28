package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/kristofferR/codereview-queue/internal/crq"
)

// Once a repository's records have moved to its new name, a command that names
// it the old way — `crq unhold`, `crq repos remove`, the resume line crq posts
// on a pull request — has to act on the moved records, not beside them.
func TestRenamedRepositoryArgumentsFollowTheRecords(t *testing.T) {
	cfg := crq.Config{GateRepo: "o/gate", StateRef: "crq-state-v3", Host: "test"}
	store := crq.NewMemoryStore(cfg)
	if _, err := store.Update(context.Background(), func(st *crq.State) error {
		st.RenameRepo("o/old", "o/new")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service := crq.NewService(cfg, nil, store, nil)

	got := resolveRenamedRepos(context.Background(), service,
		[]string{"unhold", "O/Old", "7", "--reason", "o/kept", "--", "o/old"})
	want := []string{"unhold", "o/new", "7", "--reason", "o/kept", "--", "o/old"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}
}
