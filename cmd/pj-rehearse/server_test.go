package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"

	"sigs.k8s.io/prow/pkg/git/localgit"
	"sigs.k8s.io/prow/pkg/git/v2"
	"sigs.k8s.io/prow/pkg/github"
)

// testRepoClient wraps a real git.RepoClient and overrides FetchRef to work
// with local test repos that don't have GitHub pull request refs. The real code
// fetches "pull/{number}/head"; in tests we fetch the PR branch directly so
// that FETCH_HEAD points to the PR tip.
type testRepoClient struct {
	git.RepoClient
	prBranch         string
	expectedFetchRef string
}

func (t *testRepoClient) FetchRef(ref string) error {
	if t.expectedFetchRef != "" && ref != t.expectedFetchRef {
		return fmt.Errorf("unexpected fetch ref: got %q, want %q", ref, t.expectedFetchRef)
	}
	return t.RepoClient.FetchRef(t.prBranch)
}

type fakeGHC struct {
	refSHA      string
	expectedRef string
}

func (f *fakeGHC) CreateComment(string, string, int, string) error                 { return nil }
func (f *fakeGHC) AddLabel(string, string, int, string) error                      { return nil }
func (f *fakeGHC) RemoveLabel(string, string, int, string) error                   { return nil }
func (f *fakeGHC) GetPullRequest(string, string, int) (*github.PullRequest, error) { return nil, nil }
func (f *fakeGHC) GetRef(_, _, ref string) (string, error) {
	if f.expectedRef != "" && ref != f.expectedRef {
		return "", fmt.Errorf("unexpected ref lookup: got %q, want %q", ref, f.expectedRef)
	}
	return f.refSHA, nil
}
func (f *fakeGHC) ListIssueComments(string, string, int) ([]github.IssueComment, error) {
	return nil, nil
}
func (f *fakeGHC) DeleteComment(string, string, int) error { return nil }
func (f *fakeGHC) IsMember(string, string) (bool, error)   { return false, nil }

func testPullRequest() *github.PullRequest {
	return &github.PullRequest{
		Number: 123,
		Base: github.PullRequestBranch{
			Ref: "main",
			Repo: github.Repo{
				Owner: github.User{Login: "org"},
				Name:  "repo",
			},
		},
		Head: github.PullRequestBranch{
			SHA: "pr-head-sha",
			Ref: "feature-branch",
		},
		User:  github.User{Login: "author"},
		Title: "Test PR",
	}
}

// TestPrepareCandidateRebaseDirection verifies that prepareCandidate rebases PR
// commits onto the base branch, not the other way around.
//
// With the inverted rebase (main onto PR), main's commit history is replayed
// onto the PR tip. When main contains intermediate states that conflict with
// the PR (e.g., a file deleted then re-created), this causes spurious rebase
// conflicts even though the final states are compatible. A correctly-directed
// rebase (PR onto main) replays PR commits onto the main tip and avoids these
// intermediate-state conflicts.
func TestPrepareCandidateRebaseDirection(t *testing.T) {
	lg, clients, err := localgit.NewV2()
	if err != nil {
		t.Fatalf("failed to create localgit: %v", err)
	}
	lg.InitialBranch = "main"
	defer func() {
		if err := lg.Clean(); err != nil {
			t.Errorf("localgit cleanup failed: %v", err)
		}
	}()
	defer func() {
		if err := clients.Clean(); err != nil {
			t.Errorf("client factory cleanup failed: %v", err)
		}
	}()

	if err := lg.MakeFakeRepo("org", "repo"); err != nil {
		t.Fatalf("failed to make fake repo: %v", err)
	}

	// Set up main branch with a base file
	if err := lg.AddCommit("org", "repo", map[string][]byte{"base.txt": []byte("base")}); err != nil {
		t.Fatalf("failed to add base commit: %v", err)
	}

	// Branch off for the PR and add a PR-only file
	if err := lg.CheckoutNewBranch("org", "repo", "pr-branch"); err != nil {
		t.Fatalf("failed to create PR branch: %v", err)
	}
	if err := lg.AddCommit("org", "repo", map[string][]byte{"pr.txt": []byte("pr-change")}); err != nil {
		t.Fatalf("failed to add PR commit: %v", err)
	}

	// Go back to main and add a main-only file (creating divergence)
	if err := lg.Checkout("org", "repo", "main"); err != nil {
		t.Fatalf("failed to checkout main: %v", err)
	}
	if err := lg.AddCommit("org", "repo", map[string][]byte{"main-only.txt": []byte("main-change")}); err != nil {
		t.Fatalf("failed to add main commit: %v", err)
	}

	// Record main SHA before prepareCandidate runs
	mainSHABefore, err := lg.RevParse("org", "repo", "main")
	if err != nil {
		t.Fatalf("failed to rev-parse main: %v", err)
	}
	mainSHABefore = strings.TrimSpace(mainSHABefore)

	// Get a real git client (clones the repo)
	repoClient, err := clients.ClientFor("org", "repo")
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer func() {
		if err := repoClient.Clean(); err != nil {
			t.Errorf("repoClient cleanup failed: %v", err)
		}
	}()

	wrapped := &testRepoClient{RepoClient: repoClient, prBranch: "pr-branch", expectedFetchRef: "pull/123/head"}
	s := &server{ghc: &fakeGHC{refSHA: mainSHABefore, expectedRef: "heads/main"}}
	logger := logrus.NewEntry(logrus.StandardLogger())

	if _, err := s.prepareCandidate(wrapped, testPullRequest(), logger); err != nil {
		t.Fatalf("prepareCandidate failed: %v", err)
	}

	dir := repoClient.Directory()

	// Both PR and main files must exist in the working tree
	for _, f := range []string{"pr.txt", "main-only.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected %s to exist in working tree after rebase: %v", f, err)
		}
	}

	// The main ref in the clone must not have been modified by the rebase.
	// With an inverted rebase, git rewrites the main branch itself (HEAD ends
	// up ON main). With the correct rebase, main is untouched and HEAD is
	// ahead of it with the PR commits on top.
	mainSHAAfter, err := repoClient.RevParse("main")
	if err != nil {
		t.Fatalf("failed to rev-parse main after rebase: %v", err)
	}
	mainSHAAfter = strings.TrimSpace(mainSHAAfter)
	if mainSHAAfter != mainSHABefore {
		t.Fatalf("expected main ref to stay unchanged, before=%s after=%s", mainSHABefore, mainSHAAfter)
	}
	headSHA, err := repoClient.RevParse("HEAD")
	if err != nil {
		t.Fatalf("failed to rev-parse HEAD after rebase: %v", err)
	}
	headSHA = strings.TrimSpace(headSHA)
	if mainSHAAfter == headSHA {
		t.Error("HEAD should be ahead of main after rebasing PR onto main, but they point to the same commit (rebase direction is likely inverted)")
	}

	// Diff between main and HEAD should show exactly the PR file
	changes, err := repoClient.Diff("main", "HEAD")
	if err != nil {
		t.Fatalf("failed to diff: %v", err)
	}
	if len(changes) != 1 || changes[0] != "pr.txt" {
		t.Errorf("expected diff main..HEAD to show only [pr.txt], got %v", changes)
	}
}
