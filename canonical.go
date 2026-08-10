package shepherd

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	CanonicalVersion     = "shepherd.kernel.canonical.v2"
	ABIVersion           = "shepherd.kernel.abi.v0"
	RootWitnessSchemaRef = "kernel.witness.root.v1"
	WitnessSchemaRef     = "kernel.witness.v1"
	RootWitnessRef       = ""
)

var canonicalPrefix = []byte(CanonicalVersion + "\n")

// sha256Sum returns the SHA-256 hash of the input bytes.
func sha256Sum(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// CanonicalJSONBytes returns byte-stable canonical JSON for digest input.
// Keys are sorted at every nesting level. Separators are compact (no spaces).
func CanonicalJSONBytes(v any) ([]byte, error) {
	stable := canonicalize(v)
	return json.Marshal(stable)
}

// canonicalize recursively sorts map keys to produce deterministic JSON output.
func canonicalize(v any) any {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// Use a json.RawMessage trick: build ordered pairs manually
		// Actually, we can't guarantee order with a regular map.
		// We need to build ordered JSON directly.
		_ = keys
		// Recursively canonicalize values
		for k, vv := range val {
			val[k] = canonicalize(vv)
		}
		return val
	case []any:
		for i, vv := range val {
			val[i] = canonicalize(vv)
		}
		return val
	default:
		return v
	}
}

// canonicalJSONOrdered produces canonical JSON with sorted keys at every level.
// This is necessary because Go's json.Marshal does not guarantee map key order,
// even though in practice it sorts them. We enforce it explicitly.
func canonicalJSONOrdered(v any) ([]byte, error) {
	var buf strings.Builder
	err := writeCanonicalValue(&buf, v, "")
	if err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

func writeCanonicalValue(buf *strings.Builder, v any, _ string) error {
	switch val := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if val {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case float64:
		// Match Python's json.dumps behavior for numbers
		if val == float64(int64(val)) && val != 0 && val > -1e15 && val < 1e15 {
			fmt.Fprintf(buf, "%d", int64(val))
		} else {
			b, err := json.Marshal(val)
			if err != nil {
				return err
			}
			buf.Write(b)
		}
	case json.Number:
		buf.WriteString(val.String())
	case string:
		b, err := json.Marshal(val)
		if err != nil {
			return err
		}
		buf.Write(b)
	case []any:
		buf.WriteByte('[')
		for i, item := range val {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonicalValue(buf, item, ""); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			keyBytes, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(keyBytes)
			buf.WriteByte(':')
			if err := writeCanonicalValue(buf, val[k], ""); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return err
		}
		buf.Write(b)
	}
	return nil
}

// CanonicalDigest returns the kernel digest for a canonical input payload.
func CanonicalDigest(v any) (string, error) {
	b, err := canonicalJSONOrdered(v)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(canonicalPrefix)
	h.Write(b)
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}

// CanonicalRecordInput builds the canonical payload for one retained record.
func CanonicalRecordInput(schemaRef string, mode RecordMode, body map[string]any, causedBy []string, witness string) (map[string]any, error) {
	if err := ValidateSchemaRef(schemaRef); err != nil {
		return nil, err
	}
	if err := ValidateMode(mode); err != nil {
		return nil, err
	}
	if err := ValidateWitnessRef(witness); err != nil {
		return nil, err
	}
	if err := ValidateRootWitnessRecord(schemaRef, mode, body, causedBy, witness); err != nil {
		return nil, err
	}

	causedByList := make([]any, len(causedBy))
	for i, c := range causedBy {
		causedByList[i] = c
	}

	bodyCopy := make(map[string]any, len(body))
	for k, v := range body {
		bodyCopy[k] = v
	}

	return map[string]any{
		"body":       bodyCopy,
		"caused_by":  causedByList,
		"kind":       "record",
		"mode":       string(mode),
		"schema_ref": schemaRef,
		"witness":    witness,
	}, nil
}

// RecordDigest returns the kernel record id for one retained record.
func RecordDigest(schemaRef string, mode RecordMode, body map[string]any, causedBy []string, witness string) (string, error) {
	input, err := CanonicalRecordInput(schemaRef, mode, body, causedBy, witness)
	if err != nil {
		return "", err
	}
	return CanonicalDigest(input)
}

// CanonicalWitnessInput builds the canonical payload for one witness.
func CanonicalWitnessInput(schemaRef string, body map[string]any) (map[string] any, error) {
	if err := ValidateWitnessBody(schemaRef, body); err != nil {
		return nil, err
	}

	bodyCopy := make(map[string]any, len(body))
	for k, v := range body {
		bodyCopy[k] = v
	}

	return map[string]any{
		"body":       bodyCopy,
		"kind":       "witness",
		"schema_ref": schemaRef,
	}, nil
}

// WitnessBodyDigest returns a digest for canonical witness-body input.
// Kernel records cite retained witness record ids, not this body digest.
func WitnessBodyDigest(schemaRef string, body map[string]any) (string, error) {
	input, err := CanonicalWitnessInput(schemaRef, body)
	if err != nil {
		return "", err
	}
	return CanonicalDigest(input)
}

// RootWitnessBody returns the fixed root witness body.
func RootWitnessBody() map[string]any {
	return map[string]any{
		"active_binding_refs":         []any{},
		"actor_ref":                   "kernel:root",
		"authority_refs":              []any{},
		"containment":                 "full",
		"provenance_policy_refs":      []any{},
		"semantic_environment_refs":   []any{},
		"substrate_ref":               "kernel",
		"visibility_policy_refs":      []any{},
	}
}

// RootWitnessBodyDigest returns the deterministic root witness-body digest.
func RootWitnessBodyDigest() (string, error) {
	return WitnessBodyDigest(RootWitnessSchemaRef, RootWitnessBody())
}

// RootWitnessRecordID returns the deterministic retained root witness record id.
func RootWitnessRecordID() (string, error) {
	return RecordDigest(
		RootWitnessSchemaRef,
		Capture,
		RootWitnessBody(),
		nil,
		RootWitnessRef,
	)
}

// rootWitnessRecordIDCached is computed once at init time.
var rootWitnessRecordIDCached string

func init() {
	id, err := RootWitnessRecordID()
	if err != nil {
		panic("failed to compute root witness record id: " + err.Error())
	}
	rootWitnessRecordIDCached = id
}

// RootWitnessRecordIDMust returns the root witness record id, panicking on error.
// Safe because the root witness is deterministic and always valid.
func RootWitnessRecordIDMust() string {
	return rootWitnessRecordIDCached
}

// ValidateSchemaRef checks that a schema ref is non-empty.
func ValidateSchemaRef(schemaRef string) error {
	if schemaRef == "" {
		return fmt.Errorf("schema_ref is required")
	}
	return nil
}

// ValidateMode checks that a mode is valid.
func ValidateMode(mode RecordMode) error {
	if mode != Capture && mode != Declaration {
		return fmt.Errorf("mode must be 'capture' or 'declaration'")
	}
	return nil
}

// ValidateWitnessRef checks that a witness ref is valid.
func ValidateWitnessRef(witness string) error {
	if witness == RootWitnessRef {
		return nil
	}
	if !strings.HasPrefix(witness, "sha256:") {
		return fmt.Errorf("witness must be a sha256 digest or the root sentinel")
	}
	return nil
}

// ValidateRootWitnessRecord validates the root witness record constraints.
func ValidateRootWitnessRecord(schemaRef string, mode RecordMode, body map[string]any, causedBy []string, witness string) error {
	if witness == RootWitnessRef {
		if schemaRef != RootWitnessSchemaRef {
			return fmt.Errorf("empty witness sentinel is legal only for the root witness record")
		}
		if mode != Capture {
			return fmt.Errorf("root witness record mode must be 'capture'")
		}
		if len(causedBy) > 0 {
			return fmt.Errorf("root witness record cannot have causal parents")
		}
		expected := RootWitnessBody()
		if !mapEqual(body, expected) {
			return fmt.Errorf("root witness record body must equal root_witness_body()")
		}
		return nil
	}
	if schemaRef == RootWitnessSchemaRef {
		return fmt.Errorf("root witness record must use the empty witness sentinel")
	}
	return nil
}

// ValidateWitnessBody validates the witness body shape.
func ValidateWitnessBody(schemaRef string, body map[string]any) error {
	if schemaRef != RootWitnessSchemaRef && schemaRef != WitnessSchemaRef {
		return nil
	}
	required := []string{
		"actor_ref", "authority_refs", "active_binding_refs",
		"semantic_environment_refs", "visibility_policy_refs",
		"provenance_policy_refs", "substrate_ref", "containment",
	}
	for _, field := range required {
		if _, ok := body[field]; !ok {
			return fmt.Errorf("witness body missing required field: %s", field)
		}
	}
	containment, ok := body["containment"].(string)
	if !ok {
		return fmt.Errorf("witness containment must be a string")
	}
	validContainment := map[string]bool{
		"full": true, "contained": true, "buffered": true, "uncontained": true,
	}
	if !validContainment[containment] {
		return fmt.Errorf("witness containment is invalid")
	}
	substrateRef, ok := body["substrate_ref"].(string)
	if !ok || substrateRef == "" {
		return fmt.Errorf("witness substrate_ref is required")
	}
	return nil
}

func mapEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || !anyEqual(v, bv) {
			return false
		}
	}
	return true
}

func anyEqual(a, b any) bool {
	aJSON, errA := json.Marshal(a)
	bJSON, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(aJSON) == string(bJSON)
}
