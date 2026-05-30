# Doris Stream Load Go SDK

`dorisstreamload` is a Go SDK for sending CSV rows or JSON objects to Apache Doris Stream Load.

It keeps the public model small:

- `ModeCSV` and `ModeJSON`
- string input only
- `Send(...)` for one item
- `SendBatch(...)` for many items
- batching, queueing, retry, label polling, callback, handle, and stats are handled inside the SDK

## Requirements

Go `1.20` or newer.

## Install

```sh
go get github.com/wushilin/doris_go_stream_load@v1.0.0
```

Import path:

```go
import dorisstreamload "github.com/wushilin/doris_go_stream_load"
```

For v1.x releases, keep the import path as `github.com/wushilin/doris_go_stream_load`; Go modules do not add `/v1` to the path.

## FakeSend Quick Start

This example is local runnable. It does not require a Doris cluster because `FakeSend: true` bypasses real HTTP upload and returns a successful Stream Load result.

```sh
mkdir dorisstreamload-quickstart
cd dorisstreamload-quickstart
go mod init quickstart
go get github.com/wushilin/doris_go_stream_load@v1.0.0
```

Create `main.go`:

```go
package main

import (
	"fmt"
	"time"

	dorisstreamload "github.com/wushilin/doris_go_stream_load"
)

func main() {
	client, err := dorisstreamload.NewClient(dorisstreamload.Config{
		StreamLoadURL:             "http://example.invalid/api/demo/events/_stream_load",
		Columns:                   []string{"event_time", "user_id", "event_name"},
		Mode:                      dorisstreamload.ModeCSV,
		Validation:                dorisstreamload.ValidateSyntax,
		BatchBytes:                1024 * 1024,
		Linger:                    10 * time.Millisecond,
		DorisUploadWorkers:        1,
		DorisUploadRequestTimeout: 30 * time.Second,
		DorisUploadTimeout:        30 * time.Second,
		FakeSend:                  true,
		FakeSendDelay:             20 * time.Millisecond,
		FakeSendDelaySet:          true,
	})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	handle, err := client.SendBatch([]string{
		"2026-05-01T10:00:00Z,1,login",
		"2026-05-01T10:00:01Z,2,logout",
	})
	if err != nil {
		panic(err)
	}

	result := handle.Wait()
	if result.Err != nil {
		panic(result.Err)
	}

	fmt.Printf("success label=%s attempts=%d records=%d\n",
		result.Response.Label,
		result.Attempts,
		client.Stats().RecordsSent,
	)
}
```

Run it:

```sh
go run .
```

The repository also contains:

- `example/`: simple runnable FakeSend examples for CSV, JSON, callbacks, handles, and stats
- `benchmark_demo.go` plus `benchmark_demo.conf`: a larger benchmark-style demo for throughput, wait modes, batching, callbacks, and periodic stats
- `normal_demo.go`: a plain Doris Stream Load lifecycle demo without this SDK

## Real Doris Cluster

For a real cluster, create a table first. The following DDL assumes append-only event data, common time-range queries, and a small/general-purpose cluster. Tune partitions, buckets, and replication for your own data volume and cluster size.

```sql
CREATE DATABASE IF NOT EXISTS demo;

CREATE TABLE IF NOT EXISTS demo.events (
    event_time DATETIME NOT NULL,
    user_id BIGINT NOT NULL,
    event_name VARCHAR(64) NOT NULL
)
DUPLICATE KEY(event_time, user_id)
PARTITION BY RANGE(event_time) ()
DISTRIBUTED BY HASH(user_id) BUCKETS 8
PROPERTIES (
    "dynamic_partition.enable" = "true",
    "dynamic_partition.time_unit" = "DAY",
    "dynamic_partition.start" = "-30",
    "dynamic_partition.end" = "3",
    "dynamic_partition.prefix" = "p",
    "replication_num" = "1",
    "compression" = "zstd"
);
```

Then configure the SDK with either a full Stream Load URL:

```go
client, err := dorisstreamload.NewClient(dorisstreamload.Config{
	StreamLoadURL:       "http://127.0.0.1:8030/api/demo/events/_stream_load",
	Columns:             []string{"event_time", "user_id", "event_name"},
	Mode:                dorisstreamload.ModeCSV,
	AuthenticationType:  dorisstreamload.AuthenticationBasic,
	AuthenticationToken: "root:password",
})
```

Or configure endpoint, database, and table separately:

```go
client, err := dorisstreamload.NewClient(dorisstreamload.Config{
	Endpoint:            "http://127.0.0.1:8030",
	Database:            "demo",
	Table:               "events",
	Columns:             []string{"event_time", "user_id", "event_name"},
	Mode:                dorisstreamload.ModeJSON,
	AuthenticationType:  dorisstreamload.AuthenticationBasic,
	AuthenticationToken: "root:password",
})
```

For basic auth, use `AuthenticationToken: "user:password"`. If Doris does not require auth, leave `AuthenticationType` and `AuthenticationToken` empty.

## CSV And JSON

Each submitted string is one logical item.

In `ModeCSV`, one string is one CSV row:

```go
client.Send("2026-05-01T10:00:00Z,1,login")
client.SendBatch([]string{
	"2026-05-01T10:00:01Z,2,logout",
	"2026-05-01T10:00:02Z,3,purchase",
})
```

When several CSV items are coalesced into one outbound Doris request, the body is newline joined:

```text
row1
row2
row3
```

In `ModeJSON`, one string is one JSON object:

```go
client.Send(`{"event_time":"2026-05-01T10:00:00Z","user_id":1,"event_name":"login"}`)
client.SendBatch([]string{
	`{"event_time":"2026-05-01T10:00:01Z","user_id":2,"event_name":"logout"}`,
	`{"event_time":"2026-05-01T10:00:02Z","user_id":3,"event_name":"purchase"}`,
})
```

When several JSON items are coalesced into one outbound Doris request, the body is one JSON array:

```json
[{"id":1},{"id":2},{"id":3}]
```

Validation is controlled by `Config.Validation`:

| Value | CSV behavior | JSON behavior |
|---|---|---|
| `ValidateNone` | No parsing before queue admission | No parsing before queue admission |
| `ValidateSyntax` | Non-blank, parses as one row, field count matches `Columns` | Non-blank, valid JSON object |
| `ValidateStrict` | Same as syntax today | Valid object, every configured column present, no extra keys |

CSV formatting defaults to separator `,` and quote `"`. Override with `CSVSeparator` and `CSVQuote`.

## Callback, Handle, Stats

Every accepted send returns a `Handle`. A submitted batch has one shared handle because it concludes as one Doris load outcome.

```go
handle, err := client.SendBatch([]string{
	"2026-05-01T10:00:00Z,1,login",
	"2026-05-01T10:00:01Z,2,logout",
})
if err != nil {
	return err
}

result := handle.Wait()
if result.Err != nil {
	return result.Err
}
```

Supported handle methods:

- `Wait() DeliveryResult`
- `WaitContext(ctx context.Context) (DeliveryResult, error)`
- `Done() <-chan struct{}`
- `IsDone() bool`
- `Result() (DeliveryResult, bool)`

Callbacks run once per submitted batch, not once per row:

```go
handle, err := client.SendBatchWithCallback(func(result dorisstreamload.DeliveryResult) {
	if result.Err != nil {
		fmt.Printf("delivery failed attempts=%d err=%v\n", result.Attempts, result.Err)
		return
	}
	fmt.Printf("delivered label=%s\n", result.Response.Label)
}, []string{
	"2026-05-01T10:00:00Z,1,login",
	"2026-05-01T10:00:01Z,2,logout",
})
```

`DeliveryResult` includes `Err`, `Attempts`, `StatusCode`, `Response`, `StartedAt`, and `FinishedAt`.

Use `client.Stats()` for a lifetime snapshot:

```go
stats := client.Stats()
fmt.Printf("jobs=%d errors=%d records=%d bytes=%d p99=%s\n",
	stats.TotalLoadJobs,
	stats.ErrorJobs,
	stats.RecordsSent,
	stats.TotalBytesSent,
	stats.P99LoadTime,
)
```

Stats include worker counts, job counts, error rate, retry average, total upload attempts, bytes/records sent, average rates, and p50/p90/p99/p999 load time.

## Parameters

Required:

| Field | Description |
|---|---|
| `Columns` | Doris target columns in record order |
| `Mode` | `ModeCSV` or `ModeJSON`; defaults to `ModeCSV` when unset |
| `StreamLoadURL` | Full URL like `http://host:8030/api/db/table/_stream_load` |
| `Endpoint` + `Database` + `Table` | Alternative to `StreamLoadURL` |

Connection and auth:

| Field | Default | Description |
|---|---|---|
| `AuthenticationType` | `AuthenticationNone` | Use `AuthenticationBasic` for basic auth |
| `AuthenticationToken` | empty | For basic auth, `user:password` |
| `Headers` | empty | Extra HTTP headers sent to Doris |
| `TLSSkipVerify` | `false` | Skip TLS certificate verification |
| `TLSCACertPath` | empty | Custom CA certificate path |
| `HTTPClient` | SDK-created client | Optional custom HTTP client |

Batching and queueing:

| Field | Default | Description |
|---|---|---|
| `BatchBytes` | `90 MiB` | Max outbound request body size; also the per-send admission limit |
| `Linger` | `5ms` | Max age of an open outbound batch before dispatch |
| `MaxQueueSize` | `100000` | Max submitted batches in the intake queue |
| `MaxQueueWaitTime` | `0` | How long `Send` waits for queue space; `0` waits indefinitely |
| `MaxUploadQueueSize` | `1` | Channel depth between batcher and upload workers |
| `DorisUploadWorkers` | `1` | Concurrent upload goroutines |

Retry and timing:

| Field | Default | Description |
|---|---|---|
| `DorisUploadRequestTimeout` | `300s` | HTTP deadline for one upload or label-poll request; minimum `10s` |
| `DorisUploadTimeout` | `300s` | Total retry decision budget after retriable upload outcomes |
| `StatusPollTimeout` | `300s` | Max time spent polling a label after an ambiguous outcome |
| `CallbackTimeout` | `100ms` | Reserved callback timing budget in config |
| `SlowCallbackWarn` | `10ms` | Slow callback warning threshold |

Behavior:

| Field | Default | Description |
|---|---|---|
| `Validation` | `ValidateSyntax` | `ValidateNone`, `ValidateSyntax`, or `ValidateStrict` |
| `LabelPrefix` | `go_stream_load` | Prefix for generated Doris labels |
| `FakeSend` | `false` | Bypass real HTTP upload and return fake success |
| `FakeSendDelay` | `500ms` | Artificial fake-send delay |
| `FakeSendDelaySet` | `false` | Set true to make explicit `0` fake-send delay distinct from unset |
| `Logger` | nil | Optional logger with `Printf` |
| `LogLevel` | `LogLevelInfo` | `LogLevelError`, `LogLevelInfo`, or `LogLevelDebug` |
| `LogLevelSet` | `false` | Set true when explicitly configuring `LogLevelError`, because error is the zero value |

`BatchBytes` and `Linger` work together like Kafka `batch.size` and `linger.ms`: the SDK dispatches when the payload reaches `BatchBytes` or the open batch reaches `Linger`, whichever happens first.

## LoaderConfig

`LoaderConfig` is the JSON-serializable counterpart of `Config`.

```go
lc, err := dorisstreamload.LoadLoaderConfig("loader.json")
if err != nil {
	return err
}
cfg, err := lc.Config()
if err != nil {
	return err
}
client, err := dorisstreamload.NewClient(cfg)
```

Duration fields use Go duration strings such as `"500ms"`, `"2s"`, and `"5m"`. JSON logging also supports `log_level_set`; set it to `true` when `log_level` is `0` and you mean `LogLevelError`.

## Common Errors

`Send(...)` and `SendBatch(...)` can fail before data enters the SDK queue:

| Error | Meaning | Typical action |
|---|---|---|
| `ErrClientClosed` | The client is closed | Stop sending or create a new client |
| `ErrQueueFull` | The intake queue stayed full longer than `MaxQueueWaitTime` | Increase workers, queue size, or timeout; reduce producer rate |
| `ErrSendTooLarge` | One submitted item or batch is larger than `BatchBytes` | Split the caller-side batch or raise `BatchBytes` up to the 90 MiB limit |
| validation error | CSV/JSON failed configured validation | Fix the row/object or loosen `Validation` |

Delivery can fail after queue admission; check `DeliveryResult.Err` from the handle or callback:

| Failure | Meaning | Typical action |
|---|---|---|
| HTTP 4xx from Doris | Bad URL, auth, table, schema, label, or data format | Inspect `StatusCode` and Doris response message |
| HTTP 5xx or retriable transport error | Doris/BE/network may be unavailable | The SDK retries within `DorisUploadTimeout`; check cluster health |
| ambiguous transport error | Request may have reached Doris but response was lost | The SDK polls the load label to decide whether it became visible |
| label state `UNKNOWN` | Doris does not know the label | The transaction was not registered; final result is failure |
| status poll timeout | Doris did not reach a final visible/failed state in time | Increase `StatusPollTimeout` or inspect Doris load jobs |
| callback too slow | Callback exceeded `SlowCallbackWarn` and produced an info log | Keep callbacks small; hand work to another goroutine if needed |

## Shutdown

`Close()` stops intake, drains accepted work, and waits for in-flight deliveries. After `Close()`, new sends return `ErrClientClosed`; already accepted handles still complete.
