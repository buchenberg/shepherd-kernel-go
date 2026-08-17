package shepherd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rmFile removes a file from the test repo, failing the test on error.
func rmFile(t *testing.T, dir, name string) error {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatalf("remove %s: %v", name, err)
	}
	return nil
}

// fileExists reports whether a path exists in the test repo.
func fileExists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// mustHead returns the current HEAD SHA, failing the test on error.
func mustHead(t *testing.T, dir string) string {
	t.Helper()
	sha, err := (&gitRunner{repoPath: dir}).headSHA()
	if err != nil {
		t.Fatalf("headSHA: %v", err)
	}
	return sha
}

func TestTreeState_CaptureCleanRepo(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:tree-clean")
	repo := newTestRepo(t)

	ts, err := scope.CaptureTree(repo)
	if err != nil {
		t.Fatalf("CaptureTree: %v", err)
	}
	if ts.StashSHA != "" {
		t.Errorf("clean repo should have empty stash SHA, got %s", ts.StashSHA)
	}
	if ts.HeadSHA == "" {
		t.Error("clean repo should have a HEAD SHA")
	}
}

func TestTreeState_CaptureDirtyRepo(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:tree-dirty")
	repo := newTestRepo(t)

	writeFile(t, repo, "README.md", "# Modified")

	ts, err := scope.CaptureTree(repo)
	if err != nil {
		t.Fatalf("CaptureTree: %v", err)
	}
	if ts.StashSHA == "" {
		t.Error("dirty repo should produce a non-empty stash SHA")
	}
}

func TestTreeState_ApplyRoundTrip_Uncommitted(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:tree-rt")
	repo := newTestRepo(t)

	writeFile(t, repo, "tracked.txt", "original")
	writeFile(t, repo, "dirty.txt", "dirty original")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "add files")

	ts, err := scope.CaptureTree(repo)
	if err != nil {
		t.Fatalf("CaptureTree: %v", err)
	}

	// Mutate: modify tracked, add untracked, delete a file.
	writeFile(t, repo, "tracked.txt", "mutated")
	writeFile(t, repo, "untracked/new.txt", "untracked content")
	if err := rmFile(t, repo, "dirty.txt"); err != nil {
		t.Fatalf("remove dirty.txt: %v", err)
	}

	if err := scope.ApplyTree(repo, ts); err != nil {
		t.Fatalf("ApplyTree: %v", err)
	}

	if got := readFile(t, repo, "tracked.txt"); got != "original" {
		t.Errorf("tracked.txt = %q, want %q", got, "original")
	}
	if got := readFile(t, repo, "dirty.txt"); got != "dirty original" {
		t.Errorf("dirty.txt = %q, want %q", got, "dirty original")
	}
	if fileExists(t, repo, "untracked/new.txt") {
		t.Error("untracked/new.txt should be removed by ApplyTree")
	}
}

func TestTreeState_ApplyRoundTrip_CommitsRolledBack(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:tree-rt-commit")
	repo := newTestRepo(t)

	ts, err := scope.CaptureTree(repo)
	if err != nil {
		t.Fatalf("CaptureTree: %v", err)
	}

	// Commit new work after the capture.
	writeFile(t, repo, "after.txt", "committed after capture")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "work after capture")

	if err := scope.ApplyTree(repo, ts); err != nil {
		t.Fatalf("ApplyTree: %v", err)
	}

	if fileExists(t, repo, "after.txt") {
		t.Error("post-capture commit should be rolled back by ApplyTree")
	}
	headAfter, err := (&gitRunner{repoPath: repo}).headSHA()
	if err != nil {
		t.Fatalf("headSHA after apply: %v", err)
	}
	if headAfter != ts.HeadSHA {
		t.Errorf("HEAD after apply = %s, want %s", headAfter, ts.HeadSHA)
	}
}

func TestTreeState_ApplyIsRepeatable(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:tree-repeat")
	repo := newTestRepo(t)

	writeFile(t, repo, "base.txt", "base")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "base")

	ts, err := scope.CaptureTree(repo)
	if err != nil {
		t.Fatalf("CaptureTree: %v", err)
	}

	for i := 0; i < 3; i++ {
		writeFile(t, repo, "scratch.txt", strings.Repeat("x", i+1))
		if err := scope.ApplyTree(repo, ts); err != nil {
			t.Fatalf("ApplyTree iteration %d: %v", i, err)
		}
		if fileExists(t, repo, "scratch.txt") {
			t.Fatalf("iteration %d: scratch.txt should be cleaned by ApplyTree", i)
		}
	}
}

func TestTreeState_ApplyNil(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:tree-nil")

	if err := scope.ApplyTree("", nil); err == nil {
		t.Error("ApplyTree with nil TreeState should error")
	}
}

func TestDiffSince_UncommittedAndUntracked(t *testing.T) {
	repo := newTestRepo(t)
	baseHead := mustHead(t, repo)

	writeFile(t, repo, "README.md", "# Changed")
	writeFile(t, repo, "brand-new.txt", "untracked file")

	diff, files, err := DiffSince(repo, baseHead, 0)
	if err != nil {
		t.Fatalf("DiffSince: %v", err)
	}
	if !strings.Contains(diff, "# Changed") {
		t.Error("diff should contain the modified content")
	}
	if !strings.Contains(diff, "brand-new.txt") {
		t.Error("diff should contain the untracked file")
	}
	if len(files) != 2 {
		t.Errorf("files = %v, want 2 entries", files)
	}
}

func TestDiffSince_MaxLinesTruncates(t *testing.T) {
	repo := newTestRepo(t)
	baseHead := mustHead(t, repo)

	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString("line\n")
	}
	writeFile(t, repo, "big.txt", sb.String())

	diff, files, err := DiffSince(repo, baseHead, 10)
	if err != nil {
		t.Fatalf("DiffSince: %v", err)
	}
	if got := strings.Count(diff, "\n"); got > 11 {
		t.Errorf("truncated diff has %d newlines, want at most 11 (10 lines + marker)", got)
	}
	if !strings.Contains(diff, "...[diff truncated]") {
		t.Error("truncated diff should carry the marker")
	}
	if len(files) != 1 {
		t.Errorf("files = %v, want 1 entry even when diff is truncated", files)
	}
}

func TestDiffSince_CleanTree(t *testing.T) {
	repo := newTestRepo(t)
	baseHead := mustHead(t, repo)

	diff, files, err := DiffSince(repo, baseHead, 0)
	if err != nil {
		t.Fatalf("DiffSince: %v", err)
	}
	if diff != "" {
		t.Errorf("clean tree diff = %q, want empty", diff)
	}
	if len(files) != 0 {
		t.Errorf("clean tree files = %v, want empty", files)
	}
}
