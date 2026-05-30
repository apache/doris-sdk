package dorisstreamload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type sender interface {
	Send(ctx context.Context, batch *deliveryBatch) (sendOutcome, error)
	PollLabel(ctx context.Context, label string) (loadStateResponse, error)
}

type sendOutcome struct {
	statusCode int
	response   *StreamLoadResponse
}

type loadStateResponse struct {
	StatusCode int
	State      string
	Message    string
}

type flexibleString string

func (s *flexibleString) UnmarshalJSON(data []byte) error {
	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		*s = flexibleString(asString)
		return nil
	}

	var asNumber json.Number
	if err := json.Unmarshal(data, &asNumber); err == nil {
		*s = flexibleString(asNumber.String())
		return nil
	}

	return fmt.Errorf("unsupported json scalar: %s", strings.TrimSpace(string(data)))
}

type httpSender struct {
	cfg Config
}

type fakeSuccessSender struct {
	delay time.Duration
}

type StreamLoadResponse struct {
	TxnID              int64  `json:"TxnId"`
	Label              string `json:"Label"`
	Status             string `json:"Status"`
	ExistingJobStatus  string `json:"ExistingJobStatus"`
	Message            string `json:"Message"`
	ErrorURL           string `json:"ErrorURL"`
	NumberTotalRows    int64  `json:"NumberTotalRows"`
	NumberLoadedRows   int64  `json:"NumberLoadedRows"`
	NumberFilteredRows int64  `json:"NumberFilteredRows"`
	NumberUnselected   int64  `json:"NumberUnselectedRows"`
	LoadBytes          int64  `json:"LoadBytes"`
	LoadTimeMS         int64  `json:"LoadTimeMs"`
}

func (s *fakeSuccessSender) Send(_ context.Context, batch *deliveryBatch) (sendOutcome, error) {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}

	return sendOutcome{
		statusCode: http.StatusOK,
		response: &StreamLoadResponse{
			Label:            batch.label,
			Status:           "Success",
			Message:          "fake send success",
			NumberTotalRows:  int64(batch.len()),
			NumberLoadedRows: int64(batch.len()),
			LoadBytes:        int64(batch.byteSize),
			LoadTimeMS:       s.delay.Milliseconds(),
		},
	}, nil
}

func (s *fakeSuccessSender) PollLabel(_ context.Context, _ string) (loadStateResponse, error) {
	return loadStateResponse{StatusCode: http.StatusOK, State: "VISIBLE", Message: "fake send success"}, nil
}

type streamLoadError struct {
	StatusCode int
	Message    string
	Retriable  bool
	Ambiguous  bool
	Response   *StreamLoadResponse
}

func (e *streamLoadError) Error() string {
	switch {
	case e.Response != nil && e.Response.Status != "":
		return fmt.Sprintf("stream load failed: http=%d status=%s message=%s", e.StatusCode, e.Response.Status, e.Response.Message)
	case e.Message != "":
		return fmt.Sprintf("stream load failed: http=%d message=%s", e.StatusCode, e.Message)
	default:
		return fmt.Sprintf("stream load failed: http=%d", e.StatusCode)
	}
}

func (e *streamLoadError) Is(target error) bool {
	_, ok := target.(*streamLoadError)
	return ok
}

func (s *httpSender) Send(ctx context.Context, batch *deliveryBatch) (sendOutcome, error) {
	body, contentType, err := batch.encodeBody()
	if err != nil {
		return sendOutcome{}, &streamLoadError{StatusCode: 0, Message: err.Error(), Retriable: false}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.streamLoadURL(), bytes.NewReader(body))
	if err != nil {
		return sendOutcome{}, &streamLoadError{StatusCode: 0, Message: err.Error(), Retriable: false}
	}

	req.Header.Set("Content-Type", contentType)
	req.Header.Set("columns", strings.Join(s.cfg.Columns, ","))
	req.Header.Set("Expect", "100-continue")
	req.Header.Set("label", batch.label)
	req.Header.Set("format", batch.headerFormat())
	if batch.mode == ModeJSON {
		req.Header.Set("strip_outer_array", "true")
		req.Header.Set("read_json_by_line", "false")
	} else {
		req.Header.Set("column_separator", s.cfg.CSVSeparator)
		req.Header.Set("enclose", s.cfg.CSVQuote)
	}
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))

	for key, values := range s.cfg.Headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	if s.cfg.AuthenticationType == AuthenticationBasic {
		username, password, _ := strings.Cut(s.cfg.AuthenticationToken, ":")
		req.SetBasicAuth(username, password)
	}

	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return sendOutcome{}, classifyTransportError(err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return sendOutcome{}, classifyTransportError(err)
	}

	outcome := sendOutcome{statusCode: resp.StatusCode}
	parsed := &StreamLoadResponse{}
	parsedOK := false
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, parsed); err == nil {
			outcome.response = parsed
			parsedOK = true
		}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if len(respBody) == 0 {
			return outcome, &streamLoadError{
				StatusCode: resp.StatusCode,
				Message:    "missing stream load response body",
				Retriable:  false,
				Ambiguous:  true,
			}
		}
		if !parsedOK {
			return outcome, &streamLoadError{
				StatusCode: resp.StatusCode,
				Message:    fmt.Sprintf("invalid stream load response body: %s", strings.TrimSpace(string(respBody))),
				Retriable:  false,
				Ambiguous:  true,
			}
		}
	}

	if !isHTTPSuccess(resp.StatusCode, outcome.response) {
		return outcome, classifyResponseError(resp.StatusCode, respBody, outcome.response)
	}

	return outcome, nil
}

func (s *httpSender) PollLabel(ctx context.Context, label string) (loadStateResponse, error) {
	database := s.pollDatabase()
	if strings.TrimSpace(database) == "" {
		return loadStateResponse{}, &streamLoadError{
			StatusCode: 0,
			Message:    "database is required to poll load label state",
			Retriable:  false,
			Ambiguous:  false,
		}
	}

	endpoint, err := s.loadStateURL(database, label)
	if err != nil {
		return loadStateResponse{}, &streamLoadError{StatusCode: 0, Message: err.Error(), Retriable: false}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return loadStateResponse{}, &streamLoadError{StatusCode: 0, Message: err.Error(), Retriable: false}
	}
	if s.cfg.AuthenticationType == AuthenticationBasic {
		username, password, _ := strings.Cut(s.cfg.AuthenticationToken, ":")
		req.SetBasicAuth(username, password)
	}
	for key, values := range s.cfg.Headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return loadStateResponse{}, classifyTransportError(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return loadStateResponse{}, classifyTransportError(err)
	}

	var parsed struct {
		Msg   string         `json:"msg"`
		Code  flexibleString `json:"code"`
		Data  string         `json:"data"`
		Count flexibleString `json:"count"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return loadStateResponse{}, &streamLoadError{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("invalid load state response: %s", strings.TrimSpace(string(body))),
			Retriable:  false,
		}
	}

	// The authoritative answer is in the data field — it contains the transaction
	// state string directly. Use it if present; do not gate on msg/code, which vary
	// across Doris versions and are unreliable as a proxy for whether the state is known.
	if state := strings.TrimSpace(parsed.Data); state != "" {
		return loadStateResponse{
			StatusCode: resp.StatusCode,
			State:      state,
			Message:    parsed.Msg,
		}, nil
	}

	// data is empty: the API call itself failed (auth error, label lookup error, etc.).
	// Use msg and code for error context.
	return loadStateResponse{}, &streamLoadError{
		StatusCode: resp.StatusCode,
		Message:    fmt.Sprintf("load state request failed: msg=%s code=%s", parsed.Msg, parsed.Code),
		Retriable:  false,
	}
}

func (s *httpSender) streamLoadURL() string {
	if s.cfg.StreamLoadURL != "" {
		return s.cfg.StreamLoadURL
	}
	base, err := url.Parse(s.cfg.Endpoint)
	if err != nil {
		return s.cfg.Endpoint
	}
	base.Path = path.Join(base.Path, "api", s.cfg.Database, s.cfg.Table, "_stream_load")
	return base.String()
}

func (s *httpSender) loadStateURL(database, label string) (string, error) {
	baseURL := s.cfg.StreamLoadURL
	if baseURL == "" {
		baseURL = s.cfg.Endpoint
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	base.Path = path.Join("/", "api", database, "get_load_state")
	query := base.Query()
	query.Set("label", label)
	base.RawQuery = query.Encode()
	return base.String(), nil
}

func (s *httpSender) pollDatabase() string {
	if strings.TrimSpace(s.cfg.Database) != "" {
		return s.cfg.Database
	}
	if s.cfg.StreamLoadURL == "" {
		return ""
	}
	base, err := url.Parse(s.cfg.StreamLoadURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(base.Path, "/"), "/")
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] == "api" && parts[i+3] == "_stream_load" {
			return parts[i+1]
		}
	}
	return ""
}

func classifyTransportError(err error) error {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		// TCP connection was never established; the request never reached Doris.
		// Safe to retry immediately — no label was registered, no polling needed.
		return &streamLoadError{StatusCode: 0, Message: err.Error(), Retriable: true, Ambiguous: false}
	}
	// All other transport failures (timeout, context cancellation, mid-transfer drop)
	// are ambiguous: the request may have reached Doris before the error occurred.
	return &streamLoadError{StatusCode: 0, Message: err.Error(), Retriable: false, Ambiguous: true}
}

func classifyResponseError(statusCode int, body []byte, response *StreamLoadResponse) error {
	message := strings.TrimSpace(string(body))
	if response != nil && response.Message != "" {
		message = response.Message
	}

	retriable := false
	switch {
	case statusCode == http.StatusTooManyRequests:
		retriable = true
	case statusCode == http.StatusRequestTimeout:
		retriable = true
	case statusCode >= 500:
		retriable = true
	}

	if response != nil {
		switch response.Status {
		case "Success", "Publish Timeout":
			retriable = false
		case "Label Already Exists":
			retriable = false
			return &streamLoadError{
				StatusCode: statusCode,
				Message:    message,
				Retriable:  false,
				Ambiguous:  strings.EqualFold(response.ExistingJobStatus, "RUNNING"),
				Response:   response,
			}
		case "Fail":
			if statusCode >= 500 {
				retriable = true
			}
		}
	}

	return &streamLoadError{
		StatusCode: statusCode,
		Message:    message,
		Retriable:  retriable,
		Response:   response,
	}
}

func isHTTPSuccess(statusCode int, response *StreamLoadResponse) bool {
	if statusCode < 200 || statusCode >= 300 {
		return false
	}
	if response == nil {
		return true
	}
	switch response.Status {
	case "", "Success", "Publish Timeout":
		return true
	case "Label Already Exists":
		// ExistingJobStatus FINISHED means a prior request with this label already
		// completed successfully; treat as success to avoid a spurious error.
		return strings.EqualFold(response.ExistingJobStatus, "FINISHED")
	default:
		return false
	}
}
