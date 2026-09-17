package shepherd

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitTimeout bounds each git subprocess. A hung git process (e.g. waiting on a
// credential prompt or a dead NFS mount) must not block the caller
// indefinitely, even when the caller passes context.Background().
const gitTimeout = 30 * time.Second

// gitRunner executes git commands in a directory with a bounded timeout and
// terminal prompts disabled, returning trimmed stdout.
type gitRunner struct {
	dir string
}

// run executes a git command. The caller's context is honored for its values but
// NOT for cancellation: git is the substrate that keeps the workspace
// consistent, and aborting mid-operation leaves exactly the damage these
// methods exist to prevent (a staged index after `git add -A`, a half-applied
// reset, an abandoned worktree). Each subprocess is instead bounded by
// gitTimeout, which is the ceiling that actually protects the caller.
//
// Remote backends are free to honor cancellation; this backend deliberately
// does not. Terminal prompts are disabled so a command that would ask for
// credentials fails fast instead of hanging.
func (g *gitRunner) run(ctx context.Context, args ...string) (string, error) {
	return g.runWithEnv(ctx, nil, args...)
}

// runWithEnv is run with extra environment variables appended. It is how the
// scratch-index operations redirect git's index (GIT_INDEX_FILE) without ever
// touching the caller's.
func (g *gitRunner) runWithEnv(ctx context.Context, env []string, args ...string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w (stderr: %s)",
			strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// indexEnv returns the environment that redirects git at a scratch index. It is
// defined once so the variable name and format cannot drift between the
// creation site and the commands that consume it.
func indexEnv(path string) []string {
	return []string{"GIT_INDEX_FILE=" + path}
}

// stagedIndex stages the full working tree — including untracked files — into a
// scratch index inside a private temp directory. The caller's real index is
// never touched, so Capture and Diff stay non-mutating even when the caller had
// staged changes before the call. The 0700 directory also keeps the staged
// snapshot unreadable to other local users; a bare file in a shared temp dir
// would not, because git rewrites the index through a 0644 lock-and-rename.
//
// The scratch index is seeded from the repository's real index when one exists,
// so git's stat cache survives and `git add -A` only hashes changed files.
// Otherwise it falls back to `git read-tree HEAD`, which also stages deletions
// correctly (an empty index would treat every tracked file as newly added).
// `git stash create --include-untracked` is NOT an alternative: that flag is
// silently ignored by the plumbing `git stash create` command (only
// `git stash push`/`save` honour it).
//
// Returns the scratch index path and a cleanup func the caller must always call.
func (g *gitRunner) stagedIndex(ctx context.Context) (string, func(), error) {
	dir, err := os.MkdirTemp("", "shepherd-index-*")
	if err != nil {
		return "", nil, fmt.Errorf("create scratch index dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "index")

	env := indexEnv(path)
	if !g.seedScratchIndex(ctx, path) {
		if _, err := g.runWithEnv(ctx, env, "read-tree", "HEAD"); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("seed scratch index: %w", err)
		}
	}
	if _, err := g.runWithEnv(ctx, env, "add", "-A"); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("stage into scratch index: %w", err)
	}
	return path, cleanup, nil
}

// seedScratchIndex copies the repository's real index to dst so the stat cache
// (and therefore `git add -A`'s ability to skip unchanged files) is preserved.
// It reports false when no readable index exists, leaving the caller to seed via
// `git read-tree HEAD`.
func (g *gitRunner) seedScratchIndex(ctx context.Context, dst string) bool {
	out, err := g.run(ctx, "rev-parse", "--git-path", "index")
	if err != nil || out == "" {
		return false
	}
	src := out
	if !filepath.IsAbs(src) {
		src = filepath.Join(g.dir, src)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return false
	}
	return os.WriteFile(dst, data, 0o600) == nil
}

// stashCreate runs `git stash create` against a scratch index and returns the
// stash commit SHA. Returns an empty string if the working tree is clean.
func (g *gitRunner) stashCreate(ctx context.Context, index string) (string, error) {
	out, err := g.runWithEnv(ctx, indexEnv(index), "stash", "create")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// headSHA returns the current HEAD commit SHA.
func (g *gitRunner) headSHA(ctx context.Context) (string, error) {
	return g.run(ctx, "rev-parse", "HEAD")
}

// resetHardTo runs `git reset --hard <sha>`.
func (g *gitRunner) resetHardTo(ctx context.Context, sha string) error {
	_, err := g.run(ctx, "reset", "--hard", sha)
	return err
}

// cleanFiles runs `git clean -fd` — removes untracked files and directories.
func (g *gitRunner) cleanFiles(ctx context.Context) error {
	_, err := g.run(ctx, "clean", "-fd")
	return err
}

// stashApply runs `git stash apply <sha>`.
func (g *gitRunner) stashApply(ctx context.Context, sha string) error {
	_, err := g.run(ctx, "stash", "apply", sha)
	return err
}

// isRepo reports whether dir is inside a git working tree.
func (g *gitRunner) isRepo(ctx context.Context) bool {
	_, err := g.run(ctx, "rev-parse", "--is-inside-work-tree")
	return err == nil
}

// GitSandbox materializes a git working tree on the host filesystem.
//
// It has two modes, distinguished only by lifecycle:
//
//   - In-place (NewLocalGitSandbox): operates directly on the caller's
//     repository. It is a materializer, not an isolation boundary, so
//     Isolated is false and Containment is ContainUncontained. Create
//     validates that the path is a repository and Destroy is a no-op — a
//     sandbox must never delete a user's repository.
//
//   - Worktree (NewWorktreeSandbox): allocates a detached `git worktree` for
//     the scope. Create and Destroy manage real resources, Isolated is true,
//     and Containment is ContainBuffered: effects are held in a separate
//     checkout and only graduate when the caller applies that state to the
//     parent's in-place sandbox.
//
// Capture, Apply, and Diff are identical across modes and are shared: git
// worktrees share the object store, so a stash commit created in a worktree can
// be applied in the main tree of the same repository.
//
// Exec, ReadFile, and WriteFile return ErrUnsupported. Agent processes are the
// caller's concern; this backend owns git state only.
type GitSandbox struct {
	repoPath     string
	worktreePath string
}

var _ Sandbox = (*GitSandbox)(nil)

// NewLocalGitSandbox returns an in-place sandbox over repoPath.
func NewLocalGitSandbox(repoPath string) *GitSandbox {
	return &GitSandbox{repoPath: repoPath}
}

// NewWorktreeSandbox returns an isolated sandbox that checks out repoPath at a
// detached worktree in worktreePath. repoPath remains the source of truth for
// git administrative commands; worktreePath is where workspace operations run.
func NewWorktreeSandbox(repoPath, worktreePath string) *GitSandbox {
	return &GitSandbox{repoPath: repoPath, worktreePath: worktreePath}
}

// Backend identifies the backend.
func (g *GitSandbox) Backend() string { return "git" }

// isWorktree reports whether this sandbox owns a detached worktree.
func (g *GitSandbox) isWorktree() bool { return g.worktreePath != "" }

// workDir is the directory workspace operations run in.
func (g *GitSandbox) workDir() string {
	if g.isWorktree() {
		return g.worktreePath
	}
	return g.repoPath
}

// repo returns a runner bound to the repository (for administrative commands).
func (g *GitSandbox) repo() *gitRunner { return &gitRunner{dir: g.repoPath} }

// work returns a runner bound to the active workspace directory.
func (g *GitSandbox) work() *gitRunner { return &gitRunner{dir: g.workDir()} }

// Capabilities reports the git backend's support matrix.
func (g *GitSandbox) Capabilities() SandboxCapabilities {
	if g.isWorktree() {
		return SandboxCapabilities{
			Lifecycle:   true,
			Exec:        false,
			FileIO:      false,
			Diff:        true,
			Isolated:    true,
			Containment: ContainBuffered,
		}
	}
	return SandboxCapabilities{
		Lifecycle:   false,
		Exec:        false,
		FileIO:      false,
		Diff:        true,
		Isolated:    false,
		Containment: ContainUncontained,
	}
}

// Create validates the repository, and in worktree mode allocates the worktree.
//
// The worktree is created detached (`--detach`) so sibling variants cannot
// contend over a branch. Stale administrative entries from a previous crash are
// pruned first, which is a no-op when there is nothing to prune.
func (g *GitSandbox) Create(ctx context.Context, spec SandboxSpec) error {
	repo := g.repo()
	if !repo.isRepo(ctx) {
		return fmt.Errorf("git sandbox: %s is not a git repository", g.repoPath)
	}

	if !g.isWorktree() {
		return nil
	}

	if g.worktreePath == g.repoPath {
		return fmt.Errorf("git sandbox: worktree path must differ from the repository path")
	}

	if _, err := repo.run(ctx, "worktree", "prune"); err != nil {
		return fmt.Errorf("git sandbox: worktree prune: %w", err)
	}

	// Refuse to clobber an existing non-empty directory: it may be a live
	// worktree belonging to another scope, or unrelated user data.
	if entries, err := os.ReadDir(g.worktreePath); err == nil {
		if len(entries) > 0 {
			return fmt.Errorf("git sandbox: worktree path %s already exists and is not empty", g.worktreePath)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("git sandbox: inspect worktree path %s: %w", g.worktreePath, err)
	}

	if err := os.MkdirAll(filepath.Dir(g.worktreePath), 0o755); err != nil {
		return fmt.Errorf("git sandbox: create worktree parent: %w", err)
	}

	head, err := repo.headSHA(ctx)
	if err != nil {
		return fmt.Errorf("git sandbox: resolve HEAD: %w", err)
	}

	if _, err := repo.run(ctx, "worktree", "add", "--detach", g.worktreePath, head); err != nil {
		return fmt.Errorf("git sandbox: worktree add: %w", err)
	}
	return nil
}

// Destroy removes the worktree this sandbox allocated. In-place mode is a no-op
// so a scope can never delete the user's repository.
//
// On Windows a leftover handle (editor, shell, indexer, antivirus) can make
// `git worktree remove` fail; the directory is then removed directly and the
// administrative entry pruned so the worktree does not become unusable.
func (g *GitSandbox) Destroy(ctx context.Context) error {
	if !g.isWorktree() {
		return nil
	}

	repo := g.repo()
	if _, err := repo.run(ctx, "worktree", "remove", "--force", g.worktreePath); err != nil {
		// Use a fresh context: cleanup must not be abandoned because the
		// caller's context expired mid-run.
		if rmErr := os.RemoveAll(g.worktreePath); rmErr != nil {
			return fmt.Errorf("git sandbox: remove worktree %s: %w (git worktree remove also failed: %v)",
				g.worktreePath, rmErr, err)
		}
	}

	if _, err := repo.run(context.WithoutCancel(ctx), "worktree", "prune"); err != nil {
		return fmt.Errorf("git sandbox: prune after remove: %w", err)
	}
	return nil
}

// Capture records the current workspace state as a reusable WorkspaceState.
//
// The working tree — including untracked files — is staged into a scratch index
// (never the caller's), `git stash create` turns it into an unreferenced commit
// SHA, and HEAD is recorded. The stash is not popped, so the working tree is
// unchanged and a caller that had staged changes keeps them staged.
func (g *GitSandbox) Capture(ctx context.Context) (WorkspaceState, error) {
	w := g.work()
	if !w.isRepo(ctx) {
		return WorkspaceState{}, fmt.Errorf("git sandbox: %s is not a git working tree", g.workDir())
	}

	index, cleanupIndex, err := w.stagedIndex(ctx)
	if err != nil {
		return WorkspaceState{}, fmt.Errorf("capture: %w", err)
	}
	defer cleanupIndex()

	stashSHA, err := w.stashCreate(ctx, index)
	if err != nil {
		return WorkspaceState{}, fmt.Errorf("capture: git stash create: %w", err)
	}

	headSHA, err := w.headSHA(ctx)
	if err != nil {
		return WorkspaceState{}, fmt.Errorf("capture: git rev-parse HEAD: %w", err)
	}

	return WorkspaceState{
		Backend:  "git",
		Revision: headSHA,
		Data: map[string]any{
			"head_sha":  headSHA,
			"stash_sha": stashSHA,
		},
	}, nil
}

// Apply resets the workspace to a captured state. The state is reusable: Apply
// consumes nothing, so the same value may be applied repeatedly (this is what
// fork-and-choose needs).
func (g *GitSandbox) Apply(ctx context.Context, ws WorkspaceState) error {
	if ws.Backend != "git" {
		return fmt.Errorf("git sandbox: cannot apply state from backend %q", ws.Backend)
	}
	headSHA, _ := ws.Data["head_sha"].(string)
	stashSHA, _ := ws.Data["stash_sha"].(string)
	if headSHA == "" {
		return fmt.Errorf("git sandbox: workspace state is missing head_sha")
	}
	return g.applyState(ctx, headSHA, stashSHA)
}

// applyState is the destructive reset sequence shared by Apply. It resets to the
// captured HEAD (rolling back commits made after the capture), removes untracked
// files, then re-applies the captured uncommitted changes.
func (g *GitSandbox) applyState(ctx context.Context, headSHA, stashSHA string) error {
	w := g.work()

	if err := w.resetHardTo(ctx, headSHA); err != nil {
		return fmt.Errorf("apply: git reset --hard %s: %w", headSHA, err)
	}

	if err := w.cleanFiles(ctx); err != nil {
		return fmt.Errorf("apply: git clean -fd: %w", err)
	}

	if stashSHA != "" {
		if err := w.stashApply(ctx, stashSHA); err != nil {
			return fmt.Errorf("apply: git stash apply %s: %w", stashSHA, err)
		}
	}
	return nil
}

// Diff returns the unified diff and changed file paths between ws and the
// current workspace.
//
// Untracked files are made visible through a scratch index, so Diff is
// non-mutating: it never touches the caller's index, matching Capture.
func (g *GitSandbox) Diff(ctx context.Context, ws WorkspaceState, maxLines int) (string, []string, error) {
	if ws.Backend != "git" {
		return "", nil, fmt.Errorf("git sandbox: cannot diff state from backend %q", ws.Backend)
	}
	if ws.Revision == "" {
		return "", nil, fmt.Errorf("git sandbox: workspace state is missing a revision")
	}

	w := g.work()
	if !w.isRepo(ctx) {
		return "", nil, fmt.Errorf("git sandbox: %s is not a git working tree", g.workDir())
	}

	index, cleanupIndex, err := w.stagedIndex(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("diff: %w", err)
	}
	defer cleanupIndex()

	env := indexEnv(index)
	names, err := w.runWithEnv(ctx, env, "diff", "--cached", "--name-only", ws.Revision)
	if err != nil {
		return "", nil, fmt.Errorf("diff: git diff --name-only: %w", err)
	}
	var files []string
	if names != "" {
		files = strings.Split(names, "\n")
	}

	full, err := w.runWithEnv(ctx, env, "diff", "--cached", ws.Revision)
	if err != nil {
		return "", nil, fmt.Errorf("diff: git diff: %w", err)
	}

	if maxLines > 0 {
		lines := strings.Split(full, "\n")
		if len(lines) > maxLines {
			full = strings.Join(lines[:maxLines], "\n") + "\n...[diff truncated]"
		}
	}

	return full, files, nil
}

// Exec is not implemented: the git backend owns git state, not processes.
func (g *GitSandbox) Exec(context.Context, ExecRequest) (ExecResult, error) {
	return ExecResult{}, ErrUnsupported
}

// ReadFile is not implemented.
func (g *GitSandbox) ReadFile(context.Context, string) ([]byte, error) {
	return nil, ErrUnsupported
}

// WriteFile is not implemented.
func (g *GitSandbox) WriteFile(context.Context, string, []byte, fs.FileMode) error {
	return ErrUnsupported
}
