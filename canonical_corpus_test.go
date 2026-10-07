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
	"os"
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
