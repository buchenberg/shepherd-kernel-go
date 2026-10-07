package shepherd

// The T1.3 property corpus: 200 seeded-random payloads whose expected bytes
// and digests were produced by the Python reference, not by Go.
//
// Generator: `testdata/generate_canonical_corpus.py`, which imports the real
// `shepherd2.kernel.canonical` so the oracles are authoritative. The seed is
// fixed in the script and recorded in the file; regeneration is
// byte-reproducible, which the pinned hash in golden_provenance_test.go turns
// into a property: a changed corpus means the seed, the reference commit or
// the generator changed — never chance.
//
// The edge-vector file (canonical_edge_test.go) covers the deliberate
// divergences by name: HTML escaping, CPython's float repr, the notation
// threshold. This corpus covers the rest of the space statistically —
// arbitrary 64-bit float patterns including subnormals, integers beyond
// float64's exact range, strings across every Unicode range the encoder can
// legally emit, and nested structures mixing all of them. R1 in plan 00
// section 11 is the risk it exists to retire: a float-formatting divergence
// on a rare value that no hand-picked list would contain.

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
)

type canonicalCorpusFile struct {
	CanonicalVersion string            `json:"canonical_version"`
	Generator        string            `json:"generator"`
	SourceRepo       string            `json:"source_repo"`
	SourceCommit     string            `json:"source_commit"`
	PythonVersion    string            `json:"python_version"`
	Seed             int64             `json:"seed"`
	Floats           []canonicalVector `json:"floats"`
	Ints             []canonicalVector `json:"ints"`
	Strings          []canonicalVector `json:"strings"`
	Nested           []canonicalVector `json:"nested"`
}

func (f canonicalCorpusFile) groups() map[string][]canonicalVector {
	return map[string][]canonicalVector{
		"floats":  f.Floats,
		"ints":    f.Ints,
		"strings": f.Strings,
		"nested":  f.Nested,
	}
}

// corpusSizes pins the corpus's shape. A regeneration that changes the counts
// is a different corpus and should be a visible decision, not a silent drift.
var corpusSizes = map[string]int{
	"floats":  60,
	"ints":    40,
	"strings": 50,
	"nested":  50,
}

func loadCorpus(t *testing.T) canonicalCorpusFile {
	t.Helper()
	data, err := os.ReadFile("testdata/canonical_corpus_v0.json")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	// UseNumber, as everywhere the kernel decodes a payload for digesting: the
	// difference between JSON `1` and `1.0` is the difference between canonical
	// "1" and "1.0", and only the token text carries it.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc canonicalCorpusFile
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	return doc
}

// TestCanonicalCorpusMatchesPython replays every randomized payload through the
// Go writer and asserts byte-identical canonical bytes and digests.
func TestCanonicalCorpusMatchesPython(t *testing.T) {
	doc := loadCorpus(t)

	if doc.CanonicalVersion != CanonicalVersion {
		t.Errorf("corpus canonical_version = %q, want %q", doc.CanonicalVersion, CanonicalVersion)
	}
	if doc.Seed == 0 {
		t.Error("corpus does not record its seed; it cannot be reproduced")
	}
	// The provenance must be usable, not merely present: the generator's
	// git_commit swallows every failure and returns "unknown (...)", so a
	// corpus regenerated without a usable Python checkout would carry
	// provenance that names nothing — and pass every digest assertion, since
	// those check the bytes, not where they came from.
	if doc.Generator != "testdata/generate_canonical_corpus.py" {
		t.Errorf("corpus generator = %q, want testdata/generate_canonical_corpus.py", doc.Generator)
	}
	if doc.SourceRepo == "" {
		t.Error("corpus has no source_repo, so its provenance names no checkout")
	}
	if doc.SourceCommit == "" || strings.HasPrefix(doc.SourceCommit, "unknown") {
		t.Errorf("corpus source_commit %q does not identify a revision", doc.SourceCommit)
	}
	if doc.PythonVersion == "" {
		t.Error("corpus does not record the Python version that produced it")
	}

	groups := doc.groups()
	total := 0
	for name, want := range corpusSizes {
		group, ok := groups[name]
		if !ok {
			t.Fatalf("corpus is missing the %q group", name)
		}
		if len(group) != want {
			t.Errorf("group %q has %d payloads, want %d", name, len(group), want)
		}
		total += len(group)
	}
	if total != 200 {
		t.Errorf("corpus has %d payloads, want 200", total)
	}

	for name, group := range groups {
		for _, v := range group {
			if v.Label == "" || v.CanonicalBytes == "" || v.Digest == "" {
				t.Fatalf("%s/%s: malformed corpus entry", name, v.Label)
			}
			got, err := CanonicalJSONBytes(v.Payload)
			if err != nil {
				t.Errorf("%s/%s: CanonicalJSONBytes: %v", name, v.Label, err)
				continue
			}
			if string(got) != v.CanonicalBytes {
				t.Errorf("%s/%s: canonical bytes differ\n got: %s\nwant: %s",
					name, v.Label, got, v.CanonicalBytes)
			}
			digest, err := CanonicalDigest(v.Payload)
			if err != nil {
				t.Errorf("%s/%s: CanonicalDigest: %v", name, v.Label, err)
				continue
			}
			if digest != v.Digest {
				t.Errorf("%s/%s: digest = %s, want %s", name, v.Label, digest, v.Digest)
			}
		}
	}
}

// TestCanonicalCorpusCoversItsAdvertisedShapes guards against a regeneration
// quietly weakening the corpus. The first frozen version advertised
// subnormals and both notation boundaries but contained none of the former
// and nothing around the upper one: the arbitrary-bit branch draws a
// subnormal with probability ~1/2048, so 106 floats came up empty — and
// every digest still matched, because the corpus tests bytes, not coverage.
// These assertions run against the pinned file, so a regeneration that drops
// a band fails here instead of shipping a weaker corpus.
func TestCanonicalCorpusCoversItsAdvertisedShapes(t *testing.T) {
	doc := loadCorpus(t)

	const smallestNormal = 2.2250738585072014e-308
	subnormals, upperBoundary, lowerBoundary := 0, 0, 0
	for _, group := range [][]canonicalVector{doc.Floats, doc.Nested} {
		for _, v := range group {
			scanNumbers(t, v.Payload, func(f float64) {
				switch abs := math.Abs(f); {
				case f != 0 && abs < smallestNormal:
					subnormals++
				case abs > 1e15 && abs < 1e17:
					upperBoundary++
				case abs > 1e-5 && abs < 1e-3:
					lowerBoundary++
				}
			})
		}
	}
	if subnormals == 0 {
		t.Error("corpus contains no subnormals; the sampler's subnormal band is gone")
	}
	if upperBoundary == 0 {
		t.Error("corpus contains nothing around the 1e16 notation boundary; the upper band is gone")
	}
	if lowerBoundary == 0 {
		t.Error("corpus contains nothing around the 1e-4 notation boundary; the lower band is gone")
	}
	t.Logf("coverage: %d subnormals, %d around 1e16, %d around 1e-4",
		subnormals, upperBoundary, lowerBoundary)
}

// scanNumbers visits every JSON number in a corpus payload, parsing the
// json.Number token text the corpus decodes with.
func scanNumbers(t *testing.T, v any, visit func(f float64)) {
	t.Helper()
	switch val := v.(type) {
	case json.Number:
		if f, err := val.Float64(); err == nil {
			visit(f)
		}
	case map[string]any:
		for _, x := range val {
			scanNumbers(t, x, visit)
		}
	case []any:
		for _, x := range val {
			scanNumbers(t, x, visit)
		}
	}
}
