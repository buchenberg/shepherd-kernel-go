package shepherd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// samePath reports whether two paths refer to the same location, resolving
// symlinks so a temp-dir prefix difference (e.g. /var vs /private/var) does not
// produce a false mismatch.
func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

func TestWorkspaceState_DigestIsOrderIndependent(t *testing.T) {
	a := WorkspaceState{
		Backend:  "git",
		Revision: "abc",
		Data:     map[string]any{"head_sha": "abc", "stash_sha": "def"},
	}
	b := WorkspaceState{
		Backend:  "git",
		Revision: "abc",
		Data:     map[string]any{"stash_sha": "def", "head_sha": "abc"},
	}

	da, err := a.Digest()
	if err != nil {
		t.Fatalf("digest a: %v", err)
	}
	db, err := b.Digest()
	if err != nil {
		t.Fatalf("digest b: %v", err)
	}
	if da != db {
		t.Errorf("digest must be independent of map iteration order: %s vs %s", da, db)
	}
	if !strings.HasPrefix(da, "sha256:") {
		t.Errorf("digest = %q, want a sha256: prefix", da)
	}
}

func TestWorkspaceState_DigestSensitivity(t *testing.T) {
	base := WorkspaceState{
		Backend:  "git",
		Revision: "abc",
		Data:     map[string]any{"stash_sha": "def"},
	}
	baseDigest, err := base.Digest()
	if err != nil {
		t.Fatalf("base digest: %v", err)
	}

	cases := map[string]WorkspaceState{
		"backend":  {Backend: "containerd", Revision: "abc", Data: map[string]any{"stash_sha": "def"}},
		"revision": {Backend: "git", Revision: "xyz", Data: map[string]any{"stash_sha": "def"}},
		"data":     {Backend: "git", Revision: "abc", Data: map[string]any{"stash_sha": "zzz"}},
		"extra":    {Backend: "git", Revision: "abc", Data: map[string]any{"stash_sha": "def", "extra": "1"}},
	}
	for name, ws := range cases {
		t.Run(name, func(t *testing.T) {
			d, err := ws.Digest()
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			if d == baseDigest {
				t.Errorf("digest must change when %s changes", name)
			}
		})
	}
}

func TestWorkspaceState_DigestEmptyState(t *testing.T) {
	if _, err := (WorkspaceState{}).Digest(); err != nil {
		t.Fatalf("empty state digest: %v", err)
	}
}

func TestGitSandbox_Capabilities(t *testing.T) {
	repo := newTestRepo(t)

	inPlace := NewLocalGitSandbox(repo).Capabilities()
	if inPlace.Lifecycle {
		t.Error("in-place mode must not claim a real lifecycle")
	}
	if inPlace.Isolated {
		t.Error("in-place mode must not claim isolation")
	}
	if !inPlace.Diff {
		t.Error("git backend should support Diff")
	}
	if inPlace.Containment != ContainUncontained {
		t.Errorf("in-place containment = %q, want %q", inPlace.Containment, ContainUncontained)
	}
	if inPlace.Exec || inPlace.FileIO {
		t.Error("git backend must not claim Exec or FileIO")
	}

	worktree := NewWorktreeSandbox(repo, t.TempDir()).Capabilities()
	if !worktree.Lifecycle {
		t.Error("worktree mode should claim a real lifecycle")
	}
	if !worktree.Isolated {
		t.Error("worktree mode should claim isolation")
	}
	if worktree.Containment != ContainBuffered {
		t.Errorf("worktree containment = %q, want %q", worktree.Containment, ContainBuffered)
	}
}

func TestGitSandbox_UnsupportedMethods(t *testing.T) {
	repo := newTestRepo(t)
	sb := NewLocalGitSandbox(repo)
	ctx := context.Background()

	if _, err := sb.Exec(ctx, ExecRequest{Command: "true"}); err != ErrUnsupported {
		t.Errorf("Exec err = %v, want ErrUnsupported", err)
	}
	if _, err := sb.ReadFile(ctx, "README.md"); err != ErrUnsupported {
		t.Errorf("ReadFile err = %v, want ErrUnsupported", err)
	}
	if err := sb.WriteFile(ctx, "x", nil, 0o644); err != ErrUnsupported {
		t.Errorf("WriteFile err = %v, want ErrUnsupported", err)
	}
}

func TestGitSandbox_InPlaceCreateValidatesRepo(t *testing.T) {
	ctx := context.Background()

	if err := NewLocalGitSandbox(t.TempDir()).Create(ctx, SandboxSpec{}); err == nil {
		t.Error("Create on a non-repository should fail")
	}
	if err := NewLocalGitSandbox(newTestRepo(t)).Create(ctx, SandboxSpec{}); err != nil {
		t.Errorf("Create on a repository should succeed: %v", err)
	}
}

func TestGitSandbox_InPlaceDestroyIsNoOp(t *testing.T) {
	repo := newTestRepo(t)
	sb := NewLocalGitSandbox(repo)

	if err := sb.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	// The user's repository must survive: an in-place sandbox never owns it.
	if !fileExists(t, repo, "README.md") {
		t.Error("Destroy must not delete the repository")
	}
}

// worktreePaths returns the paths git currently tracks as worktrees.
func worktreePaths(t *testing.T, repo string) []string {
	t.Helper()
	out, err := (&gitRunner{dir: repo}).run(context.Background(), "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatalf("git worktree list: %v", err)
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "worktree "); ok {
			paths = append(paths, rest)
		}
	}
	return paths
}

func TestWorktreeSandbox_CreateAndDestroy(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")

	sb := NewWorktreeSandbox(repo, wtPath)
	if err := sb.Create(ctx, SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !fileExists(t, wtPath, "README.md") {
		t.Fatal("worktree should be checked out at HEAD")
	}

	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree directory should be removed, stat err=%v", err)
	}
	for _, p := range worktreePaths(t, repo) {
		if samePath(t, p, wtPath) {
			t.Errorf("worktree %s still registered after Destroy", p)
		}
	}
	// The repository must always survive.
	if !fileExists(t, repo, "README.md") {
		t.Error("Destroy must not delete the repository")
	}
}

func TestWorktreeSandbox_CreateRefusesNonEmptyPath(t *testing.T) {
	repo := newTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")
	writeFile(t, wtPath, "occupied.txt", "do not clobber")

	sb := NewWorktreeSandbox(repo, wtPath)
	if err := sb.Create(context.Background(), SandboxSpec{}); err == nil {
		t.Error("Create must refuse a non-empty worktree path")
	}
}

func TestWorktreeSandbox_RecoversFromStaleEntry(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")

	sb := NewWorktreeSandbox(repo, wtPath)
	if err := sb.Create(ctx, SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate a crash: the directory disappears but git's administrative
	// entry under .git/worktrees survives.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("remove worktree dir: %v", err)
	}

	// Destroy must clean the stale entry rather than failing.
	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy after stale removal: %v", err)
	}

	// And the same path must be reusable.
	if err := sb.Create(ctx, SandboxSpec{}); err != nil {
		t.Fatalf("Create after recovery: %v", err)
	}
	t.Cleanup(func() { _ = sb.Destroy(context.Background()) })

	for _, p := range worktreePaths(t, repo) {
		if samePath(t, p, wtPath) {
			return
		}
	}
	t.Errorf("worktree %s not registered after recovery", wtPath)
}

func TestWorktreeSandbox_ApplySeedsForkState(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)

	// Dirty the parent tree: a modified tracked file and an untracked file.
	writeFile(t, repo, "README.md", "# Fork point")
	writeFile(t, repo, "untracked.txt", "also at fork point")

	main := NewLocalGitSandbox(repo)
	forkState, err := main.Capture(ctx)
	if err != nil {
		t.Fatalf("capture fork state: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "wt")
	wt := NewWorktreeSandbox(repo, wtPath)
	if err := wt.Create(ctx, SandboxSpec{}); err != nil {
		t.Fatalf("worktree Create: %v", err)
	}
	t.Cleanup(func() { _ = wt.Destroy(context.Background()) })

	if err := wt.Apply(ctx, forkState); err != nil {
		t.Fatalf("worktree Apply: %v", err)
	}

	if got := readFile(t, wtPath, "README.md"); got != "# Fork point" {
		t.Errorf("worktree README.md = %q, want the parent's dirty content", got)
	}
	if got := readFile(t, wtPath, "untracked.txt"); got != "also at fork point" {
		t.Errorf("worktree untracked.txt = %q, want the parent's untracked file", got)
	}
}

func TestWorktreeSandbox_SiblingsAreIndependent(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)

	root := t.TempDir()
	wtA := NewWorktreeSandbox(repo, filepath.Join(root, "a"))
	wtB := NewWorktreeSandbox(repo, filepath.Join(root, "b"))
	if err := wtA.Create(ctx, SandboxSpec{}); err != nil {
		t.Fatalf("create A: %v", err)
	}
	t.Cleanup(func() { _ = wtA.Destroy(context.Background()) })
	if err := wtB.Create(ctx, SandboxSpec{}); err != nil {
		t.Fatalf("create B: %v", err)
	}
	t.Cleanup(func() { _ = wtB.Destroy(context.Background()) })

	writeFile(t, filepath.Join(root, "a"), "only-a.txt", "variant A")
	if fileExists(t, filepath.Join(root, "b"), "only-a.txt") {
		t.Error("a file written in worktree A must not appear in worktree B")
	}
	if fileExists(t, repo, "only-a.txt") {
		t.Error("a file written in worktree A must not appear in the main tree")
	}

	stateA, err := wtA.Capture(ctx)
	if err != nil {
		t.Fatalf("capture A: %v", err)
	}
	// Capturing A must not perturb B.
	if fileExists(t, filepath.Join(root, "b"), "only-a.txt") {
		t.Error("capturing A must not affect worktree B")
	}
	if _, err := wtB.Capture(ctx); err != nil {
		t.Fatalf("capture B: %v", err)
	}
	if stateA.Revision == "" {
		t.Error("captured state should carry a revision")
	}
}

func TestWorktreeSandbox_CaptureAppliesToMainTree(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)

	wtPath := filepath.Join(t.TempDir(), "wt")
	wt := NewWorktreeSandbox(repo, wtPath)
	if err := wt.Create(ctx, SandboxSpec{}); err != nil {
		t.Fatalf("worktree Create: %v", err)
	}

	// Speculative work happens only in the worktree.
	writeFile(t, wtPath, "feature.txt", "from worktree")
	if fileExists(t, repo, "feature.txt") {
		t.Fatal("the main tree must not see worktree changes before Apply")
	}

	variant, err := wt.Capture(ctx)
	if err != nil {
		t.Fatalf("worktree Capture: %v", err)
	}

	// The worktree is torn down before the choice is made, so the state must
	// survive in the shared object store.
	if err := wt.Destroy(ctx); err != nil {
		t.Fatalf("worktree Destroy: %v", err)
	}

	main := NewLocalGitSandbox(repo)
	if err := main.Apply(ctx, variant); err != nil {
		t.Fatalf("apply variant to main tree: %v", err)
	}
	if got := readFile(t, repo, "feature.txt"); got != "from worktree" {
		t.Errorf("feature.txt = %q, want the variant's content", got)
	}
}

func TestWorktreeSandbox_CreateAtRepoPathRejected(t *testing.T) {
	repo := newTestRepo(t)
	sb := NewWorktreeSandbox(repo, repo)
	if err := sb.Create(context.Background(), SandboxSpec{}); err == nil {
		t.Error("Create must reject a worktree path equal to the repository path")
	}
}

// TestWorktreeSandbox_DestroyRetriesOnBusyDirectory pins the Windows fallback:
// the worktree is removed even when `git worktree remove` cannot complete.
func TestWorktreeSandbox_DestroyRetriesOnBusyDirectory(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")

	sb := NewWorktreeSandbox(repo, wtPath)
	if err := sb.Create(ctx, SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A lock file inside the worktree makes `git worktree remove` fail on
	// some platforms; the fallback must still leave nothing behind.
	writeFile(t, wtPath, ".git-lock-probe", "x")
	time.Sleep(10 * time.Millisecond)

	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree should be gone, stat err=%v", err)
	}
	for _, p := range worktreePaths(t, repo) {
		if samePath(t, p, wtPath) {
			t.Errorf("worktree %s still registered after Destroy", p)
		}
	}
}
