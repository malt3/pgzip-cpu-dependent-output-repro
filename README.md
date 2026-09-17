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
