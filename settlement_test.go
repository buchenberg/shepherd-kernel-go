package shepherd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSeal_FreezesChanges covers T3.5's Seal: registered output, classified
// changes frozen at seal time, sealed record on the child's path, and the
// no-baseline refusal.
func TestSeal_FreezesChanges(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:seal-parent", NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:seal-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("winner"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}

	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if out.ID() == "" || out.ScopeID() != child.ID() {
		t.Errorf("output identity: %+v", out)
	}
	if len(out.Changes()) != 1 || out.Changes()[0].Path != "result.txt" || out.Changes()[0].Kind != ChangeCreate {
		t.Errorf("changes: %+v, want one create of result.txt", out.Changes())
	}
	if out.State() != OutputUnconsumed {
		t.Errorf("state = %s, want unconsumed", out.State())
	}

	// Sealing does not merge: the scope stays active, the parent is untouched.
	if child.State() != ScopeActive {
		t.Errorf("child state = %s, want active after seal", child.State())
	}
	if fileExists(t, repo, "result.txt") {
		t.Error("sealing must not touch the parent workspace")
	}

	// Registry lookup paths.
	if got, ok := mgr.Output(out.ID()); !ok || got != out {
		t.Errorf("Output(%s) = %p, %v; want the sealed output", out.ID(), got, ok)
	}
	outs := mgr.OutputsForScope(child.ID())
	if len(outs) != 1 || outs[0] != out {
		t.Errorf("OutputsForScope: %+v, want the one output", outs)
	}

	// The sealed record is durable-trace, not advisory.
	sealed := findRecords(t, store, "set:seal-child", SchemaRunOutputSealed)
	if len(sealed) != 1 {
		t.Fatalf("sealed records: %d, want 1", len(sealed))
	}
	if fmt.Sprint(sealed[0].Body.Payload["output_id"]) != out.ID() {
		t.Errorf("sealed payload output_id = %v, want %s", sealed[0].Body.Payload["output_id"], out.ID())
	}

	// Later writes do not leak into the frozen output.
	if err := os.WriteFile(filepath.Join(wtPath, "extra.txt"), []byte("late"), 0o644); err != nil {
		t.Fatalf("write extra: %v", err)
	}
	if len(out.Changes()) != 1 {
		t.Errorf("changes after later writes: %d, want still 1 (frozen at seal)", len(out.Changes()))
	}

	// A scope without a fork baseline cannot seal.
	causalParent, _ := mgr.Create("set:seal-causal", nil)
	causalChild, err := mgr.Fork(causalParent.ID(), "set:seal-causal-child", nil)
	if err != nil {
		t.Fatalf("causal fork: %v", err)
	}
	if _, err := mgr.Seal(ctx, causalChild.ID()); err == nil {
		t.Error("Seal on a scope without a baseline should fail")
	}

	// An unregistered output (Scope.Seal directly) cannot settle.
	bare, err := child.Seal(ctx)
	if err != nil {
		t.Fatalf("Scope.Seal: %v", err)
	}
	if err := bare.Settle(ctx, SettleRelease); err == nil {
		t.Error("Settle on an unregistered output should fail")
	}
}

// TestSettle_SelectOnUnmovedParent covers the select verb end to end:
// fast-forward-only (strict digest equality), physical propagation, causal
// merge, worktree teardown, consume-once.
func TestSettle_SelectOnUnmovedParent(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:sel-parent", NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:sel-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("winner"), 0o644); err != nil {
		t.Fatalf("write result: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if err := mgr.Settle(ctx, out.ID(), SettleSelect); err != nil {
		t.Fatalf("SettleSelect: %v", err)
	}
	if got := readRepoFile(t, repo, "result.txt"); got != "winner" {
		t.Errorf("parent result.txt = %q, want the winner's content", got)
	}
	if child.State() != ScopeMerged {
		t.Errorf("child state = %s, want merged", child.State())
	}
	if _, statErr := os.Stat(wtPath); !os.IsNotExist(statErr) {
		t.Errorf("child worktree should be destroyed after select, stat err = %v", statErr)
	}
	if out.State() != OutputSelected || out.Action() != SettleSelect {
		t.Errorf("output state/action = %s/%s, want selected", out.State(), out.Action())
	}

	// Consume-once.
	err = mgr.Settle(ctx, out.ID(), SettleSelect)
	if !errors.Is(err, ErrOutputConsumed) {
		t.Errorf("second settle: %v, want ErrOutputConsumed", err)
	}

	// The settled record cites the sealed one.
	if recs := findRecords(t, store, "set:sel-child", SchemaRunOutputSettled); len(recs) != 1 {
		t.Errorf("settled records: %d, want 1", len(recs))
	}
}

// TestSettle_SelectRefusesMovedParent covers the fast-forward-only rule: a
// parent that advanced since the fork gets an actionable error pointing at
// apply, and the output is not consumed by the refusal.
func TestSettle_SelectRefusesMovedParent(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:move-parent", NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:move-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "result.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The parent advances after the seal.
	mustGit(t, repo, "commit", "--allow-empty", "-m", "advance")

	err = mgr.Settle(ctx, out.ID(), SettleSelect)
	if err == nil || !strings.Contains(err.Error(), "SettleApply") {
		t.Fatalf("select on moved parent: %v, want an error pointing at SettleApply", err)
	}
	if out.State() != OutputUnconsumed {
		t.Errorf("refused select consumed the output: state = %s", out.State())
	}
	// The refusal is not consumption: apply still works on the moved parent.
	if err := mgr.Settle(ctx, out.ID(), SettleApply); err != nil {
		t.Fatalf("SettleApply after refused select: %v", err)
	}
}

// TestSettle_ApplyDisjointSucceeds covers the apply verb's happy path: the
// parent moved on disjoint paths, the sealed delta merges onto it.
func TestSettle_ApplyDisjointSucceeds(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:app-parent", NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:app-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "feature.txt"), []byte("from child"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The parent advances on a disjoint path after the fork.
	if err := os.WriteFile(filepath.Join(repo, "parent.txt"), []byte("from parent"), 0o644); err != nil {
		t.Fatalf("write parent.txt: %v", err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "parent work")

	if err := mgr.Settle(ctx, out.ID(), SettleApply); err != nil {
		t.Fatalf("SettleApply (disjoint): %v", err)
	}
	if got := readRepoFile(t, repo, "feature.txt"); got != "from child" {
		t.Errorf("feature.txt = %q, want the child's content on the moved parent", got)
	}
	if got := readRepoFile(t, repo, "parent.txt"); got != "from parent" {
		t.Errorf("parent.txt = %q, want the parent's own work preserved", got)
	}
	if child.State() != ScopeMerged {
		t.Errorf("child state = %s, want merged", child.State())
	}
	if out.State() != OutputApplied {
		t.Errorf("output state = %s, want applied", out.State())
	}
	if recs := findRecords(t, store, "set:app-child", SchemaRunOutputSettled); len(recs) != 1 {
		t.Errorf("settled records: %d, want 1", len(recs))
	}
}

// TestSettle_ApplyConflictKeepsUnconsumed covers the overlap guard: a path
// the parent changed since the baseline is refused with ErrApplyConflict,
// and the failure does not consume.
func TestSettle_ApplyConflictKeepsUnconsumed(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:conf-parent", NewLocalGitSandbox(repo))

	// The child will change shared.txt.
	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:conf-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "shared.txt"), []byte("child's version"), 0o644); err != nil {
		t.Fatalf("write shared: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// The parent changes the same path after the fork.
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("parent's version"), 0o644); err != nil {
		t.Fatalf("write shared: %v", err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "parent moves shared")

	err = mgr.Settle(ctx, out.ID(), SettleApply)
	if !errors.Is(err, ErrApplyConflict) {
		t.Fatalf("SettleApply (overlap): %v, want ErrApplyConflict", err)
	}
	if !strings.Contains(err.Error(), "shared.txt") {
		t.Errorf("conflict text = %q, want it to name the overlapping path", err.Error())
	}
	if out.State() != OutputUnconsumed {
		t.Errorf("conflicting apply consumed the output: state = %s", out.State())
	}
	// The parent's version is untouched by the refusal.
	if got := readRepoFile(t, repo, "shared.txt"); got != "parent's version" {
		t.Errorf("shared.txt = %q, want the parent's version", got)
	}
	// Still unconsumed, the output can be discarded instead.
	if err := mgr.Settle(ctx, out.ID(), SettleDiscard); err != nil {
		t.Fatalf("SettleDiscard after conflict: %v", err)
	}
	if child.State() != ScopeDiscarded {
		t.Errorf("child state = %s, want discarded", child.State())
	}
}

// TestSettle_ReleaseAndUnknownAction covers release (mark consumed, no
// workspace change, no scope lifecycle change) and the unknown-action guard.
func TestSettle_ReleaseAndUnknownAction(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:rel-parent", NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:rel-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "kept.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if err := mgr.Settle(ctx, out.ID(), SettleRelease); err != nil {
		t.Fatalf("SettleRelease: %v", err)
	}
	if out.State() != OutputReleased {
		t.Errorf("state = %s, want released", out.State())
	}
	// Release changed nothing: the child is still active with its worktree.
	if child.State() != ScopeActive {
		t.Errorf("child state = %s, want active after release", child.State())
	}
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Errorf("worktree must survive release: %v", statErr)
	}
	if fileExists(t, repo, "kept.txt") {
		t.Error("release must not touch the parent workspace")
	}
	// The caller still owns the scope's lifecycle: discard it now.
	if err := mgr.Discard(child.ID()); err != nil {
		t.Fatalf("Discard after release: %v", err)
	}

	// Unknown actions refuse without consuming.
	child2, _ := forkWorktreeChild(t, mgr, parent.ID(), "set:rel-child2", repo, "wt2")
	out2, err := mgr.Seal(ctx, child2.ID())
	if err != nil {
		t.Fatalf("Seal 2: %v", err)
	}
	if err := mgr.Settle(ctx, out2.ID(), SettlementAction("teleport")); err == nil {
		t.Error("unknown action must be rejected")
	}
	if out2.State() != OutputUnconsumed {
		t.Errorf("unknown action consumed the output: state = %s", out2.State())
	}
}

// TestSettle_DiscardDestroysChild covers the discard verb: parent untouched,
// child discarded, owned worktree destroyed, sealed state still inspectable.
func TestSettle_DiscardDestroysChild(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("set:disc-parent", NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "set:disc-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "loser.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	out, err := mgr.Seal(ctx, child.ID())
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	if err := mgr.Settle(ctx, out.ID(), SettleDiscard); err != nil {
		t.Fatalf("SettleDiscard: %v", err)
	}
	if child.State() != ScopeDiscarded {
		t.Errorf("child state = %s, want discarded", child.State())
	}
	if _, statErr := os.Stat(wtPath); !os.IsNotExist(statErr) {
		t.Errorf("discarded child's worktree should be destroyed, stat err = %v", statErr)
	}
	if fileExists(t, repo, "loser.txt") {
		t.Error("discard must not touch the parent workspace")
	}
	// The sealed state is retained as a record.
	if out.State() != OutputDiscarded {
		t.Errorf("output state = %s, want discarded", out.State())
	}
}

// readRepoFile reads a file from the repo working tree, failing the test
// when it is missing.
func readRepoFile(t *testing.T, repo, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}
