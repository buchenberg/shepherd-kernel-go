package shepherd

// Handles integration test (plan 02 §6): a task tree with spawn, adopt and
// abandon, projected from the run frontier, plus the restart law — reopen
// the store file and re-project everything, identically.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

// historySummary flattens an EffectiveHistory into a comparable shape for
// the restart law: identities and kinds, not pointers.
type historySummary struct {
	RootStatus  ExecutionStatus
	RootTaskRef string
	Children    []childSummary
	Published   []string
}

type childSummary struct {
	TaskRef  string
	Status   ExecutionStatus
	Kind     RelationKind
	Relation string
}

func summarizeHistory(h *EffectiveHistory) historySummary {
	s := historySummary{RootStatus: h.Root.Status, RootTaskRef: h.Root.TaskRef}
	for _, child := range h.Children {
		s.Children = append(s.Children, childSummary{
			TaskRef:  child.Execution.TaskRef,
			Status:   child.Execution.Status,
			Kind:     child.Relation.RelationKind,
			Relation: child.Relation.RelationID,
		})
	}
	for _, fact := range h.PublishedFacts {
		s.Published = append(s.Published, fact.Kind)
	}
	return s
}

// TestHandlesIntegrationTree builds the plan-02 fixture: a parent that
// publishes, spawns two children (one later abandoned), adopts an externally
// created execution, and completes. The effective history from the run
// frontier must show exactly that tree, and reopening the store file must
// re-project it identically.
func TestHandlesIntegrationTree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "handles.sqlite")
	store, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	reg := NewRegistry()
	reg.Register("LeafTask", func(control *TaskControl) (map[string]any, error) {
		return map[string]any{"leaf": true}, nil
	})
	reg.Register("ParentTask", func(control *TaskControl) (map[string]any, error) {
		if _, err := control.Publish(context.Background(), "phase", map[string]any{"at": "start"}); err != nil {
			return nil, err
		}
		kept, err := control.Spawn(context.Background(), "LeafTask", map[string]any{"x": json.Number("1")}, reg)
		if err != nil {
			return nil, err
		}
		dropped, err := control.Spawn(context.Background(), "LeafTask", map[string]any{"x": json.Number("2")}, reg)
		if err != nil {
			return nil, err
		}
		if _, err := control.AwaitTerminal(context.Background(), kept); err != nil {
			return nil, err
		}
		if _, err := control.Abandon(context.Background(), dropped); err != nil {
			return nil, err
		}

		// Adopt an execution created outside this tree.
		external, err := StartTaskSync(context.Background(), store, reg, "LeafTask", "integ:external", map[string]any{"x": json.Number("3")})
		if err != nil {
			return nil, err
		}
		if _, err := control.Adopt(context.Background(), external.ExecutionID(), external.FrontierID(), ""); err != nil {
			return nil, err
		}
		if _, err := control.Publish(context.Background(), "phase", map[string]any{"at": "end"}); err != nil {
			return nil, err
		}
		return map[string]any{"children": json.Number("2")}, nil
	})

	run, err := StartTaskSync(context.Background(), store, reg, "ParentTask", "integ:parent", nil)
	if err != nil {
		t.Fatalf("StartTaskSync: %v", err)
	}

	exec, err := run.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exec.Status != ExecutionSucceeded {
		t.Fatalf("parent status = %s, want succeeded", exec.Status)
	}

	cutoff, err := run.Cutoff(context.Background())
	if err != nil {
		t.Fatalf("Cutoff: %v", err)
	}
	history, err := ProjectEffectiveHistoryFromStore(context.Background(), store, reader, cutoff)
	if err != nil {
		t.Fatalf("ProjectEffectiveHistoryFromStore: %v", err)
	}
	want := historySummary{
		RootStatus:  ExecutionSucceeded,
		RootTaskRef: "ParentTask",
		Children: []childSummary{
			{TaskRef: "LeafTask", Status: ExecutionSucceeded, Kind: RelationSpawned},
			{TaskRef: "LeafTask", Status: ExecutionSucceeded, Kind: RelationAdopted},
		},
		Published: []string{"phase", "phase"},
	}
	got := summarizeHistory(history)
	if got.RootStatus != want.RootStatus || got.RootTaskRef != want.RootTaskRef {
		t.Errorf("root = %s %s, want %s %s", got.RootStatus, got.RootTaskRef, want.RootStatus, want.RootTaskRef)
	}
	if len(got.Children) != 2 {
		t.Fatalf("children = %+v, want the spawned and adopted leaves (abandoned dropped)", got.Children)
	}
	for i, child := range want.Children {
		if got.Children[i].TaskRef != child.TaskRef || got.Children[i].Status != child.Status || got.Children[i].Kind != child.Kind {
			t.Errorf("child[%d] = %+v, want %+v", i, got.Children[i], child)
		}
	}
	if len(got.Published) != 2 || got.Published[0] != "phase" || got.Published[1] != "phase" {
		t.Errorf("published = %v, want [phase phase]", got.Published)
	}
	summaryBefore := got

	// The restart law: close the store, reopen the same file, re-project —
	// the summary must be identical.
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := NewSQLiteTraceStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close(context.Background())

	reopenedCutoff, err := reopened.ReadOwnerCutoff(context.Background(), run.FrontierID())
	if err != nil {
		t.Fatalf("ReadOwnerCutoff after restart: %v", err)
	}
	historyAfter, err := ProjectEffectiveHistoryFromStore(context.Background(), reopened, reader, reopenedCutoff)
	if err != nil {
		t.Fatalf("re-project: %v", err)
	}
	summaryAfter := summarizeHistory(historyAfter)
	if summaryAfter.RootStatus != summaryBefore.RootStatus ||
		summaryAfter.RootTaskRef != summaryBefore.RootTaskRef ||
		len(summaryAfter.Children) != len(summaryBefore.Children) ||
		len(summaryAfter.Published) != len(summaryBefore.Published) {
		t.Errorf("history changed across restart: %+v vs %+v", summaryAfter, summaryBefore)
	}
	for i, child := range summaryBefore.Children {
		if summaryAfter.Children[i] != child {
			t.Errorf("child[%d] changed across restart: %+v vs %+v", i, summaryAfter.Children[i], child)
		}
	}
	for i, kind := range summaryBefore.Published {
		if summaryAfter.Published[i] != kind {
			t.Errorf("published[%d] changed across restart: %s vs %s", i, summaryAfter.Published[i], kind)
		}
	}
}
