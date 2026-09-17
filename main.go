// Reproduces non-deterministic pgzip output across CPU architectures.
//
// Compressing the same input, with the same level and the same explicit
// concurrency, produces different (though both valid) compressed bytes on
// amd64 vs arm64. Decompressed content is identical either way; only the
// compressed representation differs.
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

	"github.com/klauspost/pgzip"
)

func main() {
	in, err := os.Open(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer in.Close()

	h := sha256.New()
	zw, err := pgzip.NewWriterLevel(h, 3)
	if err != nil {
		panic(err)
	}
	if err := zw.SetConcurrency(1<<20, 1); err != nil {
		panic(err)
	}
	n, err := io.Copy(zw, in)
	if err != nil {
		panic(err)
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	fmt.Printf("GOARCH=%s uncompressed=%d compressed_sha256=%s\n", runtime.GOARCH, n, hex.EncodeToString(h.Sum(nil)))
}
