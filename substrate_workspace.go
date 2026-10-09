package shepherd

// WorkspaceSubstrate (plan 03 §3, Go-native bridge): a Substrate over a
// Sandbox, so recorded intents can actually reach a workspace (git or
// containerd) through the same declare -> capture rhythm as the kernel.
//
// This substrate has no Python counterpart — it is the deliberate Go-native
// payoff of plan 03 — so its schema vocabulary is defined here:
//
//	declarations
//	  workspace.file.write.v1  {path, content_b64, perm?}
//	  workspace.exec.v1        {command, args, cwd?, env?, timeout_ms?}
//	captures
//	  workspace.file.write.applied.v1  {path, digest}
//	  workspace.exec.applied.v1         {command, exit_code, stdout_digest, stderr_digest}
//
// substrate_ref identifies the KIND ("workspace.sandbox.v1"); the backend
// goes in the receipt's world-side anchors, not the ref. Captures cite their
// declaration, so an applied capture is always traceable to the intent that
// caused it.
//
// workspace.file.delete.v1 is deliberately NOT in the vocabulary (recorded
// decision): the Sandbox contract has no delete operation, and the only
// thing WriteFile could do — truncate to empty — is not a delete. Recording
// "delete applied" over a truncation would write a trace that lies about
// the world. When the Sandbox interface grows a real delete (a
// yaah-coordinated change), the schema can light up without breaking
// anything: dispatch selects on declaration schema, so an unknown schema
// already fails loudly as unsupported.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"
)

const (
	// WorkspaceSubstrateRef identifies the workspace substrate kind.
	WorkspaceSubstrateRef = "workspace.sandbox.v1"

	// SchemaWorkspaceFileWrite declares an intent to write a file.
	SchemaWorkspaceFileWrite = "workspace.file.write.v1"
	// SchemaWorkspaceExec declares an intent to run a command.
	SchemaWorkspaceExec = "workspace.exec.v1"

	// SchemaWorkspaceFileWriteApplied is emitted when a write landed.
	SchemaWorkspaceFileWriteApplied = "workspace.file.write.applied.v1"
	// SchemaWorkspaceExecApplied is emitted when an exec completed.
	SchemaWorkspaceExecApplied = "workspace.exec.applied.v1"
)

// WorkspaceSubstrate materializes file and exec intents into a Sandbox.
type WorkspaceSubstrate struct {
	sandbox Sandbox
}

// NewWorkspaceSubstrate bridges a Sandbox into the substrate contract.
func NewWorkspaceSubstrate(sb Sandbox) *WorkspaceSubstrate {
	return &WorkspaceSubstrate{sandbox: sb}
}

// Sandbox returns the bridged sandbox.
func (w *WorkspaceSubstrate) Sandbox() Sandbox { return w.sandbox }

func (w *WorkspaceSubstrate) SubstrateRef() string { return WorkspaceSubstrateRef }

func (w *WorkspaceSubstrate) DeclarationSchemas() []string {
	return []string{SchemaWorkspaceFileWrite, SchemaWorkspaceExec}
}

func (w *WorkspaceSubstrate) CaptureSchemas() []string {
	return []string{SchemaWorkspaceFileWriteApplied, SchemaWorkspaceExecApplied}
}

// Containment reports what the bridged sandbox claims, so the witness tells
// the truth about git-in-place (uncontained) versus isolated backends.
func (w *WorkspaceSubstrate) Containment() Containment {
	return w.sandbox.Capabilities().Containment
}

// Materialize applies each declaration to the sandbox and emits one applied
// capture per declaration.
//
// Capability gating happens before the first mutation: a batch that needs a
// capability the backend does not have fails cleanly with nothing applied —
// the git backend's honest-failure story, never a partial append.
//
// Once a sandbox call has been made for a record, a failure of that call is
// reported as split_state even when no capture was emitted and nothing else
// landed: backends write files in chunks and run commands to completion
// before teardown, so a failed call may still have changed the world, and
// claiming clean_failure would assert a world state that was not verified.
// clean_failure is therefore reserved for failures that provably touched
// nothing — malformed declarations, unknown schemas, capability gates. The
// captures and anchors in a split_state receipt cover only the records that
// verifiably landed, and the failure reason names the record whose
// world-side state is uncertain.
//
// An exec that runs to completion is a success even with a nonzero exit
// code: the exit code is the observed result, recorded in the capture. Only
// infrastructure failures (the command could not run, a write failed) are
// substrate failures.
func (w *WorkspaceSubstrate) Materialize(ctx context.Context, records []Record) (MaterializationResult, error) {
	caps := w.sandbox.Capabilities()

	needsFileIO, needsExec := false, false
	for _, record := range records {
		switch record.Envelope.SchemaRef {
		case SchemaWorkspaceFileWrite:
			needsFileIO = true
		case SchemaWorkspaceExec:
			needsExec = true
		}
	}
	if needsFileIO && !caps.FileIO {
		return MaterializationResult{
			Outcome:       MaterializationCleanFailure,
			FailureReason: fmt.Sprintf("sandbox backend %q does not support file IO", w.sandbox.Backend()),
		}, nil
	}
	if needsExec && !caps.Exec {
		return MaterializationResult{
			Outcome:       MaterializationCleanFailure,
			FailureReason: fmt.Sprintf("sandbox backend %q does not support exec", w.sandbox.Backend()),
		}, nil
	}

	captures := make([]RecordDraft, 0, len(records))
	anchors := make([]map[string]any, 0, len(records))
	for _, record := range records {
		switch record.Envelope.SchemaRef {
		case SchemaWorkspaceFileWrite:
			draft, anchor, err := w.applyWrite(ctx, record)
			if err != nil {
				return splitOrClean(captures, anchors, record, err), nil
			}
			captures = append(captures, draft)
			anchors = append(anchors, anchor)
		case SchemaWorkspaceExec:
			draft, anchor, err := w.applyExec(ctx, record)
			if err != nil {
				return splitOrClean(captures, anchors, record, err), nil
			}
			captures = append(captures, draft)
			anchors = append(anchors, anchor)
		default:
			return splitOrClean(captures, anchors, record, &SubstrateError{
				fmt.Sprintf("workspace substrate does not accept schema %q", record.Envelope.SchemaRef),
			}), nil
		}
	}
	return MaterializationResult{
		Outcome:          MaterializationSuccess,
		CaptureDrafts:    captures,
		WorldSideAnchors: anchors,
	}, nil
}

// splitOrClean turns a mid-batch record failure into the honest outcome.
//
// The dividing line is whether the world was touched, not whether captures
// exist: a failure *after* a sandbox call was made may have partially applied
// (the containerd backend writes files in chunks through its exec shellouts,
// so a write can fail with earlier chunks already on disk; an exec can run to
// completion and fail during teardown), so reporting clean_failure would
// claim a world state that was not verified. Such failures are split_state
// even with zero captures — the captures cover only what verifiably landed,
// and the failure reason names the record whose world-side state is
// uncertain.
//
// A failure before any sandbox call for that record — a malformed
// declaration, an unknown schema — touched nothing, so it is clean_failure
// when nothing else landed either. The landed records keep their anchors as
// well as their captures, so the receipt describes both halves of what
// landed.
func splitOrClean(captures []RecordDraft, anchors []map[string]any, failed Record, err error) MaterializationResult {
	reason := fmt.Sprintf("record %s: %v", failed.Envelope.RecordID, err)
	var world *worldTouchedError
	if len(captures) > 0 || errors.As(err, &world) {
		return MaterializationResult{
			Outcome:          MaterializationSplitState,
			CaptureDrafts:    captures,
			WorldSideAnchors: anchors,
			FailureReason:    reason,
		}
	}
	return MaterializationResult{Outcome: MaterializationCleanFailure, FailureReason: reason}
}

// worldTouchedError marks a failure that occurred after a sandbox call was
// made for the failing record: the world may have changed even though no
// capture was emitted, so the batch must not be reported as a clean failure.
type worldTouchedError struct{ err error }

func (e *worldTouchedError) Error() string { return e.err.Error() }
func (e *worldTouchedError) Unwrap() error { return e.err }

func (w *WorkspaceSubstrate) applyWrite(ctx context.Context, record Record) (RecordDraft, map[string]any, error) {
	path, _ := record.Body.Payload["path"].(string)
	if path == "" {
		return RecordDraft{}, nil, &SubstrateError{"workspace file write requires a non-empty path"}
	}
	contentB64, ok := record.Body.Payload["content_b64"]
	if !ok {
		return RecordDraft{}, nil, &SubstrateError{fmt.Sprintf("workspace file write %q requires content_b64", path)}
	}
	contentB64String, ok := contentB64.(string)
	if !ok {
		return RecordDraft{}, nil, &SubstrateError{fmt.Sprintf("workspace file write %q: content_b64 must be a base64 string", path)}
	}
	// An empty string is the encoding of empty content: a write may create an
	// empty file, and rejecting it would make that intent inexpressible.
	content, err := base64.StdEncoding.DecodeString(contentB64String)
	if err != nil {
		return RecordDraft{}, nil, &SubstrateError{fmt.Sprintf("workspace file write %q: content_b64 is not valid base64", path)}
	}
	perm := fs.FileMode(0o644)
	if raw, ok := record.Body.Payload["perm"]; ok {
		perm, err = fileModeFromPayload(raw)
		if err != nil {
			return RecordDraft{}, nil, &SubstrateError{fmt.Sprintf("workspace file write %q: %v", path, err)}
		}
	}

	if err := w.sandbox.WriteFile(ctx, path, content, perm); err != nil {
		// The write may have partially landed (chunked shellouts), so this
		// is a world-touched failure: splitOrClean reports split_state.
		return RecordDraft{}, nil, &worldTouchedError{fmt.Errorf("write %q: %w", path, err)}
	}

	sum := sha256.Sum256(content)
	draft := RecordDraft{
		Mode:            Capture,
		SchemaRef:       SchemaWorkspaceFileWriteApplied,
		KindLabel:       "workspace_file_write_applied",
		Payload:         map[string]any{"path": path, "digest": hex.EncodeToString(sum[:])},
		CausedByFactIDs: []string{record.Envelope.RecordID},
	}
	anchor := map[string]any{
		"kind":          "workspace_file",
		"path":          path,
		"substrate_ref": w.SubstrateRef(),
		"backend":       w.sandbox.Backend(),
	}
	return draft, anchor, nil
}

func (w *WorkspaceSubstrate) applyExec(ctx context.Context, record Record) (RecordDraft, map[string]any, error) {
	command, _ := record.Body.Payload["command"].(string)
	if command == "" {
		return RecordDraft{}, nil, &SubstrateError{"workspace exec requires a command"}
	}
	req := ExecRequest{Command: command}
	if raw, ok := record.Body.Payload["args"]; ok {
		args, err := stringSliceFromPayload(raw, "args")
		if err != nil {
			return RecordDraft{}, nil, &SubstrateError{fmt.Sprintf("workspace exec %q: %v", command, err)}
		}
		req.Args = args
	}
	if raw, ok := record.Body.Payload["cwd"]; ok {
		cwd, ok := raw.(string)
		if !ok {
			return RecordDraft{}, nil, &SubstrateError{fmt.Sprintf("workspace exec %q: cwd must be a string", command)}
		}
		req.Cwd = cwd
	}
	if raw, ok := record.Body.Payload["env"]; ok {
		env, err := stringMapFromPayload(raw, "env")
		if err != nil {
			return RecordDraft{}, nil, &SubstrateError{fmt.Sprintf("workspace exec %q: %v", command, err)}
		}
		req.Env = env
	}
	if raw, ok := record.Body.Payload["timeout_ms"]; ok {
		ms, err := payloadInt(raw, "timeout_ms")
		if err != nil {
			return RecordDraft{}, nil, &SubstrateError{fmt.Sprintf("workspace exec %q: %v", command, err)}
		}
		if ms < 0 {
			return RecordDraft{}, nil, &SubstrateError{fmt.Sprintf("workspace exec %q: timeout_ms must be non-negative", command)}
		}
		req.Timeout = time.Duration(ms) * time.Millisecond
	}
	// The declared timeout is enforced here rather than delegated: a sandbox
	// backend is only required to honor the context it is given, and the
	// containerd backend observes ctx without reading ExecRequest.Timeout.
	// Deriving a deadline-bearing context makes the declaration mean the
	// same thing for every backend.
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	result, err := w.sandbox.Exec(ctx, req)
	if err != nil {
		// The command may have run to completion and failed during teardown,
		// so this is a world-touched failure: splitOrClean reports
		// split_state.
		return RecordDraft{}, nil, &worldTouchedError{fmt.Errorf("exec %q: %w", command, err)}
	}

	stdoutSum := sha256.Sum256([]byte(result.Stdout))
	stderrSum := sha256.Sum256([]byte(result.Stderr))
	draft := RecordDraft{
		Mode:      Capture,
		SchemaRef: SchemaWorkspaceExecApplied,
		KindLabel: "workspace_exec_applied",
		Payload: map[string]any{
			"command":       command,
			"exit_code":     result.ExitCode,
			"stdout_digest": hex.EncodeToString(stdoutSum[:]),
			"stderr_digest": hex.EncodeToString(stderrSum[:]),
		},
		CausedByFactIDs: []string{record.Envelope.RecordID},
	}
	anchor := map[string]any{
		"kind":          "workspace_exec",
		"command":       command,
		"substrate_ref": w.SubstrateRef(),
		"backend":       w.sandbox.Backend(),
	}
	return draft, anchor, nil
}

// fileModeFromPayload accepts the JSON shapes a declaration can carry:
// a number (json.Number from a stored record, int from a hand-built draft)
// or a string like "0755".
func fileModeFromPayload(raw any) (fs.FileMode, error) {
	switch v := raw.(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("perm: %w", err)
		}
		return fs.FileMode(n), nil
	case int:
		return fs.FileMode(v), nil
	case float64:
		return fs.FileMode(int64(v)), nil
	case string:
		var n int64
		if _, err := fmt.Sscanf(v, "%o", &n); err != nil {
			return 0, fmt.Errorf("perm %q is not octal", v)
		}
		return fs.FileMode(n), nil
	default:
		return 0, fmt.Errorf("perm must be a number or an octal string")
	}
}

// payloadInt reads an int field from a payload that may hold json.Number
// (stored record) or a plain number (hand-built draft).
func payloadInt(raw any, field string) (int, error) {
	switch v := raw.(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, fmt.Errorf("%s: %w", field, err)
		}
		return int(n), nil
	case int:
		return v, nil
	case float64:
		return int(v), nil
	default:
		return 0, fmt.Errorf("%s must be a number", field)
	}
}

func stringSliceFromPayload(raw any, field string) ([]string, error) {
	switch v := raw.(type) {
	case []string:
		return v, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s entries must be strings", field)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be a list of strings", field)
	}
}

func stringMapFromPayload(raw any, field string) (map[string]string, error) {
	switch v := raw.(type) {
	case map[string]string:
		return v, nil
	case map[string]any:
		out := make(map[string]string, len(v))
		for k, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s values must be strings", field)
			}
			out[k] = s
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be a map of strings", field)
	}
}
