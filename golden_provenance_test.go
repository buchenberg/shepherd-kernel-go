package shepherd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

// vectorFile is a shared cross-language oracle: a file whose contents were
// produced by the Python reference and against which this implementation is
// measured.
//
// Its content hash is pinned because a vector file is the thing tests are
// measured *against*. An unexpected edit to one is therefore either a deliberate
// regeneration — in which case update the hash in the same commit — or silent
// damage to the oracle, which would otherwise turn failing tests green.
type vectorFile struct {
	path string
	// wantSHA256 is the hex-encoded SHA-256 of the file's bytes.
	wantSHA256 string
	// reference is the counterpart in the Python repo, relative to this repo, when
	// one exists. Empty for vendored files that have no upstream copy.
	reference string
	// generatedBy records provenance for vendored files.
	generatedBy string
}

var vectorFiles = []vectorFile{
	{
		// The shared ABI golden, byte-identical to shepherd2's copy. Verified
		// identical (6559 bytes, same SHA-256) on 2026-10-07.
		path:       "testdata/kernel_abi_v0.json",
		wantSHA256: "88a5222d272ecfc422f9f10dc14271d74c34411d76a55695bae2f20520f00639",
		reference:  "../shepherd/shepherd2/tests/golden/kernel_abi_v0.json",
	},
	{
		// Vendored rather than co-generated in the Python repo, which plan 01 §3
		// permits when the Python side cannot take the change promptly. It carries
		// its own provenance header (source_commit, python_version), asserted by
		// TestCanonicalEdgeVectorsMatchPython.
		path:        "testdata/canonical_edge_vectors_v0.json",
		wantSHA256:  "9a8df49c36abcd7bdca1e883f7273c1070efba671d8bcf225d75557acbb91587",
		generatedBy: "shepherd2@d34d5ca334871dfcb5a3dc76dd78045829fa4e56 (CPython 3.13.13)",
	},
}

// TestVectorFilesMatchTheirPinnedHashes catches drift in the oracles themselves.
//
// Without this, editing a vector file is the one change that can make every
// cross-language test pass while the implementation diverges: the tests compare
// against these bytes, so weakening the bytes weakens the evidence.
func TestVectorFilesMatchTheirPinnedHashes(t *testing.T) {
	for _, vf := range vectorFiles {
		t.Run(vf.path, func(t *testing.T) {
			data, err := os.ReadFile(vf.path)
			if err != nil {
				t.Fatalf("read %s: %v", vf.path, err)
			}
			sum := sha256.Sum256(data)
			got := hex.EncodeToString(sum[:])
			if got != vf.wantSHA256 {
				t.Errorf("content hash changed\n got: %s\nwant: %s\n"+
					"If this is a deliberate regeneration, update wantSHA256 in the same "+
					"commit and say what changed in the vectors. If it is not, the oracle "+
					"has been damaged and any test measured against it is now meaningless.",
					got, vf.wantSHA256)
			}
		})
	}
}

// TestVectorFilesMatchThePythonReference compares each vector file with its
// counterpart in the Python checkout.
//
// It skips, loudly, when the reference is not present, because CI checks out only
// this repository — so this is a local drift check rather than a CI gate. The
// pinned-hash test above is what runs everywhere.
func TestVectorFilesMatchThePythonReference(t *testing.T) {
	for _, vf := range vectorFiles {
		if vf.reference == "" {
			t.Logf("%s: vendored (%s), no upstream counterpart to compare", vf.path, vf.generatedBy)
			continue
		}

		t.Run(vf.path, func(t *testing.T) {
			ours, err := os.ReadFile(vf.path)
			if err != nil {
				t.Fatalf("read %s: %v", vf.path, err)
			}
			theirs, err := os.ReadFile(vf.reference)
			if err != nil {
				t.Skipf("reference %s not present (expected outside a full workspace, "+
					"including in CI): %v", vf.reference, err)
			}

			if !bytes.Equal(ours, theirs) {
				ourSum := sha256.Sum256(ours)
				theirSum := sha256.Sum256(theirs)
				t.Errorf("%s diverged from %s\n ours: %s (%d bytes)\n"+
					"theirs: %s (%d bytes)\n"+
					"The two copies are meant to be byte-identical; regenerate one from the "+
					"other rather than editing either by hand.",
					vf.path, vf.reference,
					hex.EncodeToString(ourSum[:]), len(ours),
					hex.EncodeToString(theirSum[:]), len(theirs))
			}
		})
	}
}
