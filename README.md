# pgzip behavioral difference between amd64 and aarch64

## Expectation

When using `github.com/klauspost/pgzip`, I expect the compressed byte stream to be identical (independent fromt the CPU architecture).

## Reality

For a small subset of inputs, this doesn't hold.

## How to reproduce

To reproduce, run this on a Mac with Rosetta or Linux with QEMU user emulation.
Alternatively, run the two binaries natively on the right CPU architecture.

```
GOARCH=arm64 go build -o rep_arm64
GOARCH=amd64 go build -o rep_amd64
./rep_arm64 testdata.bin
GOARCH=arm64 uncompressed=589605 compressed_sha256=83fd0e6ca9f7f608b694cfc46a67e14ce804cf8a4fd331c2d7d64de0327eb8d8
./rep_amd64 testdata.bin
GOARCH=amd64 uncompressed=589605 compressed_sha256=5b9cc20bb02558027778edf7d71d7ed6e4742f6215690ce0a69c8476345a2e98
```
