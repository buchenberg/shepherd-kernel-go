package shepherd

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Supervisor schema refs for trace recording.
const (
	SchemaSupervisorInject = "shepherd.supervisor.inject.v1"
	SchemaSupervisorHalt   = "shepherd.supervisor.halt.v1"
)

// InterventionType identifies the kind of supervisor action.
type InterventionType string

const (
	InterventionInject  InterventionType = "inject"  // push guidance into sub-agent
	InterventionFork    InterventionType = "fork"    // fork before risky action
	InterventionDiscard InterventionType = "discard" // discard failed branch
	InterventionHalt    InterventionType = "halt"    // stop sub-agent
)

// Intervention is a supervisor action on a sub-agent scope.
type Intervention struct {
	Type    InterventionType
	ScopeID string
	Payload any
	Time    time.Time
}

// SupervisionRule defines when and how to intervene.
type SupervisionRule struct {
	// Name identifies this rule in logs and traces.
	Name string
	// Match returns true if this rule applies to the effect event.
	Match func(EffectEvent) bool
	// Action returns the intervention to take, or nil for observe-only.
	// The scope parameter is the ScopeManager lookup for the event's owner.
	Action func(event EffectEvent) *Intervention
}

// Supervisor watches sub-agent effect streams and evaluates rules against
// each event. When a rule matches, the supervisor emits an Intervention
// that the orchestrator can act on.
//
// The supervisor is stateless by default — rules that need state (error
// rates, repetition tracking) maintain their own via closures.
type Supervisor struct {
	manager       *ScopeManager
	bus           *EffectBus
	rules         []SupervisionRule
	interventions chan Intervention
	subscription  <-chan EffectEvent
	subscriberID  string
	done          chan struct{}
	closeOnce     sync.Once
	mu            sync.RWMutex
}

// NewSupervisor creates a supervisor that watches the given bus and
// evaluates rules against each event. Interventions are sent to the
// returned channel.
//
// Call Close() to stop the supervisor's background goroutine.
func NewSupervisor(manager *ScopeManager, bus *EffectBus) *Supervisor {
	return &Supervisor{
		manager:       manager,
		bus:           bus,
		interventions: make(chan Intervention, 64),
		done:          make(chan struct{}),
	}
}

// AddRule registers a supervision rule. Rules are evaluated in order;
// the first matching rule that produces an intervention wins.
func (s *Supervisor) AddRule(rule SupervisionRule) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = append(s.rules, rule)
}

// Start begins watching the bus for effect events. It subscribes to the
// bus and runs a background goroutine that evaluates rules.
func (s *Supervisor) Start(subscriberID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.subscription != nil {
		return // already started
	}
	if s.bus == nil {
		return // no bus to watch
	}

	s.subscriberID = subscriberID
	s.subscription = s.bus.Subscribe(subscriberID)
	go s.loop()
}

// Interventions returns a receive-only channel of interventions.
func (s *Supervisor) Interventions() <-chan Intervention {
	return s.interventions
}

// Close stops the supervisor and closes the interventions channel.
// It unsubscribes from the bus. Safe to call multiple times.
func (s *Supervisor) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		if s.bus != nil && s.subscriberID != "" {
			s.bus.Unsubscribe(s.subscriberID)
		}
	})
}

func (s *Supervisor) loop() {
	defer close(s.interventions)

	for {
		select {
		case <-s.done:
			return
		case event, ok := <-s.subscription:
			if !ok {
				return
			}
			s.evaluate(event)
		}
	}
}

func (s *Supervisor) evaluate(event EffectEvent) {
	s.mu.RLock()
	rules := make([]SupervisionRule, len(s.rules))
	copy(rules, s.rules)
	s.mu.RUnlock()

	var chosen *Intervention
	for _, rule := range rules {
		if !rule.Match(event) {
			continue
		}
		intervention := rule.Action(event)
		if intervention == nil || chosen != nil {
			continue
		}
		chosen = intervention
	}
	if chosen == nil {
		return
	}

	// Non-blocking send — drop if the orchestrator is too slow.
	select {
	case s.interventions <- *chosen:
	default:
		// Drop oldest
		select {
		case <-s.interventions:
		default:
		}
		select {
		case s.interventions <- *chosen:
		default:
		}
	}
}

// Inject records a supervisor inject action in the target scope's trace.
func (s *Supervisor) Inject(scopeID, guidance string) error {
	scope, ok := s.manager.Get(scopeID)
	if !ok {
		return fmt.Errorf("scope %s not found", scopeID)
	}

	_, err := scope.store.Append(TrustedAppendContext, AppendBatch{
		AppendIntentID: fmt.Sprintf("%s:supervisor:inject:%d", scope.OwnerID(), time.Now().UnixNano()),
		Groups: []AppendGroup{{
			TraceOwnerID: scope.OwnerID(),
			FactDrafts: []RecordDraft{{
				Mode:      Declaration,
				SchemaRef: SchemaSupervisorInject,
				KindLabel: "supervisor:inject",
				Payload: map[string]any{
					"guidance": guidance,
				},
			}},
		}},
	})
	return err
}

// Halt records a supervisor halt action and marks the scope as discarded.
func (s *Supervisor) Halt(scopeID string) error {
	scope, ok := s.manager.Get(scopeID)
	if !ok {
		return fmt.Errorf("scope %s not found", scopeID)
	}

	return scope.Halt()
}

// --- Built-in rules ---

// DestructiveToolRule intervenes before potentially destructive file
// operations. Matches bash commands containing rm, mv, chmod, or write
// tool calls that look like overwrites.
func DestructiveToolRule() SupervisionRule {
	return SupervisionRule{
		Name: "destructive_file_guard",
		Match: func(e EffectEvent) bool {
			if e.Mode != Declaration {
				return false // only intercept intents, not outcomes
			}
			return isDestructiveToolCall(e)
		},
		Action: func(e EffectEvent) *Intervention {
			return &Intervention{
				Type:    InterventionInject,
				ScopeID: fmt.Sprintf("scope:%s", e.TraceOwnerID),
				Payload: fmt.Sprintf("WARNING: destructive operation detected (%s). Proceed with caution.", e.KindLabel),
				Time:    time.Now(),
			}
		},
	}
}

func isDestructiveToolCall(e EffectEvent) bool {
	// Check bash commands for destructive patterns
	if e.SchemaRef == "yaah.tool.bash.v1" || e.KindLabel == "bash" {
		if cmd, ok := e.Payload["cmd"].(string); ok {
			destructive := []string{"rm ", "rm\t", "rmdir", "mv ", "chmod", "chown", "mkfs", "dd ", "format", "> /", ">> /"}
			for _, pattern := range destructive {
				if strings.Contains(cmd, pattern) {
					return true
				}
			}
		}
	}

	// Check write tool for overwrites
	if e.SchemaRef == "yaah.tool.write.v1" || e.KindLabel == "write" {
		return true // all writes are potentially destructive
	}

	// Check delete tool
	if e.SchemaRef == "yaah.tool.delete.v1" || e.KindLabel == "delete" {
		return true
	}

	return false
}

// HighErrorRateRule intervenes when a sub-agent's error rate exceeds a
// threshold within a sliding window of recent tool calls.
//
// This rule maintains per-owner state via closures.
func HighErrorRateRule(threshold float64, window int) SupervisionRule {
	if window <= 0 {
		window = 10
	}
	type ownerState struct {
		results []bool // true = success, false = error
	}
	states := make(map[string]*ownerState)
	var mu sync.Mutex

	return SupervisionRule{
		Name: "error_rate_guard",
		Match: func(e EffectEvent) bool {
			if e.Mode != Capture || e.KindLabel == "turn:started" || e.KindLabel == "turn:completed" || e.KindLabel == "turn:failed" {
				return false
			}
			_, hasSuccess := e.Payload["success"]
			return hasSuccess
		},
		Action: func(e EffectEvent) *Intervention {
			mu.Lock()
			defer mu.Unlock()

			st, ok := states[e.TraceOwnerID]
			if !ok {
				st = &ownerState{}
				states[e.TraceOwnerID] = st
			}

			success, _ := e.Payload["success"].(bool)
			st.results = append(st.results, success)

			if len(st.results) > window {
				st.results = st.results[len(st.results)-window:]
			}

			// Need at least half the window to evaluate
			if len(st.results) < window/2 {
				return nil
			}

			errors := 0
			for _, s := range st.results {
				if !s {
					errors++
				}
			}
			rate := float64(errors) / float64(len(st.results))

			if rate >= threshold {
				st.results = nil // reset to avoid repeated warnings
				return &Intervention{
					Type:    InterventionInject,
					ScopeID: fmt.Sprintf("scope:%s", e.TraceOwnerID),
					Payload: fmt.Sprintf("WARNING: error rate %.0f%% (%d/%d recent calls). Consider a different approach.", rate*100, errors, len(st.results)),
					Time:    time.Now(),
				}
			}
			return nil
		},
	}
}

// StuckDetectionRule intervenes when a sub-agent repeats the same tool
// call (by kind label + args hash) more than threshold times in a row.
//
// This rule maintains per-owner state via closures.
func StuckDetectionRule(repeatThreshold int) SupervisionRule {
	type ownerState struct {
		lastCall string
		repeat   int
	}
	states := make(map[string]*ownerState)
	var mu sync.Mutex

	return SupervisionRule{
		Name: "stuck_detection",
		Match: func(e EffectEvent) bool {
			return e.Mode == Declaration && e.KindLabel != "turn:created" && e.KindLabel != "turn:started"
		},
		Action: func(e EffectEvent) *Intervention {
			mu.Lock()
			defer mu.Unlock()

			st, ok := states[e.TraceOwnerID]
			if !ok {
				st = &ownerState{}
				states[e.TraceOwnerID] = st
			}

			// Build a call signature from kind label + all payload keys in sorted order
			callSig := e.KindLabel
			if len(e.Payload) > 0 {
				keys := make([]string, 0, len(e.Payload))
				for k := range e.Payload {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					callSig += fmt.Sprintf(":%s=%v", k, e.Payload[k])
				}
			}

			if callSig == st.lastCall {
				st.repeat++
			} else {
				st.lastCall = callSig
				st.repeat = 1
			}

			if st.repeat >= repeatThreshold {
				st.repeat = 0 // reset to avoid spamming
				return &Intervention{
					Type:    InterventionInject,
					ScopeID: fmt.Sprintf("scope:%s", e.TraceOwnerID),
					Payload: fmt.Sprintf("WARNING: same tool call repeated %d times. You may be stuck. Try a different approach.", repeatThreshold),
					Time:    time.Now(),
				}
			}
			return nil
		},
	}
}
