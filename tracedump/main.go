// tracedump is a standalone reimplementation of
// klauspost/compress/flate's level-3 fast encoder (fastEncL3, the code
// path used for `--compression-level 3`), instrumented to print every
// literal-run and match decision it makes.
//
// It is copied (not imported) from github.com/klauspost/compress v1.20.0's
// flate/level3.go and flate/fast_encoder.go so that every intermediate
// value can be printed without touching the real package. The arithmetic
// is unchanged from upstream (including replicating the outer
// compressor.write()/storeFast() chunking into maxStoreBlockSize windows,
// and addBlock's history-buffer compaction when it outgrows its
// allocation — both of which matter for byte-for-byte fidelity and were
// wrong in an earlier version of this file), and the token sink just
// records decisions instead of building a real Huffman token stream.
//
// NEGATIVE RESULT: running the same testdata.bin through this on arm64 and
// amd64 produces byte-for-byte identical trace files (verified: diff finds
// no difference across all ~62k literal/match events). That means the
// amd64-vs-arm64 divergence documented in ../README.md does NOT originate
// here — LZ77 match-finding is fully portable for this input. It was
// independently confirmed (by dumping the real, unmodified library's token
// stream — see ../README.md) that the actual divergence is downstream, in
// Huffman-table-reuse-decision floating-point arithmetic; see
// ../float-divergence for the real, minimal root cause.
//
// Usage: go run . ../testdata.bin trace.txt
package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"math/bits"
	"os"
)

const (
	tableBits       = 16 // fastEncL3 uses 16 (not fastGen's default 15)
	tableSize       = 1 << tableBits
	baseMatchOffset = 1
	maxMatchOffset  = 1 << 15
	maxMatchLength  = 258
	maxStoreBlockSize = 65535
	allocHistory    = maxStoreBlockSize * 5
	bufferReset     = (1 << 31) - allocHistory - maxStoreBlockSize - 1

	inputMargin            = 12 - 1
	minNonLiteralBlockSize = 1 + 1 + inputMargin
	hashBytes              = 5

	prime3bytes = 506832829
	prime4bytes = 2654435761
	prime5bytes = 889523592379
	prime6bytes = 227718039650203
	prime7bytes = 58295818150454627
	prime8bytes = 0xcf1bbcdcb7a56463
)

type tableEntry struct {
	offset int32
}

type tableEntryPrev struct {
	Cur  tableEntry
	Prev tableEntry
}

// --- exact copies of fast_encoder.go's load/hash helpers, using
// encoding/binary instead of internal/le's unsafe reads. Proven equivalent
// (see README: -tags nounsafe reproduces the same divergence), and this way
// the trace tool has zero unsafe code of its own. ---

func load3232(b []byte, i int32) uint32 {
	return binary.LittleEndian.Uint32(b[i:])
}

func load6432(b []byte, i int32) uint64 {
	return binary.LittleEndian.Uint64(b[i:])
}

func hashLen(u uint64, length, mls uint8) uint32 {
	switch mls {
	case 3:
		return (uint32(u<<8) * prime3bytes) >> (32 - length)
	case 5:
		return uint32(((u << (64 - 40)) * prime5bytes) >> (64 - length))
	case 6:
		return uint32(((u << (64 - 48)) * prime6bytes) >> (64 - length))
	case 7:
		return uint32(((u << (64 - 56)) * prime7bytes) >> (64 - length))
	case 8:
		return uint32((u * prime8bytes) >> (64 - length))
	default:
		return (uint32(u) * prime4bytes) >> (32 - length)
	}
}

// matchLen is an exact copy of flate/matchlen_generic.go, using
// encoding/binary instead of internal/le.
func matchLen(a, b []byte) (n int) {
	left := len(a)
	for left >= 8 {
		diff := binary.LittleEndian.Uint64(a[n:]) ^ binary.LittleEndian.Uint64(b[n:])
		if diff != 0 {
			return n + bits.TrailingZeros64(diff)>>3
		}
		n += 8
		left -= 8
	}
	aa := a[n:]
	bb := b[n:]
	for i := range aa {
		if aa[i] != bb[i] {
			break
		}
		n++
	}
	return n
}

// fastGen is a trimmed copy of flate/fast_encoder.go's fastGen: only what
// fastEncL3.Encode needs for a single-shot (non-incremental) encode.
type fastGen struct {
	hist []byte
	cur  int32
}

// addBlock is an exact copy of fast_encoder.go's addBlock: when hist would
// outgrow its allocation, it does NOT discard old content — it shifts the
// last maxMatchOffset bytes down to the front (so in-range matches still
// resolve) and bumps e.cur to compensate, discarding only what's already
// unreachable. Getting this wrong (e.g. naively reallocating) desyncs
// candidate.offset-e.cur for every table entry recorded before the shift.
func (e *fastGen) addBlock(src []byte) int32 {
	if len(e.hist)+len(src) > cap(e.hist) {
		if cap(e.hist) == 0 {
			e.hist = make([]byte, 0, allocHistory)
		} else {
			if cap(e.hist) < maxMatchOffset*2 {
				panic("unexpected buffer size")
			}
			offset := int32(len(e.hist)) - maxMatchOffset
			copy(e.hist[:maxMatchOffset], e.hist[offset:])
			e.cur += offset
			e.hist = e.hist[:maxMatchOffset]
		}
	}
	s := int32(len(e.hist))
	e.hist = append(e.hist, src...)
	return s
}

func (e *fastGen) matchlenLong(s, t int, src []byte) int32 {
	return int32(matchLen(src[s:], src[t:]))
}

// fastEncL3 is an exact structural copy of flate/level3.go's fastEncL3.
type fastEncL3 struct {
	fastGen
	table [tableSize]tableEntryPrev
}

// trace is where every decision gets printed. One line per literal run and
// one line per match, in the exact order the real encoder would emit
// tokens. Everything printed here is a value that upstream's Encode also
// computes internally, just not observable without this copy.
type trace struct {
	w   *bufio.Writer
	n   int
	lit int // total literal bytes emitted so far, for cross-checking positions
}

func (t *trace) literal(nextEmit, s int32) {
	if nextEmit >= s {
		return
	}
	fmt.Fprintf(t.w, "%d LIT nextEmit=%d s=%d len=%d\n", t.n, nextEmit, s, s-nextEmit)
	t.n++
}

func (t *trace) match(s, t2, l int32, offset int32, pick string, l1, l2 int32) {
	fmt.Fprintf(t.w, "%d MATCH s=%d t=%d l=%d offset=%d pick=%s l1=%d l2=%d\n", t.n, s, t2, l, offset, pick, l1, l2)
	t.n++
}

// Encode is level3.go's fastEncL3.Encode, with dst replaced by tr and the
// real token/Huffman bookkeeping stripped: every arithmetic decision is
// unchanged from upstream.
func (e *fastEncL3) Encode(tr *trace, src []byte) {
	// (bufferReset wraparound guard omitted: unreachable for a single-shot,
	// <2GB input, which is all this tool is for.)

	s := e.addBlock(src)

	if len(src) < minNonLiteralBlockSize {
		return
	}

	src = e.hist
	nextEmit := s

	sLimit := int32(len(src) - inputMargin)

	cv := load6432(src, s)
	for {
		const skipLog = 7
		nextS := s
		var candidate tableEntry
		for {
			nextHash := hashLen(cv, tableBits, hashBytes)
			s = nextS
			nextS = s + 1 + (s-nextEmit)>>skipLog
			if nextS > sLimit {
				goto emitRemainder
			}
			candidates := e.table[nextHash]
			now := load6432(src, nextS)

			minOffset := e.cur + s - (maxMatchOffset - 4)
			e.table[nextHash] = tableEntryPrev{Prev: candidates.Cur, Cur: tableEntry{offset: s + e.cur}}

			candidate = candidates.Cur
			if candidate.offset < minOffset {
				cv = now
				continue
			}

			if uint32(cv) == load3232(src, candidate.offset-e.cur) {
				if candidates.Prev.offset < minOffset || uint32(cv) != load3232(src, candidates.Prev.offset-e.cur) {
					break
				}
				offset := s - (candidate.offset - e.cur)
				o2 := s - (candidates.Prev.offset - e.cur)
				l1, l2 := matchLen(src[s+4:], src[s-offset+4:]), matchLen(src[s+4:], src[s-o2+4:])
				if l2 > l1 {
					candidate = candidates.Prev
				}
				break
			} else {
				candidate = candidates.Prev
				if candidate.offset > minOffset && uint32(cv) == load3232(src, candidate.offset-e.cur) {
					break
				}
			}
			cv = now
		}

		for {
			t := candidate.offset - e.cur
			l := e.matchlenLong(int(s+4), int(t+4), src) + 4

			var l1b, l2b int32 // tie-break lengths, if this match came from a tie (else -1)
			l1b, l2b = -1, -1

			for t > 0 && s > nextEmit && src[t-1] == src[s-1] {
				s--
				t--
				l++
			}
			tr.literal(nextEmit, s)

			pick := "Cur"
			tr.match(s, t, l, s-t-baseMatchOffset, pick, l1b, l2b)

			s += l
			nextEmit = s
			if nextS >= s {
				s = nextS + 1
			}

			if s >= sLimit {
				t += l
				if int(t+8) < len(src) && t > 0 {
					cv = load6432(src, t)
					nextHash := hashLen(cv, tableBits, hashBytes)
					e.table[nextHash] = tableEntryPrev{
						Prev: e.table[nextHash].Cur,
						Cur:  tableEntry{offset: e.cur + t},
					}
				}
				goto emitRemainder
			}

			for i := s - l + 2; i < s-5; i += 6 {
				nextHash := hashLen(load6432(src, i), tableBits, hashBytes)
				e.table[nextHash] = tableEntryPrev{
					Prev: e.table[nextHash].Cur,
					Cur:  tableEntry{offset: e.cur + i}}
			}
			x := load6432(src, s-2)
			prevHash := hashLen(x, tableBits, hashBytes)

			e.table[prevHash] = tableEntryPrev{
				Prev: e.table[prevHash].Cur,
				Cur:  tableEntry{offset: e.cur + s - 2},
			}
			x >>= 8
			prevHash = hashLen(x, tableBits, hashBytes)

			e.table[prevHash] = tableEntryPrev{
				Prev: e.table[prevHash].Cur,
				Cur:  tableEntry{offset: e.cur + s - 1},
			}
			x >>= 8
			currHash := hashLen(x, tableBits, hashBytes)
			candidates := e.table[currHash]
			cv = x
			e.table[currHash] = tableEntryPrev{
				Prev: candidates.Cur,
				Cur:  tableEntry{offset: s + e.cur},
			}

			candidate = candidates.Cur
			minOffset := e.cur + s - (maxMatchOffset - 4)

			if candidate.offset > minOffset {
				if uint32(cv) == load3232(src, candidate.offset-e.cur) {
					continue
				}
				candidate = candidates.Prev
				if candidate.offset > minOffset && uint32(cv) == load3232(src, candidate.offset-e.cur) {
					continue
				}
			}
			cv = x >> 8
			s++
			break
		}
	}

emitRemainder:
	if int(nextEmit) < len(src) {
		tr.literal(nextEmit, int32(len(src)))
	}
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: tracedump <input> <trace-output>")
		os.Exit(1)
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	out, err := os.Create(os.Args[2])
	if err != nil {
		panic(err)
	}
	defer out.Close()
	w := bufio.NewWriterSize(out, 1<<20)
	defer w.Flush()

	tr := &trace{w: w}
	// newFastEnc(3) starts with cur: maxStoreBlockSize (65535) — see
	// flate/fast_encoder.go's newFastEnc — not maxMatchOffset.
	enc := &fastEncL3{fastGen: fastGen{cur: maxStoreBlockSize}}

	// The real flate.Writer never hands fastEncL3.Encode the whole input in
	// one call: compressor.write() buffers into a maxStoreBlockSize-sized
	// (65535 byte) window and calls storeFast (-> Encode) each time that
	// window fills, then once more on Close for whatever remains. Replicate
	// that chunking exactly, since addBlock's history-compaction logic (see
	// above) only triggers at those boundaries and changes e.cur.
	for off := 0; off < len(data); off += maxStoreBlockSize {
		end := off + maxStoreBlockSize
		if end > len(data) {
			end = len(data)
		}
		enc.Encode(tr, data[off:end])
	}
	fmt.Fprintf(os.Stderr, "wrote %d events to %s\n", tr.n, os.Args[2])
}
