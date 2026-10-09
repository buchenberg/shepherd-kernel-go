package shepherd

// Substrate contract tests (plan 03 §4): registry behavior, the echo
// declaration->capture pairing, KV payload validation, and the
// WorkspaceSubstrate capability gate with a fake sandbox. The dispatch-level
// cases live in materialize_test.go; these pin the contract one level down,
// where no store is involved.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// --- registry ---

func TestSubstrateRegistryRegisterGet(t *testing.T) {
	reg := NewSubstrateRegistry()
	echo := NewEchoSubstrate("test.echo.v1", "example.write.v1")
	if err := reg.Register(echo); err != nil {
		t.Fatalf("register: %v", err)
	}

	got, err := reg.Get("test.echo.v1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != Substrate(echo) {
		t.Errorf("get returned a different substrate")
	}

	// Re-registering the same substrate is a no-op, mirroring Python's
	// identity comparison.
	if err := reg.Register(echo); err != nil {
		t.Errorf("re-register same instance: %v", err)
	}

	// A different substrate under the same ref fails closed.
	other := NewEchoSubstrate("test.echo.v1", "other.v1")
	if err := reg.Register(other); err == nil {
		t.Error("registering a second substrate under the same ref succeeded")
	}
}

func TestSubstrateRegistryUnknownSubstrate(t *testing.T) {
	reg := NewSubstrateRegistry()
	_, err := reg.Get("missing.substrate.v1")
	if !errors.Is(err, ErrUnknownSubstrate) {
		t.Fatalf("error = %v, want ErrUnknownSubstrate", err)
	}
	var unknown *UnknownSubstrateError
	if !errors.As(err, &unknown) {
		t.Errorf("error = %T, want *UnknownSubstrateError", err)
	}
	if unknown.substrateRef != "missing.substrate.v1" {
		t.Errorf("substrateRef = %q, want missing.substrate.v1", unknown.substrateRef)
	}
}

// nonComparableSubstrate is a value-typed Substrate holding a map, so its
// dynamic type cannot be compared with == without panicking. It exists to
// pin that the registry fails closed instead.
type nonComparableSubstrate struct{ seen map[string]bool }

func (n nonComparableSubstrate) SubstrateRef() string         { return "test.noncomparable.v1" }
func (n nonComparableSubstrate) DeclarationSchemas() []string { return nil }
func (n nonComparableSubstrate) CaptureSchemas() []string     { return nil }
func (n nonComparableSubstrate) Containment() Containment     { return ContainContained }
func (n nonComparableSubstrate) Materialize(context.Context, []Record) (MaterializationResult, error) {
	return MaterializationResult{}, nil
}

func TestSubstrateRegistryNonComparableFailsClosed(t *testing.T) {
	reg := NewSubstrateRegistry()
	s := nonComparableSubstrate{seen: map[string]bool{}}
	if err := reg.Register(s); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Re-registration cannot be identity-checked, so it must fail closed
	// rather than panic on the interface comparison.
	if err := reg.Register(s); err == nil {
		t.Error("re-registering a non-comparable substrate succeeded, want a fail-closed error")
	}
}

// --- echo substrate ---

func TestEchoSubstratePairsDeclarationsWithCaptures(t *testing.T) {
	echo := NewEchoSubstrate("test.echo.v1", "example.write.v1")
	if echo.SubstrateRef() != "test.echo.v1" {
		t.Errorf("ref = %q", echo.SubstrateRef())
	}
	if echo.Containment() != ContainContained {
		t.Errorf("containment = %q, want contained (Python dataclass default)", echo.Containment())
	}
	if !reflect.DeepEqual(echo.CaptureSchemas(), echo.DeclarationSchemas()) {
		t.Errorf("echo capture schemas = %v, want same as declarations %v", echo.CaptureSchemas(), echo.DeclarationSchemas())
	}

	records := []Record{{
		Envelope: RecordEnvelope{RecordID: "sha256:decl", SchemaRef: "example.write.v1", Mode: Declaration},
		Body:     RecordBody{Payload: map[string]any{"path": "note.txt", "text": "hello"}},
		View:     &RecordView{KindLabel: "write"},
	}}
	result, err := echo.Materialize(context.Background(), records)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if result.Outcome != MaterializationSuccess {
		t.Errorf("outcome = %q, want success", result.Outcome)
	}
	if len(result.CaptureDrafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(result.CaptureDrafts))
	}
	draft := result.CaptureDrafts[0]
	if draft.Mode != Capture || draft.SchemaRef != "example.write.v1" || draft.KindLabel != "write" {
		t.Errorf("draft = %+v", draft)
	}
	if !reflect.DeepEqual(draft.CausedByFactIDs, []string{"sha256:decl"}) {
		t.Errorf("caused_by = %v, want the declaration record id", draft.CausedByFactIDs)
	}
	if !reflect.DeepEqual(draft.Payload, map[string]any{"path": "note.txt", "text": "hello"}) {
		t.Errorf("payload = %v", draft.Payload)
	}
}

// --- KV substrate ---

func TestKVSubstratePayloadValidation(t *testing.T) {
	kv, err := NewKVSubstrate(":memory:")
	if err != nil {
		t.Fatalf("NewKVSubstrate: %v", err)
	}
	defer kv.Close()

	if kv.SubstrateRef() != KVSQLiteSubstrateRef {
		t.Errorf("ref = %q, want %q", kv.SubstrateRef(), KVSQLiteSubstrateRef)
	}
	if !reflect.DeepEqual(kv.DeclarationSchemas(), []string{KVPutDeclarationSchema}) {
		t.Errorf("declarations = %v", kv.DeclarationSchemas())
	}
	if !reflect.DeepEqual(kv.CaptureSchemas(), []string{KVPutCaptureSchema}) {
		t.Errorf("captures = %v", kv.CaptureSchemas())
	}

	cases := []struct {
		name   string
		record Record
	}{
		{"missing key", Record{Body: RecordBody{Payload: map[string]any{"value": jsonNumber(t, 1)}}}},
		{"non-string key", Record{Body: RecordBody{Payload: map[string]any{"key": jsonNumber(t, 5), "value": jsonNumber(t, 1)}}}},
		{"missing value", Record{Body: RecordBody{Payload: map[string]any{"key": "k"}}}},
	}
	for _, tc := range cases {
		_, err := kv.Materialize(context.Background(), []Record{tc.record})
		if err == nil {
			t.Errorf("%s: materialize succeeded, want error", tc.name)
			continue
		}
		if _, ok := err.(*SubstrateError); !ok {
			t.Errorf("%s: error = %T (%v), want *SubstrateError", tc.name, err, err)
		}
	}
}

func TestKVSubstrateUpsertAndRoundTrip(t *testing.T) {
	kv, err := NewKVSubstrate(filepath.Join(t.TempDir(), "kv.sqlite"))
	if err != nil {
		t.Fatalf("NewKVSubstrate: %v", err)
	}
	defer kv.Close()

	apply := func(key string, value any) {
		t.Helper()
		result, err := kv.Materialize(context.Background(), []Record{{
			Envelope: RecordEnvelope{RecordID: "sha256:" + key, Mode: Declaration},
			Body:     RecordBody{Payload: map[string]any{"key": key, "value": value}},
		}})
		if err != nil {
			t.Fatalf("materialize %q: %v", key, err)
		}
		if result.Outcome != MaterializationSuccess || len(result.CaptureDrafts) != 1 {
			t.Fatalf("result = %+v", result)
		}
	}

	apply("answer", map[string]any{"n": jsonNumber(t, 42)})
	apply("answer", map[string]any{"n": jsonNumber(t, 43)}) // upsert overwrites

	got, err := kv.Get("answer")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(got, map[string]any{"n": jsonNumber(t, 43)}) {
		t.Errorf("kv[answer] = %v, want {n: 43}", got)
	}

	missing, err := kv.Get("absent")
	if err != nil || missing != nil {
		t.Errorf("kv[absent] = %v, %v; want nil, nil", missing, err)
	}

	// Value identity survives the JSON round trip: 42 stays an int, so a
	// capture digest over the value matches the stored value.
	apply("num", jsonNumber(t, 7))
	if got, _ := kv.Get("num"); got != any(jsonNumber(t, 7)) {
		t.Errorf("kv[num] = %v (%T), want json.Number 7", got, got)
	}
}

// --- workspace substrate ---

// fakeSandbox is an in-memory Sandbox for capability-gating tests. It is
// deliberately configurable in its capabilities so the same fake covers the
// full-matrix backend and the diff-only backend.
type fakeSandbox struct {
	backend string
	caps    SandboxCapabilities
	files   map[string][]byte
	execs   []ExecRequest
	// execResult is returned for every Exec call.
	execResult ExecResult
	// writeErr, when set, fails every WriteFile call.
	writeErr error
	// writeHook, when set, can fail individual writes by path.
	writeHook func(path string) error
	// execDeadlines records, per Exec call, whether the ctx carried a
	// deadline — the observable effect of a declared timeout_ms.
	execDeadlines []bool
}

func (f *fakeSandbox) Backend() string { return f.backend }
func (f *fakeSandbox) Capabilities() SandboxCapabilities {
	return f.caps
}
func (f *fakeSandbox) Create(context.Context, SandboxSpec) error { return nil }
func (f *fakeSandbox) Destroy(context.Context) error             { return nil }
func (f *fakeSandbox) Capture(context.Context) (WorkspaceState, error) {
	return WorkspaceState{Backend: f.backend}, nil
}
func (f *fakeSandbox) Apply(context.Context, WorkspaceState) error { return nil }
func (f *fakeSandbox) Diff(context.Context, WorkspaceState, int) (string, []string, error) {
	return "", nil, nil
}
func (f *fakeSandbox) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	f.execs = append(f.execs, req)
	_, hasDeadline := ctx.Deadline()
	f.execDeadlines = append(f.execDeadlines, hasDeadline)
	return f.execResult, nil
}
func (f *fakeSandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	if data, ok := f.files[path]; ok {
		return data, nil
	}
	return nil, os.ErrNotExist
}
func (f *fakeSandbox) WriteFile(_ context.Context, path string, data []byte, _ fs.FileMode) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.writeHook != nil {
		if err := f.writeHook(path); err != nil {
			return err
		}
	}
	f.files[path] = data
	return nil
}

func fullCapsFake() *fakeSandbox {
	return &fakeSandbox{
		backend: "fake",
		caps: SandboxCapabilities{
			Lifecycle:   true,
			Exec:        true,
			FileIO:      true,
			Diff:        true,
			Isolated:    true,
			Containment: ContainFull,
		},
		files: map[string][]byte{},
	}
}

func workspaceDecl(schema, id string, payload map[string]any) Record {
	return Record{
		Envelope: RecordEnvelope{RecordID: id, SchemaRef: schema, Mode: Declaration},
		Body:     RecordBody{Payload: payload},
		View:     &RecordView{KindLabel: "decl"},
	}
}

func TestWorkspaceSubstrateCapabilityGatingFailsCleanly(t *testing.T) {
	// The git-in-place story: no FileIO, no Exec. The write declaration must
	// fail cleanly — nothing applied, no captures, no partial append.
	gitLike := &fakeSandbox{
		backend: "git",
		caps: SandboxCapabilities{
			Diff:        true,
			Isolated:    false,
			Containment: ContainUncontained,
		},
		files: map[string][]byte{},
	}
	ws := NewWorkspaceSubstrate(gitLike)

	content := base64.StdEncoding.EncodeToString([]byte("hello"))
	result, err := ws.Materialize(context.Background(), []Record{
		workspaceDecl(SchemaWorkspaceFileWrite, "sha256:w1", map[string]any{
			"path": "note.txt", "content_b64": content,
		}),
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if result.Outcome != MaterializationCleanFailure {
		t.Errorf("outcome = %q, want clean_failure", result.Outcome)
	}
	if result.FailureReason == "" {
		t.Error("clean failure carried no reason")
	}
	if len(result.CaptureDrafts) != 0 {
		t.Errorf("captures = %d, want none on a gated batch", len(result.CaptureDrafts))
	}
	if len(gitLike.files) != 0 {
		t.Errorf("files = %v, want nothing written", gitLike.files)
	}

	// Same for exec on a backend without it.
	result, err = ws.Materialize(context.Background(), []Record{
		workspaceDecl(SchemaWorkspaceExec, "sha256:e1", map[string]any{"command": "ls"}),
	})
	if err != nil {
		t.Fatalf("materialize exec: %v", err)
	}
	if result.Outcome != MaterializationCleanFailure || len(result.CaptureDrafts) != 0 {
		t.Errorf("result = %+v, want clean_failure with no captures", result)
	}
	if len(gitLike.execs) != 0 {
		t.Errorf("execs = %d, want none", len(gitLike.execs))
	}
}

func TestWorkspaceSubstrateRoundTripsFileWrites(t *testing.T) {
	sb := fullCapsFake()
	ws := NewWorkspaceSubstrate(sb)
	if ws.Containment() != ContainFull {
		t.Errorf("containment = %q, want the sandbox's own (full)", ws.Containment())
	}

	content := "hello, workspace"
	result, err := ws.Materialize(context.Background(), []Record{
		workspaceDecl(SchemaWorkspaceFileWrite, "sha256:w1", map[string]any{
			"path":        "notes/greeting.txt",
			"content_b64": base64.StdEncoding.EncodeToString([]byte(content)),
			"perm":        json.Number("420"),
		}),
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if result.Outcome != MaterializationSuccess {
		t.Fatalf("outcome = %q, want success (%s)", result.Outcome, result.FailureReason)
	}
	if len(result.CaptureDrafts) != 1 {
		t.Fatalf("captures = %d, want 1", len(result.CaptureDrafts))
	}

	got, err := sb.ReadFile(context.Background(), "notes/greeting.txt")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != content {
		t.Errorf("file content = %q, want %q", got, content)
	}

	draft := result.CaptureDrafts[0]
	if draft.SchemaRef != SchemaWorkspaceFileWriteApplied || draft.Mode != Capture {
		t.Errorf("draft = %+v", draft)
	}
	if draft.Payload["path"] != "notes/greeting.txt" {
		t.Errorf("payload path = %v", draft.Payload["path"])
	}
	if draft.Payload["digest"] != sha256Hex(content) {
		t.Errorf("payload digest = %v, want sha256 of content", draft.Payload["digest"])
	}
	if !reflect.DeepEqual(draft.CausedByFactIDs, []string{"sha256:w1"}) {
		t.Errorf("caused_by = %v", draft.CausedByFactIDs)
	}

	anchor := map[string]any{
		"kind":          "workspace_file",
		"path":          "notes/greeting.txt",
		"substrate_ref": WorkspaceSubstrateRef,
		"backend":       "fake",
	}
	if len(result.WorldSideAnchors) != 1 || !reflect.DeepEqual(result.WorldSideAnchors[0], anchor) {
		t.Errorf("anchors = %v, want [%v]", result.WorldSideAnchors, anchor)
	}
}

// TestWorkspaceSubstrateWritesEmptyFile pins that empty content is a write,
// not a missing payload: "" is base64 for zero bytes, and rejecting it would
// make creating an empty file inexpressible.
func TestWorkspaceSubstrateWritesEmptyFile(t *testing.T) {
	sb := fullCapsFake()
	ws := NewWorkspaceSubstrate(sb)

	result, err := ws.Materialize(context.Background(), []Record{
		workspaceDecl(SchemaWorkspaceFileWrite, "sha256:empty", map[string]any{
			"path":        "empty.txt",
			"content_b64": "",
		}),
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if result.Outcome != MaterializationSuccess {
		t.Fatalf("outcome = %q, want success (%s)", result.Outcome, result.FailureReason)
	}
	got, err := sb.ReadFile(context.Background(), "empty.txt")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("file content = %q, want empty", got)
	}
	if digest := result.CaptureDrafts[0].Payload["digest"]; digest != sha256Hex("") {
		t.Errorf("digest = %v, want the sha256 of empty content", digest)
	}
}

func TestWorkspaceSubstrateRecordsExecOutcome(t *testing.T) {
	sb := fullCapsFake()
	sb.execResult = ExecResult{ExitCode: 3, Stdout: "out", Stderr: "err"}
	ws := NewWorkspaceSubstrate(sb)

	// A nonzero exit code is an observed result, not a substrate failure.
	// An empty-string argument is a legitimate argument, not a malformed
	// entry; only non-strings are rejected.
	result, err := ws.Materialize(context.Background(), []Record{
		workspaceDecl(SchemaWorkspaceExec, "sha256:e1", map[string]any{
			"command":    "grep",
			"args":       []any{"-c", "needle", ""},
			"cwd":        "/work",
			"timeout_ms": json.Number("500"),
		}),
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if result.Outcome != MaterializationSuccess {
		t.Fatalf("outcome = %q, want success even with a nonzero exit code", result.Outcome)
	}
	if len(sb.execs) != 1 {
		t.Fatalf("execs = %d, want 1", len(sb.execs))
	}
	if sb.execs[0].Command != "grep" || !reflect.DeepEqual(sb.execs[0].Args, []string{"-c", "needle", ""}) ||
		sb.execs[0].Cwd != "/work" || sb.execs[0].Timeout.String() != "500ms" {
		t.Errorf("exec request = %+v", sb.execs[0])
	}
	// The declared timeout is enforced by the substrate via the context, so
	// it reaches every backend regardless of whether the backend reads
	// Exe