package main

import (
	"fmt"
	"time"

	dorisstreamload "github.com/wushilin/doris_go_stream_load"
)

func main() {
	if err := runCSVExamples(); err != nil {
		panic(err)
	}
	if err := runJSONExamples(); err != nil {
		panic(err)
	}
}

func runCSVExamples() error {
	fmt.Println("== CSV examples ==")

	client, err := dorisstreamload.NewClient(dorisstreamload.Config{
		StreamLoadURL:             "http://example.invalid/api/demo/events/_stream_load",
		Columns:                   []string{"event_time", "user_id", "event_name"},
		Mode:                      dorisstreamload.ModeCSV,
		Validation:                dorisstreamload.ValidateSyntax,
		DorisUploadWorkers:        2,
		BatchBytes:                1024 * 1024,
		Linger:                    10 * time.Millisecond,
		FakeSend:                  true,
		FakeSendDelay:             20 * time.Millisecond,
		FakeSendDelaySet:          true,
		DorisUploadRequestTimeout: 30 * time.Second,
		DorisUploadTimeout:        30 * time.Second,
	})
	if err != nil {
		return err
	}
	defer client.Close()

	// 1. Single send + explicit handle wait.
	handle, err := client.Send("2026-05-01T10:00:00Z,1,login")
	if err != nil {
		return err
	}
	result := handle.Wait()
	fmt.Printf("csv send: success=%t attempts=%d label=%s\n", result.Success(), result.Attempts, labelOf(result))

	// 2. Single send + Done()/Result().
	doneHandle, err := client.Send("2026-05-01T10:00:01Z,2,logout")
	if err != nil {
		return err
	}
	<-doneHandle.Done()
	doneResult, ok := doneHandle.Result()
	fmt.Printf("csv send done/result: ok=%t success=%t label=%s\n", ok, doneResult.Success(), labelOf(doneResult))

	// 3. Batch send + one shared batch handle.
	handle, err = client.SendBatch([]string{
		"2026-05-01T10:00:02Z,3,purchase",
		"2026-05-01T10:00:03Z,4,refund",
		"2026-05-01T10:00:04Z,5,search",
	})
	if err != nil {
		return err
	}
	batchResult := handle.Wait()
	fmt.Printf("csv send batch: success=%t label=%s\n", batchResult.Success(), labelOf(batchResult))

	// 4. Single send with callback.
	callbackDone := make(chan dorisstreamload.DeliveryResult, 1)
	_, err = client.SendWithCallback(func(r dorisstreamload.DeliveryResult) {
		callbackDone <- r
	}, "2026-05-01T10:00:05Z,6,callback")
	if err != nil {
		return err
	}
	callbackResult := <-callbackDone
	fmt.Printf("csv send with callback: success=%t label=%s\n", callbackResult.Success(), labelOf(callbackResult))

	// 5. Batch send with callback + one shared batch handle.
	batchCallbackDone := make(chan dorisstreamload.DeliveryResult, 1)
	handle, err = client.SendBatchWithCallback(func(r dorisstreamload.DeliveryResult) {
		batchCallbackDone <- r
	}, []string{
		"2026-05-01T10:00:06Z,7,batch-callback-a",
		"2026-05-01T10:00:07Z,8,batch-callback-b",
	})
	if err != nil {
		return err
	}
	handleResult := handle.Wait()
	fmt.Printf("csv send batch with callback handle: success=%t label=%s\n", handleResult.Success(), labelOf(handleResult))
	batchCallbackResult := <-batchCallbackDone
	fmt.Printf("csv send batch with callback: success=%t label=%s\n", batchCallbackResult.Success(), labelOf(batchCallbackResult))

	stats := client.Stats()
	fmt.Printf("csv stats: jobs=%d errors=%d records=%d bytes=%d avg_load=%s p50=%s attempts=%d\n",
		stats.TotalLoadJobs,
		stats.ErrorJobs,
		stats.RecordsSent,
		stats.TotalBytesSent,
		stats.AverageLoadTime.Round(time.Millisecond),
		stats.P50LoadTime.Round(time.Millisecond),
		stats.TotalUploadAttempts,
	)

	return nil
}

func runJSONExamples() error {
	fmt.Println("== JSON examples ==")

	client, err := dorisstreamload.NewClient(dorisstreamload.Config{
		StreamLoadURL:             "http://example.invalid/api/demo/events/_stream_load",
		Columns:                   []string{"event_time", "user_id", "event_name"},
		Mode:                      dorisstreamload.ModeJSON,
		Validation:                dorisstreamload.ValidateSyntax,
		DorisUploadWorkers:        2,
		BatchBytes:                1024 * 1024,
		Linger:                    10 * time.Millisecond,
		FakeSend:                  true,
		FakeSendDelay:             20 * time.Millisecond,
		FakeSendDelaySet:          true,
		DorisUploadRequestTimeout: 30 * time.Second,
		DorisUploadTimeout:        30 * time.Second,
	})
	if err != nil {
		return err
	}
	defer client.Close()

	handle, err := client.Send(`{"event_time":"2026-05-01T11:00:00Z","user_id":101,"event_name":"login"}`)
	if err != nil {
		return err
	}
	result := handle.Wait()
	fmt.Printf("json send: success=%t label=%s\n", result.Success(), labelOf(result))

	handle, err = client.SendBatch([]string{
		`{"event_time":"2026-05-01T11:00:01Z","user_id":102,"event_name":"search"}`,
		`{"event_time":"2026-05-01T11:00:02Z","user_id":103,"event_name":"purchase"}`,
	})
	if err != nil {
		return err
	}
	batchResult := handle.Wait()
	fmt.Printf("json send batch: success=%t label=%s\n", batchResult.Success(), labelOf(batchResult))

	stats := client.Stats()
	fmt.Printf("json stats: jobs=%d errors=%d records=%d bytes=%d avg_load=%s p50=%s attempts=%d\n",
		stats.TotalLoadJobs,
		stats.ErrorJobs,
		stats.RecordsSent,
		stats.TotalBytesSent,
		stats.AverageLoadTime.Round(time.Millisecond),
		stats.P50LoadTime.Round(time.Millisecond),
		stats.TotalUploadAttempts,
	)

	return nil
}

func labelOf(result dorisstreamload.DeliveryResult) string {
	if result.Response == nil {
		return ""
	}
	return result.Response.Label
}
