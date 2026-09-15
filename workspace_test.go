package shepherd

import (
	"context"
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
	sha, err := (&gitRunner{dir: dir}).headSHA(context.Background())
	if err != nil {
		t.Fatalf("headSHA: %v", err)
	}
	return sha
}

// stagedFiles returns the staged file list, used to pin the non-mutating
// contract of Capture and Diff.
func stagedFiles(t *testing.T, dir string) string {
	t.Helper()
	out, err := (&gitRunner{dir: dir}).run(context.Background(), "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	return out
}

func TestWorkspace_CaptureCleanRepo(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:tree-clean", repo)

	ws, err := scope.CaptureWorkspace(context.Background())
	if err != nil {
		t.Fatalf("CaptureWorkspace: %v", err)
	}
	if got := stashOf(&Checkpoint{Workspace: ws}); got != "" {
		t.Errorf("clean repo should have empty stash SHA, got %s", got)
	}
	if ws.Revision == "" {
		t.Error("clean repo should have a HEAD revision")
	}
	if ws.Backend != "git" {
		t.Errorf("backend = %q, want git", ws.Backend)
	}
}

func TestWorkspace_CaptureDirtyRepo(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:tree-dirty", repo)

	writeFile(t, repo, "README.md", "# Modified")

	ws, err := scope.CaptureWorkspace(context.Background())
	if err != nil {
		t.Fatalf("CaptureWorkspace: %v", err)
	}
	if got := stashOf(&Checkpoint{Workspace: ws}); got == "" {
		t.Error("dirty repo should produce a non-empty stash SHA")
	}
}

func TestWorkspace_ApplyRoundTrip_Uncommitted(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:tree-rt", repo)

	writeFile(t, repo, "tracked.txt", "original")
	writeFile(t, repo, "dirty.txt", "dirty original")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "add files")

	ws, err := scope.CaptureWorkspace(context.Background())
	if err != nil {
		t.Fatalf("CaptureWorkspace: %v", err)
	}

	// Mutate: modify tracked, add untracked, delete a file.
	writeFile(t, repo, "tracked.txt", "mutated")
	writeFile(t, repo, "untracked/new.txt", "untracked content")
	if err := rmFile(t, repo, "dirty.txt"); err != nil {
		t.Fatalf("remove dirty.txt: %v", err)
	}

	if err := scope.ApplyWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}

	if got := readFile(t, repo, "tracked.txt"); got != "original" {
		t.Errorf("tracked.txt = %q, want %q", got, "original")
	}
	if got := readFile(t, repo, "dirty.txt"); got != "dirty original" {
		t.Errorf("dirty.txt = %q, want %q", got, "dirty original")
	}
	if fileExists(t, repo, "untracked/new.txt") {
		t.Error("untracked/new.txt should be removed by ApplyWorkspace")
	}
}

func TestWorkspace_ApplyRoundTrip_CommitsRolledBack(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:tree-rt-commit", repo)

	ws, err := scope.CaptureWorkspace(context.Background())
	if err != nil {
		t.Fatalf("CaptureWorkspace: %v", err)
	}

	// Commit new work after the capture.
	writeFile(t, repo, "after.txt", "committed after capture")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "work after capture")

	if err := scope.ApplyWorkspace(context.Background(), ws); err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}

	if fileExists(t, repo, "after.txt") {
		t.Error("post-capture commit should be rolled back by ApplyWorkspace")
	}
	if headAfter := mustHead(t, repo); headAfter != ws.Revision {
		t.Errorf("HEAD after apply = %s, want %s", headAfter, ws.Revision)
	}
}

func TestWorkspace_ApplyIsRepeatable(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:tree-repeat", repo)

	writeFile(t, repo, "base.txt", "base")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "base")

	ws, err := scope.CaptureWorkspace(context.Background())
	if err != nil {
		t.Fatalf("CaptureWorkspace: %v", err)
	}

	for i := 0; i < 3; i++ {
		writeFile(t, repo, "scratch.txt", strings.Repeat("x", i+1))
		if err := scope.ApplyWorkspace(context.Background(), ws); err != nil {
			t.Fatalf("ApplyWorkspace iteration %d: %v", i, err)
		}
		if fileExists(t, repo, "scratch.txt") {
			t.Fatalf("iteration %d: scratch.txt should be cleaned by ApplyWorkspace", i)
		}
	}
}

func TestWorkspace_ApplyRejectsForeignBackend(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:tree-nil", repo)

	err := scope.ApplyWorkspace(context.Background(), WorkspaceState{Backend: "containerd"})
	if err == nil {
		t.Error("ApplyWorkspace with a foreign backend should error")
	}
}

func TestWorkspace_CaptureRecordsInTrace(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:ws-trace", repo)

	if _, err := scope.CaptureWorkspace(context.Background()); err != nil {
		t.Fatalf("CaptureWorkspace: %v", err)
	}
	if err := scope.ApplyWorkspace(context.Background(), WorkspaceState{
		Backend:  "git",
		Revision: mustHead(t, repo),
		Data:     map[string]any{"head_sha": mustHead(t, repo), "stash_sha": ""},
	}); err != nil {
		t.Fatalf("ApplyWorkspace: %v", err)
	}

	slice, err := store.ReadOwnerPrefix(TrustedReadContext, "sub:ws-trace", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	var foundCaptured, foundApplied bool
	for _, id := range slice.FactIDs() {
		env := slice.FactsByID[id].GetEnvelope()
		switch env.SchemaRef {
		case SchemaWorkspaceCaptured:
			foundCaptured = true
		case SchemaWorkspaceApplied:
			foundApplied = true
		}
	}
	if !foundCaptured {
		t.Error("expected workspace.captured declaration in trace")
	}
	if !foundApplied {
		t.Error("expected workspace.applied capture in trace")
	}
	// The old tree schemas must be gone.
	for _, id := range slice.FactIDs() {
		if ref := slice.FactsByID[id].GetEnvelope().SchemaRef; strings.HasPrefix(ref, "shepherd.tree.") {
			t.Errorf("unexpected legacy schema ref %q", ref)
		}
	}
}

func TestWorkspace_CaptureDoesNotLeaveIndexStaged(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:ws-index", repo)

	writeFile(t, repo, "README.md", "# Modified")
	writeFile(t, repo, "untracked.txt", "new untracked")

	if _, err := scope.CaptureWorkspace(context.Background()); err != nil {
		t.Fatalf("CaptureWorkspace: %v", err)
	}
	if staged := stagedFiles(t, repo); staged != "" {
		t.Errorf("index left staged after capture: %q", staged)
	}
}

func TestWorkspace_DiffUncommittedAndUntracked(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:diff", repo)
	base := mustHead(t, repo)

	writeFile(t, repo, "README.md", "# Changed")
	writeFile(t, repo, "brand-new.txt", "untracked file")

	ws := WorkspaceState{Backend: "git", Revision: base}
	diff, files, err := scope.DiffWorkspace(context.Background(), ws, 0)
	if err != nil {
		t.Fatalf("DiffWorkspace: %v", err)
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

func TestWorkspace_DiffIsNonMutating(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:diff-index", repo)
	base := mustHead(t, repo)

	writeFile(t, repo, "README.md", "# Changed")
	writeFile(t, repo, "brand-new.txt", "untracked file")

	if _, _, err := scope.DiffWorkspace(context.Background(), WorkspaceState{Backend: "git", Revision: base}, 0); err != nil {
		t.Fatalf("DiffWorkspace: %v", err)
	}
	// Diff must restore the caller's index, unlike the old DiffSince.
	if staged := stagedFiles(t, repo); staged != "" {
		t.Errorf("index left staged after diff: %q", staged)
	}
}

func TestWorkspace_DiffMaxLinesTruncates(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:diff-trunc", repo)
	base := mustHead(t, repo)

	var sb strings.Builder
	for i := 0; i < 50; i++ {
		sb.WriteString("line\n")
	}
	writeFile(t, repo, "big.txt", sb.String())

	diff, files, err := scope.DiffWorkspace(context.Background(), WorkspaceState{Backend: "git", Revision: base}, 10)
	if err != nil {
		t.Fatalf("DiffWorkspace: %v", err)
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

func TestWorkspace_DiffCleanTree(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:diff-clean", repo)
	base := mustHead(t, repo)

	diff, files, err := scope.DiffWorkspace(context.Background(), WorkspaceState{Backend: "git", Revision: base}, 0)
	if err != nil {
		t.Fatalf("DiffWorkspace: %v", err)
	}
	if diff != "" {
		t.Errorf("clean tree diff = %q, want empty", diff)
	}
	if len(files) != 0 {
		t.Errorf("clean tree files = %v, want empty", files)
	}
}
