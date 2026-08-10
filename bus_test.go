package shepherd

import (
	"sync"
	"testing"
	"time"
)

func TestEffectBus_PublishSubscribe(t *testing.T) {
	bus := NewEffectBus(16)
	defer bus.Close()

	ch := bus.Subscribe("test-sub")

	event := EffectEvent{
		RecordID:      "sha256:abc123",
		IntentID:      "intent:1",
		TraceOwnerID:  "sub:test",
		Mode:          Declaration,
		SchemaRef:     "yaah.tool.bash.v1",
		KindLabel:     "bash",
		Payload:       map[string]any{"cmd": "ls"},
		CausalParents: nil,
		Timestamp:     time.Now(),
	}

	bus.Publish(event)

	select {
	case received := <-ch:
		if received.RecordID != "sha256:abc123" {
			t.Errorf("expected RecordID sha256:abc123, got %s", received.RecordID)
		}
		if received.Mode != Declaration {
			t.Errorf("expected mode Declaration, got %s", received.Mode)
		}
		if received.SchemaRef != "yaah.tool.bash.v1" {
			t.Errorf("expected schema yaah.tool.bash.v1, got %s", received.SchemaRef)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestEffectBus_MultipleSubscribers(t *testing.T) {
	bus := NewEffectBus(16)
	defer bus.Close()

	ch1 := bus.Subscribe("sub-1")
	ch2 := bus.Subscribe("sub-2")
	ch3 := bus.Subscribe("sub-3")

	event := EffectEvent{
		RecordID: "sha256:shared",
		Mode:     Capture,
	}

	bus.Publish(event)

	for _, ch := range []<-chan EffectEvent{ch1, ch2, ch3} {
		select {
		case received := <-ch:
			if received.RecordID != "sha256:shared" {
				t.Errorf("expected RecordID sha256:shared, got %s", received.RecordID)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for event on subscriber")
		}
	}
}

func TestEffectBus_Unsubscribe(t *testing.T) {
	bus := NewEffectBus(16)
	defer bus.Close()

	ch := bus.Subscribe("temp-sub")
	if bus.SubscriberCount() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", bus.SubscriberCount())
	}

	bus.Unsubscribe("temp-sub")
	if bus.SubscriberCount() != 0 {
		t.Fatalf("expected 0 subscribers after unsubscribe, got %d", bus.SubscriberCount())
	}

	// Channel should be closed
	_, ok := <-ch
	if ok {
		t.Error("expected channel to be closed after unsubscribe")
	}
}

func TestEffectBus_ReplaceSubscription(t *testing.T) {
	bus := NewEffectBus(16)
	defer bus.Close()

	ch1 := bus.Subscribe("same-id")

	// Publish to first subscription
	bus.Publish(EffectEvent{RecordID: "first"})

	// Drain the event from ch1 so the buffer is empty
	<-ch1

	ch2 := bus.Subscribe("same-id") // replaces ch1

	// ch1 should be closed now (buffer was empty when closed)
	_, ok := <-ch1
	if ok {
		t.Error("expected old channel to be closed on replacement")
	}

	// ch2 should receive new events
	bus.Publish(EffectEvent{RecordID: "second"})

	select {
	case received := <-ch2:
		if received.RecordID != "second" {
			t.Errorf("expected RecordID second, got %s", received.RecordID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out on replacement channel")
	}
}

func TestEffectBus_NonBlockingPublish(t *testing.T) {
	bus := NewEffectBus(2) // tiny buffer
	defer bus.Close()

	ch := bus.Subscribe("slow-sub")

	// Fill the buffer
	bus.Publish(EffectEvent{RecordID: "1"})
	bus.Publish(EffectEvent{RecordID: "2"})

	// This should NOT block — it drops the oldest and enqueues
	bus.Publish(EffectEvent{RecordID: "3"})

	// Should see events 2 and 3 (event 1 was dropped)
	e1 := <-ch
	e2 := <-ch

	if e1.RecordID != "2" {
		t.Errorf("expected first event to be '2' (oldest dropped), got %s", e1.RecordID)
	}
	if e2.RecordID != "3" {
		t.Errorf("expected second event to be '3', got %s", e2.RecordID)
	}
}

func TestEffectBus_Close(t *testing.T) {
	bus := NewEffectBus(16)

	ch1 := bus.Subscribe("a")
	ch2 := bus.Subscribe("b")

	bus.Close()

	// All channels should be closed
	_, ok1 := <-ch1
	_, ok2 := <-ch2
	if ok1 || ok2 {
		t.Error("expected channels to be closed after bus close")
	}

	// Subscriber count should be 0
	if bus.SubscriberCount() != 0 {
		t.Errorf("expected 0 subscribers after close, got %d", bus.SubscriberCount())
	}

	// Publish after close should be a no-op (no panic)
	bus.Publish(EffectEvent{RecordID: "after-close"})
}

func TestEffectBus_SubscribeAfterClose(t *testing.T) {
	bus := NewEffectBus(16)
	bus.Close()

	ch := bus.Subscribe("late")
	_, ok := <-ch
	if ok {
		t.Error("expected channel to be closed when subscribing to closed bus")
	}
}

func TestEffectBus_ConcurrentPublish(t *testing.T) {
	bus := NewEffectBus(256)
	defer bus.Close()

	ch := bus.Subscribe("concurrent")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				bus.Publish(EffectEvent{
					RecordID: "event",
					Payload:  map[string]any{"goroutine": id, "seq": j},
				})
			}
		}(i)
	}

	wg.Wait()

	// Drain all events — should not panic
	count := 0
	for {
		select {
		case <-ch:
			count++
		default:
			goto done
		}
	}
done:

	if count == 0 {
		t.Error("expected to receive at least some events")
	}
	// We may have dropped some due to buffer overflow — that's fine
	t.Logf("received %d of 1000 events (drops expected with buffer=256)", count)
}

func TestEffectBus_DefaultBufferSize(t *testing.T) {
	bus := NewEffectBus(0) // should use default
	defer bus.Close()

	if bus.bufferSize != defaultBusBufferSize {
		t.Errorf("expected default buffer size %d, got %d", defaultBusBufferSize, bus.bufferSize)
	}
}

func TestEffectBus_PublishToNoSubscribers(t *testing.T) {
	bus := NewEffectBus(16)
	defer bus.Close()

	// Should not panic
	bus.Publish(EffectEvent{RecordID: "lonely"})
}

func TestEffectBus_StoreIntegration(t *testing.T) {
	bus := NewEffectBus(64)
	defer bus.Close()

	store := newMemStore(t)
	store.WithBus(bus)

	ch := bus.Subscribe("store-watcher")

	// Append a declaration
	receipt, err := store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: "test:intent:1",
		Groups: []AppendGroup{{
			TraceOwnerID: "sub:test-agent",
			FactDrafts: []RecordDraft{{
				Mode:      Declaration,
				SchemaRef: "yaah.tool.bash.v1",
				KindLabel: "bash",
				Payload:   map[string]any{"cmd": "ls -la"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Append failed: %v", err)
	}
	if len(receipt.FactIDs) != 1 {
		t.Fatalf("expected 1 fact ID, got %d", len(receipt.FactIDs))
	}

	// Should have received an event
	select {
	case event := <-ch:
		if event.RecordID != receipt.FactIDs[0] {
			t.Errorf("expected RecordID %s, got %s", receipt.FactIDs[0], event.RecordID)
		}
		if event.Mode != Declaration {
			t.Errorf("expected mode Declaration, got %s", event.Mode)
		}
		if event.TraceOwnerID != "sub:test-agent" {
			t.Errorf("expected owner sub:test-agent, got %s", event.TraceOwnerID)
		}
		if event.SchemaRef != "yaah.tool.bash.v1" {
			t.Errorf("expected schema yaah.tool.bash.v1, got %s", event.SchemaRef)
		}
		if event.KindLabel != "bash" {
			t.Errorf("expected kind bash, got %s", event.KindLabel)
		}
		if event.IntentID != "test:intent:1" {
			t.Errorf("expected intent test:intent:1, got %s", event.IntentID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for store event")
	}
}

func TestEffectBus_StoreIntegration_MultipleDrafts(t *testing.T) {
	bus := NewEffectBus(64)
	defer bus.Close()

	store := newMemStore(t)
	store.WithBus(bus)

	ch := bus.Subscribe("multi-watcher")

	// Append a batch with multiple drafts in one group
	_, err := store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: "test:multi:1",
		Groups: []AppendGroup{{
			TraceOwnerID: "sub:multi",
			FactDrafts: []RecordDraft{
				{Mode: Declaration, SchemaRef: "tool.a.v1", KindLabel: "a", Payload: map[string]any{"x": 1}},
				{Mode: Capture, SchemaRef: "tool.a.v1.applied", KindLabel: "a:result", Payload: map[string]any{"ok": true}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	// Should receive 2 events
	for i := 0; i < 2; i++ {
		select {
		case event := <-ch:
			if i == 0 && event.Mode != Declaration {
				t.Errorf("first event should be Declaration, got %s", event.Mode)
			}
			if i == 1 && event.Mode != Capture {
				t.Errorf("second event should be Capture, got %s", event.Mode)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", i)
		}
	}
}

func TestEffectBus_StoreNoBus(t *testing.T) {
	store := newMemStore(t)
	// No bus attached — should work fine (no panic)

	_, err := store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: "test:no-bus:1",
		Groups: []AppendGroup{{
			TraceOwnerID: "sub:quiet",
			FactDrafts: []RecordDraft{{
				Mode:      Declaration,
				SchemaRef: "test.v1",
				KindLabel: "test",
				Payload:   map[string]any{},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Append without bus should still work: %v", err)
	}
}

func TestEffectBus_StoreIdempotentAppend(t *testing.T) {
	bus := NewEffectBus(64)
	defer bus.Close()

	store := newMemStore(t)
	store.WithBus(bus)

	ch := bus.Subscribe("idempotent-watcher")

	batch := AppendBatch{
		AppendIntentID: "test:idempotent:1",
		Groups: []AppendGroup{{
			TraceOwnerID: "sub:idempotent",
			FactDrafts: []RecordDraft{{
				Mode:      Declaration,
				SchemaRef: "test.v1",
				KindLabel: "test",
				Payload:   map[string]any{"v": 1},
			}},
		}},
	}

	// First append
	_, err := store.Append(TrustedAppendContext, batch)
	if err != nil {
		t.Fatalf("first append: %v", err)
	}

	// Second append (same intent ID — should be idempotent)
	_, err = store.Append(TrustedAppendContext, batch)
	if err != nil {
		t.Fatalf("second append: %v", err)
	}

	// Should receive exactly 1 event (idempotent append doesn't re-publish)
	select {
	case <-ch:
		// good
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first event")
	}

	// Should NOT receive a second event
	select {
	case <-ch:
		t.Error("idempotent append should not publish a second event")
	case <-time.After(50 * time.Millisecond):
		// expected — no second event
	}
}
