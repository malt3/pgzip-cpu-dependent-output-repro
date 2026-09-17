// Reproduces non-deterministic compressed output across CPU architectures.
//
// Compressing the same input, at the same level, produces different (though
// both valid) compressed bytes on amd64 vs arm64. Decompressed content is
// identical either way; only the compressed representation differs.
//
// Usage: go run . testdata.bin
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"

	kflate "github.com/klauspost/compress/flate"
	"github.com/klauspost/pgzip"

	sflate "compress/flate"
)

const level = 3

func hashOf(name string, compress func(w io.Writer) (io.WriteCloser, error), data []byte) {
	h := sha256.New()
	wc, err := compress(h)
	if err != nil {
		fmt.Printf("%-24s error creating writer: %v\n", name, err)
		return
	}
	if _, err := wc.Write(data); err != nil {
		fmt.Printf("%-24s error writing: %v\n", name, err)
		return
	}
	if err := wc.Close(); err != nil {
		fmt.Printf("%-24s error closing: %v\n", name, err)
		return
	}
	fmt.Printf("%-24s GOARCH=%-6s sha256=%s\n", name, runtime.GOARCH, hex.EncodeToString(h.Sum(nil)))
}

func main() {
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}

	hashOf("pgzip (jobs=1)", func(w io.Writer) (io.WriteCloser, error) {
		zw, err := pgzip.NewWriterLevel(w, level)
		if err != nil {
			return nil, err
		}
		if err := zw.SetConcurrency(1<<20, 1); err != nil {
			return nil, err
		}
		return zw, nil
	}, data)

	hashOf("klauspost/compress/flate", func(w io.Writer) (io.WriteCloser, error) {
		return kflate.NewWriter(w, level)
	}, data)

	hashOf("stdlib compress/flate", func(w io.Writer) (io.WriteCloser, error) {
		return sflate.NewWriter(w, level)
	}, data)
}
