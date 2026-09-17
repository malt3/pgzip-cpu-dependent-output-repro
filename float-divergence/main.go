// This is the exact root cause of the amd64-vs-arm64 compressed-output
// divergence in ../ (pgzip / klauspost/compress/flate / stdlib flate on Go
// 1.27+): it has nothing to do with LZ77 match-finding. A byte-identical
// token stream (verified separately by dumping klauspost/compress's real
// *tokens before Huffman coding — see ../README.md) still produces
// different compressed bytes, because (*tokens).EstimatedBits() — a
// Shannon-entropy size *estimate* used only to decide whether to reuse the
// previous block's Huffman table (huffman_bit_writer.go's writeBlockDynamic,
// "newSize < reuseSize") — depends on this float32 fast-log2 approximation
// (klauspost/compress/flate/token.go's mFastLog2), and its Horner-form
// polynomial evaluates to a result that differs in the last couple of
// mantissa bits between amd64 and arm64, confirmed by disassembly to be FMA
// contraction: arm64 compiles the polynomial to FMADDS+FNMSUBS (two fused,
// single-rounding ops); amd64 GOAMD64=v1 (no assumed FMA3) compiles it to
// four separate MULSS/ADDSS/MULSS/SUBSS (four roundings); amd64 GOAMD64=v3
// lands in between, fusing only the first multiply-add. That's normally
// invisible — except EstimatedBits() truncates the accumulated sum with a
// bare int(shannon), and when the sum lands within ~0.1 of a whole number
// (as it does for one specific block below), the different rounding
// produces *different integers*, which flips the table-reuse decision and
// produces a completely different (but equally valid) compressed byte
// stream from that point on.
//
// Two candidate fixes below, both verified end-to-end (see ../README.md):
// applying either as a one-function patch to a real klauspost/compress
// checkout and recompressing the entire ../testdata.bin at level 3 produces
// byte-for-byte identical output on arm64 and amd64.
//
//   - mFastLog2Portable evaluates the polynomial in float64 instead of
//     float32. It still gets fused into FMADDD/FNMSUBD on arm64 (confirmed
//     by disassembly) — the fix isn't "no fusion", it's that float64's
//     precision margin (~2^29x float32's here) makes the fused-vs-unfused
//     difference too small to ever flip an int() truncation for a
//     realistic accumulated sum. Simple, but not a guarantee: an
//     adversarial-enough histogram could in principle still hit its own
//     boundary. Also not literally bit-identical across arches for the
//     underlying float32 value (48747e80 on arm64 vs 48747e84 on amd64) —
//     it's the *truncated int* that matches, which is all EstimatedBits
//     actually returns.
//
//   - mFastLog2NoFuse instead wraps each multiply in an explicit
//     float32(...) conversion before the following add/sub, which denies
//     the compiler a legal "a*b+c" tree to contract (confirmed by
//     disassembly: plain FMULS/FADDS/FMULS/FSUBS on arm64, no FMADDS at
//     all) — bit-identical per call, on any architecture. But that alone
//     was NOT sufficient to fix EstimatedBits(): its own accumulation line,
//     `shannon += atLeastOne(-log2(...)) * nn`, is an *independent*
//     multiply-accumulate that also fuses into FMADDS on arm64 regardless
//     of which mFastLog2 variant feeds it (confirmed by disassembly). Only
//     wrapping *both* — see estimatedBitsNoFuse below — makes the whole
//     thing bit-identical (verified: 48747e84 on both architectures,
//     matching exactly, not just after truncation). The lesson: this
//     "insert a conversion" technique works, but it's whack-a-mole — every
//     multiply-then-add/sub site in the call chain needs its own wrap, and
//     missing one (as the first attempt here did) silently leaves the bug
//     in place.
//
// No file, no klauspost/compress import, no pgzip: this only needs
// mFastLog2/EstimatedBits, copied verbatim from
// klauspost/compress@v1.20.0's flate/token.go, fed one real histogram
// (captured from compressing ../testdata.bin — see ../README.md for how).
//
// Usage: go run . on arm64 and on amd64 (Rosetta/Docker/native); compare
// all four printed lines.
package main

import (
	"fmt"
	"math"
)

// mFastLog2 and atLeastOne are exact copies from
// klauspost/compress/flate/token.go v1.20.0.

func mFastLog2(val float32) float32 {
	ux := int32(math.Float32bits(val))
	log2 := (float32)(((ux >> 23) & 255) - 128)
	ux &= -0x7f800001
	ux += 127 << 23
	uval := math.Float32frombits(uint32(ux))
	log2 += ((-0.34484843)*uval+2.02466578)*uval - 0.67487759
	return log2
}

// mFastLog2Portable evaluates the refinement polynomial in float64 instead
// of float32; see the package comment above.
func mFastLog2Portable(val float32) float32 {
	ux := int32(math.Float32bits(val))
	log2 := float64(((ux >> 23) & 255) - 128)
	ux &= -0x7f800001
	ux += 127 << 23
	uval := float64(math.Float32frombits(uint32(ux)))
	log2 += ((-0.34484843)*uval+2.02466578)*uval - 0.67487759
	return float32(log2)
}

// mFastLog2NoFuse evaluates the same polynomial in float32, with explicit
// conversions denying the compiler a fusable multiply-add tree; see the
// package comment above. In isolation (every value in litHist below, fed
// straight into this function) this is bit-identical across arm64/amd64 —
// but see estimatedBitsNoFuse for why that alone wasn't enough.
func mFastLog2NoFuse(val float32) float32 {
	ux := int32(math.Float32bits(val))
	log2 := float32(((ux >> 23) & 255) - 128)
	ux &= -0x7f800001
	ux += 127 << 23
	uval := math.Float32frombits(uint32(ux))

	p := float32(-0.34484843*uval) + 2.02466578
	q := float32(p*uval) - 0.67487759
	log2 += q

	return log2
}

func atLeastOne(v float32) float32 {
	if v < 1 {
		return 1
	}
	if v > 15 {
		return 15
	}
	return v
}

const literalCount = 286
const offsetCodeCount = 30

var lengthExtraBits = [32]uint8{ /* unused for this repro's estimate path beyond indexing */ }
var offsetExtraBits = [32]uint8{}

// estimatedBits is an exact copy of (*tokens).EstimatedBits, taking the raw
// histograms directly instead of a *tokens. log2 selects which mFastLog2
// variant to use, so original/portable can be compared against the same
// input. (mFastLog2NoFuse is deliberately NOT wired in here — see
// estimatedBitsNoFuse, which additionally patches the accumulation line
// that this function's plain `shannon += x * nn` would otherwise still
// fuse on arm64, defeating mFastLog2NoFuse's own fix.)
func estimatedBits(n int, nFilled int, litHist [256]uint16, extraHist [32]uint16, offHist [32]uint16, log2 func(float32) float32) (float32, int) {
	shannon := float32(0)
	bits := 0
	nMatches := 0
	total := n + nFilled
	if total > 0 {
		invTotal := 1.0 / float32(total)
		for _, v := range litHist[:] {
			if v > 0 {
				nn := float32(v)
				shannon += atLeastOne(-log2(nn*invTotal)) * nn
			}
		}
		shannon += 15
		for i, v := range extraHist[1 : literalCount-256] {
			if v > 0 {
				nn := float32(v)
				shannon += atLeastOne(-log2(nn*invTotal)) * nn
				bits += int(lengthExtraBits[i&31]) * int(v)
				nMatches += int(v)
			}
		}
	}
	if nMatches > 0 {
		invTotal := 1.0 / float32(nMatches)
		for i, v := range offHist[:offsetCodeCount] {
			if v > 0 {
				nn := float32(v)
				shannon += atLeastOne(-log2(nn*invTotal)) * nn
				bits += int(offsetExtraBits[i&31]) * int(v)
			}
		}
	}
	return shannon, int(shannon) + bits
}

// estimatedBitsNoFuse is estimatedBits, hardwired to mFastLog2NoFuse, with
// its own accumulation line ALSO wrapped in float32(...) to deny that
// site's fusion opportunity too. This is the version that's actually
// bit-identical across architectures end to end.
func estimatedBitsNoFuse(n int, nFilled int, litHist [256]uint16, extraHist [32]uint16, offHist [32]uint16) (float32, int) {
	shannon := float32(0)
	bits := 0
	nMatches := 0
	total := n + nFilled
	if total > 0 {
		invTotal := 1.0 / float32(total)
		for _, v := range litHist[:] {
			if v > 0 {
				nn := float32(v)
				shannon = shannon + float32(atLeastOne(-mFastLog2NoFuse(nn*invTotal))*nn)
			}
		}
		shannon += 15
		for i, v := range extraHist[1 : literalCount-256] {
			if v > 0 {
				nn := float32(v)
				shannon = shannon + float32(atLeastOne(-mFastLog2NoFuse(nn*invTotal))*nn)
				bits += int(lengthExtraBits[i&31]) * int(v)
				nMatches += int(v)
			}
		}
	}
	if nMatches > 0 {
		invTotal := 1.0 / float32(nMatches)
		for i, v := range offHist[:offsetCodeCount] {
			if v > 0 {
				nn := float32(v)
				shannon = shannon + float32(atLeastOne(-mFastLog2NoFuse(nn*invTotal))*nn)
				bits += int(offsetExtraBits[i&31]) * int(v)
			}
		}
	}
	return shannon, int(shannon) + bits
}

func main() {
	// Captured verbatim from a real run of klauspost/compress/flate v1.20.0
	// compressing pgzip-minimal-repro/testdata.bin at level 3 (2nd call to
	// EstimatedBits during that compression; see debugdump.go).
	n := 33969
	nFilled := 0
	litHist := [256]uint16{490, 1734, 1455, 1384, 977, 2200, 525, 1282, 969, 476, 976, 396, 409, 261, 293, 491, 361, 356, 417, 230, 206, 227, 196, 166, 153, 293, 123, 144, 136, 132, 170, 131, 267, 109, 100, 76, 73, 95, 124, 108, 149, 94, 140, 110, 94, 85, 93, 85, 84, 158, 137, 93, 107, 128, 90, 74, 104, 83, 91, 97, 87, 76, 85, 77, 62, 75, 58, 58, 88, 45, 36, 31, 86, 86, 101, 69, 41, 42, 42, 48, 49, 41, 39, 47, 38, 44, 41, 30, 42, 25, 38, 47, 28, 43, 19, 23, 34, 24, 22, 27, 46, 25, 11, 24, 27, 36, 22, 26, 29, 14, 14, 27, 27, 18, 23, 21, 18, 23, 17, 20, 16, 11, 15, 12, 14, 19, 24, 13, 100, 53, 77, 58, 41, 56, 54, 47, 53, 50, 54, 48, 70, 64, 80, 60, 88, 44, 52, 62, 89, 62, 68, 34, 34, 50, 44, 33, 55, 44, 51, 51, 75, 63, 60, 53, 67, 64, 69, 47, 55, 38, 46, 49, 66, 50, 80, 58, 58, 54, 52, 38, 47, 52, 55, 50, 52, 51, 56, 54, 62, 31, 49, 66, 87, 40, 44, 42, 45, 43, 50, 45, 53, 49, 56, 55, 79, 36, 49, 67, 59, 45, 60, 39, 50, 51, 57, 46, 46, 45, 45, 43, 48, 42, 121, 43, 58, 43, 53, 45, 54, 47, 51, 45, 56, 38, 46, 29, 45, 30, 41, 36, 63, 38, 72, 37, 37, 42, 46, 47, 43, 49, 42, 37, 44, 33, 61, 61}
	extraHist := [32]uint16{1, 0, 0, 948, 759, 557, 335, 169, 139, 295, 153, 152, 59, 137, 42, 17, 16, 28, 11, 3, 5, 5, 2, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	offHist := [32]uint16{0, 5, 0, 6, 71, 86, 80, 45, 89, 106, 159, 81, 241, 83, 270, 83, 155, 144, 160, 139, 217, 87, 179, 149, 263, 119, 232, 160, 229, 195, 0, 0}

	shannon, result := estimatedBits(n, nFilled, litHist, extraHist, offHist, mFastLog2)
	fmt.Printf("original:            shannon_bits=%08x shannon=%v result=%d\n", math.Float32bits(shannon), shannon, result)

	pShannon, pResult := estimatedBits(n, nFilled, litHist, extraHist, offHist, mFastLog2Portable)
	fmt.Printf("portable (float64):  shannon_bits=%08x shannon=%v result=%d\n", math.Float32bits(pShannon), pShannon, pResult)

	nShannon, nResult := estimatedBits(n, nFilled, litHist, extraHist, offHist, mFastLog2NoFuse)
	fmt.Printf("no-fuse, partial:    shannon_bits=%08x shannon=%v result=%d  (log2 fixed, accumulation line still fuses)\n", math.Float32bits(nShannon), nShannon, nResult)

	fShannon, fResult := estimatedBitsNoFuse(n, nFilled, litHist, extraHist, offHist)
	fmt.Printf("no-fuse, full:       shannon_bits=%08x shannon=%v result=%d  (both sites fixed)\n", math.Float32bits(fShannon), fShannon, fResult)
}
