package shepherd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRepo creates a temp git repo with an initial commit, isolated from
// the developer's global and system git config (gpgsign, hooksPath, commit
// templates, etc.) so a host-machine setting can't break the tests.
func newTestRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)

	dir := t.TempDir()

	// Point git at empty global/system config so inherited settings
	// (commit.gpgsign=true, core.hooksPath, commit.template, ...) don't
	// make the test commit fail in ways that look like checkpoint defects.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "gitconfig-system"))

	mustGit(t, dir, "init")
	mustGit(t, dir, "config", "user.email", "test@test.com")
	mustGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test"), 0644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustGit(t, dir, "add", "-A")
	mustGit(t, dir, "commit", "-m", "init")
	return dir
}

// requireGit skips the test when the git binary is unavailable, producing a
// clear skip instead of a confusing failure mid-test.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
}

// mustGit runs git in a dir, fails the test on error.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	g := &gitRunner{dir: dir}
	if _, err := g.run(context.Background(), args...); err != nil {
		t.Fatalf("git %s in %s: %v", args[0], dir, err)
	}
}

// gitScope returns a root scope backed by an in-place git sandbox over repo.
// The scope does not own the sandbox, so lifecycle operations never touch the
// caller's repository.
func gitScope(store *SQLiteTraceStore, owner, repo string) *Scope {
	return NewScope(store, owner).WithSandbox(NewLocalGitSandbox(repo), false)
}

// stashOf extracts the stash SHA from a checkpoint's backend-neutral workspace
// state.
func stashOf(cp *Checkpoint) string {
	s, _ := cp.Workspace.Data["stash_sha"].(string)
	return s
}

// writeFile is a test helper that writes a file, failing the test on error.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// readFile reads a file, failing the test on error.
func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestCheckpoint_CreateCleanRepo(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:clean", repo)

	cp, err := scope.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if cp.State != CheckpointValid {
		t.Errorf("expected valid, got %s", cp.State)
	}
	if got := stashOf(cp); got != "" {
		t.Errorf("clean repo should have empty stash SHA, got %s", got)
	}
	if cp.Workspace.Revision == "" {
		t.Error("clean repo should have a HEAD revision")
	}
	if cp.Workspace.Backend != "git" {
		t.Errorf("backend = %q, want git", cp.Workspace.Backend)
	}
	if cp.ScopeID != scope.ID() {
		t.Errorf("expected scope %s, got %s", scope.ID(), cp.ScopeID)
	}
}

func TestCheckpoint_CreateDirtyRepo(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:dirty", repo)

	// Modify a tracked file
	writeFile(t, repo, "README.md", "# Modified")

	cp, err := scope.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if stashOf(cp) == "" {
		t.Error("dirty repo should produce a non-empty stash SHA")
	}
}

func TestCheckpoint_CreateWithUntracked(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:untracked", repo)

	// Add an untracked file
	writeFile(t, repo, "new_file.go", "package main")

	cp, err := scope.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if stashOf(cp) == "" {
		t.Error("repo with untracked file should produce a non-empty stash SHA")
	}
}

func TestCheckpoint_CreateRecordsInTrace(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:trace-cp", repo)

	_, err := scope.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	slice, err := store.ReadOwnerPrefix(TrustedReadContext, "sub:trace-cp", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	found := false
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		if rec.GetEnvelope().SchemaRef == SchemaCheckpointCreated &&
			rec.GetEnvelope().Mode == Declaration {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected checkpoint:created declaration in trace")
	}
}

func TestCheckpoint_CreateRecordsBackendNeutralPayload(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:trace-payload", repo)

	cp, err := scope.CreateCheckpoint(context.Background(), []byte(`{"m":1}`))
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	slice, err := store.ReadOwnerPrefix(TrustedReadContext, "sub:trace-payload", 99, ModeDeclarationsOnly)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	var payload map[string]any
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		if rec.GetEnvelope().SchemaRef == SchemaCheckpointCreated {
			if r, ok := rec.(Record); ok {
				payload = r.Body.Payload
			}
		}
	}
	if payload == nil {
		t.Fatal("checkpoint:created payload not found")
	}
	for _, key := range []string{"checkpoint_id", "backend", "revision", "state_digest", "has_snapshot"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("payload missing %q: %v", key, payload)
		}
	}
	// The v1 payload's git-specific keys must be gone.
	for _, key := range []string{"stash_sha", "head_sha"} {
		if _, ok := payload[key]; ok {
			t.Errorf("payload should not carry git-specific key %q", key)
		}
	}
	digest, _ := cp.Workspace.Digest()
	if payload["state_digest"] != digest {
		t.Errorf("state_digest = %v, want %s", payload["state_digest"], digest)
	}
	if payload["has_snapshot"] != true {
		t.Errorf("has_snapshot = %v, want true", payload["has_snapshot"])
	}
}

func TestCheckpoint_RestoreRevertsModifications(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:revert-mod", repo)

	// Initial content
	writeFile(t, repo, "main.go", "package main\n")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "add main.go")

	// Checkpoint
	cp, err := scope.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	// Modify the file
	writeFile(t, repo, "main.go", "BROKEN")

	// Restore
	if _, err := scope.RestoreCheckpoint(context.Background(), cp); err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}

	got := readFile(t, repo, "main.go")
	// Git autocrlf may convert \n to \r\n on Windows; normalize for comparison.
	if strings.TrimSpace(got) != "package main" {
		t.Errorf("expected 'package main', got %q", got)
	}
}

func TestCheckpoint_RestoreRevertsUntracked(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:revert-untracked", repo)

	cp, err := scope.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	// Create a new untracked file
	writeFile(t, repo, "unwanted.go", "UNWANTED")

	// Restore
	if _, err := scope.RestoreCheckpoint(context.Background(), cp); err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}

	// File should be gone
	if _, err := os.Stat(filepath.Join(repo, "unwanted.go")); !os.IsNotExist(err) {
		t.Errorf("expected untracked file to be removed, got err=%v", err)
	}
}

func TestCheckpoint_RestoreRevertsDeletions(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:revert-del", repo)

	cp, err := scope.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	// Delete a tracked file
	if err := os.Remove(filepath.Join(repo, "README.md")); err != nil {
		t.Fatalf("remove README: %v", err)
	}

	// Restore
	if _, err := scope.RestoreCheckpoint(context.Background(), cp); err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}

	if got := readFile(t, repo, "README.md"); got != "# Test" {
		t.Errorf("expected '# Test', got %q", got)
	}
}

func TestCheckpoint_RestoreReturnsSnapshot(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:snapshot", repo)

	snapshot := []byte(`{"messages":["system","task"]}`)
	cp, err := scope.CreateCheckpoint(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	returned, err := scope.RestoreCheckpoint(context.Background(), cp)
	if err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}
	if string(returned) != string(snapshot) {
		t.Errorf("snapshot mismatch: expected %q, got %q", snapshot, returned)
	}
}

func TestCheckpoint_RestoreRecordsInTrace(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:trace-restore", repo)

	cp, err := scope.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	if _, err := scope.RestoreCheckpoint(context.Background(), cp); err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}

	slice, err := store.ReadOwnerPrefix(TrustedReadContext, "sub:trace-restore", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	found := false
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		if rec.GetEnvelope().SchemaRef == SchemaCheckpointRestored &&
			rec.GetEnvelope().Mode == Capture {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected checkpoint:restored capture in trace")
	}
}

func TestCheckpoint_RestoreTwiceFails(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:double-restore", repo)

	cp, err := scope.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	if _, err := scope.RestoreCheckpoint(context.Background(), cp); err != nil {
		t.Fatalf("first RestoreCheckpoint: %v", err)
	}

	if _, err := scope.RestoreCheckpoint(context.Background(), cp); err == nil {
		t.Error("second restore should fail")
	}
	if cp.State != CheckpointUsed {
		t.Errorf("expected state used, got %s", cp.State)
	}
}

func TestCheckpoint_NonGitRepo(t *testing.T) {
	store := newMemStore(t)
	// t.TempDir() is not a git repo
	repo := t.TempDir()
	scope := gitScope(store, "sub:no-git", repo)

	if _, err := scope.CreateCheckpoint(context.Background(), nil); err == nil {
		t.Error("CreateCheckpoint on non-git dir should fail")
	}
}

func TestCheckpoint_RestoreWrongScope(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scopeA := gitScope(store, "sub:a", repo)
	scopeB := gitScope(store, "sub:b", repo)

	cp, err := scopeA.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	if _, err := scopeB.RestoreCheckpoint(context.Background(), cp); err == nil {
		t.Error("restore on wrong scope should fail")
	}
}

func TestCheckpoint_NotOnActiveScope(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:halted", repo)

	// Create a checkpoint while the scope is active.
	cp, err := scope.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	// Halt the scope.
	if err := scope.Halt(); err != nil {
		t.Fatalf("Halt: %v", err)
	}

	// CreateCheckpoint on a halted scope must fail.
	if _, err := scope.CreateCheckpoint(context.Background(), nil); err == nil {
		t.Error("CreateCheckpoint on halted scope should fail")
	}

	// RestoreCheckpoint on a halted scope must fail too — the checkpoint
	// was created while active, but the scope is now halted.
	if _, err := scope.RestoreCheckpoint(context.Background(), cp); err == nil {
		t.Error("RestoreCheckpoint on halted scope should fail")
	}
}

func TestCheckpoint_StashApplyConflict(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:conflict", repo)

	// Initial content + commit
	writeFile(t, repo, "main.go", "package main\n\nfunc a() {}\n")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "add main.go")

	// Checkpoint (stash will capture "package main\n\nfunc a() {}")
	cp, err := scope.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	// Modify the same file differently and commit.
	writeFile(t, repo, "main.go", "package main\n\nfunc b() {}\n")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "change main.go")

	// Restore should work — reset --hard goes back to the checkpoint's
	// revision, then the stash (captured when the tree was clean) applies
	// cleanly. Both outcomes are explicitly asserted.
	if _, err := scope.RestoreCheckpoint(context.Background(), cp); err != nil {
		// A conflict is acceptable — the stash apply can fail if the
		// checkpoint's HEAD differs from where the stash was created.
		if cp.State != CheckpointInvalid {
			t.Errorf("expected invalid state after failed restore, got %s", cp.State)
		}
		return
	}
	// Success path: checkpoint must be marked used and content restored.
	if cp.State != CheckpointUsed {
		t.Errorf("expected used state after successful restore, got %s", cp.State)
	}
	if got := readFile(t, repo, "main.go"); !strings.Contains(got, "func a()") {
		t.Errorf("expected checkpointed content with func a(), got %q", got)
	}
}

func TestCheckpoint_RestorePreservesCleanRepo(t *testing.T) {
	// Edge case: checkpoint on clean repo, no changes, restore should be a no-op
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:noop", repo)

	cp, err := scope.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	// No changes made between checkpoint and restore

	if _, err := scope.RestoreCheckpoint(context.Background(), cp); err != nil {
		t.Fatalf("RestoreCheckpoint on clean repo: %v", err)
	}

	if got := readFile(t, repo, "README.md"); got != "# Test" {
		t.Errorf("expected '# Test', got %q", got)
	}
}

func TestCheckpoint_DoesNotLeaveIndexStaged(t *testing.T) {
	store := newMemStore(t)
	repo := newTestRepo(t)
	scope := gitScope(store, "sub:index", repo)

	// Dirty the tree both ways: a modified tracked file and a new
	// untracked file. A checkpoint must capture both in the stash but
	// leave the caller's index unmodified.
	writeFile(t, repo, "README.md", "# Modified")
	writeFile(t, repo, "untracked.txt", "new untracked")

	cp, err := scope.CreateCheckpoint(context.Background(), nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if stashOf(cp) == "" {
		t.Fatal("expected a stash for a dirty tree")
	}

	// Nothing may remain staged after the checkpoint.
	g := &gitRunner{dir: repo}
	staged, err := g.run(context.Background(), "diff", "--cached", "--name-only")
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	if staged != "" {
		t.Errorf("index left staged after checkpoint: %q", staged)
	}

	// The working tree is untouched.
	if got := readFile(t, repo, "README.md"); got != "# Modified" {
		t.Errorf("README.md = %q, want %q", got, "# Modified")
	}
	if got := readFile(t, repo, "untracked.txt"); got != "new untracked" {
		t.Errorf("untracked.txt = %q, want %q", got, "new untracked")
	}

	// The stash still captured both changes: restore reproduces them.
	if _, err := scope.RestoreCheckpoint(context.Background(), cp); err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}
	if got := readFile(t, repo, "untracked.txt"); got != "new untracked" {
		t.Errorf("after restore untracked.txt = %q, want %q", got, "new untracked")
	}
}

func TestCheckpoint_NoSandboxReturnsErrNoSandbox(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:no-sandbox")

	if _, err := scope.CreateCheckpoint(context.Background(), nil); err != ErrNoSandbox {
		t.Errorf("CreateCheckpoint err = %v, want ErrNoSandbox", err)
	}
	if _, err := scope.CaptureWorkspace(context.Background()); err != ErrNoSandbox {
		t.Errorf("CaptureWorkspace err = %v, want ErrNoSandbox", err)
	}
	if err := scope.ApplyWorkspace(context.Background(), WorkspaceState{Backend: "git"}); err != ErrNoSandbox {
		t.Errorf("ApplyWorkspace err = %v, want ErrNoSandbox", err)
	}
	if _, _, err := scope.DiffWorkspace(context.Background(), WorkspaceState{}, 0); err != ErrNoSandbox {
		t.Errorf("DiffWorkspace err = %v, want ErrNoSandbox", err)
	}
}
