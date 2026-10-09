package shepherd

// Minimal vNext substrate contract and registry (plan 03 §1), mirroring
// shepherd2/src/shepherd2/vnext/substrates.py.
//
// vNext is outside the ABI freeze, so the Go shape has latitude — but the
// outcome vocabulary, the substrate refs and the schema names below are
// cross-language contract surface: Python's test_materialize.py and the Go
// materialize vectors pin them.
//
// The outcome vocabulary mirrors the reference exactly: "success",
// "clean_failure", "split_state". The plan sketch's "succeeded | failed |
// partially_applied" was a guess made before the reference was read; where they
// disagree, the Python implementation decides (PARITY-PLAN §2, vectors over
// prose).

import (
	"context"
	"errors"
	"fmt"
)

// MaterializationOutcome is the result vocabulary a substrate reports.
type MaterializationOutcome string

const (
	// MaterializationSuccess means every declaration was applied and every
	// intended capture is present in the result.
	MaterializationSuccess MaterializationOutcome = "success"
	// MaterializationCleanFailure means nothing was applied and the failure
	// reason explains why. No captures are emitted.
	MaterializationCleanFailure MaterializationOutcome = "clean_failure"
	// MaterializationSplitState means some declarations were applied and
	// others were not; the captures describe the part that landed.
	MaterializationSplitState MaterializationOutcome = "split_state"
)

// ErrUnknownSubstrate is the sentinel behind UnknownSubstrateError, so
// callers can errors.Is(err, ErrUnknownSubstrate) without matching strings.
var ErrUnknownSubstrate = errors.New("shepherd: unknown substrate")

// SubstrateError is raised for substrate registration or dispatch failures.
// It mirrors Python's SubstrateError (RuntimeError subclass).
type SubstrateError struct {
	msg string
}

func (e *SubstrateError) Error() string { return e.msg }

// UnknownSubstrateError is raised when materialize dispatch names a
// substrate that is not registered.
type UnknownSubstrateError struct {
	substrateRef string
}

func (e *UnknownSubstrateError) Error() string {
	return fmt.Sprintf("unknown substrate: %s", e.substrateRef)
}

func (e *UnknownSubstrateError) Is(target error) bool { return target == ErrUnknownSubstrate }

// MaterializationResult is the substrate-produced result before the kernel
// appends captures. Substrates never append themselves: dispatch owns the
// append (materialize.go), which keeps substrates testable without a store.
type MaterializationResult struct {
	Outcome          MaterializationOutcome
	CaptureDrafts    []RecordDraft
	FailureReason    string
	WorldSideAnchors []map[string]any
}

// MaterializationReceipt is the kernel materialize transition receipt handed
// back to the caller and stored in the completed-intent ledger. Its field set
// mirrors Python's MaterializationReceipt exactly, because the ledger's
// receipt_json is serialized from it and must stay cross-readable.
type MaterializationReceipt struct {
	Outcome           MaterializationOutcome
	SubstrateRef      string
	TargetRecordIDs   []string
	ProducedRecordIDs []string
	FailureReason     string
	WorldSideAnchors  []map[string]any
}

// Substrate is a registered unit of declaration materialization.
//
// DeclarationSchemas and CaptureSchemas are treated as sets; a duplicate
// entry is harmless. Materialize receives the selected declaration records
// (already retained in the store, addressed by the request) and returns
// capture drafts; it never appends them itself.
type Substrate interface {
	// SubstrateRef identifies the KIND of substrate ("kv.sqlite.local.v1",
	// "workspace.sandbox.v1"). Backend details belong in capture payloads.
	SubstrateRef() string
	// DeclarationSchemas lists the intent schemas this substrate accepts.
	DeclarationSchemas() []string
	// CaptureSchemas lists the receipt schemas it emits.
	CaptureSchemas() []string
	// Containment is the witness-level containment classification stamped
	// into the retained context of the materialization captures.
	Containment() Containment
	// Materialize applies the declarations and returns capture drafts.
	Materialize(ctx context.Context, records []Record) (MaterializationResult, error)
}

// SubstrateRegistry is a fail-closed process-local substrate registry.
// Registering a second, different substrate under an existing ref is an
// error; registering the same substrate twice is a no-op (Python compares
// identity, Go compares interface equality, which is identity for pointers).
type SubstrateRegistry struct {
	substrates map[string]Substrate
}

// NewSubstrateRegistry returns an empty registry.
func NewSubstrateRegistry() *SubstrateRegistry {
	return &SubstrateRegistry{substrates: map[string]Substrate{}}
}

// Register adds a substrate under its ref.
func (r *SubstrateRegistry) Register(s Substrate) error {
	if existing, ok := r.substrates[s.SubstrateRef()]; ok && existing != s {
		return &SubstrateError{fmt.Sprintf("substrate %q is already registered", s.SubstrateRef())}
	}
	r.substrates[s.SubstrateRef()] = s
	return nil
}

// Get returns the substrate registered under ref.
func (r *SubstrateRegistry) Get(substrateRef string) (Substrate, error) {
	if s, ok := r.substrates[substrateRef]; ok {
		return s, nil
	}
	return nil, &UnknownSubstrateError{substrateRef: substrateRef}
}
