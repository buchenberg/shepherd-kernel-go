package shepherd

import (
	"encoding/json"
	"os"
	"testing"
)

func loadGolden(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("testdata/kernel_abi_v0.json")
	if err != nil {
		t.Fatalf("failed to load golden vectors: %v", err)
	}
	var golden map[string]any
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatalf("failed to parse golden vectors: %v", err)
	}
	return golden
}

func getNested(t *testing.T, m map[string]any, keys ...string) any {
	t.Helper()
	current := any(m)
	for _, k := range keys {
		obj, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("expected map at key %q, got %T", k, current)
		}
		current = obj[k]
	}
	return current
}

func getString(t *testing.T, m map[string]any, keys ...string) string {
	t.Helper()
	v := getNested(t, m, keys...)
	s, ok := v.(string)
	if !ok {
		t.Fatalf("expected string at %v, got %T", keys, v)
	}
	return s
}

func getMap(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	v := getNested(t, m, keys...)
	obj, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected map at %v, got %T", keys, v)
	}
	return obj
}

func TestKernelABIVersionsAreFrozen(t *testing.T) {
	golden := loadGolden(t)

	if ABIVersion != "shepherd.kernel.abi.v0" {
		t.Errorf("ABI_VERSION = %q, want %q", ABIVersion, "shepherd.kernel.abi.v0")
	}
	if CanonicalVersion != "shepherd.kernel.canonical.v2" {
		t.Errorf("CANONICAL_VERSION = %q, want %q", CanonicalVersion, "shepherd.kernel.canonical.v2")
	}
	if getString(t, golden, "abi_version") != ABIVersion {
		t.Error("golden abi_version does not match constant")
	}
	if getString(t, golden, "canonical_version") != CanonicalVersion {
		t.Error("golden canonical_version does not match constant")
	}
}

func TestRootWitnessBodyMatchesGolden(t *testing.T) {
	golden := loadGolden(t)
	body := RootWitnessBody()

	bodyJSON, _ := CanonicalJSONBytes(body)
	goldenBody := getMap(t, golden, "root_witness", "body")
	goldenJSON, _ := CanonicalJSONBytes(goldenBody)

	if string(bodyJSON) != string(goldenJSON) {
		t.Errorf("root witness body mismatch:\n  got:  %s\n  want: %s", bodyJSON, goldenJSON)
	}
}

func TestRootWitnessBodyDigestMatchesGolden(t *testing.T) {
	golden := loadGolden(t)
	expected := getString(t, golden, "root_witness", "body_digest")

	digest, err := RootWitnessBodyDigest()
	if err != nil {
		t.Fatalf("RootWitnessBodyDigest() error: %v", err)
	}
	if digest != expected {
		t.Errorf("root witness body digest = %q, want %q", digest, expected)
	}
}

func TestRootWitnessRecordIDMatchesGolden(t *testing.T) {
	golden := loadGolden(t)
	expected := getString(t, golden, "root_witness", "record_id")

	recordID, err := RootWitnessRecordID()
	if err != nil {
		t.Fatalf("RootWitnessRecordID() error: %v", err)
	}
	if recordID != expected {
		t.Errorf("root witness record ID = %q, want %q", recordID, expected)
	}
}

func TestRootWitnessRecordInputMatchesGolden(t *testing.T) {
	golden := loadGolden(t)
	expected := getString(t, golden, "root_witness", "record_id")

	input := getMap(t, golden, "root_witness", "record_input")
	digest, err := CanonicalDigest(input)
	if err != nil {
		t.Fatalf("CanonicalDigest() error: %v", err)
	}
	if digest != expected {
		t.Errorf("canonical digest of golden record_input = %q, want %q", digest, expected)
	}
}

func TestOrdinaryWitnessBodyDigestMatchesGolden(t *testing.T) {
	golden := loadGolden(t)
	expected := getString(t, golden, "ordinary_witness", "body_digest")
	body := getMap(t, golden, "ordinary_witness", "body")
	schemaRef := getString(t, golden, "ordinary_witness", "schema_ref")

	digest, err := WitnessBodyDigest(schemaRef, body)
	if err != nil {
		t.Fatalf("WitnessBodyDigest() error: %v", err)
	}
	if digest != expected {
		t.Errorf("ordinary witness body digest = %q, want %q", digest, expected)
	}
}

func TestOrdinaryWitnessRecordIDMatchesGolden(t *testing.T) {
	golden := loadGolden(t)
	expected := getString(t, golden, "ordinary_witness", "record_id")
	input := getMap(t, golden, "ordinary_witness", "record_input")

	digest, err := CanonicalDigest(input)
	if err != nil {
		t.Fatalf("CanonicalDigest() error: %v", err)
	}
	if digest != expected {
		t.Errorf("ordinary witness record ID = %q, want %q", digest, expected)
	}
}

func TestAlternateWitnessBodyDigestMatchesGolden(t *testing.T) {
	golden := loadGolden(t)
	expected := getString(t, golden, "alternate_witness", "body_digest")
	body := getMap(t, golden, "alternate_witness", "body")
	schemaRef := getString(t, golden, "alternate_witness", "schema_ref")

	digest, err := WitnessBodyDigest(schemaRef, body)
	if err != nil {
		t.Fatalf("WitnessBodyDigest() error: %v", err)
	}
	if digest != expected {
		t.Errorf("alternate witness body digest = %q, want %q", digest, expected)
	}
}

func TestAlternateWitnessRecordIDMatchesGolden(t *testing.T) {
	golden := loadGolden(t)
	expected := getString(t, golden, "alternate_witness", "record_id")
	input := getMap(t, golden, "alternate_witness", "record_input")

	digest, err := CanonicalDigest(input)
	if err != nil {
		t.Fatalf("CanonicalDigest() error: %v", err)
	}
	if digest != expected {
		t.Errorf("alternate witness record ID = %q, want %q", digest, expected)
	}
}

func TestCaptureRecordDigestMatchesGolden(t *testing.T) {
	golden := loadGolden(t)
	expected := getString(t, golden, "capture_record", "digest")
	input := getMap(t, golden, "capture_record", "input")

	digest, err := CanonicalDigest(input)
	if err != nil {
		t.Fatalf("CanonicalDigest() error: %v", err)
	}
	if digest != expected {
		t.Errorf("capture record digest = %q, want %q", digest, expected)
	}
}

func TestDeclarationRecordDigestMatchesGolden(t *testing.T) {
	golden := loadGolden(t)
	expected := getString(t, golden, "declaration_record", "digest")
	input := getMap(t, golden, "declaration_record", "input")

	digest, err := CanonicalDigest(input)
	if err != nil {
		t.Fatalf("CanonicalDigest() error: %v", err)
	}
	if digest != expected {
		t.Errorf("declaration record digest = %q, want %q", digest, expected)
	}
}

func TestOrderedParentRecordDigestMatchesGolden(t *testing.T) {
	golden := loadGolden(t)
	expected := getString(t, golden, "ordered_parent_record", "digest")
	input := getMap(t, golden, "ordered_parent_record", "input")

	digest, err := CanonicalDigest(input)
	if err != nil {
		t.Fatalf("CanonicalDigest() error: %v", err)
	}
	if digest != expected {
		t.Errorf("ordered parent record digest = %q, want %q", digest, expected)
	}
}

func TestReversedParentRecordDigestMatchesGolden(t *testing.T) {
	golden := loadGolden(t)
	expected := getString(t, golden, "reversed_parent_record", "digest")
	input := getMap(t, golden, "reversed_parent_record", "input")

	digest, err := CanonicalDigest(input)
	if err != nil {
		t.Fatalf("CanonicalDigest() error: %v", err)
	}
	if digest != expected {
		t.Errorf("reversed parent record digest = %q, want %q", digest, expected)
	}
}

func TestCausalParentOrderChangesDigest(t *testing.T) {
	golden := loadGolden(t)
	ordered := getString(t, golden, "ordered_parent_record", "digest")
	reversed := getString(t, golden, "reversed_parent_record", "digest")

	if ordered == reversed {
		t.Error("ordered and reversed parent records should have different digests")
	}
}

func TestWitnessRecordIDsAreNotBodyDigests(t *testing.T) {
	golden := loadGolden(t)

	for _, witness := range []string{"root_witness", "ordinary_witness", "alternate_witness"} {
		recordID := getString(t, golden, witness, "record_id")
		bodyDigest := getString(t, golden, witness, "body_digest")
		if recordID == bodyDigest {
			t.Errorf("%s: record_id should differ from body_digest", witness)
		}
	}
}

func TestOrdinaryWitnessCitesRootWitness(t *testing.T) {
	golden := loadGolden(t)
	rootRecordID := getString(t, golden, "root_witness", "record_id")
	ordinaryInput := getMap(t, golden, "ordinary_witness", "record_input")
	ordinaryWitnessRef := ordinaryInput["witness"].(string)

	if ordinaryWitnessRef != rootRecordID {
		t.Errorf("ordinary witness should cite root witness as ref, got %q, want %q",
			ordinaryWitnessRef, rootRecordID)
	}
}

func TestAlternateWitnessCitesRootWitness(t *testing.T) {
	golden := loadGolden(t)
	rootRecordID := getString(t, golden, "root_witness", "record_id")
	alternateInput := getMap(t, golden, "alternate_witness", "record_input")
	alternateWitnessRef := alternateInput["witness"].(string)

	if alternateWitnessRef != rootRecordID {
		t.Errorf("alternate witness should cite root witness as ref, got %q, want %q",
			alternateWitnessRef, rootRecordID)
	}
}

func TestAlternateWitnessRecordUsesAlternateWitness(t *testing.T) {
	golden := loadGolden(t)
	alternateWitnessRecordID := getString(t, golden, "alternate_witness", "record_id")
	alternateRecordInput := getMap(t, golden, "alternate_witness_record", "input")
	alternateRecordWitnessRef := alternateRecordInput["witness"].(string)

	if alternateRecordWitnessRef != alternateWitnessRecordID {
		t.Errorf("alternate witness record should cite alternate witness, got %q, want %q",
			alternateRecordWitnessRef, alternateWitnessRecordID)
	}
}

func TestRecordDigestFunctionMatchesGolden(t *testing.T) {
	golden := loadGolden(t)

	// Test capture record
	captureInput := getMap(t, golden, "capture_record", "input")
	captureDigest, err := RecordDigest(
		captureInput["schema_ref"].(string),
		RecordMode(captureInput["mode"].(string)),
		captureInput["body"].(map[string]any),
		nil,
		captureInput["witness"].(string),
	)
	if err != nil {
		t.Fatalf("RecordDigest() error: %v", err)
	}
	expectedCapture := getString(t, golden, "capture_record", "digest")
	if captureDigest != expectedCapture {
		t.Errorf("RecordDigest capture = %q, want %q", captureDigest, expectedCapture)
	}

	// Test declaration record
	declInput := getMap(t, golden, "declaration_record", "input")
	declDigest, err := RecordDigest(
		declInput["schema_ref"].(string),
		RecordMode(declInput["mode"].(string)),
		declInput["body"].(map[string]any),
		nil,
		declInput["witness"].(string),
	)
	if err != nil {
		t.Fatalf("RecordDigest() error: %v", err)
	}
	expectedDecl := getString(t, golden, "declaration_record", "digest")
	if declDigest != expectedDecl {
		t.Errorf("RecordDigest declaration = %q, want %q", declDigest, expectedDecl)
	}
}

func TestCanonicalJSONCompactSeparators(t *testing.T) {
	input := map[string]any{
		"b": 2,
		"a": 1,
	}
	b, err := CanonicalJSONBytes(input)
	if err != nil {
		t.Fatalf("CanonicalJSONBytes() error: %v", err)
	}
	expected := `{"a":1,"b":2}`
	if string(b) != expected {
		t.Errorf("CanonicalJSONBytes() = %q, want %q", string(b), expected)
	}
}

func TestCanonicalJSONNestedSorted(t *testing.T) {
	input := map[string]any{
		"z": map[string]any{
			"b": 2,
			"a": 1,
		},
		"a": "hello",
	}
	b, err := CanonicalJSONBytes(input)
	if err != nil {
		t.Fatalf("CanonicalJSONBytes() error: %v", err)
	}
	expected := `{"a":"hello","z":{"a":1,"b":2}}`
	if string(b) != expected {
		t.Errorf("CanonicalJSONBytes() = %q, want %q", string(b), expected)
	}
}
