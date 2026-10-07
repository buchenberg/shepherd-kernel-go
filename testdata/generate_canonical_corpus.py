#!/usr/bin/env python3
"""Generate testdata/canonical_corpus_v0.json from the Python reference.

Run from the repository root:

    python testdata/generate_canonical_corpus.py testdata/canonical_corpus_v0.json

This is the T1.3 property corpus: ~200 randomized payloads whose expected
bytes and digests come from shepherd2's own `canonical_json_bytes` /
`canonical_digest`, not from a reimplementation. The edge-vector file covers
the *deliberate* divergences (HTML escaping, float repr thresholds); this file
covers the rest of the space statistically — arbitrary 64-bit float patterns,
integers beyond float64's range, strings across every Unicode range the
encoder can legally emit, and nested structures mixing all of them.

The payloads are randomized once and then frozen. The seed is fixed below and
recorded in the file, so "regenerate" is reproducible: the same seed, the same
Python reference commit, the same bytes. A changed hash in
golden_provenance_test.go therefore means one of those moved, never chance.

The Python checkout defaults to the sibling `../shepherd` directory and can be
overridden with SHEPHERD_REPO (or SHEPHERD2_SRC for the import path alone).

Do not edit the generated JSON by hand. If the vectors change, update the
pinned SHA-256 in golden_provenance_test.go in the same commit and say why.
"""

from __future__ import annotations

import json
import math
import os
import random
import struct
import subprocess
import sys
from pathlib import Path

DEFAULT_SHEPHERD_REPO = str(Path(__file__).resolve().parent.parent.parent / "shepherd")
DEFAULT_SHEPHERD2_SRC = str(Path(DEFAULT_SHEPHERD_REPO) / "shepherd2" / "src")

SRC = os.environ.get("SHEPHERD2_SRC", DEFAULT_SHEPHERD2_SRC)
REPO = os.environ.get("SHEPHERD_REPO", DEFAULT_SHEPHERD_REPO)
sys.path.insert(0, SRC)

from shepherd2.kernel.canonical import (  # noqa: E402
    CANONICAL_VERSION,
    canonical_digest,
    canonical_json_bytes,
)

# Fixed seed. Changing it regenerates every payload and changes the pinned
# hash; do that only for a reason, and record it here.
SEED = 20261007

GROUP_SIZES = {
    "floats": 60,
    "ints": 40,
    "strings": 50,
    "nested": 50,
}

# Strings deliberately avoid the surrogate blocks U+D800..U+DFFF: a Python str
# containing a lone surrogate cannot be encoded to UTF-8, and canonical JSON
# rejects it on both sides of the port. Everything else is fair game.
ESCAPABLE_ASCII = "<>&\"\\/';`$%^*()[]{}#~!|"
WORDS = ["step", "run", "value", "path", "tmp", "note"]


def random_float(rng: random.Random) -> float:
    shape = rng.randrange(3)
    if shape == 0:
        # Arbitrary 64-bit patterns: every exponent and mantissa combination,
        # including subnormals and the repr boundaries — the strongest case,
        # because no hand-picked list thinks of these.
        while True:
            bits = rng.getrandbits(64)
            value = struct.unpack("<d", struct.pack("<Q", bits))[0]
            if math.isfinite(value):
                return value
    if shape == 1:
        # Magnitude-stratified, crossing the fixed/scientific notation boundary.
        return rng.uniform(-1.0, 1.0) * 10 ** rng.uniform(-10.0, 10.0)
    # Simple decimal fractions of the kind real payloads carry.
    return round(rng.uniform(-1e6, 1e6), rng.randrange(0, 6))


def random_int(rng: random.Random) -> int:
    shape = rng.randrange(3)
    if shape == 0:
        # Inside float64's exact range.
        return rng.randint(-(2**53), 2**53)
    if shape == 1:
        # Arbitrary precision, far beyond float64: Python emits the full
        # digits, and the Go writer must pass them through verbatim.
        return rng.randint(-(10**30), 10**30)
    return rng.randint(-(2**31), 2**31)


def random_codepoint(rng: random.Random) -> str:
    kind = rng.randrange(4)
    if kind == 0:  # C0 controls and DEL
        return chr(rng.choice([rng.randrange(0x20), 0x7F]))
    if kind == 1:  # BMP outside the surrogate block
        while True:
            cp = rng.randrange(0x10000)
            if not 0xD800 <= cp <= 0xDFFF:
                return chr(cp)
    if kind == 2:  # astral planes
        return chr(rng.randrange(0x10000, 0x110000))
    return rng.choice(ESCAPABLE_ASCII)


def random_string(rng: random.Random) -> str:
    parts = []
    for _ in range(rng.randrange(1, 12)):
        kind = rng.randrange(5)
        if kind == 0:
            parts.append(rng.choice(ESCAPABLE_ASCII))
        elif kind == 1:
            parts.append(random_codepoint(rng))
        elif kind == 2:
            parts.append(random_codepoint(rng))
        elif kind == 3:
            parts.append(rng.choice(WORDS) + str(rng.randrange(100)))
        else:
            parts.append(" ")
    return "".join(parts)


def random_value(rng: random.Random, depth: int):
    if depth <= 0 or rng.randrange(4) == 0:
        kind = rng.randrange(5)
        if kind == 0:
            return random_float(rng)
        if kind == 1:
            return random_int(rng)
        if kind == 2:
            return random_string(rng)
        if kind == 3:
            return rng.choice([True, False])
        return None
    if rng.randrange(2) == 0:
        return [random_value(rng, depth - 1) for _ in range(rng.randrange(0, 5))]
    out = {}
    for _ in range(rng.randrange(0, 5)):
        out[random_string(rng)] = random_value(rng, depth - 1)
    return out


def group_vectors(rng: random.Random, size: int, make) -> list:
    out = []
    for i in range(size):
        payload = make()
        wrapped = {"v": payload}
        out.append(
            {
                "label": f"{i:03d}",
                "payload": wrapped,
                "canonical_bytes": canonical_json_bytes(wrapped).decode("utf-8"),
                "digest": canonical_digest(wrapped),
            }
        )
    return out


def git_commit(path):
    try:
        return (
            subprocess.check_output(
                ["git", "-C", path, "rev-parse", "HEAD"], stderr=subprocess.DEVNULL
            )
            .decode()
            .strip()
        )
    except Exception as exc:  # pragma: no cover - provenance only
        return f"unknown ({exc})"


def main():
    if len(sys.argv) != 2:
        print(__doc__)
        return 2

    rng = random.Random(SEED)

    doc = {
        "canonical_version": CANONICAL_VERSION,
        "generator": "testdata/generate_canonical_corpus.py",
        "source_repo": "../shepherd",
        "source_commit": git_commit(REPO),
        "python_version": sys.version.split()[0],
        "seed": SEED,
        "floats": group_vectors(rng, GROUP_SIZES["floats"], lambda: random_float(rng)),
        "ints": group_vectors(rng, GROUP_SIZES["ints"], lambda: random_int(rng)),
        "strings": group_vectors(rng, GROUP_SIZES["strings"], lambda: random_string(rng)),
        "nested": group_vectors(rng, GROUP_SIZES["nested"], lambda: random_value(rng, 3)),
    }

    dest = sys.argv[1]
    # newline="\n" matters: the pinned hash in golden_provenance_test.go is computed
    # over the LF form, so writing CRLF here would change it on Windows only.
    with open(dest, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(doc, fh, ensure_ascii=False, indent=2, sort_keys=True)
        fh.write("\n")

    total = sum(len(doc[g]) for g in GROUP_SIZES)
    print(f"wrote {dest}")
    print(f"  source_commit: {doc['source_commit'][:12]}  seed: {SEED}")
    print(f"  floats={len(doc['floats'])} ints={len(doc['ints'])} "
          f"strings={len(doc['strings'])} nested={len(doc['nested'])} total={total}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
