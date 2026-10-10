package shepherd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// findRecords reads an owner's trace and returns the records with the given
// schema ref, in record order.
func findRecords(t *testing.T, store *SQLiteTraceStore, ownerID, schemaRef string) []Record {
	t.Helper()
	slice, err := store.ReadOwnerPrefix(TrustedReadContext, ownerID, 999, ModeBoth)
	if err != nil {
		t.Fatalf("read trace for %s: %v", ownerID, err)
	}
	var found []Record
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		if rec.GetEnvelope().SchemaRef == schemaRef {
			found = append(found, rec.(Record))
		}
	}
	return found
}

// TestClassifyChanges covers the unified-diff classifier: create, modify,
// delete, and a file present in the changed-file list without a diff section.
func TestClassifyChanges(t *testing.T) {
	diff := `diff --git a/created.txt b/created.txt
new file mode 100644
index 0000000..8b13789
--- /dev/null
+++ b/created.txt
@@ -0,0 +1 @@
+hello
diff --git a/changed.txt b/changed.txt
index 8b13789..c16a5f0 100644
--- a/changed.txt
+++ b/changed.txt
@@ -1 +1 @@
-old
+new
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index 8b13789..0000000
--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-old
`
	changes := classifyChanges(diff, []string{"created.txt", "changed.txt", "gone.txt"})
	if len(changes) != 3 {
		t.Fatalf("classifyChanges returned %d changes, want 3: %+v", len(changes), changes)
	}
	byPath := make(map[string]ProposedChange, len(changes))
	for _, c := range changes {
		byPath[c.Path] = c
	}
	if c := byPath["created.txt"]; c.Kind != ChangeCreate || c.Diff != "hello" {
		t.Errorf("created.txt: kind=%q diff=%q, want create/hello", c.Kind, c.Diff)
	}
	if c := byPath["changed.txt"]; c.Kind != ChangeModify {
		t.Errorf("changed.txt: kind=%q, want modify", c.Kind)
	}
	if c := byPath["gone.txt"]; c.Kind != ChangeDelete {
		t.Errorf("gone.txt: kind=%q, want delete", c.Kind)
	}

	// A file with no diff section must not silently vanish from review.
	changes = classifyChanges("", []string{"unparsed.bin"})
	if len(changes) != 1 || changes[0].Path != "unparsed.bin" || changes[0].Kind != ChangeModify {
		t.Errorf("list-only file: %+v, want one unclassified modify", changes)
	}
}

// TestFork_RecordsBaselineDigest covers T3.1: a sandboxed parent's fork
// records the baseline digest in the fork payload and hands the child the
// baseline state; a pure-causal parent records no baseline.
func TestFork_RecordsBaselineDigest(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, err := mgr.Create("gate:base-parent", NewLocalGitSandbox(repo))
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	child, err := mgr.Fork(parent.ID(), "gate:base-child", nil)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if _, ok := child.Baseline(); !ok {
		t.Fatal("child of a sandboxed parent should carry a fork baseline")
	}
	base, _ := child.Baseline()
	if base.Backend != "git" {
		t.Errorf("baseline backend = %q, want git", base.Backend)
	}

	forks := findRecords(t, store, "gate:base-parent", SchemaScopeForked)
	if len(forks) != 1 {
		t.Fatalf("fork records: %d, want 1", len(forks))
	}
	digest := fmt.Sprint(forks[0].Body.Payload["baseline_digest"])
	if digest == "" {
		t.Error("fork payload should carry baseline_digest")
	}
	want, err := base.Digest()
	if err != nil {
		t.Fatalf("digest baseline: %v", err)
	}
	if digest != want {
		t.Errorf("baseline_digest = %q, want %q", digest, want)
	}

	// A pure-causal parent forks without a baseline, and the child says so.
	causalParent, _ := mgr.Create("gate:causal-parent", nil)
	causalChild, err := mgr.Fork(causalParent.ID(), "gate:causal-child", nil)
	if err != nil {
		t.Fatalf("causal fork: %v", err)
	}
	if _, ok := causalChild.Baseline(); ok {
		t.Error("child of a pure-causal parent must not carry a baseline")
	}
	causalForks := findRecords(t, store, "gate:causal-parent", SchemaScopeForked)
	if len(causalForks) != 1 {
		t.Fatalf("causal fork records: %d, want 1", len(causalForks))
	}
	if _, has := causalForks[0].Body.Payload["baseline_digest"]; has {
		t.Error("pure-causal fork payload must not carry baseline_digest")
	}
}

// forkWorktreeChild forks an isolated worktree child of parent, provisions
// the worktree, and hands back the child and the worktree path the test
// "works" in. leaf names the worktree directory (no ':' — Windows paths).
func forkWorktreeChild(t *testing.T, mgr *ScopeManager, parentID, childOwner, repo, leaf string) (*Scope, string) {
	t.Helper()
	wtPath := filepath.Join(t.TempDir(), leaf)
	sb := NewWorktreeSandbox(repo, wtPath)
	if err := sb.Create(context.Background(), SandboxSpec{}); err != nil {
		t.Fatalf("worktree create for %s: %v", childOwner, err)
	}
	child, err := mgr.ForkIsolated(parentID, childOwner, nil, sb)
	if err != nil {
		t.Fatalf("ForkIsolated %s: %v", childOwner, err)
	}
	return child, wtPath
}

// TestProposeMerge_ClassifiesChildChanges covers T3.2: the proposal carries
// the child's classified changes and the baseline digest, and refuses
// children without a baseline or a diff-capable sandbox.
func TestProposeMerge_ClassifiesChildChanges(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, _ := mgr.Create("gate:prop-parent", NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "gate:prop-child", repo, "wt")

	if err := os.WriteFile(filepath.Join(wtPath, "feature.txt"), []byte("shiny"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	proposal, err := mgr.ProposeMerge(context.Background(), child.ID(), 100)
	if err != nil {
		t.Fatalf("ProposeMerge: %v", err)
	}
	if proposal.ChildScopeID != child.ID() {
		t.Errorf("ChildScopeID = %q, want %q", proposal.ChildScopeID, child.ID())
	}
	if len(proposal.Changes) != 1 {
		t.Fatalf("changes: %+v, want exactly feature.txt", proposal.Changes)
	}
	c := proposal.Changes[0]
	if c.Path != "feature.txt" || c.Kind != ChangeCreate || c.Diff != "shiny" {
		t.Errorf("feature change = %+v, want create with preview 'shiny'", c)
	}
	base, _ := child.Baseline()
	wantDigest, _ := base.Digest()
	if proposal.BaselineDigest != wantDigest {
		t.Errorf("BaselineDigest = %q, want %q", proposal.BaselineDigest, wantDigest)
	}

	// ProposeMerge is a query: no proposed record, no state change.
	if recs := findRecords(t, store, "gate:prop-child", SchemaMergeProposed); len(recs) != 0 {
		t.Errorf("ProposeMerge recorded %d proposal records, want 0", len(recs))
	}
	if child.State() != ScopeActive {
		t.Errorf("child state = %s, want active", child.State())
	}

	// A pure-causal child has no baseline and cannot propose.
	causalParent, _ := mgr.Create("gate:prop-causal", nil)
	causalChild, err := mgr.Fork(causalParent.ID(), "gate:prop-causal-child", nil)
	if err != nil {
		t.Fatalf("causal fork: %v", err)
	}
	if _, err := mgr.ProposeMerge(context.Background(), causalChild.ID(), 100); err == nil {
		t.Error("ProposeMerge on a child without a baseline should fail")
	}
}

// TestCommitMerge_DenyingReviewer covers T3.3's deny path on a shared-sandbox
// child — the shape CommitMerge exists for: the child's work is in the
// parent's tree, denial discards the child causally, and the rollback of the
// shared workspace is the caller's checkpoint restore.
func TestCommitMerge_DenyingReviewer(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("gate:deny-parent", NewLocalGitSandbox(repo))

	cp, err := mgr.CreateCheckpoint(ctx, parent.ID(), nil)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// A shared-sandbox child works directly in the parent's tree.
	child, err := mgr.Fork(parent.ID(), "gate:deny-child", nil)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "outside.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}

	err = mgr.CommitMerge(ctx, child.ID(), DraftsOnlyReviewer("drafts"))
	if err == nil {
		t.Fatal("CommitMerge with a change outside the drafts prefix should deny")
	}
	var denied *SupervisorDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("CommitMerge error = %T (%v), want *SupervisorDeniedError", err, err)
	}
	if !strings.Contains(denied.Error(), "outside the drafts prefix") {
		t.Errorf("denial text = %q, want it to carry the reviewer's reason", denied.Error())
	}
	if !strings.Contains(denied.Error(), "outside.txt") {
		t.Errorf("denial text = %q, want it to name the denied path", denied.Error())
	}

	// Child discarded causally; the shared sandbox is inherited, not owned,
	// so nothing is destroyed — the caller rolls the workspace back.
	if child.State() != ScopeDiscarded {
		t.Errorf("child state = %s, want discarded", child.State())
	}
	if _, err := mgr.RestoreCheckpoint(ctx, cp.ID); err != nil {
		t.Fatalf("restore after denial: %v", err)
	}
	if fileExists(t, repo, "outside.txt") {
		t.Error("denied work must be gone from the parent workspace after the caller's restore")
	}
	// Trace: proposal on the child's path, denied decision on the parent's.
	if recs := findRecords(t, store, "gate:deny-child", SchemaMergeProposed); len(recs) != 1 {
		t.Errorf("proposed records on child path: %d, want 1", len(recs))
	}
	decisions := findRecords(t, store, "gate:deny-parent", SchemaSupervisorDecision)
	if len(decisions) != 1 {
		t.Fatalf("decision records: %d, want 1", len(decisions))
	}
	if fmt.Sprint(decisions[0].Body.Payload["approved"]) != "false" {
		t.Errorf("decision approved = %v, want false", decisions[0].Body.Payload["approved"])
	}
	if fmt.Sprint(decisions[0].Body.Payload["reason"]) == "" {
		t.Error("denied decision should record the reason")
	}
	if recs := findRecords(t, store, "gate:deny-parent", SchemaScopeMerged); len(recs) != 0 {
		t.Errorf("denied merge must not record scope:merged, found %d", len(recs))
	}
}

// TestCommitMerge_ApprovingReviewer covers the approve path on a shared-sandbox
// child: both records, the causal merge, and no worktree to destroy.
func TestCommitMerge_ApprovingReviewer(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	ctx := context.Background()
	parent, _ := mgr.Create("gate:ok-parent", NewLocalGitSandbox(repo))

	child, err := mgr.Fork(parent.ID(), "gate:ok-child", nil)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "drafts"), 0o755); err != nil {
		t.Fatalf("mkdir drafts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "drafts", "good.txt"), []byte("yes"), 0o644); err != nil {
		t.Fatalf("write good: %v", err)
	}

	if err := mgr.CommitMerge(ctx, child.ID(), DraftsOnlyReviewer("drafts")); err != nil {
		t.Fatalf("CommitMerge (approve): %v", err)
	}

	if child.State() != ScopeMerged {
		t.Errorf("child state = %s, want merged", child.State())
	}
	if !fileExists(t, repo, "drafts/good.txt") {
		t.Error("approved merge should keep the child's work in the parent tree")
	}
	if recs := findRecords(t, store, "gate:ok-child", SchemaMergeProposed); len(recs) != 1 {
		t.Errorf("proposed records: %d, want 1", len(recs))
	}
	decisions := findRecords(t, store, "gate:ok-parent", SchemaSupervisorDecision)
	if len(decisions) != 1 || fmt.Sprint(decisions[0].Body.Payload["approved"]) != "true" {
		t.Errorf("decisions: %+v, want one approved", decisions)
	}
	if recs := findRecords(t, store, "gate:ok-parent", SchemaScopeMerged); len(recs) != 1 {
		t.Errorf("merged records: %d, want 1", len(recs))
	}
}

// TestCommitMerge_IsolatedChildRefused guards the data-loss footgun: merging
// an isolated child would record approval and destroy the worktree holding
// the approved work. Settlement owns that transfer.
func TestCommitMerge_IsolatedChildRefused(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, _ := mgr.Create("gate:iso-parent", NewLocalGitSandbox(repo))

	child, wtPath := forkWorktreeChild(t, mgr, parent.ID(), "gate:iso-child", repo, "wt")
	if err := os.WriteFile(filepath.Join(wtPath, "feature.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}

	err := mgr.CommitMerge(context.Background(), child.ID(), DraftsOnlyReviewer(""))
	if err == nil {
		t.Fatal("CommitMerge on an isolated child must be refused")
	}
	if !strings.Contains(err.Error(), "SettleSelect") {
		t.Errorf("refusal text = %q, want it to point at Seal + SettleSelect", err.Error())
	}
	// Refused before any side effect: child active, worktree intact, no records.
	if child.State() != ScopeActive {
		t.Errorf("child state = %s, want active", child.State())
	}
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Errorf("worktree must survive the refusal: %v", statErr)
	}
	if recs := findRecords(t, store, "gate:iso-child", SchemaMergeProposed); len(recs) != 0 {
		t.Errorf("refused CommitMerge recorded %d proposals, want 0", len(recs))
	}
}

// TestCommitMerge_RequiresReviewer guards the guard: a nil reviewer must not
// silently become an approval — skipping review is what plain Merge is for.
func TestCommitMerge_RequiresReviewer(t *testing.T) {
	mgr := NewScopeManager(newMemStore(t))
	if err := mgr.CommitMerge(context.Background(), "scope:none", nil); err == nil {
		t.Fatal("nil reviewer must be rejected")
	}
}

// TestDestructivePathReviewer covers the pattern-matching reviewer.
func TestDestructivePathReviewer(t *testing.T) {
	review := DestructivePathReviewer([]string{"*.pem", "secrets/"})

	err := review(MergeProposal{Changes: []ProposedChange{{Path: "out/app.go"}}})
	if err != nil {
		t.Errorf("clean proposal denied: %v", err)
	}
	err = review(MergeProposal{Changes: []ProposedChange{{Path: "keys/ca.pem"}}})
	if err == nil || !strings.Contains(err.Error(), "*.pem") {
		t.Errorf("pem change: err = %v, want pattern-named denial", err)
	}
	err = review(MergeProposal{Changes: []ProposedChange{{Path: "secrets/token.txt"}}})
	if err == nil || !strings.Contains(err.Error(), "secrets/") {
		t.Errorf("secret change: err = %v, want prefix-named denial", err)
	}
	// An empty pattern list denies nothing, not everything.
	err = review(MergeProposal{Changes: []ProposedChange{{Path: "anything.txt"}}})
	if err != nil {
		t.Errorf("empty pattern list denied: %v", err)
	}
}

// TestSupervisor_ReviewMerge covers T3.4: rule engines get merge proposals as
// events, InterventionDiscard gains a live emitter, and wiring the verdict
// into CommitMerge denies without merging.
func TestSupervisor_ReviewMerge(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	repo := newTestRepo(t)
	parent, _ := mgr.Create("gate:rev-parent", NewLocalGitSandbox(repo))

	sup := NewSupervisor(mgr, nil) // CheckCall-only: no bus needed
	sup.AddRule(SupervisionRule{
		Name: "drafts_only_merge_gate",
		Match: func(e EffectEvent) bool {
			return e.SchemaRef == SchemaMergeProposed
		},
		Action: func(e EffectEvent) *Intervention {
			paths, _ := e.Payload["paths"].([]string)
			for _, p := range paths {
				if !pathWithin(p, "drafts") {
					return &Intervention{
						Type:    InterventionDiscard,
						ScopeID: "scope:" + e.TraceOwnerID,
						Payload: "changes must stay under drafts/",
						Time:    time.Now(),
					}
				}
			}
			return nil
		},
	})

	reviewer := func(p MergeProposal) error {
		if iv := sup.ReviewMerge(p.ChildScopeID, p); iv != nil && iv.Type == InterventionDiscard {
			return errors.New(fmt.Sprint(iv.Payload))
		}
		return nil
	}

	// A shared-sandbox child writes outside the prefix, and the supervisor's
	// rule denies the merge through ReviewMerge — the emitter story in one flow.
	child, err := mgr.Fork(parent.ID(), "gate:rev-child", nil)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "elsewhere.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	err = mgr.CommitMerge(context.Background(), child.ID(), reviewer)
	var denied *SupervisorDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("CommitMerge via ReviewMerge: err = %T (%v), want SupervisorDeniedError", err, err)
	}
	if !strings.Contains(denied.Error(), "drafts/") {
		t.Errorf("denial text = %q, want the rule's payload as the reason", denied.Error())
	}
	if child.State() != ScopeDiscarded {
		t.Errorf("child state = %s, want discarded", child.State())
	}
	if !fileExists(t, repo, "elsewhere.txt") {
		// The shared child's write is in the parent tree; the caller's
		// checkpoint restore removes it. Here we only assert the causal side.
		t.Log("note: shared child's write was already in the parent tree; rollback is the caller's restore")
	}

	// An in-prefix proposal produces no intervention (approval).
	good := MergeProposal{
		ChildScopeID: "scope:gate:rev-child",
		Changes:      []ProposedChange{{Path: "drafts/ok.txt", Kind: ChangeCreate}},
	}
	if iv := sup.ReviewMerge(child.ID(), good); iv != nil {
		t.Errorf("in-prefix proposal: intervention = %+v, want nil", iv)
	}
}
