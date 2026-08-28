// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package dorisstreamload

// End-to-end tests of the retry / ambiguity-resolution policy against a mock
// Doris backend. They drive the real httpSender (HTTP, headers, redirects,
// label polling) rather than the in-process fakeSender, and each test states
// how the Flink connector (DorisBatchStreamLoad.load) behaves in the same
// situation so the parity — and the deliberate improvements — are explicit.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockLoad is one _stream_load request as seen by the mock backend.
type mockLoad struct {
	label string
	body  string
	auth  string
}

// mockLoadHandler scripts the backend's reaction to one _stream_load attempt.
type mockLoadHandler func(w http.ResponseWriter, load mockLoad)

// mockDoris is a scripted stand-in for a Doris FE/BE. It serves
// PUT /api/{db}/{table}/_stream_load according to `script` (one handler per
// attempt, the last one repeating) and GET /api/{db}/get_load_state?label=...
// according to `states` (one state per poll, the last one repeating).
type mockDoris struct {
	t      *testing.T
	server *httptest.Server

	mu     sync.Mutex
	script []mockLoadHandler
	states map[string][]string
	loads  []mockLoad
	polls  []string
}

func newMockDoris(t *testing.T, script ...mockLoadHandler) *mockDoris {
	t.Helper()
	m := &mockDoris{t: t, script: script, states: map[string][]string{}}
	m.server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockDoris) streamLoadURL() string {
	return m.server.URL + "/api/db/table/_stream_load"
}

// setStates scripts the get_load_state answers for a label ("*" matches any
// label); the last state repeats for every further poll.
func (m *mockDoris) setStates(label string, states ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[label] = states
}

func (m *mockDoris) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/_stream_load"):
		body, err := io.ReadAll(r.Body)
		if err != nil {
			m.t.Errorf("mock doris: read body: %v", err)
			return
		}
		load := mockLoad{label: r.Header.Get("label"), body: string(body), auth: r.Header.Get("Authorization")}
		m.mu.Lock()
		m.loads = append(m.loads, load)
		idx := len(m.loads) - 1
		var handler mockLoadHandler
		if len(m.script) > 0 {
			if idx >= len(m.script) {
				idx = len(m.script) - 1
			}
			handler = m.script[idx]
		}
		m.mu.Unlock()
		if handler == nil {
			handler = loadSuccess
		}
		handler(w, load)
	case strings.HasSuffix(r.URL.Path, "/get_load_state"):
		label := r.URL.Query().Get("label")
		m.mu.Lock()
		m.polls = append(m.polls, label)
		state := "UNKNOWN"
		key := label
		if _, ok := m.states[key]; !ok {
			key = "*"
		}
		if states := m.states[key]; len(states) > 0 {
			state = states[0]
			if len(states) > 1 {
				m.states[key] = states[1:]
			}
		}
		m.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"msg": "success", "code": 0, "data": state, "count": 0})
	default:
		http.NotFound(w, r)
	}
}

func (m *mockDoris) Loads() []mockLoad {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mockLoad(nil), m.loads...)
}

func (m *mockDoris) Polls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.polls...)
}

func (m *mockDoris) labels() []string {
	loads := m.Loads()
	labels := make([]string, 0, len(loads))
	for _, l := range loads {
		labels = append(labels, l.label)
	}
	return labels
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// --- scripted backend reactions -------------------------------------------

func loadSuccess(w http.ResponseWriter, load mockLoad) {
	writeJSON(w, http.StatusOK, map[string]any{
		"TxnId": 1, "Label": load.label, "Status": "Success", "Message": "OK",
		"NumberTotalRows": 1, "NumberLoadedRows": 1,
	})
}

// loadFail mimics how Doris reports load failures: HTTP 200 + Status Fail.
func loadFail(message string) mockLoadHandler {
	return func(w http.ResponseWriter, load mockLoad) {
		writeJSON(w, http.StatusOK, map[string]any{
			"TxnId": 1, "Label": load.label, "Status": "Fail", "Message": message,
			"ErrorURL": "http://be:8040/api/_load_error_log?file=__shard_0/error_log_x",
		})
	}
}

func loadHTTPStatus(status int, body string) mockLoadHandler {
	return func(w http.ResponseWriter, _ mockLoad) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func loadLabelAlreadyExists(existingJobStatus string) mockLoadHandler {
	return func(w http.ResponseWriter, load mockLoad) {
		writeJSON(w, http.StatusOK, map[string]any{
			"TxnId": -1, "Label": load.label, "Status": "Label Already Exists",
			"ExistingJobStatus": existingJobStatus,
			"Message":           fmt.Sprintf("errCode = 2, detailMessage = Label [%s] has already been used, relate to txn [42]", load.label),
		})
	}
}

// loadDropConnection reads the whole request, then closes the TCP connection
// without answering: the client cannot know whether Doris committed the data.
func loadDropConnection(w http.ResponseWriter, _ mockLoad) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("mock doris: response writer is not a Hijacker")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		panic(err)
	}
	_ = conn.Close()
}

// --- helpers --------------------------------------------------------------

const tooManyVersionsMsg = "[INTERNAL_ERROR]tablet error: [E-235]failed to init rowset builder. version count: 2001, exceed limit: 2000. tablet: 10042"
const txnLimitMsg = "current running txns on db 10003 is 1000, larger than limit 1000"

func newMockClient(t *testing.T, m *mockDoris, mutate func(*Config)) *Client {
	t.Helper()
	withTestUploadRetryBackoff(t, time.Millisecond, 2*time.Millisecond)
	withTestPollBackoff(t, time.Millisecond, 2*time.Millisecond)
	cfg := Config{
		StreamLoadURL:       m.streamLoadURL(),
		Columns:             []string{"c1"},
		Mode:                ModeCSV,
		Linger:              5 * time.Millisecond,
		DorisUploadWorkers:  1,
		MaxUploadQueueSize:  8,
		AuthenticationType:  AuthenticationBasic,
		AuthenticationToken: "root:secret",
		StatusPollTimeout:   2 * time.Second,
		Logger:              log.New(io.Discard, "", 0),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	client, err := NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func sendOne(t *testing.T, client *Client) DeliveryResult {
	t.Helper()
	handle, err := client.SendRecord("row-1")
	if err != nil {
		t.Fatalf("SendRecord() error = %v", err)
	}
	return handle.Wait()
}

func assertSameBodies(t *testing.T, loads []mockLoad) {
	t.Helper()
	for i := 1; i < len(loads); i++ {
		if loads[i].body != loads[0].body {
			t.Fatalf("retry %d re-sent a different body: %q vs %q", i, loads[i].body, loads[0].body)
		}
	}
}

// --- Doris-level failures: retried with a fresh label (same as Flink) ------

func TestMockDorisTooManyVersionsIsRetriedWithFreshLabel(t *testing.T) {
	// Flink: HTTP 200 + Status Fail -> DorisBatchLoadException -> retried with
	// label_N. Before this change the Go SDK gave up immediately.
	m := newMockDoris(t, loadFail(tooManyVersionsMsg), loadFail(tooManyVersionsMsg), loadSuccess)
	client := newMockClient(t, m, nil)

	result := sendOne(t, client)
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success after retries", result.Err)
	}
	if result.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3", result.Attempts)
	}
	loads := m.Loads()
	if len(loads) != 3 {
		t.Fatalf("backend saw %d loads, want 3", len(loads))
	}
	assertRetryLabels(t, m.labels())
	assertSameBodies(t, loads)
	if got := m.Polls(); len(got) != 0 {
		t.Fatalf("polls = %v, want none (definitive Fail is not ambiguous)", got)
	}
	for i, l := range loads {
		if l.auth == "" {
			t.Fatalf("load %d lost basic auth on retry", i)
		}
	}
}

func TestMockDorisTxnLimitExhaustsMaxRetries(t *testing.T) {
	// Flink: retry until sink.max-retries then throw with the last message.
	m := newMockDoris(t, loadFail(txnLimitMsg))
	client := newMockClient(t, m, func(c *Config) { c.MaxRetries = 2 })

	result := sendOne(t, client)
	if result.Err == nil {
		t.Fatal("Wait() error = nil, want failure after MaxRetries")
	}
	if result.Attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (1 + MaxRetries)", result.Attempts)
	}
	if got := len(m.Loads()); got != 3 {
		t.Fatalf("backend saw %d loads, want 3", got)
	}
	assertRetryLabels(t, m.labels())
	if result.Response == nil || result.Response.Status != "Fail" {
		t.Fatalf("result.Response = %+v, want last Doris Fail response", result.Response)
	}
	msg := result.Err.Error()
	if !strings.Contains(msg, txnLimitMsg) || !strings.Contains(msg, "see more in http://be:8040") {
		t.Fatalf("error = %q, want Doris message and ErrorURL", msg)
	}
}

func TestMockDorisNon2xxIsRetried(t *testing.T) {
	// Flink: "stream load failed with {}, reason {}, to retry".
	m := newMockDoris(t, loadHTTPStatus(http.StatusServiceUnavailable, "<html>be busy</html>"), loadHTTPStatus(http.StatusBadGateway, ""), loadSuccess)
	client := newMockClient(t, m, nil)

	result := sendOne(t, client)
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success", result.Err)
	}
	if got := len(m.Loads()); got != 3 {
		t.Fatalf("backend saw %d loads, want 3", got)
	}
	assertRetryLabels(t, m.labels())
}

func TestMockDorisNegativeMaxRetriesFailsFast(t *testing.T) {
	m := newMockDoris(t, loadFail(tooManyVersionsMsg), loadSuccess)
	client := newMockClient(t, m, func(c *Config) { c.MaxRetries = -1 })

	result := sendOne(t, client)
	if result.Err == nil {
		t.Fatal("Wait() error = nil, want failure with retries disabled")
	}
	if got := len(m.Loads()); got != 1 {
		t.Fatalf("backend saw %d loads, want 1", got)
	}
}

func TestMockDorisUploadTimeoutBoundsRetries(t *testing.T) {
	// DorisUploadTimeout caps the retry window even when MaxRetries is large.
	m := newMockDoris(t, loadFail(txnLimitMsg))
	client := newMockClient(t, m, func(c *Config) {
		c.MaxRetries = 1000
		c.DorisUploadTimeout = 30 * time.Millisecond
	})

	result := sendOne(t, client)
	if result.Err == nil {
		t.Fatal("Wait() error = nil, want failure")
	}
	if !strings.Contains(result.Err.Error(), "doris upload timeout") {
		t.Fatalf("error = %v, want upload timeout", result.Err)
	}
	if got := len(m.Loads()); got < 2 || got > 200 {
		t.Fatalf("backend saw %d loads, want a handful bounded by the 30ms budget", got)
	}
}

// --- connection refused: retried, no polling (same as Flink) ---------------

func TestMockDorisConnectionRefusedIsRetriedWithoutPolling(t *testing.T) {
	m := newMockDoris(t, loadSuccess)

	// The first dial goes to a closed port; later dials reach the mock.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()

	var dials int32
	var dialMu sync.Mutex
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialMu.Lock()
			dials++
			first := dials == 1
			dialMu.Unlock()
			if first {
				addr = closedAddr
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	client := newMockClient(t, m, func(c *Config) { c.HTTPClient = &http.Client{Transport: transport} })

	result := sendOne(t, client)
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success after dial retry", result.Err)
	}
	if result.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", result.Attempts)
	}
	loads := m.Loads()
	if len(loads) != 1 {
		t.Fatalf("backend saw %d loads, want 1 (first attempt never connected)", len(loads))
	}
	if !strings.HasSuffix(loads[0].label, "_1") {
		t.Fatalf("label = %q, want retry suffix _1", loads[0].label)
	}
	if got := m.Polls(); len(got) != 0 {
		t.Fatalf("polls = %v, want none (request never reached Doris)", got)
	}
}

// --- ambiguous outcomes: resolved by polling (better than Flink batch) -----

func TestMockDorisDroppedResponseAfterCommitIsNotResent(t *testing.T) {
	// Doris committed the txn but the response was lost. Flink batch mode
	// re-sends under label_1 and writes the data twice; the Go SDK polls
	// get_load_state, sees VISIBLE and reports success without re-sending.
	m := newMockDoris(t, loadDropConnection, loadSuccess)
	m.setStates("*", "VISIBLE")
	client := newMockClient(t, m, nil)

	result := sendOne(t, client)
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success resolved via poll", result.Err)
	}
	if result.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no re-send)", result.Attempts)
	}
	loads := m.Loads()
	if len(loads) != 1 {
		t.Fatalf("backend saw %d loads, want 1 — data must not be written twice", len(loads))
	}
	polls := m.Polls()
	if len(polls) == 0 || polls[0] != loads[0].label {
		t.Fatalf("polls = %v, want poll of label %q", polls, loads[0].label)
	}
	if result.Response == nil || result.Response.Status != "Success" || result.Response.Message != "VISIBLE" {
		t.Fatalf("result.Response = %+v, want synthesized Success/VISIBLE", result.Response)
	}
}

func TestMockDorisDroppedResponseAbortedIsRetriedWithFreshLabel(t *testing.T) {
	// Doris aborted the txn (nothing loaded) -> safe to retry with a new label.
	m := newMockDoris(t, loadDropConnection, loadSuccess)
	m.setStates("*", "ABORTED")
	client := newMockClient(t, m, nil)

	result := sendOne(t, client)
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success after ABORTED retry", result.Err)
	}
	if result.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", result.Attempts)
	}
	loads := m.Loads()
	if len(loads) != 2 {
		t.Fatalf("backend saw %d loads, want 2", len(loads))
	}
	assertRetryLabels(t, m.labels())
	assertSameBodies(t, loads)
	if polls := m.Polls(); len(polls) != 1 || polls[0] != loads[0].label {
		t.Fatalf("polls = %v, want exactly one poll of %q", polls, loads[0].label)
	}
}

func TestMockDorisDroppedResponseUnknownLabelIsRetried(t *testing.T) {
	// Label never registered (request died before FE begin_txn) -> retry.
	m := newMockDoris(t, loadDropConnection, loadSuccess)
	// default poll state is UNKNOWN
	client := newMockClient(t, m, nil)

	result := sendOne(t, client)
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success", result.Err)
	}
	if got := len(m.Loads()); got != 2 {
		t.Fatalf("backend saw %d loads, want 2", got)
	}
	assertRetryLabels(t, m.labels())
}

func TestMockDorisDroppedResponsePendingStatesArePolledUntilVisible(t *testing.T) {
	// Txn still in PREPARE/PRECOMMITTED: keep polling, never re-send.
	m := newMockDoris(t, loadDropConnection, loadSuccess)
	m.setStates("*", "PREPARE", "PREPARE", "PRECOMMITTED", "COMMITTED")
	client := newMockClient(t, m, nil)

	result := sendOne(t, client)
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success once COMMITTED", result.Err)
	}
	if got := len(m.Loads()); got != 1 {
		t.Fatalf("backend saw %d loads, want 1", got)
	}
	if got := len(m.Polls()); got < 4 {
		t.Fatalf("polls = %d, want at least 4 (PREPARE, PREPARE, PRECOMMITTED, COMMITTED)", got)
	}
}

func TestMockDorisPollTimeoutFailsWithoutResend(t *testing.T) {
	// Label never reaches a terminal state: report failure, never re-send —
	// the data may be in, and retrying could write it twice.
	m := newMockDoris(t, loadDropConnection, loadSuccess)
	m.setStates("*", "PREPARE")
	client := newMockClient(t, m, func(c *Config) { c.StatusPollTimeout = 50 * time.Millisecond })

	result := sendOne(t, client)
	if result.Err == nil {
		t.Fatal("Wait() error = nil, want poll timeout failure")
	}
	if !strings.Contains(result.Err.Error(), "poll timeout") {
		t.Fatalf("error = %v, want poll timeout", result.Err)
	}
	if got := len(m.Loads()); got != 1 {
		t.Fatalf("backend saw %d loads, want 1 (never re-send an unresolved label)", got)
	}
	if got := len(m.Polls()); got == 0 {
		t.Fatal("expected label polls before giving up")
	}
}

// --- Label Already Exists --------------------------------------------------

func TestMockDorisLabelAlreadyExistsFinishedIsSuccess(t *testing.T) {
	// Flink batch mode retries this with label_1 and duplicates the data.
	m := newMockDoris(t, loadLabelAlreadyExists("FINISHED"), loadSuccess)
	client := newMockClient(t, m, nil)

	result := sendOne(t, client)
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success (data already loaded)", result.Err)
	}
	if got := len(m.Loads()); got != 1 {
		t.Fatalf("backend saw %d loads, want 1", got)
	}
	if got := m.Polls(); len(got) != 0 {
		t.Fatalf("polls = %v, want none", got)
	}
}

func TestMockDorisLabelAlreadyExistsRunningIsPolledThenSuccess(t *testing.T) {
	// The earlier request with this label is still in flight: wait for it.
	m := newMockDoris(t, loadLabelAlreadyExists("RUNNING"), loadSuccess)
	m.setStates("*", "PREPARE", "VISIBLE")
	client := newMockClient(t, m, nil)

	result := sendOne(t, client)
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success via poll", result.Err)
	}
	if got := len(m.Loads()); got != 1 {
		t.Fatalf("backend saw %d loads, want 1 (must not re-send while RUNNING)", got)
	}
	if got := len(m.Polls()); got < 2 {
		t.Fatalf("polls = %d, want at least 2", got)
	}
}

func TestMockDorisLabelAlreadyExistsRunningThenAbortedIsRetried(t *testing.T) {
	m := newMockDoris(t, loadLabelAlreadyExists("RUNNING"), loadSuccess)
	m.setStates("*", "ABORTED")
	client := newMockClient(t, m, nil)

	result := sendOne(t, client)
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success after retry", result.Err)
	}
	if got := len(m.Loads()); got != 2 {
		t.Fatalf("backend saw %d loads, want 2", got)
	}
	assertRetryLabels(t, m.labels())
}

func TestMockDorisLabelAlreadyExistsUnknownStatusIsRetriedWithFreshLabel(t *testing.T) {
	// Neither FINISHED nor RUNNING: not ambiguous, retry like Flink.
	m := newMockDoris(t, loadLabelAlreadyExists(""), loadSuccess)
	client := newMockClient(t, m, nil)

	result := sendOne(t, client)
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success after retry", result.Err)
	}
	if got := len(m.Loads()); got != 2 {
		t.Fatalf("backend saw %d loads, want 2", got)
	}
	if got := m.Polls(); len(got) != 0 {
		t.Fatalf("polls = %v, want none", got)
	}
	assertRetryLabels(t, m.labels())
}

// --- FE 307 redirect to BE --------------------------------------------------

func TestMockDorisRetryAfterFERedirectUsesFreshLabelAndKeepsAuth(t *testing.T) {
	// Real deployments hit the FE, which 307-redirects each attempt to a BE.
	// Every retry must go back through the FE with a new label and its auth.
	be := newMockDoris(t, loadFail(tooManyVersionsMsg), loadSuccess)
	var feMu sync.Mutex
	var feHits int
	fe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_stream_load") {
			feMu.Lock()
			feHits++
			feMu.Unlock()
			http.Redirect(w, r, be.server.URL+r.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		// get_load_state is served by the FE itself.
		be.serve(w, r)
	}))
	t.Cleanup(fe.Close)

	client := newMockClient(t, be, func(c *Config) { c.StreamLoadURL = fe.URL + "/api/db/table/_stream_load" })

	result := sendOne(t, client)
	if result.Err != nil {
		t.Fatalf("Wait() error = %v, want success", result.Err)
	}
	feMu.Lock()
	hits := feHits
	feMu.Unlock()
	if hits != 2 {
		t.Fatalf("FE saw %d stream load requests, want 2", hits)
	}
	loads := be.Loads()
	if len(loads) != 2 {
		t.Fatalf("BE saw %d loads, want 2", len(loads))
	}
	assertRetryLabels(t, be.labels())
	assertSameBodies(t, loads)
	for i, l := range loads {
		if !strings.HasPrefix(l.auth, "Basic ") {
			t.Fatalf("BE load %d lost basic auth across redirect: %q", i, l.auth)
		}
	}
}

// --- several batches in flight keep independent retry state ----------------

func TestMockDorisConcurrentBatchesRetryIndependently(t *testing.T) {
	// Two workers, two batches: one fails once then succeeds, the other
	// succeeds outright. Each handle must see its own attempt count.
	var mu sync.Mutex
	failed := map[string]bool{}
	m := newMockDoris(t, func(w http.ResponseWriter, load mockLoad) {
		mu.Lock()
		shouldFail := strings.Contains(load.body, "flaky") && !failed[strings.TrimSuffix(load.label, "_1")]
		if shouldFail {
			failed[load.label] = true
		}
		mu.Unlock()
		if shouldFail {
			loadFail(tooManyVersionsMsg)(w, load)
			return
		}
		loadSuccess(w, load)
	})
	client := newMockClient(t, m, func(c *Config) {
		c.DorisUploadWorkers = 2
		c.BatchBytes = 8 // force one record per batch
	})

	hFlaky, err := client.SendRecord("flaky")
	if err != nil {
		t.Fatal(err)
	}
	hOK, err := client.SendRecord("fine")
	if err != nil {
		t.Fatal(err)
	}
	rFlaky, rOK := hFlaky.Wait(), hOK.Wait()
	if rFlaky.Err != nil || rOK.Err != nil {
		t.Fatalf("errors: flaky=%v ok=%v", rFlaky.Err, rOK.Err)
	}
	if rFlaky.Attempts != 2 {
		t.Fatalf("flaky attempts = %d, want 2", rFlaky.Attempts)
	}
	if rOK.Attempts != 1 {
		t.Fatalf("ok attempts = %d, want 1", rOK.Attempts)
	}
	if got := len(m.Loads()); got != 3 {
		t.Fatalf("backend saw %d loads, want 3", got)
	}
}
