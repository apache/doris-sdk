package dorisstreamload

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeSender struct {
	mu            sync.Mutex
	attempts      int
	bodies        []string
	labels        []string
	outcomes      []fakeOutcome
	pollResponses []fakePollResponse
	pollCalls     int
	sendDelay     time.Duration
}

type fakeOutcome struct {
	outcome sendOutcome
	err     error
}

type fakePollResponse struct {
	state loadStateResponse
	err   error
}

func (s *fakeSender) Send(_ context.Context, batch *deliveryBatch) (sendOutcome, error) {
	body, _, err := batch.encodeBody()
	if err != nil {
		return sendOutcome{}, err
	}
	if s.sendDelay > 0 {
		time.Sleep(s.sendDelay)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.attempts++
	s.bodies = append(s.bodies, string(body))
	s.labels = append(s.labels, batch.label)
	if len(s.outcomes) == 0 {
		return sendOutcome{statusCode: 200, response: &StreamLoadResponse{Status: "Success", Label: batch.label}}, nil
	}

	outcome := s.outcomes[0]
	s.outcomes = s.outcomes[1:]
	return outcome.outcome, outcome.err
}

func (s *fakeSender) PollLabel(_ context.Context, _ string) (loadStateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pollCalls++
	if len(s.pollResponses) == 0 {
		return loadStateResponse{StatusCode: 200, State: "VISIBLE", Message: "success"}, nil
	}
	response := s.pollResponses[0]
	s.pollResponses = s.pollResponses[1:]
	return response.state, response.err
}

func (s *fakeSender) Attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func (s *fakeSender) Bodies() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.bodies...)
}

func (s *fakeSender) Labels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.labels...)
}

func (s *fakeSender) PollCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pollCalls
}

func newTestClient(t *testing.T, sender sender, cfg Config) *Client {
	t.Helper()

	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate() error = %v", err)
	}
	return newClientWithSender(cfg, sender)
}

func withTestPollBackoff(t *testing.T, initial, max time.Duration) {
	t.Helper()
	prevInitial := statusPollInitialBackoff
	prevMax := statusPollMaxBackoff
	statusPollInitialBackoff = initial
	statusPollMaxBackoff = max
	t.Cleanup(func() {
		statusPollInitialBackoff = prevInitial
		statusPollMaxBackoff = prevMax
	})
}

func withTestUploadRetryBackoff(t *testing.T, initial, max time.Duration) {
	t.Helper()
	prevInitial := uploadRetryInitialBackoff
	prevMax := uploadRetryMaxBackoff
	uploadRetryInitialBackoff = initial
	uploadRetryMaxBackoff = max
	t.Cleanup(func() {
		uploadRetryInitialBackoff = prevInitial
		uploadRetryMaxBackoff = prevMax
	})
}

func TestCSVSingleCoalescesRows(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord("a", "b")
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}

	if got := sender.Attempts(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	if got := sender.Bodies(); len(got) != 1 || got[0] != "a\nb" {
		t.Fatalf("bodies = %#v, want [\"a\\nb\"]", got)
	}
}

func TestClientStatsTracksCompletedJobsAndRetries(t *testing.T) {
	withTestUploadRetryBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	sender := &fakeSender{
		sendDelay: 5 * time.Millisecond,
		outcomes: []fakeOutcome{
			{outcome: sendOutcome{statusCode: 502}, err: &streamLoadError{StatusCode: 502, Message: "temporary", Retriable: true}},
			{outcome: sendOutcome{statusCode: 200, response: &StreamLoadResponse{Status: "Success"}}},
			{outcome: sendOutcome{statusCode: 401}, err: &streamLoadError{StatusCode: 401, Message: "no auth", Retriable: false}},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:             "http://example.invalid/api/db/table/_stream_load",
		Columns:                   []string{"c1"},
		Mode:                      ModeCSV,
		Linger:                    1 * time.Millisecond,
		DorisUploadWorkers:        1,
		MaxUploadQueueSize:        8,
		DorisUploadRequestTimeout: 10 * time.Second,
		DorisUploadTimeout:        100 * time.Millisecond,
		Logger:                    log.New(io.Discard, "", 0),
	})
	defer client.Close()

	h1, err := client.Send("a")
	if err != nil {
		t.Fatalf("Send() first error = %v", err)
	}
	if result := h1.Wait(); result.Err != nil {
		t.Fatalf("first Wait() error = %v", result.Err)
	}
	h2, err := client.Send("b")
	if err != nil {
		t.Fatalf("Send() second error = %v", err)
	}
	if result := h2.Wait(); result.Err == nil {
		t.Fatal("second Wait() error = nil, want failure")
	}

	stats := client.Stats()
	if stats.TotalWorkers != 1 {
		t.Fatalf("TotalWorkers = %d, want 1", stats.TotalWorkers)
	}
	if stats.TotalLoadJobs != 2 {
		t.Fatalf("TotalLoadJobs = %d, want 2", stats.TotalLoadJobs)
	}
	if stats.ErrorJobs != 1 {
		t.Fatalf("ErrorJobs = %d, want 1", stats.ErrorJobs)
	}
	if stats.ErrorRate != 0.5 {
		t.Fatalf("ErrorRate = %f, want 0.5", stats.ErrorRate)
	}
	if stats.TotalUploadAttempts != 3 {
		t.Fatalf("TotalUploadAttempts = %d, want 3", stats.TotalUploadAttempts)
	}
	if stats.RecordsSent != 3 {
		t.Fatalf("RecordsSent = %d, want 3", stats.RecordsSent)
	}
	if stats.TotalBytesSent != 3 {
		t.Fatalf("TotalBytesSent = %d, want 3", stats.TotalBytesSent)
	}
	if stats.AverageRetries != 0.5 {
		t.Fatalf("AverageRetries = %f, want 0.5", stats.AverageRetries)
	}
	if stats.AverageLoadTime <= 0 {
		t.Fatalf("AverageLoadTime = %s, want > 0", stats.AverageLoadTime)
	}
	if stats.P50LoadTime <= 0 || stats.P90LoadTime <= 0 || stats.P99LoadTime <= 0 || stats.P999LoadTime <= 0 {
		t.Fatalf("percentiles = p50=%s p90=%s p99=%s p999=%s, want all > 0", stats.P50LoadTime, stats.P90LoadTime, stats.P99LoadTime, stats.P999LoadTime)
	}
	if stats.AverageBytesRate <= 0 {
		t.Fatalf("AverageBytesRate = %f, want > 0", stats.AverageBytesRate)
	}
	if stats.AverageRecordsRate <= 0 {
		t.Fatalf("AverageRecordsRate = %f, want > 0", stats.AverageRecordsRate)
	}
}

func TestClientStatsTracksBusyAndIdleWorkers(t *testing.T) {
	sender := &fakeSender{sendDelay: 50 * time.Millisecond}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:             "http://example.invalid/api/db/table/_stream_load",
		Columns:                   []string{"c1"},
		Mode:                      ModeCSV,
		Linger:                    1 * time.Millisecond,
		DorisUploadWorkers:        1,
		MaxUploadQueueSize:        8,
		DorisUploadRequestTimeout: 10 * time.Second,
		Logger:                    log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.Send("a")
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	deadline := time.Now().Add(time.Second)
	var observedBusy bool
	for time.Now().Before(deadline) {
		stats := client.Stats()
		if stats.BusyWorkers == 1 && stats.IdleWorkers == 0 {
			observedBusy = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !observedBusy {
		t.Fatalf("never observed busy worker; stats=%+v", client.Stats())
	}

	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}
	stats := client.Stats()
	if stats.BusyWorkers != 0 || stats.IdleWorkers != 1 {
		t.Fatalf("final worker stats = busy=%d idle=%d, want busy=0 idle=1", stats.BusyWorkers, stats.IdleWorkers)
	}
}

func TestDefaultBatchingConfig(t *testing.T) {
	cfg := Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
	}.withDefaults()

	if cfg.BatchBytes != 90*1024*1024 {
		t.Fatalf("BatchBytes = %d, want 90MiB", cfg.BatchBytes)
	}
	if cfg.Linger != 5*time.Millisecond {
		t.Fatalf("Linger = %s, want 5ms", cfg.Linger)
	}
	if cfg.MaxQueueSize != 100_000 {
		t.Fatalf("MaxQueueSize = %d, want 100000", cfg.MaxQueueSize)
	}
	if cfg.MaxUploadQueueSize != 1 {
		t.Fatalf("MaxUploadQueueSize = %d, want 1", cfg.MaxUploadQueueSize)
	}
	if cfg.Validation != ValidateSyntax {
		t.Fatalf("Validation = %q, want %q", cfg.Validation, ValidateSyntax)
	}
}

func TestConfigRejectsBatchSizeAboveHardCap(t *testing.T) {
	_, err := NewClient(Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		BatchBytes:    91 * 1024 * 1024,
	})
	if err == nil || !strings.Contains(err.Error(), "batch bytes cannot be greater than 90 MiB") {
		t.Fatalf("NewClient() error = %v, want batch size cap error", err)
	}
}

func TestNewClientFakeSendSucceedsAfterDelay(t *testing.T) {
	client, err := NewClient(Config{
		StreamLoadURL:             "http://example.invalid/api/db/table/_stream_load",
		Columns:                   []string{"c1"},
		Mode:                      ModeCSV,
		FakeSend:                  true,
		FakeSendDelay:             20 * time.Millisecond,
		BatchBytes:                1,
		DorisUploadRequestTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()

	started := time.Now()
	handle, err := client.SendRecord("a")
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	result := handle.Wait()
	if result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Fatalf("fake send elapsed = %s, want at least 20ms", elapsed)
	}
	if result.Response == nil || result.Response.Status != "Success" || result.Response.Message != "fake send success" {
		t.Fatalf("response = %#v, want fake success response", result.Response)
	}
}

func TestFakeSendDelayDefaultAndExplicitZero(t *testing.T) {
	defaulted := Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		FakeSend:      true,
	}.withDefaults()
	if defaulted.FakeSendDelay != 500*time.Millisecond {
		t.Fatalf("default FakeSendDelay = %s, want 500ms", defaulted.FakeSendDelay)
	}

	explicitZero := Config{
		StreamLoadURL:    "http://example.invalid/api/db/table/_stream_load",
		Columns:          []string{"c1"},
		FakeSend:         true,
		FakeSendDelay:    0,
		FakeSendDelaySet: true,
	}.withDefaults()
	if explicitZero.FakeSendDelay != 0 {
		t.Fatalf("explicit zero FakeSendDelay = %s, want 0", explicitZero.FakeSendDelay)
	}
}

func TestLogLevelErrorCanBeExplicitlyConfigured(t *testing.T) {
	cfg := Config{
		StreamLoadURL:             "http://example.invalid/api/db/t/_stream_load",
		Columns:                   []string{"c1"},
		Mode:                      ModeCSV,
		FakeSend:                  true,
		DorisUploadRequestTimeout: 30 * time.Second,
		DorisUploadTimeout:        30 * time.Second,
		LogLevel:                  LogLevelError,
		LogLevelSet:               true,
	}.withDefaults()

	if cfg.LogLevel != LogLevelError {
		t.Fatalf("LogLevel = %d, want LogLevelError", cfg.LogLevel)
	}
}

func TestBatcherLingersForNonFullBatch(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeCSV, BatchBytes: 90 * 1024 * 1024,
		Linger:             100 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord("a")
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	moreHandle, err := client.SendRecord("b")
	if err != nil {
		t.Fatalf("SendRecord() second error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := handle.WaitContext(ctx); err != nil {
		t.Fatalf("WaitContext() error = %v", err)
	}
	if _, err := moreHandle.WaitContext(ctx); err != nil {
		t.Fatalf("WaitContext() second error = %v", err)
	}
	if got := sender.Bodies(); len(got) != 1 || got[0] != "a\nb" {
		t.Fatalf("bodies = %#v, want one linger-coalesced batch", got)
	}
}

func TestBatcherLingerIsMaxBatchAge(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		BatchBytes:         90 * 1024 * 1024,
		Linger:             60 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	started := time.Now()
	first, err := client.Send("a")
	if err != nil {
		t.Fatalf("Send() first error = %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	second, err := client.Send("b")
	if err != nil {
		t.Fatalf("Send() second error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := first.WaitContext(ctx); err != nil {
		t.Fatalf("WaitContext() first error = %v", err)
	}
	if _, err := second.WaitContext(ctx); err != nil {
		t.Fatalf("WaitContext() second error = %v", err)
	}

	elapsed := time.Since(started)
	if elapsed >= 90*time.Millisecond {
		t.Fatalf("elapsed = %s, want flush near original linger deadline rather than refreshed linger", elapsed)
	}
	if got := sender.Bodies(); len(got) != 1 || got[0] != "a\nb" {
		t.Fatalf("bodies = %#v, want one coalesced batch", got)
	}
}

func TestBatcherDoesNotLingerForFullBatch(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		BatchBytes:         1,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord("a")
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := handle.WaitContext(ctx); err != nil {
		t.Fatalf("WaitContext() error = %v, want full batch dispatched before linger", err)
	}
}

func TestCSVBatchSendCoalescesRows(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeCSV, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendBatch([]string{"a", "b", "c", "d"})
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}

	if got := sender.Bodies(); len(got) != 1 || got[0] != "a\nb\nc\nd" {
		t.Fatalf("bodies = %#v, want [\"a\\nb\\nc\\nd\"]", got)
	}
}

func TestJSONSingleCoalescesRecords(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord(`{"a":1}`, `{"b":2}`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}

	if got := sender.Bodies(); len(got) != 1 || got[0] != `[{"a":1},{"b":2}]` {
		t.Fatalf("bodies = %#v, want merged json array", got)
	}
}

func TestJSONSingleRejectsInvalidRecordWhenValidationEnabled(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeJSON,
		Validation:         ValidateSyntax,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	if _, err := client.SendRecord(`{"a":`); err == nil {
		t.Fatal("SendRecord() error = nil, want invalid json record error")
	}
}

func TestJSONSingleAllowsInvalidRecordWhenValidationDisabled(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeJSON,
		Validation:         ValidateNone,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord(`{"a":`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}
	if got := sender.Bodies(); len(got) != 1 || got[0] != `[{"a":]` {
		t.Fatalf("bodies = %#v, want unvalidated payload forwarded as-is inside array", got)
	}
}

func TestJSONBatchSendCoalescesObjects(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendBatch([]string{`{"a":1}`, `{"b":2}`, `{"c":3}`})
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}

	if got := sender.Bodies(); len(got) != 1 || got[0] != `[{"a":1},{"b":2},{"c":3}]` {
		t.Fatalf("bodies = %#v, want merged array", got)
	}
}

func TestJSONBatchSendPreservesObjectWhitespace(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeJSON,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendBatch([]string{` { "a" : 1 } `, `{"b":2}`})
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}

	if got := sender.Bodies(); len(got) != 1 || got[0] != `[ { "a" : 1 } ,{"b":2}]` {
		t.Fatalf("bodies = %#v, want JSON objects to be coalesced into one array", got)
	}
}

func TestSubmittedBatchStillProducesOneOutboundRequest(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeJSON,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendBatch([]string{`{"a":1}`, `{"b":2}`})
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}

	if got := sender.Attempts(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	if got := sender.Bodies(); len(got) != 1 || got[0] != `[{"a":1},{"b":2}]` {
		t.Fatalf("bodies = %#v, want one outbound request for one submitted batch", got)
	}
}

func TestJSONBatchRejectsInvalidObjectWhenValidationEnabled(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeJSON,
		Validation:         ValidateSyntax,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	if _, err := client.SendRecord(`[{"not":"an object"}]`); err == nil {
		t.Fatal("SendRecord() error = nil, want invalid top-level json object error")
	}
	if _, err := client.SendRecord(`[{"a":]`); err == nil {
		t.Fatal("SendRecord() error = nil, want invalid JSON object error")
	}
}

func TestJSONBatchAllowsInvalidJSONWhenValidationDisabled(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeJSON,
		Validation:         ValidateNone,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendBatch([]string{`{"a":`, `{"b":2}`})
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}

	if got := sender.Bodies(); len(got) != 1 || got[0] != `[{"a":,{"b":2}]` {
		t.Fatalf("bodies = %#v, want unvalidated payloads coalesced without JSON checks", got)
	}
}

func TestClientRetriesTransientFailure(t *testing.T) {
	withTestUploadRetryBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{outcome: sendOutcome{statusCode: 502}, err: &streamLoadError{StatusCode: 502, Message: "temporary", Retriable: true}},
			{outcome: sendOutcome{statusCode: 200, response: &StreamLoadResponse{Status: "Success"}}},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		DorisUploadTimeout: 100 * time.Millisecond,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord(`{"k":1}`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}

	result := handle.Wait()
	if result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}
	if result.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", result.Attempts)
	}
	if got := sender.Attempts(); got != 2 {
		t.Fatalf("sender attempts = %d, want 2", got)
	}
}

func TestClientDoesNotRetryNonRetriableFailure(t *testing.T) {
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{outcome: sendOutcome{statusCode: 401}, err: &streamLoadError{StatusCode: 401, Message: "no auth", Retriable: false}},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord(`{"k":1}`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}

	result := handle.Wait()
	if result.Err == nil {
		t.Fatal("Wait() error = nil, want failure")
	}
	if result.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", result.Attempts)
	}
	if got := sender.Attempts(); got != 1 {
		t.Fatalf("sender attempts = %d, want 1", got)
	}
}

func TestCallbackRunsOnCompletion(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	done := make(chan DeliveryResult, 1)
	handle, err := client.SendRecordWithCallback(func(result DeliveryResult) {
		done <- result
	}, "a")
	if err != nil {
		t.Fatalf("SendRecordWithCallback() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}

	select {
	case result := <-done:
		if result.Err != nil {
			t.Fatalf("callback result = %v", result.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("callback did not fire")
	}
}

func TestBatchCallbackRunsOncePerSubmittedBatch(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	done := make(chan DeliveryResult, 2)
	handle, err := client.SendBatchWithCallback(func(result DeliveryResult) {
		done <- result
	}, []string{"a", "b"})
	if err != nil {
		t.Fatalf("SendBatchWithCallback() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}

	select {
	case result := <-done:
		if result.Err != nil {
			t.Fatalf("callback result = %v", result.Err)
		}
	case <-time.After(time.Second):
		t.Fatal("callback did not fire")
	}

	select {
	case <-done:
		t.Fatal("callback fired more than once for one submitted batch")
	default:
	}
}

func TestCloseRejectsNewMessages(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})

	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := client.SendRecord("a"); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("SendRecord() error = %v, want ErrClientClosed", err)
	}
}

func TestErrSendTooLarge(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		BatchBytes:         2,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	_, err := client.SendRecord("aaa")
	if !errors.Is(err, ErrSendTooLarge) {
		t.Fatalf("SendRecord() error = %v, want ErrSendTooLarge", err)
	}
}

func TestErrSendTooLargeForWholeSubmittedBatch(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		BatchBytes:         4,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	_, err := client.SendBatch([]string{"aa", "bb"})
	if !errors.Is(err, ErrSendTooLarge) {
		t.Fatalf("SendBatch() error = %v, want ErrSendTooLarge", err)
	}
}

func TestJSONBatchAlwaysCoalescesWhenPossible(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, BatchBytes: 1000,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendBatch([]string{`{"a":1}`, `{"b":2}`})
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}
	if got := sender.Bodies(); len(got) != 1 || got[0] != `[{"a":1},{"b":2}]` {
		t.Fatalf("bodies = %#v, want JSON arrays to coalesce whenever batching allows it", got)
	}
}

func TestSendRecordContextReturnsErrQueueFullWithoutPartialEnqueue(t *testing.T) {
	client := newAdmissionOnlyClient(Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeCSV,
		MaxQueueSize:  1,
	})
	mustEnqueueTestItems(t, client, "aa")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	_, err := client.SendRecordContext(ctx, "a", "b")
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("SendRecordContext() error = %v, want ErrQueueFull", err)
	}
	if got := client.intake.Len(); got != 1 {
		t.Fatalf("queued items = %d, want 1 (no partial enqueue)", got)
	}
}

func TestSendRecordEnqueuesOneSubmissionUnitWhenCapacityAvailable(t *testing.T) {
	client := newAdmissionOnlyClient(Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeCSV,
		MaxQueueSize:  1,
	})

	handle, err := client.SendRecord("a", "b")
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if handle == nil {
		t.Fatal("handle = nil, want shared batch handle")
	}
	if got := client.intake.Len(); got != 1 {
		t.Fatalf("queued submissions = %d, want 1", got)
	}
}

func TestSendRecordUsesQueueTimeout(t *testing.T) {
	client := newAdmissionOnlyClient(Config{
		StreamLoadURL:    "http://example.invalid/api/db/table/_stream_load",
		Columns:          []string{"c1"},
		Mode:             ModeCSV,
		MaxQueueSize:     1,
		MaxQueueWaitTime: 5 * time.Millisecond,
	})
	mustEnqueueTestItems(t, client, "x")

	_, err := client.SendRecord("a")
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("SendRecord() error = %v, want ErrQueueFull", err)
	}
	if got := client.intake.Len(); got != 1 {
		t.Fatalf("queued items = %d, want 1", got)
	}
}

func TestSendRecordBatchUsesOneQueueSlot(t *testing.T) {
	client := newAdmissionOnlyClient(Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeCSV,
		MaxQueueSize:  1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	handle, err := client.SendRecordContext(ctx, "a", "b")
	if err != nil {
		t.Fatalf("SendRecordContext() error = %v", err)
	}
	if handle == nil {
		t.Fatal("handle = nil, want shared batch handle")
	}
	if got := client.intake.Len(); got != 1 {
		t.Fatalf("queued submissions = %d, want 1", got)
	}
}

func newAdmissionOnlyClient(cfg Config) *Client {
	cfg = cfg.withDefaults()
	return &Client{
		cfg:     cfg,
		intake:  newRequestQueue(cfg.MaxQueueSize),
		closing: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func mustEnqueueTestItems(t *testing.T, client *Client, records ...string) {
	t.Helper()
	items := make([]*queueItem, 0, len(records))
	payloadBytes := 0
	for _, record := range records {
		item, err := client.prepareItem(record)
		if err != nil {
			t.Fatalf("prepareItem() error = %v", err)
		}
		items = append(items, item)
		payloadBytes += item.byteSize
	}
	submission := newQueuedSubmission(client.cfg.Mode, items, payloadBytes, nil)
	if err := client.intake.Enqueue(context.Background(), submission); err != nil {
		t.Fatalf("test enqueue error = %v", err)
	}
}

func TestHandleWaitContext(t *testing.T) {
	handle := newHandle()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	if _, err := handle.WaitContext(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitContext() error = %v, want context deadline exceeded", err)
	}
}

func TestUploadRetryBackoffGrowsExponentiallyToCap(t *testing.T) {
	withTestUploadRetryBackoff(t, 5*time.Millisecond, 12*time.Millisecond)
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{outcome: sendOutcome{statusCode: 502}, err: &streamLoadError{StatusCode: 502, Message: "temporary", Retriable: true}},
			{outcome: sendOutcome{statusCode: 502}, err: &streamLoadError{StatusCode: 502, Message: "temporary", Retriable: true}},
			{outcome: sendOutcome{statusCode: 200, response: &StreamLoadResponse{Status: "Success"}}},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		DorisUploadTimeout: 100 * time.Millisecond,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	started := time.Now()
	handle, err := client.SendRecord(`{"k":1}`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}

	if elapsed := time.Since(started); elapsed < 15*time.Millisecond {
		t.Fatalf("elapsed = %s, want at least accumulated backoff", elapsed)
	}
}

func TestConcurrentSubmission(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             20 * time.Millisecond,
		DorisUploadWorkers: 2, MaxUploadQueueSize: 64,
		Logger: log.New(io.Discard, "", 0),
	})
	defer client.Close()

	var completed atomic.Int32
	const total = 20
	errCh := make(chan error, total)

	for i := 0; i < total; i++ {
		go func() {
			handle, err := client.SendRecord("row")
			if err != nil {
				errCh <- err
				return
			}
			result := handle.Wait()
			if result.Err != nil {
				errCh <- result.Err
				return
			}
			completed.Add(1)
			errCh <- nil
		}()
	}

	for i := 0; i < total; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent send error = %v", err)
		}
	}
	if got := completed.Load(); got != total {
		t.Fatalf("completed = %d, want %d", got, total)
	}
}

func TestGeneratedLabelIsAttachedToLoadRequest(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeCSV,
		LabelPrefix:   "testprefix", Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord("a")
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}

	labels := sender.Labels()
	if len(labels) != 1 || labels[0] == "" {
		t.Fatalf("labels = %#v, want one generated label", labels)
	}
	if !strings.HasPrefix(labels[0], "testprefix_") {
		t.Fatalf("label = %q, want testprefix_ prefix", labels[0])
	}
}

func TestAmbiguousSendPollsUntilVisible(t *testing.T) {
	withTestPollBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{outcome: sendOutcome{statusCode: 0}, err: &streamLoadError{StatusCode: 0, Message: "timeout", Ambiguous: true}},
		},
		pollResponses: []fakePollResponse{
			{state: loadStateResponse{StatusCode: 200, State: "PREPARE"}},
			{state: loadStateResponse{StatusCode: 200, State: "VISIBLE"}},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		StatusPollTimeout:  200 * time.Millisecond, MaxUploadQueueSize: 8,
		Logger: log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord(`{"k":1}`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	result := handle.Wait()
	if result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}
	if got := sender.PollCalls(); got < 2 {
		t.Fatalf("poll calls = %d, want at least 2", got)
	}
}

func TestAmbiguousSendRetriesOnAborted(t *testing.T) {
	withTestPollBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	withTestUploadRetryBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	// ABORTED means the transaction was registered but the data was not loaded.
	// The client must retry with a new label rather than failing permanently.
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{outcome: sendOutcome{statusCode: 0}, err: &streamLoadError{StatusCode: 0, Message: "timeout", Ambiguous: true}},
			// second attempt (after ABORTED retry) returns default success
		},
		pollResponses: []fakePollResponse{
			{state: loadStateResponse{StatusCode: 200, State: "ABORTED"}},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		DorisUploadTimeout: 100 * time.Millisecond,
		StatusPollTimeout:  200 * time.Millisecond, MaxUploadQueueSize: 8,
		Logger: log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord(`{"k":1}`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	result := handle.Wait()
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want retry to succeed after ABORTED", result.Err)
	}
	if got := sender.Attempts(); got != 2 {
		t.Fatalf("sender attempts = %d, want 2 (initial + retry after ABORTED)", got)
	}
	if got := sender.PollCalls(); got != 1 {
		t.Fatalf("poll calls = %d, want 1", got)
	}
	labels := sender.Labels()
	if labels[0] == labels[1] {
		t.Fatalf("labels[0] == labels[1] = %q, want distinct labels: ABORTED label cannot be reused", labels[0])
	}
}

func TestConfigRejectsInvalidBasicAuthToken(t *testing.T) {
	_, err := NewClient(Config{
		StreamLoadURL:       "http://example.invalid/api/db/table/_stream_load",
		Columns:             []string{"c1"},
		Mode:                ModeCSV,
		AuthenticationType:  AuthenticationBasic,
		AuthenticationToken: "missing-separator", Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	if err == nil {
		t.Fatal("NewClient() error = nil, want invalid token error")
	}
}

func TestConfigWarnsOnUnexpectedStreamLoadURLShape(t *testing.T) {
	var buf strings.Builder
	c, err := NewClient(Config{
		StreamLoadURL: "http://example.invalid/not-a-stream-load-path",
		Columns:       []string{"c1"},
		Mode:          ModeCSV,
		Logger:        log.New(&buf, "", 0),
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v, want nil", err)
	}
	c.Close()
	if buf.Len() == 0 {
		t.Fatal("expected a warning to be logged, got none")
	}
}

func TestConfigRejectsEndpointWithPath(t *testing.T) {
	_, err := NewClient(Config{
		Endpoint: "http://example.invalid:8030/api",
		Database: "db",
		Table:    "tbl",
		Columns:  []string{"c1"},
		Mode:     ModeCSV,
	})
	if err == nil {
		t.Fatal("NewClient() error = nil, want invalid endpoint path error")
	}
}

func TestConfigAcceptsEndpointHostPortOnly(t *testing.T) {
	client, err := NewClient(Config{
		Endpoint: "http://example.invalid:8030",
		Database: "db",
		Table:    "tbl",
		Columns:  []string{"c1"},
		Mode:     ModeCSV,
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v, want success for endpoint host:port", err)
	}
	defer client.Close()
}

func TestTLSCACertPathBuildsTransportWithRootCAs(t *testing.T) {
	certPath := writePEMCert(t, generateSelfSignedCertDER(t))
	client, err := buildHTTPClient(Config{
		TLSCACertPath:             certPath,
		DorisUploadRequestTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildHTTPClient() error = %v", err)
	}

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("TLS root CAs not configured from tls_ca_cert_path")
	}
}

func TestTLSCACertPathIgnoresFileExtensionWhenPEMIsValid(t *testing.T) {
	certPath := writeNamedPEMCert(t, "truststore.data", generateSelfSignedCertDER(t))
	client, err := buildHTTPClient(Config{
		TLSCACertPath:             certPath,
		DorisUploadRequestTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildHTTPClient() error = %v", err)
	}

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("TLS root CAs not configured from extensionless/unknown-extension PEM file")
	}
}

func TestTLSSkipVerifyBuildsTransportWithInsecureFlag(t *testing.T) {
	client, err := buildHTTPClient(Config{
		TLSSkipVerify:             true,
		DorisUploadRequestTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildHTTPClient() error = %v", err)
	}

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify not propagated to TLS client config")
	}
}

func TestTLSConfigAppliesToProvidedHTTPClient(t *testing.T) {
	certPath := writePEMCert(t, generateSelfSignedCertDER(t))
	httpClient := &http.Client{Transport: &http.Transport{}}
	client, err := buildHTTPClient(Config{
		TLSCACertPath:             certPath,
		HTTPClient:                httpClient,
		DorisUploadRequestTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("buildHTTPClient() error = %v", err)
	}

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("provided HTTP client did not receive TLS root CA config")
	}
}

func TestTLSConfigRejectsUnsupportedCustomTransport(t *testing.T) {
	_, err := NewClient(Config{
		StreamLoadURL: "https://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeCSV,
		TLSSkipVerify: true,
		HTTPClient:    &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil })},
	})
	if err == nil {
		t.Fatal("NewClient() error = nil, want unsupported transport error")
	}
}

func TestHTTPSenderAppliesBasicAuthHeader(t *testing.T) {
	transport := &captureTransport{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"Status":"Success"}`)),
		},
	}

	client, err := NewClient(Config{
		StreamLoadURL:       "http://example.invalid/api/db/table/_stream_load",
		Columns:             []string{"c1"},
		Mode:                ModeCSV,
		AuthenticationType:  AuthenticationBasic,
		AuthenticationToken: "demo_user:demo_password", Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
		HTTPClient:         &http.Client{Transport: transport, Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()

	handle, err := client.SendRecord("a")
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}

	req := transport.LastRequest()
	if req == nil {
		t.Fatal("request = nil, want captured request")
	}
	if got := req.Header.Get("Authorization"); got == "" {
		t.Fatal("authorization header missing")
	}
	if got := req.Header.Get("Label"); got == "" {
		t.Fatal("label header missing")
	}
}

func TestPollLabelReadsStateFromDataField(t *testing.T) {
	// The data field is the authoritative source; msg and code are not checked for success.
	transport := &captureTransport{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"msg":"success","code":"0","data":"VISIBLE","count":"0"}`)),
		},
	}

	client, err := NewClient(Config{
		StreamLoadURL:      "http://example.invalid/api/example_db/example_table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
		HTTPClient:         &http.Client{Transport: transport, Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	state, err := (&httpSender{cfg: client.cfg}).PollLabel(context.Background(), "demo_label")
	if err != nil {
		t.Fatalf("PollLabel() error = %v", err)
	}
	if state.State != "VISIBLE" {
		t.Fatalf("state = %q, want VISIBLE", state.State)
	}
}

func TestPollLabelDataFieldTakesPrecedenceOverEnvelope(t *testing.T) {
	// Even when the envelope signals failure (msg != "success", code != "0"),
	// a non-empty data field must still be returned as the transaction state.
	transport := &captureTransport{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"msg":"error","code":"1","data":"COMMITTED"}`)),
		},
	}

	client, err := NewClient(Config{
		StreamLoadURL:      "http://example.invalid/api/example_db/example_table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
		HTTPClient:         &http.Client{Transport: transport, Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	state, err := (&httpSender{cfg: client.cfg}).PollLabel(context.Background(), "demo_label")
	if err != nil {
		t.Fatalf("PollLabel() error = %v, want state from data field regardless of envelope", err)
	}
	if state.State != "COMMITTED" {
		t.Fatalf("state = %q, want COMMITTED", state.State)
	}
}

func TestPollLabelReturnsErrorWhenDataEmpty(t *testing.T) {
	// Empty data means the API call itself failed (auth error, label lookup error, etc.).
	transport := &captureTransport{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"msg":"label not found","code":"1","data":""}`)),
		},
	}

	client, err := NewClient(Config{
		StreamLoadURL:      "http://example.invalid/api/example_db/example_table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
		HTTPClient:         &http.Client{Transport: transport, Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	if _, err := (&httpSender{cfg: client.cfg}).PollLabel(context.Background(), "demo_label"); err == nil {
		t.Fatal("PollLabel() error = nil, want error for empty data field")
	}
}

func TestAmbiguousSendRetriesOnUnknown(t *testing.T) {
	withTestPollBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	withTestUploadRetryBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	// UNKNOWN means the label was never registered in Doris — no data was loaded.
	// The client must retry with a new label rather than polling indefinitely.
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{outcome: sendOutcome{statusCode: 0}, err: &streamLoadError{StatusCode: 0, Message: "timeout", Ambiguous: true}},
			// second attempt (after UNKNOWN retry) returns default success
		},
		pollResponses: []fakePollResponse{
			{state: loadStateResponse{StatusCode: 200, State: "UNKNOWN"}},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		DorisUploadTimeout: 100 * time.Millisecond,
		StatusPollTimeout:  200 * time.Millisecond, MaxUploadQueueSize: 8,
		Logger: log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord(`{"k":1}`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	result := handle.Wait()
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want retry to succeed after UNKNOWN", result.Err)
	}
	if got := sender.Attempts(); got != 2 {
		t.Fatalf("sender attempts = %d, want 2 (initial + retry after UNKNOWN)", got)
	}
	if got := sender.PollCalls(); got != 1 {
		t.Fatalf("poll calls = %d, want 1", got)
	}
	labels := sender.Labels()
	if labels[0] == labels[1] {
		t.Fatalf("labels[0] == labels[1] = %q, want distinct labels: UNKNOWN requires a fresh label", labels[0])
	}
}

func TestDialFailureRetriesWithoutPolling(t *testing.T) {
	withTestUploadRetryBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	// A dial-level failure means the TCP connection was never established and
	// the label was never registered. The client must retry without polling.
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{outcome: sendOutcome{}, err: &streamLoadError{StatusCode: 0, Message: "connection refused", Retriable: true, Ambiguous: false}},
			// second attempt returns default success
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		DorisUploadTimeout: 100 * time.Millisecond,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord(`{"k":1}`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	result := handle.Wait()
	if result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}
	if got := sender.Attempts(); got != 2 {
		t.Fatalf("sender attempts = %d, want 2", got)
	}
	if got := sender.PollCalls(); got != 0 {
		t.Fatalf("poll calls = %d, want 0 (dial failure must not trigger polling)", got)
	}
	labels := sender.Labels()
	if labels[0] != labels[1] {
		t.Fatalf("labels differ on dial failure retry: %q != %q, want same label (label was never registered)", labels[0], labels[1])
	}
}

func TestLabelAlreadyExistsFinishedTreatedAsSuccess(t *testing.T) {
	// When Doris reports Label Already Exists with ExistingJobStatus FINISHED,
	// a prior request already loaded the data successfully. Treat as success.
	transport := &captureTransport{
		response: &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"Status":"Label Already Exists","ExistingJobStatus":"FINISHED","Label":"test_label"}`)),
		},
	}
	client, err := NewClient(Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
		HTTPClient:         &http.Client{Transport: transport, Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()

	handle, err := client.SendRecord("a")
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v, want success for Label Already Exists + FINISHED", result.Err)
	}
}

func TestConfigRejectsNonPositiveUploadTimeout(t *testing.T) {
	_, err := NewClient(Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		DorisUploadTimeout: -1,
		MaxUploadQueueSize: 8,
	})
	if err == nil {
		t.Fatal("NewClient() error = nil, want error for non-positive DorisUploadTimeout")
	}
}

func TestConfigRejectsTooShortDorisUploadTimeout(t *testing.T) {
	_, err := NewClient(Config{
		StreamLoadURL:             "http://example.invalid/api/db/table/_stream_load",
		Columns:                   []string{"c1"},
		Mode:                      ModeCSV,
		Linger:                    10 * time.Millisecond,
		DorisUploadWorkers:        1,
		DorisUploadRequestTimeout: 5 * time.Second,
		MaxUploadQueueSize:        8,
	})
	if err == nil {
		t.Fatal("NewClient() error = nil, want error for too-short DorisUploadTimeout")
	}
}

func TestBasicAuthPreservedOnFEtoBERedirect(t *testing.T) {
	// Doris FE redirects stream load requests to a BE node via HTTP 307.
	// Go strips Authorization on cross-host redirects by default; the client
	// must re-apply it so the BE does not reject the request with 401.
	transport := &sequentialTransport{
		responses: []*http.Response{
			{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": []string{"http://backend.example.invalid/api/db/table/_stream_load"}},
				Body:       io.NopCloser(strings.NewReader("")),
			},
			{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"Status":"Success","Label":"test_label"}`)),
			},
		},
	}

	client, err := NewClient(Config{
		StreamLoadURL:       "http://frontend.example.invalid/api/db/table/_stream_load",
		Columns:             []string{"c1"},
		Mode:                ModeCSV,
		AuthenticationType:  AuthenticationBasic,
		AuthenticationToken: "user:pass", Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
		HTTPClient:         &http.Client{Transport: transport, Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	defer client.Close()

	handle, err := client.SendRecord("a")
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	if result := handle.Wait(); result.Err != nil {
		t.Fatalf("Wait() error = %v", result.Err)
	}

	reqs := transport.AllRequests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (FE request + redirected BE request)", len(reqs))
	}
	if got := reqs[1].Header.Get("Authorization"); got == "" {
		t.Fatal("Authorization header missing on redirected BE request; CheckRedirect did not preserve it")
	}
}

func TestHTTPSenderRejectsMissingSuccessBody(t *testing.T) {
	transport := &sequentialTransport{
		responses: []*http.Response{
			{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
			},
		},
	}

	sender := &httpSender{cfg: Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeCSV,
		HTTPClient:    &http.Client{Transport: transport, Timeout: time.Second},
	}}

	outcome, err := sender.Send(context.Background(), &deliveryBatch{
		label:   "test_label",
		mode:    ModeCSV,
		csvRows: []string{"a"},
		items:   []*queueItem{{payload: "a"}},
	})
	if outcome.statusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want 200", outcome.statusCode)
	}

	var loadErr *streamLoadError
	if !errors.As(err, &loadErr) {
		t.Fatalf("expected *streamLoadError, got %T: %v", err, err)
	}
	if !loadErr.Ambiguous {
		t.Fatal("Ambiguous = false, want true for missing 2xx body")
	}
	if loadErr.Retriable {
		t.Fatal("Retriable = true, want false for missing 2xx body")
	}
	if !strings.Contains(loadErr.Message, "missing stream load response body") {
		t.Fatalf("message = %q, want missing-body error", loadErr.Message)
	}
}

func TestHTTPSenderRejectsInvalidSuccessBody(t *testing.T) {
	transport := &sequentialTransport{
		responses: []*http.Response{
			{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("not json")),
			},
		},
	}

	sender := &httpSender{cfg: Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeCSV,
		HTTPClient:    &http.Client{Transport: transport, Timeout: time.Second},
	}}

	outcome, err := sender.Send(context.Background(), &deliveryBatch{
		label:   "test_label",
		mode:    ModeCSV,
		csvRows: []string{"a"},
		items:   []*queueItem{{payload: "a"}},
	})
	if outcome.statusCode != http.StatusOK {
		t.Fatalf("statusCode = %d, want 200", outcome.statusCode)
	}

	var loadErr *streamLoadError
	if !errors.As(err, &loadErr) {
		t.Fatalf("expected *streamLoadError, got %T: %v", err, err)
	}
	if !loadErr.Ambiguous {
		t.Fatal("Ambiguous = false, want true for invalid 2xx body")
	}
	if loadErr.Retriable {
		t.Fatal("Retriable = true, want false for invalid 2xx body")
	}
	if !strings.Contains(loadErr.Message, "invalid stream load response body") {
		t.Fatalf("message = %q, want invalid-body error", loadErr.Message)
	}
}

// ---- HTTP response classification unit tests ----

func TestIsHTTPSuccess(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		response   *StreamLoadResponse
		want       bool
	}{
		{"2xx nil response", 200, nil, true},
		{"1xx not success", 100, nil, false},
		{"3xx not success", 302, nil, false},
		{"4xx not success", 400, nil, false},
		{"5xx not success", 500, nil, false},
		{"empty Status field", 200, &StreamLoadResponse{}, true},
		{"Status Success", 200, &StreamLoadResponse{Status: "Success"}, true},
		{"Status Publish Timeout", 200, &StreamLoadResponse{Status: "Publish Timeout"}, true},
		{"Label Already Exists FINISHED", 200, &StreamLoadResponse{Status: "Label Already Exists", ExistingJobStatus: "FINISHED"}, true},
		{"Label Already Exists finished lowercase", 200, &StreamLoadResponse{Status: "Label Already Exists", ExistingJobStatus: "finished"}, true},
		{"Label Already Exists RUNNING", 200, &StreamLoadResponse{Status: "Label Already Exists", ExistingJobStatus: "RUNNING"}, false},
		{"Label Already Exists empty ExistingJobStatus", 200, &StreamLoadResponse{Status: "Label Already Exists", ExistingJobStatus: ""}, false},
		{"Label Already Exists unknown ExistingJobStatus", 200, &StreamLoadResponse{Status: "Label Already Exists", ExistingJobStatus: "OTHER"}, false},
		{"Status Fail", 200, &StreamLoadResponse{Status: "Fail"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHTTPSuccess(tc.statusCode, tc.response); got != tc.want {
				t.Fatalf("isHTTPSuccess(%d, %+v) = %v, want %v", tc.statusCode, tc.response, got, tc.want)
			}
		})
	}
}

func TestClassifyResponseError(t *testing.T) {
	cases := []struct {
		name          string
		statusCode    int
		response      *StreamLoadResponse
		wantRetriable bool
		wantAmbiguous bool
	}{
		// HTTP code classification (no response body)
		{"HTTP 500", 500, nil, true, false},
		{"HTTP 503", 503, nil, true, false},
		{"HTTP 429", 429, nil, true, false},
		{"HTTP 408", 408, nil, true, false},
		{"HTTP 400", 400, nil, false, false},
		{"HTTP 401", 401, nil, false, false},
		{"HTTP 403", 403, nil, false, false},
		{"HTTP 404", 404, nil, false, false},
		// Status Fail with various HTTP codes
		{"Fail 200 not retriable", 200, &StreamLoadResponse{Status: "Fail"}, false, false},
		{"Fail 500 retriable", 500, &StreamLoadResponse{Status: "Fail"}, true, false},
		// Label Already Exists
		{"Label Already Exists RUNNING ambiguous", 200, &StreamLoadResponse{Status: "Label Already Exists", ExistingJobStatus: "RUNNING"}, false, true},
		{"Label Already Exists other not ambiguous", 200, &StreamLoadResponse{Status: "Label Already Exists", ExistingJobStatus: "OTHER"}, false, false},
		{"Label Already Exists empty not ambiguous", 200, &StreamLoadResponse{Status: "Label Already Exists", ExistingJobStatus: ""}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyResponseError(tc.statusCode, nil, tc.response)
			var loadErr *streamLoadError
			if !errors.As(err, &loadErr) {
				t.Fatalf("expected *streamLoadError, got %T: %v", err, err)
			}
			if loadErr.Retriable != tc.wantRetriable {
				t.Fatalf("Retriable = %v, want %v", loadErr.Retriable, tc.wantRetriable)
			}
			if loadErr.Ambiguous != tc.wantAmbiguous {
				t.Fatalf("Ambiguous = %v, want %v", loadErr.Ambiguous, tc.wantAmbiguous)
			}
		})
	}
}

func TestClassifyTransportError(t *testing.T) {
	t.Run("dial failure is retriable and not ambiguous", func(t *testing.T) {
		dialErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		err := classifyTransportError(dialErr)
		var loadErr *streamLoadError
		if !errors.As(err, &loadErr) {
			t.Fatalf("expected *streamLoadError, got %T", err)
		}
		if !loadErr.Retriable {
			t.Fatal("Retriable = false, want true for dial failure")
		}
		if loadErr.Ambiguous {
			t.Fatal("Ambiguous = true, want false for dial failure")
		}
	})

	t.Run("read error is ambiguous and not retriable", func(t *testing.T) {
		readErr := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
		err := classifyTransportError(readErr)
		var loadErr *streamLoadError
		if !errors.As(err, &loadErr) {
			t.Fatalf("expected *streamLoadError, got %T", err)
		}
		if loadErr.Retriable {
			t.Fatal("Retriable = true, want false for non-dial error")
		}
		if !loadErr.Ambiguous {
			t.Fatal("Ambiguous = false, want true for non-dial error")
		}
	})

	t.Run("context deadline exceeded is ambiguous and not retriable", func(t *testing.T) {
		err := classifyTransportError(context.DeadlineExceeded)
		var loadErr *streamLoadError
		if !errors.As(err, &loadErr) {
			t.Fatalf("expected *streamLoadError, got %T", err)
		}
		if loadErr.Retriable {
			t.Fatal("Retriable = true, want false for deadline exceeded")
		}
		if !loadErr.Ambiguous {
			t.Fatal("Ambiguous = false, want true for deadline exceeded")
		}
	})
}

// ---- Delivery outcome tests ----

func TestPublishTimeoutTreatedAsSuccess(t *testing.T) {
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{outcome: sendOutcome{statusCode: 200, response: &StreamLoadResponse{Status: "Publish Timeout", Label: "test"}}, err: nil},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord("a")
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	result := handle.Wait()
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success for Publish Timeout", result.Err)
	}
	if result.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", result.Attempts)
	}
	if got := sender.PollCalls(); got != 0 {
		t.Fatalf("poll calls = %d, want 0 (Publish Timeout is terminal success)", got)
	}
}

func TestLabelAlreadyExistsRunningTriggersPolling(t *testing.T) {
	withTestPollBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	// RUNNING means the original load is still in progress; poll until terminal.
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{
				outcome: sendOutcome{statusCode: 200, response: &StreamLoadResponse{Status: "Label Already Exists", ExistingJobStatus: "RUNNING"}},
				err:     &streamLoadError{StatusCode: 200, Message: "label already exists running", Retriable: false, Ambiguous: true},
			},
		},
		pollResponses: []fakePollResponse{
			{state: loadStateResponse{StatusCode: 200, State: "VISIBLE"}},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		StatusPollTimeout:  200 * time.Millisecond, MaxUploadQueueSize: 8,
		Logger: log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord("a")
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	result := handle.Wait()
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success via polling after Label Already Exists RUNNING", result.Err)
	}
	if got := sender.PollCalls(); got < 1 {
		t.Fatalf("poll calls = %d, want >= 1", got)
	}
	if got := sender.Attempts(); got != 1 {
		t.Fatalf("sender attempts = %d, want 1 (no send retry, only polling)", got)
	}
}

func TestLabelAlreadyExistsUnknownExistingStatusFails(t *testing.T) {
	// An ExistingJobStatus that is neither RUNNING nor FINISHED is a permanent failure.
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{
				outcome: sendOutcome{statusCode: 200, response: &StreamLoadResponse{Status: "Label Already Exists", ExistingJobStatus: ""}},
				err:     &streamLoadError{StatusCode: 200, Message: "label already exists", Retriable: false, Ambiguous: false},
			},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord("a")
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	result := handle.Wait()
	if result.Err == nil {
		t.Fatal("Wait() error = nil, want failure for Label Already Exists with unknown ExistingJobStatus")
	}
	if got := sender.PollCalls(); got != 0 {
		t.Fatalf("poll calls = %d, want 0 (non-ambiguous, must not poll)", got)
	}
	if got := sender.Attempts(); got != 1 {
		t.Fatalf("sender attempts = %d, want 1 (permanent failure, must not retry)", got)
	}
}

func TestPollCommittedTreatedAsSuccess(t *testing.T) {
	withTestPollBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	// COMMITTED is past the point of no return; treat as success without waiting for VISIBLE.
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{err: &streamLoadError{Ambiguous: true}},
		},
		pollResponses: []fakePollResponse{
			{state: loadStateResponse{StatusCode: 200, State: "COMMITTED"}},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		StatusPollTimeout:  200 * time.Millisecond, MaxUploadQueueSize: 8,
		Logger: log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord(`{"k":1}`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	result := handle.Wait()
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success for COMMITTED poll state", result.Err)
	}
	if got := sender.PollCalls(); got != 1 {
		t.Fatalf("poll calls = %d, want 1 (COMMITTED is terminal)", got)
	}
}

func TestPollPrecommittedKeepsPolling(t *testing.T) {
	withTestPollBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	// PRECOMMITTED is an intermediate state; the client must keep polling until terminal.
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{err: &streamLoadError{Ambiguous: true}},
		},
		pollResponses: []fakePollResponse{
			{state: loadStateResponse{StatusCode: 200, State: "PRECOMMITTED"}},
			{state: loadStateResponse{StatusCode: 200, State: "VISIBLE"}},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		StatusPollTimeout:  200 * time.Millisecond, MaxUploadQueueSize: 8,
		Logger: log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord(`{"k":1}`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	result := handle.Wait()
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success after PRECOMMITTED → VISIBLE", result.Err)
	}
	if got := sender.PollCalls(); got < 2 {
		t.Fatalf("poll calls = %d, want >= 2 (PRECOMMITTED must keep polling)", got)
	}
}

func TestPollTimeoutResultsInFailureWithNoSendRetry(t *testing.T) {
	withTestPollBackoff(t, 50*time.Millisecond, 50*time.Millisecond)
	// When StatusPollTimeout is exceeded the handle fails, but the send is not retried
	// because the outcome is still ambiguous (we still don't know if data was loaded).
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{err: &streamLoadError{Ambiguous: true}},
		},
		pollResponses: []fakePollResponse{
			{state: loadStateResponse{StatusCode: 200, State: "PREPARE"}},
			{state: loadStateResponse{StatusCode: 200, State: "PREPARE"}},
			{state: loadStateResponse{StatusCode: 200, State: "PREPARE"}},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		StatusPollTimeout:  30 * time.Millisecond, MaxUploadQueueSize: 8,
		Logger: log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord(`{"k":1}`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	result := handle.Wait()
	if result.Err == nil {
		t.Fatal("Wait() error = nil, want failure after poll timeout")
	}
	if got := sender.Attempts(); got != 1 {
		t.Fatalf("sender attempts = %d, want 1 (poll timeout must not trigger a send retry)", got)
	}
}

func TestUploadTimeoutStopsAdditionalRetriesOnTransientErrors(t *testing.T) {
	withTestUploadRetryBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{outcome: sendOutcome{statusCode: 502}, err: &streamLoadError{StatusCode: 502, Retriable: true}},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		DorisUploadTimeout: 1 * time.Millisecond,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord(`{"k":1}`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	result := handle.Wait()
	if result.Err == nil {
		t.Fatal("Wait() error = nil, want failure after upload timeout stops retries")
	}
	if !strings.Contains(result.Err.Error(), "upload timeout") {
		t.Fatalf("error = %v, want upload timeout error", result.Err)
	}
	if result.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1 when timeout prevents another retry", result.Attempts)
	}
	if got := sender.Attempts(); got != 1 {
		t.Fatalf("sender attempts = %d, want 1", got)
	}
}

func TestUploadTimeoutStopsAdditionalRetriesAfterAbortedPoll(t *testing.T) {
	withTestPollBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	withTestUploadRetryBackoff(t, 5*time.Millisecond, 10*time.Millisecond)
	// Once polling concludes ABORTED, the client is allowed to retry with a fresh
	// label. DorisUploadTimeout bounds whether that extra send is attempted.
	sender := &fakeSender{
		outcomes: []fakeOutcome{
			{err: &streamLoadError{Ambiguous: true}},
		},
		pollResponses: []fakePollResponse{
			{state: loadStateResponse{StatusCode: 200, State: "ABORTED"}},
		},
	}
	client := newTestClient(t, sender, Config{
		StreamLoadURL: "http://example.invalid/api/db/table/_stream_load",
		Columns:       []string{"c1"},
		Mode:          ModeJSON, Linger: 10 * time.Millisecond,
		DorisUploadWorkers: 1,
		DorisUploadTimeout: 1 * time.Millisecond,
		StatusPollTimeout:  200 * time.Millisecond, MaxUploadQueueSize: 8,
		Logger: log.New(io.Discard, "", 0),
	})
	defer client.Close()

	handle, err := client.SendRecord(`{"k":1}`)
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	result := handle.Wait()
	if result.Err == nil {
		t.Fatal("Wait() error = nil, want failure after upload timeout prevents retry")
	}
	if !strings.Contains(result.Err.Error(), "upload timeout") {
		t.Fatalf("error = %v, want upload timeout error", result.Err)
	}
	if got := sender.Attempts(); got != 1 {
		t.Fatalf("sender attempts = %d, want 1", got)
	}
	if got := sender.PollCalls(); got != 1 {
		t.Fatalf("poll calls = %d, want 1", got)
	}
}

// ---- Handle method tests ----

func TestHandleIsDoneAndResult(t *testing.T) {
	h := newHandle()
	bc := newBatchCompletion()

	if h.IsDone() {
		t.Fatal("IsDone() = true before completion, want false")
	}
	if _, ok := h.Result(); ok {
		t.Fatal("Result() ok = true before completion, want false")
	}

	want := DeliveryResult{Attempts: 1, StatusCode: 200}
	h.attach(bc)
	bc.complete(want)

	if !h.IsDone() {
		t.Fatal("IsDone() = false after completion, want true")
	}
	got, ok := h.Result()
	if !ok {
		t.Fatal("Result() ok = false after completion, want true")
	}
	if got.Attempts != want.Attempts || got.StatusCode != want.StatusCode {
		t.Fatalf("Result() = %+v, want %+v", got, want)
	}
}

func TestHandleCompleteIsIdempotent(t *testing.T) {
	h := newHandle()
	bc := newBatchCompletion()
	h.attach(bc)

	first := DeliveryResult{Attempts: 1}
	second := DeliveryResult{Attempts: 99}

	bc.complete(first)
	bc.complete(second) // must be silently ignored

	got, _ := h.Result()
	if got.Attempts != 1 {
		t.Fatalf("Attempts = %d after double complete, want 1 (first result must win)", got.Attempts)
	}
}

// ---- Client lifecycle tests ----

func TestClientClosedChannelSignalsAfterClose(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})

	select {
	case <-client.Closed():
		t.Fatal("Closed() fired before Close()")
	default:
	}

	client.Close()

	select {
	case <-client.Closed():
		// expected
	case <-time.After(time.Second):
		t.Fatal("Closed() channel did not close after Close()")
	}
}

func TestEmptyRecordRejected(t *testing.T) {
	sender := &fakeSender{}
	client := newTestClient(t, sender, Config{
		StreamLoadURL:      "http://example.invalid/api/db/table/_stream_load",
		Columns:            []string{"c1"},
		Mode:               ModeCSV,
		Linger:             10 * time.Millisecond,
		DorisUploadWorkers: 1,
		MaxUploadQueueSize: 8,
		Logger:             log.New(io.Discard, "", 0),
	})
	defer client.Close()

	if _, err := client.SendRecord(""); err == nil {
		t.Fatal("SendRecord('') error = nil, want error for empty record")
	}
	if _, err := client.SendRecord("   "); err == nil {
		t.Fatal("SendRecord('   ') error = nil, want error for whitespace-only record")
	}
	if got := sender.Attempts(); got != 0 {
		t.Fatalf("sender attempts = %d, want 0 (rejected records must not reach the sender)", got)
	}
}

// ---- Transport helpers ----

type captureTransport struct {
	mu       sync.Mutex
	request  *http.Request
	response *http.Response
}

func (t *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.request = req.Clone(req.Context())
	t.mu.Unlock()
	return t.response, nil
}

func (t *captureTransport) LastRequest() *http.Request {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.request
}

// sequentialTransport replays a fixed list of responses in order, recording
// each incoming request. Used to simulate FE→BE redirects.
type sequentialTransport struct {
	mu        sync.Mutex
	responses []*http.Response
	requests  []*http.Request
}

func (t *sequentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.requests = append(t.requests, req.Clone(req.Context()))
	if len(t.responses) == 0 {
		return nil, errors.New("sequentialTransport: no more responses")
	}
	resp := t.responses[0]
	t.responses = t.responses[1:]
	return resp, nil
}

func (t *sequentialTransport) AllRequests() []*http.Request {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*http.Request(nil), t.requests...)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func writePEMCert(t *testing.T, der []byte) string {
	t.Helper()
	path := t.TempDir() + "/ca.pem"
	return writeNamedPEMCert(t, path, der)
}

func writeNamedPEMCert(t *testing.T, path string, der []byte) string {
	t.Helper()
	if !strings.Contains(path, "/") {
		path = t.TempDir() + "/" + path
	}
	data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

func generateSelfSignedCertDER(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "test-root-ca",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatalf("CreateCertificate() error = %v", err)
	}
	return der
}
