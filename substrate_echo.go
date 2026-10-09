package shepherd

// EchoSubstrate (plan 03 §3): a test substrate that turns declarations into
// same-schema captures. It is both the parity fixture for the conformance
// story and the reference for how a minimal substrate behaves — Python's
// EchoSubstrate is the same double.

import "context"

// EchoSubstrate echoes declaration payloads into capture drafts.
type EchoSubstrate struct {
	Ref           string
	Declarations  []string
	SubstrateCont Containment
}

// NewEchoSubstrate builds an echo substrate accepting the given declaration
// schemas, contained by default (matching Python's dataclass default).
func NewEchoSubstrate(ref string, declarationSchemas ...string) *EchoSubstrate {
	return &EchoSubstrate{
		Ref:           ref,
		Declarations:  declarationSchemas,
		SubstrateCont: ContainContained,
	}
}

func (e *EchoSubstrate) SubstrateRef() string         { return e.Ref }
func (e *EchoSubstrate) DeclarationSchemas() []string { return e.Declarations }
func (e *EchoSubstrate) CaptureSchemas() []string     { return e.Declarations }
func (e *EchoSubstrate) Containment() Containment     { return e.SubstrateCont }

// Materialize copies each declaration into a same-schema capture draft,
// caused by the declaration's own record id. The kind label mirrors the
// declaration's fact kind; records read through the store always carry one.
func (e *EchoSubstrate) Materialize(_ context.Context, records []Record) (MaterializationResult, error) {
	captures := make([]RecordDraft, 0, len(records))
	for _, record := range records {
		kind := ""
		if record.View != nil {
			kind = record.View.KindLabel
		}
		captures = append(captures, RecordDraft{
			Mode:            Capture,
			SchemaRef:       record.Envelope.SchemaRef,
			KindLabel:       kind,
			Payload:         copyMap(record.Body.Payload),
			CausedByFactIDs: []string{record.Envelope.RecordID},
		})
	}
	return MaterializationResult{Outcome: MaterializationSuccess, CaptureDrafts: captures}, nil
}
