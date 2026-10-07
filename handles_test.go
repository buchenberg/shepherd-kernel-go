package shepherd

// Runtime handle tests (plan 02 §5). The acceptance anchor is
// TestStartTaskSyncMatchesPythonVector: the same fixture task run through
// the Go facade must allocate the identical record ids the Python @task run
// recorded in testdata/execution_vectors_v0.json.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestStartTaskSyncMatchesPythonVector is plan 02 §7's acceptance criterion:
// StartTaskSync's record trace is ID-identical to the Python @task run of
// the same fixture task. The registry key carries the task_ref the Python
// runtime derived from the class, and the body publishes and completes with
// the same payloads.
func TestStartTaskSyncMatchesPythonVector(t *testing.T) {
	doc := loadExecutionVectors(t)
	vec := doc.RunSequence

	reg := NewRegistry()
	reg.Register(vec.TaskRef, func(control *TaskControl) (map[string]any, error) {
		if _, err := control.Publish("note", map[string]any{"msg": "hello <>&", "n": json.Number("1.5")}); err != nil {
			return nil, err
		}
		return map[string]any{"answer": json.Number("42"), "label": "ok"}, nil
	})

	store := newMemStore(t)
	run, err := StartTaskSync(store, reg, vec.TaskRef, vec.RunID, vec.Inputs)
	if err != nil {
		t.Fatalf("StartTaskSync: %v", err)
	}
	if run.ExecutionID() != vec.ExecutionID {
		t.Errorf("execution id = %s, want %s", run.ExecutionID(), vec.ExecutionID)
	}

	slice, err := store.ReadOwnerPrefix(reader, vec.ExecutionID, 99, ModeBoth)
	if err != nil {
		t.Fatalf("ReadOwnerPrefix: %v", err)
	}
	if len(slice.FactIDs()) != len(vec.OwnerPath) {
		t.Fatalf("owner path = %v, want %v", slice.FactIDs(), vec.OwnerPath)
	}
	for i, fact := range vec.OwnerPath {
		if slice.FactIDs()[i] != fact.FactID {
			t.Errorf("owner path[%d] = %s, want %s (%s)", i, slice.FactIDs()[i], fact.FactID, fact.SchemaRef)
		}
	}

	cutoff, err := run.Cutoff(context.Background())
	if err != nil {
		t.Fatalf("Cutoff: %v", err)
	}
	want := vec.Cutoff
	if cutoff.FrontierID != want.FrontierID || cutoff.ThroughFactID != want.ThroughFactID ||
		cutoff.ThroughOwnerOrdinal != want.ThroughOwnerOrdinal || cutoff.CreatedByFactID != want.CreatedByFactID {
		t.Errorf("cutoff = %+v, want %+v", cutoff, want)
	}

	exec, err := run.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exec.Status != ExecutionSucceeded || exec.Outputs["label"] != "ok" {
		t.Errorf("waited execution = %s %v", exec.Status, exec.Outputs)
	}
}

// TestStartTaskSyncFailedRun pins the fail path: a returned error records a
// failed execution with the error text, and the terminal frontier is
// publishable (Wait projects failed).
func TestStartTaskSyncFailedRun(t *testing.T) {
	reg := NewRegistry()
	reg.Register("BadTask", func(control *TaskControl) (map[string]any, error) {
		return nil, errTaskBoom
	})
	store := newMemStore(t)

	run, err := StartTaskSync(store, reg, "BadTask", "fail:run", nil)
	if err != nil {
		t.Fatalf("StartTaskSync: %v", err)
	}
	exec, err := run.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exec.Status != ExecutionFailed {
		t.Errorf("status = %s, want failed", exec.Status)
	}
	if exec.Error != "boom" {
		t.Errorf("error = %q, want the body's error text", exec.Error)
	}
	if len(exec.Outputs) != 0 {
		t.Errorf("failed outputs = %v, want empty", exec.Outputs)
	}
}

var errTaskBoom = &taskError{"boom"}

type taskError struct{ msg string }

func (e *taskError) Error() string { return e.msg }

// TestStartTaskSyncPanicRecovered pins the panic conversion: a panicking
// body records a failed run rather than crashing the process.
func TestStartTaskSyncPanicRecovered(t *testing.T) {
	reg := NewRegistry()
	reg.Register("PanickingTask", func(control *TaskControl) (map[string]any, error) {
		panic("kaboom")
	})
	store := newMemStore(t)

	run, err := StartTaskSync(store, reg, "PanickingTask", "panic:run", nil)
	if err != nil {
		t.Fatalf("StartTaskSync: %v", err)
	}
	exec, err := run.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exec.Status != ExecutionFailed {
		t.Errorf("status = %s, want failed", exec.Status)
	}
	if !strings.HasPrefix(exec.Error, "panic: ") || !strings.Contains(exec.Error, "kaboom") {
		t.Errorf("error = %q, want the recorded panic", exec.Error)
	}
}

// TestStartTaskSyncIdempotent pins the restart semantics: rerunning the same
// run id against a durable store returns the retained run and appends
// nothing.
func TestStartTaskSyncIdempotent(t *testing.T) {
	reg := NewRegistry()
	reg.Register("OnceTask", func(control *TaskControl) (map[string]any, error) {
		return map[string]any{"n": json.Number("1")}, nil
	})
	store := newMemStore(t)

	first, err := StartTaskSync(store, reg, "OnceTask", "once:run", nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	count, err := store.FactCount()
	if err != nil {
		t.Fatalf("FactCount: %v", err)
	}

	second, err := StartTaskSync(store, reg, "OnceTask", "once:run", nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.ExecutionID() != first.ExecutionID() {
		t.Errorf("rerun allocated %s, want the retained %s", second.ExecutionID(), first.ExecutionID())
	}
	after, err := store.FactCount()
	if err != nil {
		t.Fatalf("FactCount after: %v", err)
	}
	if after != count {
		t.Errorf("rerun appended %d facts, want 0 (idempotent restart)", after-count)
	}
}

// TestStartTaskAsyncWait pins the async facade: StartTask returns
// immediately and Wait blocks until the terminal frontier exists.
func TestStartTaskAsyncWait(t *testing.T) {
	reg := NewRegistry()
	reg.Register("SlowTask", func(control *TaskControl) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	store := newMemStore(t)

	run, err := StartTask(context.Background(), store, reg, "SlowTask", "", nil)
	if err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	exec, err := run.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exec.Status != ExecutionSucceeded {
		t.Errorf("status = %s, want succeeded", exec.Status)
	}
	if exec.ExecutionID != run.ExecutionID() {
		t.Errorf("projected %s, want %s", exec.ExecutionID, run.ExecutionID())
	}
}

// TestReadExecutionUnknown pins the control's known-child boundary.
func TestReadExecutionUnknown(t *testing.T) {
	reg := NewRegistry()
	reg.Register("IdleTask", func(control *TaskControl) (map[string]any, error) {
		return nil, nil
	})
	store := newMemStore(t)
	run, err := StartTaskSync(store, reg, "IdleTask", "idle:run", nil)
	if err != nil {
		t.Fatalf("StartTaskSync: %v", err)
	}
	_ = run

	// ReadExecution goes through a control; reach one via a task that
	// captures it.
	var captured *TaskControl
	reg.Register("CapturingTask", func(control *TaskControl) (map[string]any, error) {
		captured = control
		return nil, nil
	})
	if _, err := StartTaskSync(store, reg, "CapturingTask", "capture:run", nil); err != nil {
		t.Fatalf("capturing run: %v", err)
	}
	if _, err := captured.ReadExecution("exec:unknown"); err == nil {
		t.Fatal("ReadExecution accepted an unknown execution")
	}
}
