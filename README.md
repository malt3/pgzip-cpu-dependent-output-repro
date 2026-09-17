# Compressed output differs between amd64 and arm64

## Expectation

I expect compressing the same input, at the same level, to produce the exact
same compressed byte stream regardless of CPU architecture.

## Reality

For a small subset of inputs, it isn't reproducible. This isn't limited
to `klauspost/pgzip`. The same divergence shows up in three places:

- `github.com/klauspost/pgzip` (with `SetConcurrency(1<<20, 1)`
- `github.com/klauspost/compress/flate` directly, no pgzip involved at all
- Go's own **stdlib** `compress/flate`, but only as of Go 1.27 ([golang.org/cl/707355](https://go-review.googlesource.com/c/go/+/707355))
  produces byte-for-byte the same (arch-dependent) output as
  `klauspost/compress/flate`. Built with Go 1.26.5 or earlier, stdlib's old
  encoder is unaffected and portable.

## How to reproduce

Run on a Mac with Rosetta, or Linux with QEMU user emulation, or just run the
two binaries natively on the respective CPU architecture.

```sh
go build -o rep_arm64 .
GOARCH=amd64 go build -o rep_amd64 .

./rep_arm64 testdata.bin
./rep_amd64 testdata.bin
```

### Results with the current Go toolchain (1.27.1)

```
$ ./rep_arm64 testdata.bin
pgzip (jobs=1)           GOARCH=arm64  sha256=83fd0e6ca9f7f608b694cfc46a67e14ce804cf8a4fd331c2d7d64de0327eb8d8
klauspost/compress/flate GOARCH=arm64  sha256=366af7c509b03419b7b4efb22d0b8e1ae281940f2f81d4b3f92a24ca480c6503
stdlib compress/flate    GOARCH=arm64  sha256=366af7c509b03419b7b4efb22d0b8e1ae281940f2f81d4b3f92a24ca480c6503

$ ./rep_amd64 testdata.bin
pgzip (jobs=1)           GOARCH=amd64  sha256=5b9cc20bb02558027778edf7d71d7ed6e4742f6215690ce0a69c8476345a2e98
klauspost/compress/flate GOARCH=amd64  sha256=485a8c79e4888759521afde1ea9d2cf2e3d21a621000ed0183c502d8a8a843d0
stdlib compress/flate    GOARCH=amd64  sha256=485a8c79e4888759521afde1ea9d2cf2e3d21a621000ed0183c502d8a8a843d0
```

### stdlib on Go 1.26.5 (pre-flate-rewrite): portable

Force an older toolchain to build (requires network access to fetch it once):

```sh
GOTOOLCHAIN=go1.26.5 go build -o rep_arm64_go1265 .
GOTOOLCHAIN=go1.26.5 GOARCH=amd64 go build -o rep_amd64_go1265 .

./rep_arm64_go1265 testdata.bin
./rep_amd64_go1265 testdata.bin
```

```
$ ./rep_arm64_go1265 testdata.bin
pgzip (jobs=1)           GOARCH=arm64  sha256=83fd0e6ca9f7f608b694cfc46a67e14ce804cf8a4fd331c2d7d64de0327eb8d8
klauspost/compress/flate GOARCH=arm64  sha256=366af7c509b03419b7b4efb22d0b8e1ae281940f2f81d4b3f92a24ca480c6503
stdlib compress/flate    GOARCH=arm64  sha256=c0352918b24e8c5f98569c4a03e2ba67f02e98578e3f29791a0dcadcc4659e87

$ ./rep_amd64_go1265 testdata.bin
pgzip (jobs=1)           GOARCH=amd64  sha256=5b9cc20bb02558027778edf7d71d7ed6e4742f6215690ce0a69c8476345a2e98
klauspost/compress/flate GOARCH=amd64  sha256=485a8c79e4888759521afde1ea9d2cf2e3d21a621000ed0183c502d8a8a843d0
stdlib compress/flate    GOARCH=amd64  sha256=c0352918b24e8c5f98569c4a03e2ba67f02e98578e3f29791a0dcadcc4659e87
```

## Exact root cause

It is **not** in LZ77 match-finding (`fastEncL3`, the level-3 fast encoder
that picks literals vs. back-references). Two independent ways to check
that:

1. `tracedump/` is a from-scratch, chunk-accurate reimplementation of
   `fastEncL3.Encode` (copied from `flate/level3.go` +
   `flate/fast_encoder.go`, replicating the outer 64KB-window chunking and
   the history-buffer compaction, both of which matter for fidelity) that
   prints every literal-run/match decision instead of building real tokens.
   Run it on `testdata.bin` on both architectures and `diff` the two trace
   files — they are byte-for-byte identical (all ~62k events).
2. Patching the real `klauspost/compress/flate` package to dump its actual
   token stream and literal/offset/extra-length histograms right after
   `fastEncL3.Encode` runs (before Huffman coding) shows the same thing:
   identical `sha256` of the token array and histograms, for all 9
   `Encode()` calls the 576KB input gets split into, on both architectures.

So the token stream feeding Huffman coding is provably portable. The actual
fork point is one level up, in `huffman_bit_writer.go`'s
`writeBlockDynamic`, which decides whether to **reuse the previous block's
Huffman table** or build a new one, based on comparing `newSize` (a size
estimate for a new table) against `reuseSize`. `newSize` is computed from
`(*tokens).EstimatedBits()` — a Shannon-entropy estimate — which uses
`token.go`'s `mFastLog2`, a fast float32 log2 approximation:

```go
func mFastLog2(val float32) float32 {
	ux := int32(math.Float32bits(val))
	log2 := (float32)(((ux >> 23) & 255) - 128)
	ux &= -0x7f800001
	ux += 127 << 23
	uval := math.Float32frombits(uint32(ux))
	log2 += ((-0.34484843)*uval+2.02466578)*uval - 0.67487759
	return log2
}
```

That last line is a Horner-form polynomial (two chained multiply-adds).
Evaluated on the same `uval` on arm64 vs. amd64, it comes out different in
the last couple of mantissa bits — almost certainly because arm64's Go
backend contracts the chained multiply-adds into native FMA (fused
multiply-add, rounds once) while amd64's baseline codegen (`GOAMD64=v1`,
which doesn't assume FMA3 availability) does not (multiply then add,
rounds twice). `EstimatedBits()` sums many of these per-symbol log2 terms
into a running `shannon float32`, then does a bare `int(shannon)` — no
rounding, pure truncation. For one specific block in `testdata.bin`, that
sum lands within ~0.08 of an integer boundary, and the tiny FMA-vs-no-FMA
difference truncates to a **different integer** on each architecture:

```
arm64:  shannon=250361.98  ->  int(shannon) = 250361
amd64:  shannon=250362.06  ->  int(shannon) = 250362
```

That one-off difference feeds directly into the `newSize < reuseSize`
comparison, flipping the table-reuse decision, so everything written from
that point on is a different (but equally valid — both decompress to the
same content) sequence of Huffman-coded bits.

`float-divergence/` reproduces exactly this, standalone: no file input, no
`klauspost/compress` import, no pgzip — just `mFastLog2` and
`EstimatedBits`, copied verbatim, fed one real histogram captured from the
diverging block. `go run .` on arm64 vs. amd64 (Rosetta or a real box) is
the whole repro:

```
$ ./floatdiv_arm64
shannon_bits=48747e7f shannon=250361.98 result=250361
$ ./floatdiv_amd64
shannon_bits=48747e84 shannon=250362.06 result=250362
```

I could not fully confirm the FMA-contraction mechanism directly (building
with `GOAMD64=v3`, which permits the Go compiler to assume FMA3 on amd64,
fails under Rosetta with "This program can only be run on AMD64 processors
with v3 microarchitecture support" — Rosetta doesn't expose that far), but
the numeric behavior (differs only in the last few mantissa bits, in a
pure-arithmetic function with no data-dependent branching) is the
signature of exactly that class of issue, not a logic bug in the algorithm
itself.

## Portable rewrite

`float-divergence/main.go` also has `mFastLog2Portable`: the same function
with the refinement polynomial evaluated in `float64` instead of `float32`,
converting back to `float32` only for the return value. Rationale: float64
has roughly 2^29 times float32's resolution here, so an FMA-vs-no-FMA
double-rounding difference — which shows up in float32's last 1-2 mantissa
bits — becomes many orders of magnitude smaller than float32 can even
represent. In other words, whatever tiny rounding difference the two
architectures' codegen produces gets absorbed before the final narrowing
conversion, instead of leaking into the result.

On the single diverging histogram from `testdata.bin`:

```
arm64 original: shannon=250361.98  ->  result=250361
arm64 portable: shannon=250362.00  ->  result=250362   (now matches amd64)
amd64 original: shannon=250362.06  ->  result=250362
amd64 portable: shannon=250362.06  ->  result=250362   (unchanged)
```

More importantly, this isn't just fixed for the one isolated histogram —
applying the equivalent one-line change to a real
`github.com/klauspost/compress@v1.20.0` checkout (via a `replace` directive)
and recompressing the *entire* `testdata.bin` through the unmodified
`flate.NewWriter(w, 3)` API gives byte-for-byte identical output on both
architectures:

```
arm64 (patched):  compressed_sha256=485a8c79e4888759521afde1ea9d2cf2e3d21a621000ed0183c502d8a8a843d0
amd64 (patched):  compressed_sha256=485a8c79e4888759521afde1ea9d2cf2e3d21a621000ed0183c502d8a8a843d0
```

(That's amd64's original hash — arm64 changed to match it, since amd64's
`GOAMD64=v1` baseline codegen happens to be the "no FMA" reference
behavior here.) A round-trip decompress of that patched output still
reproduces `testdata.bin` exactly (`sha256:28b4d8bb...`), so this isn't
just moving the divergence around — it's a real fix for this input.

Caveats: this widens every log2 call in the hot Shannon-estimate path from
`float32` to `float64` arithmetic, which has some (probably small, not
benchmarked here) performance cost, and it's a probabilistic argument, not
a mathematical proof of portability — float64 could in principle still hit
its own double-rounding boundary given a sufficiently adversarial (and
almost certainly practically unreachable) histogram. It has not been fuzz
tested across many inputs, only validated against the one histogram that's
known to trigger the original bug.
