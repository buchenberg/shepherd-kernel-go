package shepherd

import (
	"sync"
)

const defaultBusBufferSize = 64

// EffectBus fans out EffectEvents to subscribers. It is a transient
// pub/sub layer — not a persistence mechanism. The trace store is the
// durable record; the bus is for real-time observation.
//
// Publish is non-blocking: if a subscriber's buffer is full, the oldest
// event is dropped. This prevents a slow supervisor from blocking the
// sub-agent it's watching. A supervisor that can't keep up can always
// read the full trace from the store after the fact.
type EffectBus struct {
	mu          sync.RWMutex
	subscribers map[string]chan EffectEvent
	bufferSize  int
	closed      bool
}

// NewEffectBus creates a new effect bus with the given per-subscriber
// buffer size. If bufferSize <= 0, the default (64) is used.
func NewEffectBus(bufferSize int) *EffectBus {
	if bufferSize <= 0 {
		bufferSize = defaultBusBufferSize
	}
	return &EffectBus{
		subscribers: make(map[string]chan EffectEvent),
		bufferSize:  bufferSize,
	}
}

// Subscribe creates a new subscription with the given ID and returns a
// receive-only channel. If a subscription with this ID already exists,
// the old one is closed and replaced.
func (b *EffectBus) Subscribe(id string) <-chan EffectEvent {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		ch := make(chan EffectEvent)
		close(ch)
		return ch
	}

	// Close existing subscription if any
	if old, ok := b.subscribers[id]; ok {
		delete(b.subscribers, id)
		close(old)
	}

	ch := make(chan EffectEvent, b.bufferSize)
	b.subscribers[id] = ch
	return ch
}

// Unsubscribe removes a subscription and closes its channel.
func (b *EffectBus) Unsubscribe(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ch, ok := b.subscribers[id]; ok {
		delete(b.subscribers, id)
		close(ch)
	}
}

// Publish sends an event to all subscribers. It never blocks — if a
// subscriber's buffer is full, the oldest event is dropped.
func (b *EffectBus) Publish(event EffectEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.closed {
		return
	}

	for _, ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			// Buffer full — drop oldest event for this subscriber.
			// Non-blocking send: drain one, then enqueue.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- event:
			default:
			}
		}
	}
}

// Close shuts down the bus and closes all subscriber channels.
func (b *EffectBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return
	}
	b.closed = true

	for id, ch := range b.subscribers {
		close(ch)
		delete(b.subscribers, id)
	}
}

// SubscriberCount returns the number of active subscribers.
func (b *EffectBus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}
