package shepherd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIntegration_RollbackScenario exercises the full rollback flow:
// set up store+bus+manager+supervisor, create a scope, checkpoint,
// simulate destructive sub-agent changes, restore, verify everything
// reverted correctly, and verify the trace recorded both operations.
func TestIntegration_RollbackScenario(t *testing.T) {
	ctx := context.Background()

	// 1. Infrastructure
	store := newMemStore(t)
	bus := NewEffectBus(64)
	defer bus.Close()
	store.WithBus(bus)
	mgr := NewScopeManager(store)

	// 2. Git repo with initial code
	repo := newTestRepo(t)
	writeFile(t, repo, "main.go", "package main\n\nfunc main() {}\n")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "initial code")

	// 3. Create a scope for the sub-agent
	scope, err := mgr.Create("sub:worker", NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create scope: %v", err)
	}

	// 4. Checkpoint BEFORE the sub-agent runs
	conversationSnapshot := []byte(`{"messages":["system prompt","task: fix the bug"]}`)
	cp, err := mgr.CreateCheckpoint(ctx, scope.ID(), conversationSnapshot)
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if cp.State != CheckpointValid {
		t.Fatalf("expected valid checkpoint, got %s", cp.State)
	}

	// 5. Simulate the sub-agent making destructive changes
	writeFile(t, repo, "main.go", "BROKEN CODE THAT SHOULD BE REVERTED")
	writeFile(t, repo, "new_file.go", "UNWANTED NEW FILE")
	if err := os.Remove(filepath.Join(repo, "README.md")); err != nil {
		t.Fatalf("remove README: %v", err)
	}

	// 6. Supervisor decides to roll back
	snapshot, err := mgr.RestoreCheckpoint(ctx, cp.ID)
	if err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}

	// 7. Verify workspace restored
	mainGo := readFile(t, repo, "main.go")
	if !strings.Contains(mainGo, "package main") || strings.Contains(mainGo, "BROKEN") {
		t.Errorf("main.go not restored: %q", mainGo)
	}

	if _, err := os.Stat(filepath.Join(repo, "new_file.go")); !os.IsNotExist(err) {
		t.Error("untracked file new_file.go should be removed")
	}

	if readme := readFile(t, repo, "README.md"); readme != "# Test" {
		t.Errorf("README.md not restored: %q", readme)
	}

	// 8. Verify conversation snapshot round-tripped
	if string(snapshot) != string(conversationSnapshot) {
		t.Errorf("snapshot mismatch: expected %q, got %q", conversationSnapshot, snapshot)
	}

	// 9. Verify trace records both checkpoint.create and checkpoint.restore
	slice, err := store.ReadOwnerPrefix(TrustedReadContext, "sub:worker", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	var foundCreate, foundRestore bool
	for _, id := range slice.FactIDs() {
		env := slice.FactsByID[id].GetEnvelope()
		switch env.SchemaRef {
		case SchemaCheckpointCreated:
			if env.Mode == Declaration {
				foundCreate = true
			}
		case SchemaCheckpointRestored:
			if env.Mode == Capture {
				foundRestore = true
			}
		}
	}
	if !foundCreate {
		t.Error("trace missing checkpoint:created declaration")
	}
	if !foundRestore {
		t.Error("trace missing checkpoint:restored capture")
	}
}

// TestIntegration_SynchronousDeny simulates a supervisor blocking a
// destructive tool call before it executes, using CheckCall.
func TestIntegration_SynchronousDeny(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	supervisor := NewSupervisor(mgr, nil)

	// Install the blocking guard
	supervisor.AddRule(DestructiveToolGuard())

	// Simulate the agent loop's pre-tool check for a destructive bash command
	event := EffectEvent{
		Mode:         Declaration,
		TraceOwnerID: "sub:worker",
		KindLabel:    "bash",
		SchemaRef:    "yaah.tool.bash.v1",
		Payload:      map[string]any{"cmd": "rm -rf /important"},
	}

	iv := supervisor.CheckCall(event)
	if iv == nil {
		t.Fatal("expected intervention from CheckCall")
	}
	if iv.Type != InterventionDeny {
		t.Errorf("expected deny, got %s", iv.Type)
	}
	if iv.ScopeID != "scope:sub:worker" {
		t.Errorf("expected scope:sub:worker, got %s", iv.ScopeID)
	}

	// Verify that even a benign bash command is denied under default-deny —
	// shell commands cannot be safely allowlisted by text matching.
	safeEvent := EffectEvent{
		Mode:         Declaration,
		TraceOwnerID: "sub:worker",
		KindLabel:    "bash",
		SchemaRef:    "yaah.tool.bash.v1",
		Payload:      map[string]any{"cmd": "go test ./..."},
	}
	safeIV := supervisor.CheckCall(safeEvent)
	if safeIV == nil || safeIV.Type != InterventionDeny {
		t.Errorf("bash should be denied under default-deny, got intervention: %v", safeIV)
	}

	// Verify that a non-bash, non-write, non-delete tool is approved.
	readEvent := EffectEvent{
		Mode:         Declaration,
		TraceOwnerID: "sub:worker",
		KindLabel:    "read_file",
		SchemaRef:    "yaah.tool.read.v1",
		Payload:      map[string]any{"path": "/tmp/x"},
	}
	if readIV := supervisor.CheckCall(readEvent); readIV != nil {
		t.Errorf("read_file should be approved, got intervention: %v", readIV)
	}
}

// TestIntegration_CheckpointForkRestore exercises the pattern:
// fork a child scope, checkpoint the child, do work, restore, work again.
func TestIntegration_CheckpointForkRestore(t *testing.T) {
	ctx := context.Background()
	store := newMemStore(t)
	mgr := NewScopeManager(store)

	repo := newTestRepo(t)
	writeFile(t, repo, "app.go", "package main\n")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "add app.go")

	// Parent scope
	parent, err := mgr.Create("sub:parent", NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// Fork a child for speculative work
	child, err := mgr.Fork(parent.ID(), "sub:speculative", nil)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}

	// Checkpoint the child
	cp, err := mgr.CreateCheckpoint(ctx, child.ID(), []byte("v1"))
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// Speculative work
	writeFile(t, repo, "app.go", "CHANGED SPECULATIVELY")

	// Restore the checkpoint
	if _, err := mgr.RestoreCheckpoint(ctx, cp.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// Verify reverted
	if got := readFile(t, repo, "app.go"); strings.TrimSpace(got) != "package main" {
		t.Errorf("expected 'package main', got %q", got)
	}

	// The child is still active — can do different work
	writeFile(t, repo, "app.go", "DIFFERENT ATTEMPT")

	// Merge the child into parent (accept the speculative work)
	if err := parent.Merge(child); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if child.State() != ScopeMerged {
		t.Errorf("expected child merged, got %s", child.State())
	}
}

// TestIntegration_MultipleCheckpoints exercises creating multiple
// checkpoints and restoring to an earlier one.
func TestIntegration_MultipleCheckpoints(t *testing.T) {
	ctx := context.Background()
	store := newMemStore(t)
	mgr := NewScopeManager(store)

	repo := newTestRepo(t)
	writeFile(t, repo, "main.go", "version 1")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "v1")

	scope, err := mgr.Create("sub:multi", NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create scope: %v", err)
	}

	// Checkpoint 1
	cp1, err := mgr.CreateCheckpoint(ctx, scope.ID(), []byte("state-1"))
	if err != nil {
		t.Fatalf("cp1: %v", err)
	}

	// Work + commit
	writeFile(t, repo, "main.go", "version 2")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "v2")

	// Checkpoint 2
	time.Sleep(10 * time.Millisecond) // ensure different timestamps
	cp2, err := mgr.CreateCheckpoint(ctx, scope.ID(), []byte("state-2"))
	if err != nil {
		t.Fatalf("cp2: %v", err)
	}

	// LatestCheckpoint should be cp2
	latest := mgr.LatestCheckpoint(scope.ID())
	if latest == nil {
		t.Fatal("expected non-nil latest checkpoint")
	}
	if latest.ID != cp2.ID {
		t.Errorf("expected latest to be cp2, got %s", latest.ID)
	}

	// Work again
	writeFile(t, repo, "main.go", "version 3")

	// Restore to cp2 (the most recent checkpoint)
	if _, err := mgr.RestoreCheckpoint(ctx, cp2.ID); err != nil {
		t.Fatalf("restore cp2: %v", err)
	}
	if got := readFile(t, repo, "main.go"); strings.TrimSpace(got) != "version 2" {
		t.Errorf("expected 'version 2' after restoring cp2, got %q", got)
	}

	// cp1 is still valid — we can restore to the earlier point
	snap, err := mgr.RestoreCheckpoint(ctx, cp1.ID)
	if err != nil {
		t.Fatalf("restore cp1: %v", err)
	}
	if string(snap) != "state-1" {
		t.Errorf("expected 'state-1' snapshot, got %q", snap)
	}
	if got := readFile(t, repo, "main.go"); strings.TrimSpace(got) != "version 1" {
		t.Errorf("expected 'version 1' after restoring cp1, got %q", got)
	}
}

// TestIntegration_WorktreeForkDiscardChoose is the end-to-end isolation
// scenario: two speculative variants run in their own worktrees, both are
// discarded (which physically destroys them), and the winning variant's state
// is applied to the parent's in-place tree.
func TestIntegration_WorktreeForkDiscardChoose(t *testing.T) {
	ctx := context.Background()
	store := newMemStore(t)
	mgr := NewScopeManager(store)

	repo := newTestRepo(t)
	writeFile(t, repo, "base.txt", "base")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "base")

	parent, err := mgr.Create("sub:orchestrator", NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// Capture the fork point once. Both variants must start from it.
	forkState, err := parent.CaptureWorkspace(ctx)
	if err != nil {
		t.Fatalf("capture fork state: %v", err)
	}

	root := t.TempDir()
	type variant struct {
		child    *Scope
		worktree string
		state    WorkspaceState
		file     string
		content  string
	}

	variants := []*variant{
		{worktree: filepath.Join(root, "a"), file: "feature-a.txt", content: "variant A"},
		{worktree: filepath.Join(root, "b"), file: "feature-b.txt", content: "variant B"},
	}

	for i, v := range variants {
		childOwner := "sub:variant-" + string(rune('a'+i))
		child, err := mgr.ForkIsolated(parent.ID(), childOwner, nil, NewWorktreeSandbox(repo, v.worktree))
		if err != nil {
			t.Fatalf("ForkIsolated %d: %v", i, err)
		}
		v.child = child

		sb := child.Sandbox()
		if err := sb.Create(ctx, SandboxSpec{}); err != nil {
			t.Fatalf("variant %d create: %v", i, err)
		}
		if err := child.ApplyWorkspace(ctx, forkState); err != nil {
			t.Fatalf("variant %d seed fork state: %v", i, err)
		}

		// The variant works only inside its own worktree.
		writeFile(t, v.worktree, v.file, v.content)
		if fileExists(t, repo, v.file) {
			t.Fatalf("variant %d leaked into the main tree before choose", i)
		}

		state, err := child.CaptureWorkspace(ctx)
		if err != nil {
			t.Fatalf("variant %d capture: %v", i, err)
		}
		v.state = state
	}

	// Discard both variants. For an owned worktree sandbox this destroys the
	// worktree, which is what makes discard a physical rollback.
	for i, v := range variants {
		if err := mgr.Discard(v.child.ID()); err != nil {
			t.Fatalf("discard variant %d: %v", i, err)
		}
		if _, err := os.Stat(v.worktree); !os.IsNotExist(err) {
			t.Errorf("variant %d worktree should be removed, stat err=%v", i, err)
		}
	}

	// The parent's tree is untouched by either variant.
	for _, v := range variants {
		if fileExists(t, repo, v.file) {
			t.Errorf("main tree must not contain %s before choose", v.file)
		}
	}

	// Choose variant A: apply its captured state to the parent's in-place tree.
	if err := parent.ApplyWorkspace(ctx, variants[0].state); err != nil {
		t.Fatalf("apply winner: %v", err)
	}
	if got := readFile(t, repo, variants[0].file); got != variants[0].content {
		t.Errorf("%s = %q, want %q", variants[0].file, got, variants[0].content)
	}
	if fileExists(t, repo, variants[1].file) {
		t.Errorf("losing variant %s must not be applied", variants[1].file)
	}

	// No worktree directories or registrations may survive.
	for _, v := range variants {
		for _, p := range worktreePaths(t, repo) {
			if samePath(t, p, v.worktree) {
				t.Errorf("worktree %s still registered after discard", p)
			}
		}
	}
}
