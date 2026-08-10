package shepherd

import (
	"testing"
	"time"
)

func TestSupervisor_BasicRuleMatch(t *testing.T) {
	store := newMemStore(t)
	bus := NewEffectBus(64)
	defer bus.Close()

	mgr := NewScopeManager(store, bus)
	supervisor := NewSupervisor(mgr, bus)

	matched := false
	supervisor.AddRule(SupervisionRule{
		Name: "test_rule",
		Match: func(e EffectEvent) bool {
			return e.KindLabel == "bash"
		},
		Action: func(e EffectEvent) *Intervention {
			matched = true
			return &Intervention{
				Type:    InterventionInject,
				ScopeID: "scope:" + e.TraceOwnerID,
				Payload: "test intervention",
				Time:    time.Now(),
			}
		},
	})

	supervisor.Start("test-supervisor")
	defer supervisor.Close()

	// Publish a matching event
	bus.Publish(EffectEvent{
		RecordID:     "sha256:test",
		TraceOwnerID: "sub:worker",
		Mode:         Declaration,
		KindLabel:    "bash",
		Payload:      map[string]any{"cmd": "ls"},
	})

	// Wait for intervention
	select {
	case iv := <-supervisor.Interventions():
		if iv.Type != InterventionInject {
			t.Errorf("expected inject, got %s", iv.Type)
		}
		if iv.ScopeID != "scope:sub:worker" {
			t.Errorf("expected scope:sub:worker, got %s", iv.ScopeID)
		}
		if !matched {
			t.Error("rule action should have been called")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for intervention")
	}
}

func TestSupervisor_NonMatchingRule(t *testing.T) {
	store := newMemStore(t)
	bus := NewEffectBus(64)
	defer bus.Close()

	mgr := NewScopeManager(store, bus)
	supervisor := NewSupervisor(mgr, bus)

	supervisor.AddRule(SupervisionRule{
		Name: "bash_only",
		Match: func(e EffectEvent) bool {
			return e.KindLabel == "bash"
		},
		Action: func(e EffectEvent) *Intervention {
			return &Intervention{Type: InterventionInject, ScopeID: "x"}
		},
	})

	supervisor.Start("test-nonmatch")
	defer supervisor.Close()

	// Publish a non-matching event
	bus.Publish(EffectEvent{
		KindLabel: "read_file",
		Mode:      Declaration,
	})

	// Should NOT receive intervention
	select {
	case <-supervisor.Interventions():
		t.Error("should not receive intervention for non-matching event")
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}

func TestSupervisor_FirstRuleWins(t *testing.T) {
	store := newMemStore(t)
	bus := NewEffectBus(64)
	defer bus.Close()

	mgr := NewScopeManager(store, bus)
	supervisor := NewSupervisor(mgr, bus)

	var first, second bool
	supervisor.AddRule(SupervisionRule{
		Name: "first",
		Match: func(e EffectEvent) bool { return true },
		Action: func(e EffectEvent) *Intervention {
			first = true
			return &Intervention{Type: InterventionInject, ScopeID: "x"}
		},
	})
	supervisor.AddRule(SupervisionRule{
		Name: "second",
		Match: func(e EffectEvent) bool { return true },
		Action: func(e EffectEvent) *Intervention {
			second = true
			return &Intervention{Type: InterventionHalt, ScopeID: "x"}
		},
	})

	supervisor.Start("first-wins")
	defer supervisor.Close()

	bus.Publish(EffectEvent{KindLabel: "test"})

	<-supervisor.Interventions()

	if !first {
		t.Error("first rule should have matched")
	}
	if second {
		t.Error("second rule should NOT have matched (first wins)")
	}
}

func TestSupervisor_ObserveOnlyRule(t *testing.T) {
	store := newMemStore(t)
	bus := NewEffectBus(64)
	defer bus.Close()

	mgr := NewScopeManager(store, bus)
	supervisor := NewSupervisor(mgr, bus)

	// Rule that matches but returns nil (observe-only)
	supervisor.AddRule(SupervisionRule{
		Name: "observer",
		Match: func(e EffectEvent) bool { return true },
		Action: func(e EffectEvent) *Intervention {
			return nil // observe only
		},
	})

	supervisor.Start("observer-test")
	defer supervisor.Close()

	bus.Publish(EffectEvent{KindLabel: "test"})

	select {
	case <-supervisor.Interventions():
		t.Error("observe-only rule should not produce intervention")
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}

func TestSupervisor_Inject(t *testing.T) {
	store := newMemStore(t)
	bus := NewEffectBus(64)
	defer bus.Close()

	mgr := NewScopeManager(store, bus)
	mgr.Create("sub:inject-target")

	supervisor := NewSupervisor(mgr, bus)
	err := supervisor.Inject("scope:sub:inject-target", "try a different approach")
	if err != nil {
		t.Fatalf("inject failed: %v", err)
	}

	// Should be recorded in the trace
	slice, err := store.ReadOwnerPrefix(TrustedReadContext, "sub:inject-target", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	found := false
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		if rec.GetEnvelope().SchemaRef == SchemaSupervisorInject {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected supervisor:inject record in trace")
	}
}

func TestSupervisor_Halt(t *testing.T) {
	store := newMemStore(t)
	bus := NewEffectBus(64)
	defer bus.Close()

	mgr := NewScopeManager(store, bus)
	scope, _ := mgr.Create("sub:halt-target")

	supervisor := NewSupervisor(mgr, bus)
	err := supervisor.Halt("scope:sub:halt-target")
	if err != nil {
		t.Fatalf("halt failed: %v", err)
	}

	// Scope should be discarded
	if scope.State() != ScopeDiscarded {
		t.Errorf("expected scope discarded after halt, got %s", scope.State())
	}

	// Should be recorded in the trace
	slice, err := store.ReadOwnerPrefix(TrustedReadContext, "sub:halt-target", 99, ModeBoth)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}

	found := false
	for _, id := range slice.FactIDs() {
		rec := slice.FactsByID[id]
		if rec.GetEnvelope().SchemaRef == SchemaSupervisorHalt {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected supervisor:halt record in trace")
	}
}

func TestSupervisor_ScopeNotFound(t *testing.T) {
	store := newMemStore(t)
	mgr := NewScopeManager(store, nil)
	supervisor := NewSupervisor(mgr, nil)

	err := supervisor.Inject("scope:nonexistent", "hello")
	if err == nil {
		t.Error("inject to nonexistent scope should fail")
	}

	err = supervisor.Halt("scope:nonexistent")
	if err == nil {
		t.Error("halt of nonexistent scope should fail")
	}
}

// --- Built-in rule tests ---

func TestDestructiveToolRule_BashRm(t *testing.T) {
	rule := DestructiveToolRule()

	event := EffectEvent{
		Mode:      Declaration,
		KindLabel: "bash",
		SchemaRef: "yaah.tool.bash.v1",
		Payload:   map[string]any{"cmd": "rm -rf /tmp/test"},
	}

	if !rule.Match(event) {
		t.Error("rm command should match destructive rule")
	}

	iv := rule.Action(event)
	if iv == nil {
		t.Fatal("destructive rule should produce intervention")
	}
	if iv.Type != InterventionInject {
		t.Errorf("expected inject, got %s", iv.Type)
	}
}

func TestDestructiveToolRule_BashSafe(t *testing.T) {
	rule := DestructiveToolRule()

	event := EffectEvent{
		Mode:      Declaration,
		KindLabel: "bash",
		SchemaRef: "yaah.tool.bash.v1",
		Payload:   map[string]any{"cmd": "ls -la"},
	}

	if rule.Match(event) {
		t.Error("ls command should NOT match destructive rule")
	}
}

func TestDestructiveToolRule_Write(t *testing.T) {
	rule := DestructiveToolRule()

	event := EffectEvent{
		Mode:      Declaration,
		KindLabel: "write",
		SchemaRef: "yaah.tool.write.v1",
		Payload:   map[string]any{"path": "/tmp/test"},
	}

	if !rule.Match(event) {
		t.Error("write should match destructive rule")
	}
}

func TestDestructiveToolRule_IgnoresCaptures(t *testing.T) {
	rule := DestructiveToolRule()

	event := EffectEvent{
		Mode:      Capture,
		KindLabel: "bash",
		Payload:   map[string]any{"cmd": "rm -rf /"},
	}

	if rule.Match(event) {
		t.Error("capture events should not match (only intercept intents)")
	}
}

func TestHighErrorRateRule_Triggers(t *testing.T) {
	rule := HighErrorRateRule(0.5, 6) // 50% threshold, window of 6

	// Simulate 4 errors out of 6 calls (67% error rate)
	for i := 0; i < 4; i++ {
		event := EffectEvent{
			TraceOwnerID: "sub:error-prone",
			Mode:         Capture,
			KindLabel:    "bash:result",
			Payload:      map[string]any{"success": false, "error": "command failed"},
		}
		rule.Match(event) // register the event
		iv := rule.Action(event)
		if i < 2 && iv != nil {
			t.Errorf("should not trigger on error %d (not enough data yet)", i)
		}
	}

	// Now add 2 successes
	for i := 0; i < 2; i++ {
		event := EffectEvent{
			TraceOwnerID: "sub:error-prone",
			Mode:         Capture,
			KindLabel:    "bash:result",
			Payload:      map[string]any{"success": true},
		}
		rule.Match(event)
	}

	// Next error should trigger (4 errors / 6 window = 67% > 50%)
	event := EffectEvent{
		TraceOwnerID: "sub:error-prone",
		Mode:         Capture,
		KindLabel:    "bash:result",
		Payload:      map[string]any{"success": false, "error": "failed again"},
	}
	iv := rule.Action(event)
	if iv == nil {
		t.Error("should trigger when error rate exceeds threshold")
	}
}

func TestHighErrorRateRule_IgnoresDeclarations(t *testing.T) {
	rule := HighErrorRateRule(0.5, 6)

	event := EffectEvent{
		TraceOwnerID: "sub:test",
		Mode:         Declaration,
		KindLabel:    "bash",
	}

	if rule.Match(event) {
		t.Error("declarations should not match error rate rule")
	}
}

func TestStuckDetectionRule_Triggers(t *testing.T) {
	rule := StuckDetectionRule(3) // 3 repeats

	// Same call 3 times
	for i := 0; i < 3; i++ {
		event := EffectEvent{
			TraceOwnerID: "sub:stuck",
			Mode:         Declaration,
			KindLabel:    "bash",
			Payload:      map[string]any{"cmd": "ls -la"},
		}
		if !rule.Match(event) {
			t.Fatalf("should match on iteration %d", i)
		}
		iv := rule.Action(event)
		if i < 2 && iv != nil {
			t.Errorf("should not trigger on repeat %d", i+1)
		}
		if i == 2 && iv == nil {
			t.Error("should trigger on repeat 3")
		}
	}
}

func TestStuckDetectionRule_ResetsOnDifferentCall(t *testing.T) {
	rule := StuckDetectionRule(3)

	events := []EffectEvent{
		{TraceOwnerID: "sub:reset", Mode: Declaration, KindLabel: "bash", Payload: map[string]any{"cmd": "ls"}},
		{TraceOwnerID: "sub:reset", Mode: Declaration, KindLabel: "bash", Payload: map[string]any{"cmd": "ls"}},
		{TraceOwnerID: "sub:reset", Mode: Declaration, KindLabel: "read_file", Payload: map[string]any{"path": "x"}}, // different
		{TraceOwnerID: "sub:reset", Mode: Declaration, KindLabel: "bash", Payload: map[string]any{"cmd": "ls"}},
		{TraceOwnerID: "sub:reset", Mode: Declaration, KindLabel: "bash", Payload: map[string]any{"cmd": "ls"}},
	}

	for i, event := range events {
		rule.Match(event)
		iv := rule.Action(event)
		if iv != nil {
			t.Errorf("should not trigger on event %d (reset after different call)", i)
		}
	}
}

func TestStuckDetectionRule_IgnoresCaptures(t *testing.T) {
	rule := StuckDetectionRule(3)

	event := EffectEvent{
		TraceOwnerID: "sub:test",
		Mode:         Capture,
		KindLabel:    "bash",
	}

	if rule.Match(event) {
		t.Error("captures should not match stuck detection")
	}
}
