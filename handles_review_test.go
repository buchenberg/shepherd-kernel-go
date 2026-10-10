package shepherd

// Review-hardening tests for the runtime facade (PR #20): the history
// vector replay — a Python-run tree driven through the Go facade must
// reproduce its owner path and effective history — plus the failure-mode
// behaviors the review found missing: a body that never runs observed
// exactly once, an async failure that surfaces instead of hanging, a
// mid-flight snapshot that observes running, and adopt/abandon input
// validation.

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

// TestHistoryVectorTreeReplay drives the vector's tree fixture through the
// GO facade — the same task bodies, the same run ids — and must reproduce
// the Python run's parent owner path fact-for-fact and the effective history
// the reference projected: the spawned and adopted children active, the
// abandoned child dropped, both published facts present.
func TestHistoryVectorTreeReplay(t *testing.T) {
	doc := loadExecutionVectors(t)
	vec := doc.HistorySequence
	if vec.ExecutionID != ExecutionIDFor(vec.RunID+":create", "execution") {
		t.Fatalf("vector execution id disagrees with the derivation: %s", vec.ExecutionID)
	}

	store := newMemStore(t)
	reg := NewRegistry()
	reg.Register(vec.TaskRefs["child"], func(control *TaskControl) (map[string]any, error) {
		return map[string]any{"leaf": true}, nil
	})
	reg.Register(vec.TaskRefs["parent"], func(control *TaskControl) (map[string]any, error) {
		if _, err := control.Publish(context.Background(), "phase", map[string]any{"at": "start"}); err != nil {
			return nil, err
		}
		kept, err := control.Spawn(context.Background(), vec.TaskRefs["child"], nil, reg)
		if err != nil {
			return nil, err
		}
		_ = kept
		dropped, err := control.Spawn(context.Background(), vec.TaskRefs["child"], nil, reg)
		if err != nil {
			return nil, err
		}
		if _, err := control.Abandon(context.Background(), dropped); err != nil {
			return nil, err
		}
		external, err := StartTaskSync(context.Background(), store, reg, vec.TaskRefs["child"], vec.RunID+":external", nil)
		if err != nil {
			return nil, err
		}
		if _, err := control.Adopt(context.Background(), external.ExecutionID(), external.FrontierID(), ""); err != nil {
			return nil, err
		}
		if _, err := control.Publish(context.Background(), "phase", map[string]any{"at": "end"}); err != nil {
			return nil, err
		}
		return map[string]any{"leaves": json.Number("2")}, nil
	})

	run, err := StartTaskSync(context.Background(), store, reg, vec.TaskRefs["parent"], vec.RunID, nil)
	if err != nil {
		t.Fatalf("StartTaskSync: %v", err)
	}

	slice, err := store.ReadOwnerPrefix(context.Background(), reader, vec.ExecutionID, 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix: %v", err)
	}
	if len(slice.FactIDs()) != len(vec.ParentOwnerPath) {
		t.Fatalf("parent owner path has %d facts, want %d: %v", len(slice.FactIDs()), len(vec.ParentOwnerPath), slice.FactIDs())
	}
	for i, fact := range vec.ParentOwnerPath {
		if slice.FactIDs()[i] != fact.FactID {
			t.Errorf("parent path[%d] = %s, want %s (%s)", i, slice.FactIDs()[i], fact.FactID, fact.SchemaRef)
		}
	}

	cutoff, err := run.Cutoff(context.Background())
	if err != nil {
		t.Fatalf("Cutoff: %v", err)
	}
	history, err := ProjectEffectiveHistoryFromStore(context.Background(), store, reader, cutoff)
	if err != nil {
		t.Fatalf("ProjectEffectiveHistoryFromStore: %v", err)
	}

	if history.Root.Status != ExecutionStatus(vec.RootStatus) || history.Root.TaskRef != vec.RootTaskRef {
		t.Errorf("root = %s %s, want %s %s", history.Root.Status, history.Root.TaskRef, vec.RootStatus, vec.RootTaskRef)
	}
	if len(history.Children) != len(vec.Children) {
		t.Fatalf("effective children = %d, want %d", len(history.Children), len(vec.Children))
	}
	for i, want := range vec.Children {
		got := history.Children[i]
		if got.Execution.ExecutionID != want.ExecutionID ||
			got.Execution.TaskRef != want.TaskRef ||
			string(got.Execution.Status) != want.Status ||
			string(got.Relation.RelationKind) != want.Kind ||
			got.Relation.RelationID != want.RelationID ||
			got.Relation.ChildFrontierID != want.FrontierID {
			t.Errorf("child[%d] = (%s %s %s %s %s), want (%s %s %s %s %s)",
				i, got.Execution.ExecutionID, got.Execution.TaskRef, got.Execution.Status,
				got.Relation.RelationKind, got.Relation.RelationID,
				want.ExecutionID, want.TaskRef, want.Status, want.Kind, want.RelationID)
		}
	}
	if len(history.PublishedFacts) != len(vec.Published) {
		t.Fatalf("published facts = %d, want %d", len(history.PublishedFacts), len(vec.Published))
	}
	for i, kind := range vec.Published {
		if history.PublishedFacts[i].Kind != kind {
			t.Errorf("published[%d] = %s, want %s", i, history.PublishedFacts[i].Kind, kind)
		}
	}
}

// TestStartTaskSyncIdempotentRerunRunsBodyOnce pins the guard's actual
// behavior: a rerun whose terminal frontier already exists must NOT re-run
// the body — the body's external side effects are not idempotent even
// though the records are — against a durable store, closed and reopened.
func TestStartTaskSyncIdempotentRerunRunsBodyOnce(t *testing.T) {
	path := t.TempDir() + "/idempotent.sqlite"
	store, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var bodyRuns atomic.Int64
	reg := NewRegistry()
	reg.Register("SideEffectTask", func(control *TaskControl) (map[string]any, error) {
		bodyRuns.Add(1)
		return nil, nil
	})

	if _, err := StartTaskSync(context.Background(), store, reg, "SideEffectTask", "guard:run", nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close(context.Background())

	if _, err := StartTaskSync(context.Background(), reopened, reg, "SideEffectTask", "guard:run", nil); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if got := bodyRuns.Load(); got != 1 {
		t.Errorf("body ran %d times, want exactly 1 — the idempotency guard must skip the retained run", got)
	}
}

// TestStartTaskAsyncFailureSurfaces pins that a failed async body reaches
// Wait as an error instead of hanging: an unregistered task fails inside the
// body phase, records a failed execution, and Wait returns promptly.
func TestStartTaskAsyncFailureSurfaces(t *testing.T) {
	store := newMemStore(t)
	reg := NewRegistry()

	run, err := StartTask(context.Background(), store, reg, "NoSuchTask", "async:fail", nil)
	if err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exec, err := run.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v (a failed body must surface, not hang)", err)
	}
	// The lookup failure is a body failure: the run records failed and
	// publishes a terminal frontier, so Wait also projects it.
	if exec.Status != ExecutionFailed {
		t.Errorf("status = %s, want failed", exec.Status)
	}
	if exec.Error == "" {
		t.Error("failed run recorded no error text")
	}
}

// TestStartTaskSnapshotMidFlight pins Snapshot's contract against a live
// run: it returns the current fold — running — without waiting, and Wait
// still reaches the terminal state. Deterministic: the body signals when it
// has started and blocks until released.
func TestStartTaskSnapshotMidFlight(t *testing.T) {
	store := newMemStore(t)
	reg := NewRegistry()
	started := make(chan struct{})
	release := make(chan struct{})
	reg.Register("BlockingTask", func(control *TaskControl) (map[string]any, error) {
		close(started)
		<-release
		return map[string]any{"ok": true}, nil
	})

	run, err := StartTask(context.Background(), store, reg, "BlockingTask", "snap:run", nil)
	if err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	<-started

	snap, err := run.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot mid-flight: %v", err)
	}
	if snap.Status != ExecutionRunning {
		t.Errorf("mid-flight snapshot = %s, want running", snap.Status)
	}
	if snap.TaskRef != "BlockingTask" {
		t.Errorf("snapshot task ref = %s, want BlockingTask", snap.TaskRef)
	}

	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exec, err := run.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exec.Status != ExecutionSucceeded {
		t.Errorf("terminal status = %s, want succeeded", exec.Status)
	}
}

// TestAdoptRejectsInvalidFrontier pins Adopt's validation: an unknown
// frontier or one that targets a different execution is rejected before
// any relation is appended.
func TestAdoptRejectsInvalidFrontier(t *testing.T) {
	store := newMemStore(t)
	reg := NewRegistry()
	reg.Register("AdopterTask", func(control *TaskControl) (map[string]any, error) {
		if _, err := control.Adopt(context.Background(), "exec:someone-else", "frontier:does-not-exist", ""); err == nil {
			t.Error("Adopt accepted an unknown frontier")
		}
		return nil, nil
	})
	if _, err := StartTaskSync(context.Background(), store, reg, "AdopterTask", "adopt:bad", nil); err != nil {
		t.Fatalf("StartTaskSync: %v", err)
	}

	// A real frontier aimed at the wrong execution is also rejected. Build
	// one execution, then try to adopt its frontier under another id.
	reg.Register("AdopterTask2", func(control *TaskControl) (map[string]any, error) {
		other, err := StartTaskSync(context.Background(), store, reg, "Leaf", "adopt:other", nil)
		if err != nil {
			return nil, err
		}
		if _, err := control.Adopt(context.Background(), "exec:not-the-target", other.FrontierID(), ""); err == nil {
			t.Error("Adopt accepted a frontier that targets a different execution")
		}
		return nil, nil
	})
	reg.Register("Leaf", func(control *TaskControl) (map[string]any, error) { return nil, nil })
	if _, err := StartTaskSync(context.Background(), store, reg, "AdopterTask2", "adopt:mismatch", nil); err != nil {
		t.Fatalf("StartTaskSync: %v", err)
	}
}

// TestAbandonWithoutTerminalCutoffFails pins Abandon's single read: a child
// frontier that does not exist is an error, not an infinite poll.
func TestAbandonWithoutTerminalCutoffFails(t *testing.T) {
	store := newMemStore(t)
	handle := &ChildHandle{store: store, executionID: "exec:x", frontierID: "frontier:never", relationID: "rel:y"}
	control := newTaskControl(store, "exec:parent", "abandon:run", "")
	if _, err := control.Abandon(context.Background(), handle); err == nil {
		t.Fatal("Abandon of a child with no terminal cutoff succeeded, want a loud error")
	}
}
