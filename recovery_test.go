package shepherd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// reopenStore closes the store and reopens the same database file, the
// process-restart this suite exists to exercise.
func reopenStore(t *testing.T, store *SQLiteTraceStore, path string) *SQLiteTraceStore {
	t.Helper()
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("close store: %v", err)
	}
	reopened, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close(context.Background()) })
	return reopened
}

// fileStore opens a file-backed store so a "restart" is possible.
func fileStore(t *testing.T) (*SQLiteTraceStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.db")
	store, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store, path
}

// TestRecover_RestartRoundTrip is the acceptance scenario: create → fork →
// checkpoint → close → recover → restore works, and the recovered child can
// still propose merges against its recovered baseline.
func TestRecover_RestartRoundTrip(t *testing.T) {
	store, path := fileStore(t)
	ctx := context.Background()
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)

	parent, err := mgr.Create("rec:parent", NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	_, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "rec:child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "feature.txt"), []byte("work"), 0o644); err != nil {
		t.Fatalf("child writes: %v", err)
	}

	cp, err := mgr.CreateCheckpoint(ctx, parent.ID(), []byte("conversation"))
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// The parent's workspace moves after the checkpoint, so the restore at
	// the end must roll it back.
	if err := os.WriteFile(filepath.Join(repo, "later.txt"), []byte("stray"), 0o644); err != nil {
		t.Fatalf("write later: %v", err)
	}

	// Restart: close, reopen, recover. The resolver re-adopts the parent's
	// in-place sandbox and the child's still-existing worktree without
	// re-provisioning (the directory is already there).
	reopened := reopenStore(t, store, path)
	resolved := map[string]bool{}
	recovered, err := RecoverScopes(reopened, RecoverOptions{
		Resolver: func(scopeID, backend string) (Sandbox, error) {
			resolved[scopeID] = true
			switch scopeID {
			case "scope:rec:parent":
				return NewLocalGitSandbox(repo), nil
			case "scope:rec:child":
				return NewWorktreeSandbox(repo, wtPath), nil
			}
			return nil, nil // deliberate non-adoption
		},
	})
	if err != nil {
		t.Fatalf("RecoverScopes: %v", err)
	}
	if !resolved["scope:rec:parent"] || !resolved["scope:rec:child"] {
		t.Errorf("resolver not consulted for both scopes: %v", resolved)
	}

	rp, ok := recovered.Get("scope:rec:parent")
	if !ok {
		t.Fatal("parent not recovered")
	}
	rc, ok := recovered.Get("scope:rec:child")
	if !ok {
		t.Fatal("child not recovered")
	}
	if rp.State() != ScopeActive || rc.State() != ScopeActive {
		t.Errorf("states = %s/%s, want active/active", rp.State(), rc.State())
	}
	if rc.Parent() == nil || rc.Parent().ID() != rp.ID() {
		t.Fatal("recovered tree lost the parent link")
	}
	if _, hasBaseline := rc.Baseline(); !hasBaseline {
		t.Fatal("recovered child should carry the fork baseline")
	}
	if rc.Sandbox() == nil {
		t.Error("recovered isolated child should have its re-adopted sandbox")
	}
	if !rc.ownsSandbox {
		t.Error("recovered isolated child should own its sandbox")
	}
	if rp.Sandbox() == nil {
		t.Error("recovered root should have its re-adopted sandbox")
	}

	// The recovered child can still propose merges against the recovered
	// baseline: recovery did not just rebuild names, it rebuilt capability.
	proposal, err := recovered.ProposeMerge(ctx, rc.ID(), 100)
	if err != nil {
		t.Fatalf("ProposeMerge on recovered child: %v", err)
	}
	if len(proposal.Changes) != 1 || proposal.Changes[0].Path != "feature.txt" {
		t.Errorf("recovered proposal changes: %+v, want feature.txt", proposal.Changes)
	}

	// The recovered checkpoint restores: the stray post-checkpoint write is
	// gone, the conversation snapshot comes back.
	snapshot, err := recovered.RestoreCheckpoint(ctx, cp.ID)
	if err != nil {
		t.Fatalf("RestoreCheckpoint on recovered registry: %v", err)
	}
	if string(snapshot) != "conversation" {
		t.Errorf("recovered snapshot = %q, want the persisted bytes", snapshot)
	}
	if fileExists(t, repo, "later.txt") {
		t.Error("restore must roll the workspace back past the stray write")
	}
}

// TestRecover_TerminalStatesAndPrune covers terminal-state recovery
// (merged, discarded, halted) and PruneTerminal.
func TestRecover_TerminalStatesAndPrune(t *testing.T) {
	store, path := fileStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, _ := mgr.Create("term:parent", NewLocalGitSandbox(repo))

	merged, err := mgr.Fork(parent.ID(), "term:merged", nil)
	if err != nil {
		t.Fatalf("fork merged: %v", err)
	}
	if err := parent.Merge(merged); err != nil {
		t.Fatalf("merge: %v", err)
	}

	discarded, err := mgr.Fork(parent.ID(), "term:discarded", nil)
	if err != nil {
		t.Fatalf("fork discarded: %v", err)
	}
	if err := parent.Discard(discarded); err != nil {
		t.Fatalf("discard: %v", err)
	}

	halted, err := mgr.Fork(parent.ID(), "term:halted", nil)
	if err != nil {
		t.Fatalf("fork halted: %v", err)
	}
	if err := halted.Halt(); err != nil {
		t.Fatalf("halt: %v", err)
	}

	active, err := mgr.Fork(parent.ID(), "term:active", nil)
	if err != nil {
		t.Fatalf("fork active: %v", err)
	}
	_ = active

	reopened := reopenStore(t, store, path)
	recovered, err := RecoverScopes(reopened, RecoverOptions{})
	if err != nil {
		t.Fatalf("RecoverScopes: %v", err)
	}

	for owner, want := range map[string]ScopeState{
		"term:merged":    ScopeMerged,
		"term:discarded": ScopeDiscarded,
		"term:halted":    ScopeDiscarded,
		"term:active":    ScopeActive,
	} {
		scope, ok := recovered.Get("scope:" + owner)
		if !ok {
			t.Fatalf("scope %s not recovered", owner)
		}
		if scope.State() != want {
			t.Errorf("%s state = %s, want %s", owner, scope.State(), want)
		}
	}

	// PruneTerminal removes the terminal scopes and nothing else.
	if n := recovered.PruneTerminal(); n != 3 {
		t.Errorf("PruneTerminal removed %d, want 3", n)
	}
	for _, owner := range []string{"term:merged", "term:discarded", "term:halted"} {
		if _, ok := recovered.Get("scope:" + owner); ok {
			t.Errorf("terminal scope %s should be pruned", owner)
		}
	}
	if _, ok := recovered.Get("scope:term:active"); !ok {
		t.Error("active scope must survive PruneTerminal")
	}
	if _, ok := recovered.Get("scope:term:parent"); !ok {
		t.Error("parent must survive PruneTerminal")
	}
}

// TestRecover_CheckpointStaleness covers the never-silently-restorable rule:
// a recovered checkpoint whose recorded revision no longer matches the
// re-adopted workspace is invalid; an unmoved one restores.
func TestRecover_CheckpointStaleness(t *testing.T) {
	store, path := fileStore(t)
	ctx := context.Background()
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, _ := mgr.Create("stale:parent", NewLocalGitSandbox(repo))

	// A checkpoint whose base is then left behind by a commit.
	stale, err := mgr.CreateCheckpoint(ctx, parent.ID(), nil)
	if err != nil {
		t.Fatalf("checkpoint stale: %v", err)
	}
	mustGit(t, repo, "commit", "--allow-empty", "-m", "advance")

	// A checkpoint at the CURRENT revision: recovery must find it valid.
	fresh, err := mgr.CreateCheckpoint(ctx, parent.ID(), nil)
	if err != nil {
		t.Fatalf("checkpoint fresh: %v", err)
	}

	reopened := reopenStore(t, store, path)
	recovered, err := RecoverScopes(reopened, RecoverOptions{
		Resolver: func(scopeID, backend string) (Sandbox, error) {
			return NewLocalGitSandbox(repo), nil
		},
	})
	if err != nil {
		t.Fatalf("RecoverScopes: %v", err)
	}

	// The fresh checkpoint is at the current revision: valid and restorable.
	if cp := recovered.LatestCheckpoint("scope:stale:parent"); cp == nil || cp.ID != fresh.ID {
		t.Fatalf("LatestCheckpoint = %+v, want the fresh one", cp)
	}
	if _, err := recovered.RestoreCheckpoint(ctx, fresh.ID); err != nil {
		t.Fatalf("restore fresh: %v", err)
	}

	// The stale checkpoint is invalid: its revision no longer matches the
	// re-adopted workspace.
	if _, ok := recovered.Output(fresh.ID); ok {
		t.Error("checkpoint IDs do not share the output registry; sanity")
	}
	if _, err := recovered.RestoreCheckpoint(ctx, stale.ID); err == nil {
		t.Error("restoring a stale checkpoint must fail")
	}
}

// TestRecover_CheckpointConsumedAcrossRestart covers the single-use rule
// surviving a restart: a checkpoint restored before the restart is not
// offered again (its blob was consumed with it).
func TestRecover_CheckpointConsumedAcrossRestart(t *testing.T) {
	store, path := fileStore(t)
	ctx := context.Background()
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, _ := mgr.Create("used:parent", NewLocalGitSandbox(repo))

	used, err := mgr.CreateCheckpoint(ctx, parent.ID(), []byte("used"))
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := mgr.RestoreCheckpoint(ctx, used.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}

	reopened := reopenStore(t, store, path)
	recovered, err := RecoverScopes(reopened, RecoverOptions{})
	if err != nil {
		t.Fatalf("RecoverScopes: %v", err)
	}
	if cp := recovered.LatestCheckpoint("scope:used:parent"); cp != nil {
		t.Errorf("restored checkpoint resurfaced after restart: %+v", cp)
	}
	if _, err := recovered.RestoreCheckpoint(ctx, used.ID); err == nil {
		t.Error("restore of a pre-restart-restored checkpoint must fail")
	}
}

// TestRecover_OutputsSettleAcrossRestart covers output recovery end to end:
// an unconsumed sealed output is settle-able after restart, and a settled
// one stays consumed (the consume-once rule outlives the process).
func TestRecover_OutputsSettleAcrossRestart(t *testing.T) {
	store, path := fileStore(t)
	ctx := context.Background()
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, _ := mgr.Create("out:parent", NewLocalGitSandbox(repo))

	sealedChild, wtSealed := forkWorktreeChild(t, mgr, parent.ID(), "out:sealed", repo, "wt-sealed")
	if err := os.WriteFile(filepath.Join(wtSealed, "winner.txt"), []byte("wins"), 0o644); err != nil {
		t.Fatalf("write winner: %v", err)
	}
	sealedOut, err := mgr.Seal(ctx, sealedChild.ID())
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// A second child settles (select) BEFORE the restart.
	doneChild, wtDone := forkWorktreeChild(t, mgr, parent.ID(), "out:done", repo, "wt-done")
	if err := os.WriteFile(filepath.Join(wtDone, "settled.txt"), []byte("done"), 0o644); err != nil {
		t.Fatalf("write settled: %v", err)
	}
	doneOut, err := mgr.Seal(ctx, doneChild.ID())
	if err != nil {
		t.Fatalf("seal done: %v", err)
	}
	if err := mgr.Settle(ctx, doneOut.ID(), SettleSelect); err != nil {
		t.Fatalf("settle done: %v", err)
	}

	reopened := reopenStore(t, store, path)
	recovered, err := RecoverScopes(reopened, RecoverOptions{
		Resolver: func(scopeID, backend string) (Sandbox, error) {
			switch scopeID {
			case "scope:out:parent":
				return NewLocalGitSandbox(repo), nil
			case "scope:out:sealed":
				return NewWorktreeSandbox(repo, wtSealed), nil
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("RecoverScopes: %v", err)
	}

	// The settled output stays consumed across the restart.
	if got, ok := recovered.Output(doneOut.ID()); !ok || got.State() != OutputSelected {
		t.Errorf("settled output after restart: ok=%v state=%s, want selected", ok, got.State())
	}
	if err := recovered.Settle(ctx, doneOut.ID(), SettleSelect); !errors.Is(err, ErrOutputConsumed) {
		t.Errorf("re-settling the settled output: %v, want ErrOutputConsumed", err)
	}

	// The unconsumed output is settle-able post-restart: select lands its
	// changes on the parent. (The parent moved — the earlier select applied
	// the done child's work — so this is the apply path, exercising the
	// recovered DeltaApplier.)
	got, ok := recovered.Output(sealedOut.ID())
	if !ok {
		t.Fatal("sealed output not recovered")
	}
	if got.State() != OutputUnconsumed {
		t.Fatalf("recovered sealed output state = %s, want unconsumed", got.State())
	}
	if len(got.Changes()) != 1 || got.Changes()[0].Path != "winner.txt" {
		t.Errorf("recovered changes: %+v, want winner.txt", got.Changes())
	}
	if err := recovered.Settle(ctx, sealedOut.ID(), SettleApply); err != nil {
		t.Fatalf("SettleApply on recovered output: %v", err)
	}
	if got := readRepoFile(t, repo, "winner.txt"); got != "wins" {
		t.Errorf("winner.txt = %q, want the sealed content after apply", got)
	}
}

// TestRecover_ResolverOrphanCleans covers deliberate non-adoption: the
// resolver declines a scope's sandbox and recovery proceeds without it —
// the causal tree survives, workspace operations refuse honestly.
func TestRecover_ResolverOrphanCleans(t *testing.T) {
	store, path := fileStore(t)
	ctx := context.Background()
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, _ := mgr.Create("orph:parent", NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "orph:child", repo, "wt")
	_ = child

	reopened := reopenStore(t, store, path)
	recovered, err := RecoverScopes(reopened, RecoverOptions{
		Resolver: func(scopeID, backend string) (Sandbox, error) {
			// The host orphan-cleans the worktree: nothing is re-adopted.
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("RecoverScopes: %v", err)
	}

	rc, ok := recovered.Get("scope:orph:child")
	if !ok {
		t.Fatal("child not recovered")
	}
	if rc.Sandbox() != nil {
		t.Error("non-adopted child must have no sandbox")
	}
	if _, err := recovered.ProposeMerge(ctx, rc.ID(), 100); err == nil {
		t.Error("ProposeMerge without a sandbox must fail honestly")
	}
	// The worktree itself was the host's to clean; the kernel never touches it.
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Errorf("recovery must not destroy the orphaned worktree: %v", statErr)
	}
}

// TestCheckpointBlobLimit covers the size guard.
func TestCheckpointBlobLimit(t *testing.T) {
	store := newMemStore(t).WithCheckpointBlobLimit(8)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, _ := mgr.Create("limit:parent", NewLocalGitSandbox(repo))

	if _, err := mgr.CreateCheckpoint(context.Background(), parent.ID(), []byte("0123456789")); err == nil {
		t.Error("oversized snapshot must fail the checkpoint")
	}
	if _, err := mgr.CreateCheckpoint(context.Background(), parent.ID(), []byte("short")); err != nil {
		t.Errorf("in-limit snapshot: %v", err)
	}
}
