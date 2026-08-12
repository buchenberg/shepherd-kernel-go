package shepherd

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScope_NewRootScope(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:root")

	if scope.ID() != "scope:sub:root" {
		t.Errorf("expected ID scope:sub:root, got %s", scope.ID())
	}
	if scope.OwnerID() != "sub:root" {
		t.Errorf("expected owner sub:root, got %s", scope.OwnerID())
	}
	if scope.State() != ScopeActive {
		t.Errorf("expected state active, got %s", scope.State())
	}
	if scope.Parent() != nil {
		t.Error("root scope should have no parent")
	}
	if scope.ForkPoint() != "" {
		t.Error("root scope should have no fork point")
	}
	if len(scope.Children()) != 0 {
		t.Error("root scope should have no children")
	}
}

func TestScope_Fork(t *testing.T) {
	store := newMemStore(t)
	parent := NewScope(store, "sub:parent")

	child, err := parent.Fork("sub:child", nil)
	if err != nil {
		t.Fatalf("fork failed: %v", err)
	}

	if child.State() != ScopeActive {
		t.Errorf("child should be active, got %s", child.State())
	}
	if child.Parent() != parent {
		t.Error("child's parent should be the forking scope")
	}
	if child.ForkPoint() == "" {
		t.Error("child should have a fork point record ID")
	}
	if child.OwnerID() != "sub:child" {
		t.Errorf("expected child owner sub:child, got %s", child.OwnerID())
	}

	// Parent should list the child
	children := parent.Children()
	if len(children) != 1 || children[0] != child {
		t.Error("parent should have exactly one child")
	}
}

func TestScope_ForkRecordsInTrace(t *testing.T) {
	store := newMemStore(t)
	parent := NewScope(store, "sub:trace-parent")
	child, err := parent.Fork("sub:trace-child", nil)
	if err != nil {
		t.Fatalf("fork failed: %v", err)
	}

	// Read the parent's trace — should have a scope:forked declaration
	slice, err := store.ReadOwnerPrefix(TrustedReadContext, "sub:trace-parent", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	found := false
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		env := rec.GetEnvelope()
		if env.SchemaRef == SchemaScopeForked && env.Mode == Declaration {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected scope:forked declaration in parent trace")
	}

	// Fork point should be a valid record ID
	fp := child.ForkPoint()
	if fp == "" {
		t.Fatal("child fork point should not be empty")
	}
	_, err = store.ReadFact(TrustedReadContext, fp)
	if err != nil {
		t.Errorf("fork point record should be readable: %v", err)
	}
}

func TestScope_Merge(t *testing.T) {
	store := newMemStore(t)
	parent := NewScope(store, "sub:merge-parent")
	child, err := parent.Fork("sub:merge-child", nil)
	if err != nil {
		t.Fatalf("fork failed: %v", err)
	}

	err = parent.Merge(child)
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}

	if child.State() != ScopeMerged {
		t.Errorf("child should be merged, got %s", child.State())
	}
	if parent.State() != ScopeActive {
		t.Errorf("parent should still be active, got %s", parent.State())
	}

	// Parent trace should have scope:merged capture
	slice, err := store.ReadOwnerPrefix(TrustedReadContext, "sub:merge-parent", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	found := false
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		env := rec.GetEnvelope()
		if env.SchemaRef == SchemaScopeMerged && env.Mode == Capture {
			found = true
			// Should cite the fork point as causal parent
			if len(env.CausedByIDs) == 0 {
				t.Error("merge record should cite fork point as causal parent")
			}
			break
		}
	}
	if !found {
		t.Error("expected scope:merged capture in parent trace")
	}
}

func TestScope_Discard(t *testing.T) {
	store := newMemStore(t)
	parent := NewScope(store, "sub:discard-parent")
	child, err := parent.Fork("sub:discard-child", nil)
	if err != nil {
		t.Fatalf("fork failed: %v", err)
	}

	err = parent.Discard(child)
	if err != nil {
		t.Fatalf("discard failed: %v", err)
	}

	if child.State() != ScopeDiscarded {
		t.Errorf("child should be discarded, got %s", child.State())
	}
	if parent.State() != ScopeActive {
		t.Errorf("parent should still be active, got %s", parent.State())
	}

	// Parent trace should have scope:discarded capture
	slice, err := store.ReadOwnerPrefix(TrustedReadContext, "sub:discard-parent", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	found := false
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		env := rec.GetEnvelope()
		if env.SchemaRef == SchemaScopeDiscarded && env.Mode == Capture {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected scope:discarded capture in parent trace")
	}
}

func TestScope_ForkThenDiscard_LeavesParentUnchanged(t *testing.T) {
	store := newMemStore(t)
	parent := NewScope(store, "sub:parent")
	child, _ := parent.Fork("sub:child", nil)

	// Record something in the child's trace
	_, err := store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: "child:work:1",
		Groups: []AppendGroup{{
			TraceOwnerID: "sub:child",
			FactDrafts: []RecordDraft{{
				Mode:      Declaration,
				SchemaRef: "yaah.tool.bash.v1",
				KindLabel: "bash",
				Payload:   map[string]any{"cmd": "rm -rf /"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("append to child failed: %v", err)
	}

	// Discard the child
	parent.Discard(child)

	// Parent trace should NOT have the child's tool call
	parentSlice, err := store.ReadOwnerPrefix(TrustedReadContext, "sub:parent", 99, ModeDeclarationsOnly)
	if err != nil {
		t.Fatalf("read parent trace: %v", err)
	}
	for _, id := range parentSlice.FactIDs() {
		rec := parentSlice.FactsByID[id]
		if rec.GetEnvelope().SchemaRef == "yaah.tool.bash.v1" {
			t.Error("discarded child's tool call should not appear in parent trace")
		}
	}

	// But the child's records should still be in the store (append-only)
	childSlice, err := store.ReadOwnerPrefix(TrustedReadContext, "sub:child", 99, ModeDeclarationsOnly)
	if err != nil {
		t.Fatalf("read child trace: %v", err)
	}
	if len(childSlice.FactIDs()) == 0 {
		t.Error("discarded child's records should still exist in store (append-only)")
	}
}

func TestScope_CannotForkNonActive(t *testing.T) {
	store := newMemStore(t)
	parent := NewScope(store, "sub:parent")
	child, _ := parent.Fork("sub:child", nil)
	parent.Discard(child)

	_, err := child.Fork("sub:grandchild", nil)
	if err == nil {
		t.Error("should not be able to fork a discarded scope")
	}
}

func TestScope_CannotMergeNonActive(t *testing.T) {
	store := newMemStore(t)
	parent := NewScope(store, "sub:parent")
	child, _ := parent.Fork("sub:child", nil)
	parent.Discard(child)

	err := parent.Merge(child)
	if err == nil {
		t.Error("should not be able to merge a discarded scope")
	}
}

func TestScope_CannotMergeWrongParent(t *testing.T) {
	store := newMemStore(t)
	parent1 := NewScope(store, "sub:p1")
	parent2 := NewScope(store, "sub:p2")
	child, _ := parent1.Fork("sub:child", nil)

	err := parent2.Merge(child)
	if err == nil {
		t.Error("should not be able to merge child into non-parent scope")
	}
}

func TestScope_MultipleChildren(t *testing.T) {
	store := newMemStore(t)
	parent := NewScope(store, "sub:parent")

	c1, _ := parent.Fork("sub:c1", nil)
	c2, _ := parent.Fork("sub:c2", nil)
	c3, _ := parent.Fork("sub:c3", nil)

	children := parent.Children()
	if len(children) != 3 {
		t.Fatalf("expected 3 children, got %d", len(children))
	}

	// Merge c1, discard c2, leave c3 active
	parent.Merge(c1)
	parent.Discard(c2)

	if c1.State() != ScopeMerged {
		t.Errorf("c1 should be merged, got %s", c1.State())
	}
	if c2.State() != ScopeDiscarded {
		t.Errorf("c2 should be discarded, got %s", c2.State())
	}
	if c3.State() != ScopeActive {
		t.Errorf("c3 should still be active, got %s", c3.State())
	}
}

func TestScope_NestedFork(t *testing.T) {
	store := newMemStore(t)
	root := NewScope(store, "sub:root")

	child, _ := root.Fork("sub:child", nil)
	grandchild, _ := child.Fork("sub:grandchild", nil)

	if grandchild.Parent() != child {
		t.Error("grandchild's parent should be child")
	}
	if child.Parent() != root {
		t.Error("child's parent should be root")
	}

	// Merge grandchild into child, then child into root
	child.Merge(grandchild)
	root.Merge(child)

	if grandchild.State() != ScopeMerged {
		t.Errorf("grandchild should be merged, got %s", grandchild.State())
	}
	if child.State() != ScopeMerged {
		t.Errorf("child should be merged, got %s", child.State())
	}
}

// --- ScopeManager tests ---

func TestScopeManager_Create(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)

	scope, err := mgr.Create("sub:managed")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if scope.OwnerID() != "sub:managed" {
		t.Errorf("expected owner sub:managed, got %s", scope.OwnerID())
	}

	// Duplicate should fail
	_, err = mgr.Create("sub:managed")
	if err == nil {
		t.Error("creating duplicate scope should fail")
	}
}

func TestScopeManager_Get(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)

	mgr.Create("sub:findme")

	found, ok := mgr.Get("scope:sub:findme")
	if !ok {
		t.Fatal("expected to find scope")
	}
	if found.OwnerID() != "sub:findme" {
		t.Errorf("expected owner sub:findme, got %s", found.OwnerID())
	}

	_, ok = mgr.Get("scope:nonexistent")
	if ok {
		t.Error("should not find nonexistent scope")
	}
}

func TestScopeManager_Fork(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	parent, _ := mgr.Create("sub:parent")

	child, err := mgr.Fork(parent.ID(), "sub:child", nil)
	if err != nil {
		t.Fatalf("fork failed: %v", err)
	}

	// Child should be registered
	found, ok := mgr.Get(child.ID())
	if !ok {
		t.Fatal("child should be registered with manager")
	}
	if found != child {
		t.Error("Get should return the same child scope")
	}
}

func TestScopeManager_ForkParentNotFound(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)

	_, err := mgr.Fork("scope:nonexistent", "sub:child", nil)
	if err == nil {
		t.Error("forking from nonexistent parent should fail")
	}
}

func TestScopeManager_Merge(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	parent, _ := mgr.Create("sub:parent")
	child, _ := mgr.Fork(parent.ID(), "sub:child", nil)

	err := mgr.Merge(child.ID())
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if child.State() != ScopeMerged {
		t.Errorf("child should be merged, got %s", child.State())
	}
}

func TestScopeManager_Discard(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	parent, _ := mgr.Create("sub:parent")
	child, _ := mgr.Fork(parent.ID(), "sub:child", nil)

	err := mgr.Discard(child.ID())
	if err != nil {
		t.Fatalf("discard failed: %v", err)
	}
	if child.State() != ScopeDiscarded {
		t.Errorf("child should be discarded, got %s", child.State())
	}
}

func TestScopeManager_ActiveScopes(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	p, _ := mgr.Create("sub:parent")
	c1, _ := mgr.Fork(p.ID(), "sub:c1", nil)
	c2, _ := mgr.Fork(p.ID(), "sub:c2", nil)
	mgr.Fork(p.ID(), "sub:c3", nil)

	mgr.Merge(c1.ID())
	mgr.Discard(c2.ID())

	active := mgr.ActiveScopes()
	// Should be: parent + c3 = 2 active (c1 merged, c2 discarded)
	if len(active) != 2 {
		t.Errorf("expected 2 active scopes, got %d", len(active))
	}
}

func TestScopeManager_MergeRootScopeFails(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	root, _ := mgr.Create("sub:root")

	err := mgr.Merge(root.ID())
	if err == nil {
		t.Error("merging a root scope should fail (no parent)")
	}
}

func TestScopeManager_ConcurrentFork(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	parent, _ := mgr.Create("sub:concurrent")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			ownerID := "sub:child:" + string(rune('a'+id))
			_, err := mgr.Fork(parent.ID(), ownerID, nil)
			if err != nil {
				t.Errorf("fork %d failed: %v", id, err)
			}
		}(i)
	}
	wg.Wait()

	if len(parent.Children()) != 10 {
		t.Errorf("expected 10 children, got %d", len(parent.Children()))
	}
}

// --- Snapshot tests ---

func TestScope_SnapshotNilForRoot(t *testing.T) {
	store := newMemStore(t)
	scope := NewScope(store, "sub:root")

	if scope.Snapshot() != nil {
		t.Error("root scope should have nil snapshot")
	}
}

func TestScope_SnapshotPreservedOnFork(t *testing.T) {
	store := newMemStore(t)
	parent := NewScope(store, "sub:parent")

	type fakeConversation struct {
		Messages []string
	}
	snapshot := &fakeConversation{Messages: []string{"hello", "world"}}

	child, err := parent.Fork("sub:child", snapshot)
	if err != nil {
		t.Fatalf("fork failed: %v", err)
	}

	got := child.Snapshot()
	if got == nil {
		t.Fatal("child snapshot should not be nil")
	}

	conv, ok := got.(*fakeConversation)
	if !ok {
		t.Fatal("snapshot should be *fakeConversation")
	}
	if len(conv.Messages) != 2 || conv.Messages[0] != "hello" {
		t.Errorf("unexpected snapshot content: %v", conv.Messages)
	}
}

func TestScope_SnapshotOpaquelyTyped(t *testing.T) {
	store := newMemStore(t)
	parent := NewScope(store, "sub:parent")

	// Snapshot can be anything — []byte, struct, slice, etc.
	byteSnapshot := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	child, _ := parent.Fork("sub:child", byteSnapshot)

	got, ok := child.Snapshot().([]byte)
	if !ok {
		t.Fatal("snapshot should be []byte")
	}
	if string(got) != string(byteSnapshot) {
		t.Errorf("snapshot content mismatch")
	}
}

func TestScope_SnapshotIndependent(t *testing.T) {
	store := newMemStore(t)
	parent := NewScope(store, "sub:parent")

	// Two forks from same parent with different snapshots
	snap1 := []string{"strategy-A"}
	snap2 := []string{"strategy-B"}

	c1, _ := parent.Fork("sub:c1", snap1)
	c2, _ := parent.Fork("sub:c2", snap2)

	got1 := c1.Snapshot().([]string)
	got2 := c2.Snapshot().([]string)

	if got1[0] != "strategy-A" {
		t.Errorf("c1 snapshot should be strategy-A, got %s", got1[0])
	}
	if got2[0] != "strategy-B" {
		t.Errorf("c2 snapshot should be strategy-B, got %s", got2[0])
	}
}

func TestScope_SnapshotNestedFork(t *testing.T) {
	store := newMemStore(t)
	root := NewScope(store, "sub:root")

	type state struct{ Turn int }
	child, _ := root.Fork("sub:child", &state{Turn: 5})
	grandchild, _ := child.Fork("sub:grandchild", &state{Turn: 10})

	if child.Snapshot().(*state).Turn != 5 {
		t.Errorf("child snapshot turn should be 5")
	}
	if grandchild.Snapshot().(*state).Turn != 10 {
		t.Errorf("grandchild snapshot turn should be 10")
	}
}

// --- Phase 3: ScopeManager checkpoint tests ---

func TestScopeManager_CreateCheckpoint(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	scope, _ := mgr.Create("sub:cp-managed")
	repo := newTestRepo(t)

	cp, err := mgr.CreateCheckpoint(scope.ID(), repo, []byte("snapshot"))
	if err != nil {
		t.Fatalf("CreateCheckpoint: %v", err)
	}
	if cp.ScopeID != scope.ID() {
		t.Errorf("expected scope %s, got %s", scope.ID(), cp.ScopeID)
	}
}

func TestScopeManager_RestoreCheckpoint(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	scope, _ := mgr.Create("sub:cp-restore")
	repo := newTestRepo(t)

	writeFile(t, repo, "main.go", "original")
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-m", "add main")

	cp, _ := mgr.CreateCheckpoint(scope.ID(), repo, []byte("conversation"))

	// Make changes
	writeFile(t, repo, "main.go", "CHANGED")

	// Restore
	snapshot, err := mgr.RestoreCheckpoint(cp.ID)
	if err != nil {
		t.Fatalf("RestoreCheckpoint: %v", err)
	}
	if string(snapshot) != "conversation" {
		t.Errorf("expected 'conversation', got %q", snapshot)
	}
	got := readFile(t, repo, "main.go")
	if strings.TrimSpace(got) != "original" {
		t.Errorf("expected 'original', got %q", got)
	}
}

func TestScopeManager_LatestCheckpoint(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)
	scope, _ := mgr.Create("sub:cp-latest")
	repo := newTestRepo(t)

	cp1, _ := mgr.CreateCheckpoint(scope.ID(), repo, nil)

	// Small delay so timestamps differ
	time.Sleep(10 * time.Millisecond)

	cp2, _ := mgr.CreateCheckpoint(scope.ID(), repo, nil)

	latest := mgr.LatestCheckpoint(scope.ID())
	if latest == nil {
		t.Fatal("expected non-nil latest checkpoint")
	}
	if latest.ID != cp2.ID {
		t.Errorf("expected cp2 (%s), got %s", cp2.ID, latest.ID)
	}
	_ = cp1 // suppress unused
}

func TestScopeManager_CheckpointScopeNotFound(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store)

	_, err := mgr.CreateCheckpoint("scope:nonexistent", ".", nil)
	if err == nil {
		t.Error("CreateCheckpoint on nonexistent scope should fail")
	}
}
