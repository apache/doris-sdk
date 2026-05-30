# Example Program

This folder contains a deliberately minimal example program that imports:

```go
import dorisstreamload "github.com/wushilin/doris_go_stream_load"
```

The example does not read a config file. It uses hard-coded demo data and `FakeSend=true` so you can see the API shape immediately.

## What It Demonstrates

The example covers:

- CSV single send
- CSV single send with `Done()` / `Result()`
- CSV batch send
- CSV single send with callback
- CSV batch send with callback
- JSON single send
- JSON batch send
- handle waiting
- group handle checking
- reading `client.Stats()`

For throughput and batching experiments, use the repository-level `benchmark_demo.go` with `benchmark_demo.conf` instead. This folder is intentionally the simple integration example.

## Run

```sh
cd example
go run .
```

## Notes

- `FakeSend=true` means no real Doris server is required.
- The example still uses realistic `StreamLoadURL`, `Columns`, batching, and timeout settings.
- `go.mod` includes:

```go
replace github.com/wushilin/doris_go_stream_load => ..
```

so the example uses the local checkout in this workspace.
