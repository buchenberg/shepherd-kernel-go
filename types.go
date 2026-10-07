package shepherd

import "sort"

// RecordMode distinguishes observed facts from stated intentions.
type RecordMode string

const (
	Capture     RecordMode = "capture"
	Declaration RecordMode = "declaration"
)

// Containment describes the sandbox containment level of a witness.
type Containment string

const (
	ContainFull        Containment = "full"
	ContainContained   Containment = "contained"
	ContainBuffered    Containment = "buffered"
	ContainUncontained Containment = "uncontained"
)

// VisibilityProfile controls what record content is exposed in a slice.
type VisibilityProfile string

const (
	VisibilityShapeOnly    VisibilityProfile = "shape_only"
	VisibilityPayload      VisibilityProfile = "payload"
	VisibilityFullInternal VisibilityProfile = "full_internal"
)

// ModeFilter controls which record modes appear in a slice.
type ModeFilter string

const (
	ModeBoth             ModeFilter = "both"
	ModeDeclarationsOnly ModeFilter = "declarations_only"
	ModeCapturesOnly     ModeFilter = "captures_only"
)

// OperationKind identifies the type of kernel operation for authorization.
type OperationKind string

const (
	OpAppend     OperationKind = "append"
	OpRead       OperationKind = "read"
	OpPublishCut OperationKind = "publish_cut"

	// OpMaterialize and OpObserve are reserved for plan 03 (substrate
	// materialization) and are not reachable today: no exported entry point
	// accepts an OperationContext, and the exported AppendContext/ReadContext
	// helpers set the operation themselves. They are declared so the vocabulary
	// is fixed before the callers exist.
	//
	// Note that OperationContext.Operation is carried but not yet consulted by any
	// authorization check. Do not rely on it to gate anything until plan 03 wires
	// it; see TestReservedOperationKindsAreUnreachable, which asserts both facts
	// so the gap is deliberate rather than assumed.
	OpMaterialize OperationKind = "materialize"
	OpObserve     OperationKind = "observe"
)

// RecordDraft is an append input. Drafts are not retained records.
type RecordDraft struct {
	Mode              RecordMode
	SchemaRef         string
	Payload           map[string]any
	KindLabel         string
	AppendLocalID     string
	CausedByFactIDs   []string
	CausedByLocalRefs []string
}

// RecordEnvelope holds stable semantic identity and causality for one record.
type RecordEnvelope struct {
	RecordID    string
	Digest      string
	SchemaRef   string
	Mode        RecordMode
	WitnessRef  string
	CausedByIDs []string
}

// RecordBody holds the retained schema-specific payload.
type RecordBody struct {
	Payload map[string]any
}

// RecordView holds path/read-view metadata outside semantic record identity.
type RecordView struct {
	TraceOwnerID string
	OwnerOrdinal int
	ContextRef   string
	KindLabel    string
}

// Record is one retained record: envelope plus body plus optional view.
type Record struct {
	Envelope RecordEnvelope
	Body     RecordBody
	View     *RecordView
}

// RecordShape is a visibility-filtered record without retained payload.
type RecordShape struct {
	Envelope     RecordEnvelope
	View         *RecordView
	HiddenReason string
}

// VisibleRecord is either a full Record or a RecordShape.
type VisibleRecord interface {
	isVisibleRecord()
	GetEnvelope() RecordEnvelope
	GetView() *RecordView
}

func (r Record) isVisibleRecord()            {}
func (r Record) GetEnvelope() RecordEnvelope { return r.Envelope }
func (r Record) GetView() *RecordView        { return r.View }

func (r RecordShape) isVisibleRecord()            {}
func (r RecordShape) GetEnvelope() RecordEnvelope { return r.Envelope }
func (r RecordShape) GetView() *RecordView        { return r.View }

// WitnessBody describes the authority and environment under which records were accepted.
type WitnessBody struct {
	ActorRef                string
	AuthorityRefs           []string
	ActiveBindingRefs       []string
	SemanticEnvironmentRefs []string
	VisibilityPolicyRefs    []string
	ProvenancePolicyRefs    []string
	SubstrateRef            string
	Containment             Containment
}

// RetainedContext is a durable semantic context stamped into retained fact envelopes.
type RetainedContext struct {
	ContextID               string
	ActiveBindingRefs       []string
	CapabilityWitnessRefs   []string
	SemanticEnvironmentRefs []string
	VisibilityPolicyRefs    []string
	SubstrateRef            string
	Containment             Containment
}

// AppendGroup is an owner-local group inside one semantic append transition.
type AppendGroup struct {
	TraceOwnerID    string
	RetainedContext *RetainedContext
	CausalParents   []string
	FactDrafts      []RecordDraft
}

// AppendBatch is an atomic semantic append attempt.
type AppendBatch struct {
	AppendIntentID string
	Groups         []AppendGroup
}

// AppendReceipt is the storage and trace identity allocated by a successful append.
type AppendReceipt struct {
	AppendIntentID  string
	FactIDs         []string
	CommitReceipts  []string
	OwnerRanges     map[string][2]int
	CausalEdges     [][2]string
	ContextReceipts []string
}

// OperationContext is the trace-facing operation context for kernel operations.
//
// Operation records which operation the context was built for, but no
// authorization check consults it yet: ensureAppendAuthorized and
// ensureReadAuthorized look only at the trust mode, authority refs and
// visibility profile. Treat it as descriptive, not enforcing, until plan 03 adds
// materialization and the checks that go with it.
type OperationContext struct {
	ActorRef               string
	Operation              OperationKind
	PresentedAuthorityRefs []string
	SchemaEnvironmentRef   string
	VisibilityProfile      VisibilityProfile
	TrustMode              string
}

// AppendContext is a compatibility write context for append operations.
type AppendContext struct {
	ActorRef             string
	PresentedWitnessRefs []string
	SchemaVersionSet     string
	TrustMode            string
}

func (c AppendContext) ToOperationContext(op OperationKind) OperationContext {
	return OperationContext{
		ActorRef:               c.ActorRef,
		Operation:              op,
		PresentedAuthorityRefs: c.PresentedWitnessRefs,
		SchemaEnvironmentRef:   c.SchemaVersionSet,
		VisibilityProfile:      VisibilityPayload,
		TrustMode:              c.TrustMode,
	}
}

// ReadContext is a compatibility read context for read operations.
type ReadContext struct {
	ActorRef             string
	PresentedWitnessRefs []string
	VisibilityProfile    VisibilityProfile
}

func (c ReadContext) ToOperationContext() OperationContext {
	return OperationContext{
		ActorRef:               c.ActorRef,
		Operation:              OpRead,
		PresentedAuthorityRefs: c.PresentedWitnessRefs,
		VisibilityProfile:      c.VisibilityProfile,
	}
}

// TrustedAppendContext is the default trusted append context for internal operations.
var TrustedAppendContext = AppendContext{
	ActorRef:             "runtime:internal",
	PresentedWitnessRefs: []string{"trusted:internal"},
	SchemaVersionSet:     "shepherd2-slice-a",
	TrustMode:            "internal",
}

// TrustedReadContext is the default trusted read context for internal operations.
var TrustedReadContext = ReadContext{
	ActorRef:             "runtime:internal",
	PresentedWitnessRefs: []string{"trusted:internal"},
	VisibilityProfile:    VisibilityPayload,
}

// Frontier is an immutable read address over retained path entries.
type Frontier struct {
	FrontierID          string
	TargetTraceOwnerID  string
	ThroughFactID       string
	ThroughOwnerOrdinal int
	PublisherOwnerID    string
	CreatedByFactID     string
}

// FrontierSpec is the append input for publishing a frontier.
type FrontierSpec struct {
	FrontierID         string
	TargetTraceOwnerID string
	ThroughFactID      string
	PublisherOwnerID   string
	AppendIntentID     string
	CausedBy           []string
}

// externalAnchorKindFact is the anchor kind for fact anchors. It mirrors the
// Python ABI default (shepherd2.kernel.facts.ExternalAnchor.anchor_kind = "fact")
// so a Go read is shape-identical to the reference instead of emitting "".
const externalAnchorKindFact = "fact"

// ExternalAnchor is a visible reference to a fact outside a slice or hidden by visibility.
type ExternalAnchor struct {
	Ref          string
	AnchorKind   string
	VisibleShape map[string]any
	HiddenReason string
}

// WitnessAnchor is a visible reference to a witness record hidden by visibility.
type WitnessAnchor struct {
	WitnessRef   string
	VisibleShape map[string]any
	HiddenReason string
}

// Slice is a graph-shaped read result over retained trace facts.
type Slice struct {
	Frontier          *Frontier
	VisibilityProfile VisibilityProfile
	ModeFilter        ModeFilter
	FactsByID         map[string]VisibleRecord
	ContextsByID      map[string]RetainedContext
	OwnerPaths        map[string][]string
	// OwnerPathOrder lists the owner paths in the order they were first
	// encountered while reading. FactIDs uses it so the flattened order is
	// deterministic and matches Python, where owner_paths is a dict and
	// TraceSlice.fact_ids() therefore flattens in insertion order. Without it,
	// FactIDs would iterate Go's map and return a different order every call.
	OwnerPathOrder  []string
	CausalEdges     [][2]string
	ExternalAnchors []ExternalAnchor
	ContextAnchors  []ContextAnchor
	WitnessesByID   map[string]VisibleRecord
	WitnessAnchors  []WitnessAnchor
}

// ContextAnchor is a visible reference to a retained context hidden by visibility.
type ContextAnchor struct {
	ContextID    string
	VisibleShape map[string]any
	HiddenReason string
}

// FactIDs returns all fact IDs across all owner paths in the slice.
//
// Paths are visited in the order they were first encountered while reading,
// matching Python's TraceSlice.fact_ids(), which flattens the insertion-ordered
// owner_paths dict. The fallback sorts rather than ranging over the map: a
// hand-built Slice should still produce a stable order, and ranging over a Go map
// does not.
func (s Slice) FactIDs() []string {
	order := s.OwnerPathOrder
	if len(order) != len(s.OwnerPaths) {
		order = make([]string, 0, len(s.OwnerPaths))
		for ref := range s.OwnerPaths {
			order = append(order, ref)
		}
		sort.Strings(order)
	}

	var ids []string
	for _, ref := range order {
		ids = append(ids, s.OwnerPaths[ref]...)
	}
	return ids
}
