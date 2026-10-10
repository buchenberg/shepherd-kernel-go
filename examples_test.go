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

// The three workspace-handle recipes from the reference (plan 04 §3), as
// executable documentation. They must pass on Windows (git worktree carrier)
// too — no OS assumptions beyond the git binary.

// TestExample_BestOfN: fork N isolated worktree scopes from one repo, each
// does work, seal all, inspect, select exactly one, discard the rest, and
// assert the parent ends up with only the winner's work.
func TestExample_BestOfN(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("bestofn", NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	const attempts = 3
	outs := make([]*RetainedOutput, 0, attempts)
	var children []*Scope
	for i := 0; i < attempts; i++ {
		child, wtPath := forkWorktreeChild(t, mgr, parent.ID(),
			fmt.Sprintf("bestofn:c%d", i), repo, fmt.Sprintf("wt-%d", i))
		children = append(children, child)

		// "Does work": each attempt writes a distinct result file.
		if err := os.WriteFile(filepath.Join(wtPath, fmt.Sprintf("result-%d.txt", i)),
			[]byte(fmt.Sprintf("attempt %d", i)), 0o644); err != nil {
			t.Fatalf("attempt %d writes: %v", i, err)
		}

		out, err := mgr.Seal(ctx, child.ID())
		if err != nil {
			t.Fatalf("seal attempt %d: %v", i, err)
		}
		// The supervisor inspects each sealed output's frozen changes.
		if len(out.Changes()) != 1 || out.Changes()[0].Path != fmt.Sprintf("result-%d.txt", i) {
			t.Fatalf("attempt %d sealed changes: %+v, want exactly its result file", i, out.Changes())
		}
		outs = append(outs, out)
	}

	// Choose attempt 1: select its output, discard the losers.
	if err := mgr.Settle(ctx, outs[1].ID(), SettleSelect); err != nil {
		t.Fatalf("select winner: %v", err)
	}
	for i, out := range outs {
		if i == 1 {
			continue
		}
		if err := mgr.Settle(ctx, out.ID(), SettleDiscard); err != nil {
			t.Fatalf("discard loser %d: %v", i, err)
		}
	}

	// The parent workspace contains only the winner.
	if got := readRepoFile(t, repo, "result-1.txt"); got != "attempt 1" {
		t.Errorf("result-1.txt = %q, want the winner's content", got)
	}
	for _, i := range []int{0, 2} {
		if fileExists(t, repo, fmt.Sprintf("result-%d.txt", i)) {
			t.Errorf("loser %d leaked into the parent workspace", i)
		}
		if children[i].State() != ScopeDiscarded {
			t.Errorf("loser %d state = %s, want discarded", i, children[i].State())
		}
	}
	if children[1].State() != ScopeMerged {
		t.Errorf("winner state = %s, want merged", children[1].State())
	}

	// Consume-once: the winner cannot be settled again.
	if err := mgr.Settle(ctx, outs[1].ID(), SettleSelect); !errors.Is(err, ErrOutputConsumed) {
		t.Errorf("second settle of winner: %v, want ErrOutputConsumed", err)
	}
}

// TestExample_RetryUntilAcceptable: loop fork → work → checkpoint →
// CommitMerge with a drafts-only reviewer → deny → discard → restore → retry
// with injected guidance, until approved. CheckCall's pre-tool deny and the
// merge gate's commit deny appear in one flow.
func TestExample_RetryUntilAcceptable(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("retry", NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// The supervision surface: DestructiveToolGuard denies calls before
	// execution; DraftsOnlyReviewer denies merges at commit time.
	sup := NewSupervisor(mgr, nil)
	sup.AddRule(DestructiveToolGuard())
	reviewer := DraftsOnlyReviewer("out")

	const maxAttempts = 3
	denied := 0
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Checkpoint before the attempt: a denial's rollback is the caller's
		// restore of the shared workspace.
		cp, err := mgr.CreateCheckpoint(ctx, parent.ID(), []byte("conversation"))
		if err != nil {
			t.Fatalf("attempt %d checkpoint: %v", attempt, err)
		}

		// The "model" first tries something destructive; the supervisor's
		// pre-execution guard denies it, and the model complies.
		badCall := EffectEvent{
			TraceOwnerID: fmt.Sprintf("retry:%d", attempt),
			Mode:         Declaration,
			SchemaRef:    "yaah.tool.bash.v1",
			KindLabel:    "bash",
			Payload:      map[string]any{"cmd": "rm -rf ."},
		}
		if iv := sup.CheckCall(badCall); iv == nil || iv.Type != InterventionDeny {
			t.Fatalf("attempt %d: destructive call not denied pre-execution (iv=%+v)", attempt, iv)
		}

		// Fork a child that shares the parent's sandbox, carrying guidance
		// from the previous denial in its snapshot (the conversation state).
		guidance := "write your result under out/"
		if attempt == 1 {
			guidance = "do the task"
		}
		child, err := mgr.Fork(parent.ID(), fmt.Sprintf("retry:%d", attempt), guidance)
		if err != nil {
			t.Fatalf("attempt %d fork: %v", attempt, err)
		}

		// The child works directly in the shared tree. Early attempts write
		// outside out/; the last one complies with the guidance.
		target := "scratch.txt"
		if attempt == maxAttempts {
			target = "out/final.txt"
			if err := os.MkdirAll(filepath.Join(repo, "out"), 0o755); err != nil {
				t.Fatalf("mkdir out: %v", err)
			}
		}
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(target)),
			[]byte(fmt.Sprintf("attempt %d", attempt)), 0o644); err != nil {
			t.Fatalf("attempt %d write: %v", attempt, err)
		}

		lastErr = mgr.CommitMerge(ctx, child.ID(), reviewer)
		var deniedErr *SupervisorDeniedError
		if errors.As(lastErr, &deniedErr) {
			denied++
			// Deny → child discarded (CommitMerge does it); the caller
			// restores the shared workspace from the checkpoint.
			if _, err := mgr.RestoreCheckpoint(ctx, cp.ID); err != nil {
				t.Fatalf("attempt %d restore: %v", attempt, err)
			}
			if fileExists(t, repo, filepath.FromSlash(target)) {
				t.Errorf("attempt %d: denied work survived the restore", attempt)
			}
			continue
		}
		if lastErr != nil {
			t.Fatalf("attempt %d CommitMerge: %v", attempt, lastErr)
		}
		break // approved and merged
	}

	if denied != maxAttempts-1 {
		t.Errorf("denials = %d, want %d", denied, maxAttempts-1)
	}
	if lastErr != nil {
		t.Fatalf("final attempt should have been approved, got: %v", lastErr)
	}
	if got := readRepoFile(t, repo, "out/final.txt"); got != fmt.Sprintf("attempt %d", maxAttempts) {
		t.Errorf("out/final.txt = %q, want the approved attempt's content", got)
	}
	if fileExists(t, repo, "scratch.txt") {
		t.Error("denied attempts' work must be gone from the parent tree")
	}

	// The trace carries the whole story: one denied decision per failed
	// attempt, one approval.
	decisions := findRecords(t, store, "retry", SchemaSupervisorDecision)
	if len(decisions) != maxAttempts {
		t.Fatalf("decision records: %d, want %d", len(decisions), maxAttempts)
	}
	approved, refused := 0, 0
	for _, d := range decisions {
		if fmt.Sprint(d.Body.Payload["approved"]) == "true" {
			approved++
		} else {
			refused++
		}
	}
	if approved != 1 || refused != maxAttempts-1 {
		t.Errorf("decisions approved/refused = %d/%d, want 1/%d", approved, refused, maxAttempts-1)
	}
}

// TestExample_ApplyOntoMovedWorkspace: seal a child output; the parent
// advances on disjoint paths and SettleApply succeeds; a second output
// overlapping the parent's concurrent change is refused with
// ErrApplyConflict and stays unconsumed.
func TestExample_ApplyOntoMovedWorkspace(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, err := mgr.Create("moved", NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	// Child 1's output: a feature file, disjoint from everything.
	child1, wt1 := forkWorktreeChild(t, mgr, parent.ID(), "moved:c1", repo, "wt1")
	if err := os.WriteFile(filepath.Join(wt1, "feature.txt"), []byte("feature"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	out1, err := mgr.Seal(ctx, child1.ID())
	if err != nil {
		t.Fatalf("seal child 1: %v", err)
	}

	// Child 2's output: it will touch shared.txt, the same path the parent
	// is about to move.
	child2, wt2 := forkWorktreeChild(t, mgr, parent.ID(), "moved:c2", repo, "wt2")
	if err := os.WriteFile(filepath.Join(wt2, "shared.txt"), []byte("child's edit"), 0o644); err != nil {
		t.Fatalf("write shared: %v", err)
	}
	out2, err := mgr.Seal(ctx, child2.ID())
	if err != nil {
		t.Fatalf("seal child 2: %v", err)
	}

	// Meanwhile the parent advances: one disjoint path (parent.txt) and one
	// that overlaps child 2 (shared.txt).
	if err := os.WriteFile(filepath.Join(repo, "parent.txt"), []byte("parent's own"), 0o644); err != nil {
		t.Fatalf("write parent.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("parent's edit"), 0o644); err != nil {
		t.Fatalf("write shared.txt: %v", err)
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "parent advances")

	// Disjoint output applies onto the moved workspace.
	if err := mgr.Settle(ctx, out1.ID(), SettleApply); err != nil {
		t.Fatalf("apply disjoint output onto moved parent: %v", err)
	}
	if got := readRepoFile(t, repo, "feature.txt"); got != "feature" {
		t.Errorf("feature.txt = %q, want the child's content", got)
	}
	if got := readRepoFile(t, repo, "parent.txt"); got != "parent's own" {
		t.Errorf("parent.txt = %q, want the parent's own work preserved", got)
	}

	// Overlapping output is refused without consuming.
	err = mgr.Settle(ctx, out2.ID(), SettleApply)
	if !errors.Is(err, ErrApplyConflict) {
		t.Fatalf("overlapping apply: %v, want ErrApplyConflict", err)
	}
	if !strings.Contains(err.Error(), "shared.txt") {
		t.Errorf("conflict text = %q, want it to name shared.txt", err.Error())
	}
	if out2.State() != OutputUnconsumed {
		t.Errorf("conflicting output state = %s, want unconsumed", out2.State())
	}
	if got := readRepoFile(t, repo, "shared.txt"); got != "parent's edit" {
		t.Errorf("shared.txt = %q, want the parent's version untouched by the refusal", got)
	}

	// Clean up the refused output's scope.
	if err := mgr.Settle(ctx, out2.ID(), SettleDiscard); err != nil {
		t.Fatalf("discard refused output: %v", err)
	}
}
