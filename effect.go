package shepherd

import "time"

// EffectEvent is a real-time notification emitted when a trace record is
// appended. It is NOT a retained record — it's a transient bus message
// that carries just enough context for live supervision.
//
// The trace store is the durable record; the event bus is ephemeral.
// Supervisors subscribe to the bus for real-time observation, and read
// from the store for historical analysis.
type EffectEvent struct {
	// RecordID is the content-addressed ID of the retained record.
	RecordID string
	// IntentID is the append intent ID (idempotency key).
	IntentID string
	// TraceOwnerID identifies the sub-agent session (e.g., "sub:abc").
	TraceOwnerID string
	// Mode distinguishes intent (Declaration) from observation (Capture).
	Mode RecordMode
	// SchemaRef identifies the effect type (e.g., "yaah.tool.bash.v1").
	SchemaRef string
	// KindLabel is the human-readable label (e.g., "bash", "turn:started").
	KindLabel string
	// Payload carries the effect data. For declarations this is typically
	// tool args; for captures it's the result metadata.
	Payload map[string]any
	// CausalParents links to preceding records in the causal graph.
	CausalParents []string
	// Timestamp is when the event was emitted (wall clock, not trace time).
	Timestamp time.Time
}
