package shepherd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestRepo creates a temp git repo with an initial commit.
func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
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

// mustGit runs git in a dir, fails the test on error.
func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	g := &gitRunner{repoPath: dir}
	if _, err := g.run(args...); err != nil {
		t.Fatalf("git %s in %s: %v", args[0], dir, err)
	}
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
	scope := NewScope(store, "sub:clean")
	repo := newTestRepo(t)

	cp, err := scope.CreateCheckpoint(repo, nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if cp.State != CheckpointValid {
		t.Errorf("expected valid, got %s", cp.State)
	}
	if cp.StashSHA != "" {
		t.Errorf("clean repo should have empty stash SHA, got %s", cp.StashSHA)
	}
	if cp.HeadSHA == "" {
		t.Error("clean repo should have a HEAD SHA")
	}
	if cp.ScopeID != scope.ID() {
		t.Errorf("expected scope %s, got %s", scope.ID(), cp.ScopeID)
	}
}

func TestCheckpoint_CreateDirtyRepo(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:dirty")
	repo := newTestRepo(t)

	// Modify a tracked file
	writeFile(t, repo, "README.md", "# Modified")

	cp, err := scope.CreateCheckpoint(repo, nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if cp.StashSHA == "" {
		t.Error("dirty repo should produce a non-empty stash SHA")
	}
}

func TestCheckpoint_CreateWithUntracked(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:untracked")
	repo := newTestRepo(t)

	// Add an untracked file
	writeFile(t, repo, "new_file.go", "package main")

	cp, err := scope.CreateCheckpoint(repo, nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if cp.StashSHA == "" {
		t.Error("repo with untracked file should produce a non-empty stash SHA")
	}
}

func TestCheckpoint_CreateRecordsInTrace(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:trace-cp")
	repo := newTestRepo(t)

	_, err := scope.CreateCheckpoint(repo, nil)
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

func TestCheckpoint_RestoreRevertsModifications(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:revert-mod")
	repo := newTestRepo(t)

	// Initial content
	writeFile(t, repo, "main.go", "package main\n")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "add main.go")

	// Checkpoint
	cp, err := scope.CreateCheckpoint(repo, nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	// Modify the file
	writeFile(t, repo, "main.go", "BROKEN")

	// Restore
	_, err = scope.RestoreCheckpoint(cp)
	if err != nil {
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
	scope := NewScope(store, "sub:revert-untracked")
	repo := newTestRepo(t)

	cp, err := scope.CreateCheckpoint(repo, nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	// Create a new untracked file
	writeFile(t, repo, "unwanted.go", "UNWANTED")

	// Restore
	_, err = scope.RestoreCheckpoint(cp)
	if err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}

	// File should be gone
	_, err = os.Stat(filepath.Join(repo, "unwanted.go"))
	if !os.IsNotExist(err) {
		t.Errorf("expected untracked file to be removed, got err=%v", err)
	}
}

func TestCheckpoint_RestoreRevertsDeletions(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:revert-del")
	repo := newTestRepo(t)

	cp, err := scope.CreateCheckpoint(repo, nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	// Delete a tracked file
	if err := os.Remove(filepath.Join(repo, "README.md")); err != nil {
		t.Fatalf("remove README: %v", err)
	}

	// Restore
	_, err = scope.RestoreCheckpoint(cp)
	if err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}

	got := readFile(t, repo, "README.md")
	if got != "# Test" {
		t.Errorf("expected '# Test', got %q", got)
	}
}

func TestCheckpoint_RestoreReturnsSnapshot(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:snapshot")
	repo := newTestRepo(t)

	snapshot := []byte(`{"messages":["system","task"]}`)
	cp, err := scope.CreateCheckpoint(repo, snapshot)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	returned, err := scope.RestoreCheckpoint(cp)
	if err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}
	if string(returned) != string(snapshot) {
		t.Errorf("snapshot mismatch: expected %q, got %q", snapshot, returned)
	}
}

func TestCheckpoint_RestoreRecordsInTrace(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:trace-restore")
	repo := newTestRepo(t)

	cp, err := scope.CreateCheckpoint(repo, nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	_, err = scope.RestoreCheckpoint(cp)
	if err != nil {
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
	scope := NewScope(store, "sub:double-restore")
	repo := newTestRepo(t)

	cp, err := scope.CreateCheckpoint(repo, nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	_, err = scope.RestoreCheckpoint(cp)
	if err != nil {
		t.Fatalf("first RestoreCheckpoint: %v", err)
	}

	_, err = scope.RestoreCheckpoint(cp)
	if err == nil {
		t.Error("second restore should fail")
	}
	if cp.State != CheckpointUsed {
		t.Errorf("expected state used, got %s", cp.State)
	}
}

func TestCheckpoint_NonGitRepo(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:no-git")

	// t.TempDir() is not a git repo
	dir := t.TempDir()

	_, err := scope.CreateCheckpoint(dir, nil)
	if err == nil {
		t.Error("CreateCheckpoint on non-git dir should fail")
	}
}

func TestCheckpoint_RestoreWrongScope(t *testing.T) {
	store := newMemStore(t)
	scopeA := NewScope(store, "sub:a")
	scopeB := NewScope(store, "sub:b")
	repo := newTestRepo(t)

	cp, err := scopeA.CreateCheckpoint(repo, nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	_, err = scopeB.RestoreCheckpoint(cp)
	if err == nil {
		t.Error("restore on wrong scope should fail")
	}
}

func TestCheckpoint_NotOnActiveScope(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:halted")
	repo := newTestRepo(t)

	// Halt the scope
	if err := scope.Halt(); err != nil {
		t.Fatalf("Halt: %v", err)
	}

	_, err := scope.CreateCheckpoint(repo, nil)
	if err == nil {
		t.Error("CreateCheckpoint on halted scope should fail")
	}

	// Also test restore on halted scope: need a valid checkpoint first
	// (halted scope can't create one, so we'll skip the restore test here)
}

func TestCheckpoint_StashApplyConflict(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:conflict")
	repo := newTestRepo(t)

	// Initial content + commit
	writeFile(t, repo, "main.go", "package main\n\nfunc a() {}\n")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "add main.go")

	// Checkpoint (stash will capture "package main\n\nfunc a() {}")
	cp, err := scope.CreateCheckpoint(repo, nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	// Modify the same file differently — the stash apply will need to
	// re-apply the old version, but reset --hard already wiped to HEAD.
	// So the stash apply should succeed because reset restored HEAD state.
	writeFile(t, repo, "main.go", "package main\n\nfunc b() {}\n")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "change main.go")

	// Restore should work — reset --hard goes to latest HEAD (func b),
	// then stash apply re-applies the checkpointed state (func a).
	// This may produce a merge conflict since HEAD changed.
	// We accept either success or a conflict error — the key is no crash.
	_, err = scope.RestoreCheckpoint(cp)
	if err != nil {
		// Conflict is acceptable — the checkpoint state can't be cleanly
		// applied after HEAD moved. State should be invalid.
		if cp.State != CheckpointInvalid {
			t.Errorf("expected invalid state after failed restore, got %s", cp.State)
		}
	}
}

func TestCheckpoint_RestorePreservesCleanRepo(t *testing.T) {
	// Edge case: checkpoint on clean repo, no changes, restore should be a no-op
	store := newMemStore(t)
	scope := NewScope(store, "sub:noop")
	repo := newTestRepo(t)

	cp, err := scope.CreateCheckpoint(repo, nil)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}

	// No changes made between checkpoint and restore

	_, err = scope.RestoreCheckpoint(cp)
	if err != nil {
		t.Fatalf("RestoreCheckpoint on clean repo: %v", err)
	}

	// README should still exist
	got := readFile(t, repo, "README.md")
	if got != "# Test" {
		t.Errorf("expected '# Test', got %q", got)
	}
}
