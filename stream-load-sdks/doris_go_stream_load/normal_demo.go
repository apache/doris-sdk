//go:build ignore

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

const sampleConfig = `# normal_demo config
# Lines beginning with # are comments.
# C-style block comments are also ignored.
# Repeat "row =" for each CSV row you want to send.
# Use {ROW_ID} inside a row to inject a 1-based generated row sequence.

/*
Example Doris table:
CREATE DATABASE IF NOT EXISTS example_db;

CREATE TABLE IF NOT EXISTS example_db.example_table (
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
*/

stream_load_url = http://127.0.0.1:8030/api/example_db/example_table/_stream_load
database = example_db
username = root
password =
columns = event_time,user_id,event_name

# Optional tuning
doris_upload_request_timeout_ms = 300000
doris_upload_timeout_ms = 300000
poll_timeout_ms = 300000
data_repeats = 1

row = 2026-04-28T10:00:00Z,{ROW_ID},login
row = 2026-04-28T10:00:01Z,{ROW_ID},logout
`

type streamLoadResponse struct {
	TxnID             int64  `json:"TxnId"`
	Label             string `json:"Label"`
	Status            string `json:"Status"`
	ExistingJobStatus string `json:"ExistingJobStatus"`
	Message           string `json:"Message"`
}

type loadStateResponse struct {
	Msg   string         `json:"msg"`
	Code  flexibleString `json:"code"`
	Data  string         `json:"data"`
	Count flexibleString `json:"count"`
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

func main() {
	configPath, printSample := parseArgs()
	if printSample {
		fmt.Print(sampleConfig)
		return
	}
	if strings.TrimSpace(configPath) == "" {
		printHelp(os.Stderr)
		os.Exit(2)
	}

	cfg, err := loadConfigFromFile(configPath)
	if err != nil {
		fatalf("config error: %v", err)
	}

	label := generateLabel()
	rows := expandRows(cfg.rows, cfg.dataRepeats)

	fmt.Printf("sending %d rows (%d base rows x %d repeats)\n", len(rows), len(cfg.rows), cfg.dataRepeats)

	for attempt := 1; ; attempt++ {
		fmt.Printf("stream load attempt %d label: %s\n", attempt, label)

		result, err := sendCSVBatch(cfg, label, rows)
		if err == nil {
			fmt.Printf("result: success status=%s label=%s attempts=%d\n", result.Status, result.Label, attempt)
			return
		}

		var loadErr *streamLoadError
		if !errors.As(err, &loadErr) {
			fatalf("send failed: %v", err)
		}

		if loadErr.ambiguous {
			fmt.Printf("immediate result: ambiguous error=%v\n", err)
			fmt.Printf("polling label state until terminal result...\n")

			state, err := waitForLabel(cfg, label, cfg.pollTimeout)
			if err != nil {
				fatalf("label polling failed: %v", err)
			}

			switch strings.ToUpper(state) {
			case "VISIBLE", "COMMITTED":
				fmt.Printf("result: success state=%s label=%s attempts=%d\n", state, label, attempt)
				return
			case "ABORTED", "UNKNOWN":
				loadErr = &streamLoadError{message: fmt.Sprintf("label %s resolved as %s", label, state), retriable: true, newLabel: true}
			default:
				fatalf("label resolved to unexpected terminal state=%s label=%s", state, label)
			}
		}

		if !loadErr.retriable {
			fatalf("send failed after %d attempt(s): %v", attempt, loadErr)
		}
		if cfg.dorisUploadTimeout <= 0 {
			fatalf("send failed after %d attempt(s): doris upload timeout exhausted: %v", attempt, loadErr)
		}

		if loadErr.newLabel {
			label = generateLabel()
		}
		backoff := retryBackoffDelay(attempt)
		if backoff > cfg.dorisUploadTimeout {
			backoff = cfg.dorisUploadTimeout
		}
		fmt.Printf("retrying after %s: %v\n", backoff, loadErr)
		time.Sleep(backoff)
		cfg.dorisUploadTimeout -= backoff
	}
}

type demoConfig struct {
	streamLoadURL             string
	database                  string
	username                  string
	password                  string
	columns                   string
	dorisUploadRequestTimeout time.Duration
	dorisUploadTimeout        time.Duration
	pollTimeout               time.Duration
	dataRepeats               int
	rows                      []string
	client                    *http.Client
}

func parseArgs() (configPath string, printSample bool) {
	flag.Usage = func() {
		printHelp(flag.CommandLine.Output())
	}

	flag.StringVar(&configPath, "config", "", "Path to config file")
	flag.BoolVar(&printSample, "sample", false, "Print a sample config file and exit")
	flag.Parse()
	return configPath, printSample
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, "normal_demo shows the normal Doris stream load lifecycle without using this library.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  go run normal_demo.go --config test.config")
	fmt.Fprintln(w, "  go run normal_demo.go --sample")
	fmt.Fprintln(w, "  go run normal_demo.go --help")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Flags:")
	flag.CommandLine.SetOutput(w)
	flag.PrintDefaults()
}

func loadConfigFromFile(path string) (demoConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return demoConfig{}, err
	}
	defer file.Close()

	cfg := demoConfig{
		dorisUploadRequestTimeout: 300 * time.Second,
		dorisUploadTimeout:        300 * time.Second,
		pollTimeout:               300 * time.Second,
		dataRepeats:               1,
	}

	scanner := bufio.NewScanner(file)
	lineNo := 0
	inBlockComment := false
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if inBlockComment {
			if strings.Contains(line, "*/") {
				inBlockComment = false
			}
			continue
		}
		if strings.HasPrefix(line, "/*") {
			if !strings.Contains(line, "*/") {
				inBlockComment = true
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return demoConfig{}, fmt.Errorf("%s:%d: expected key = value", path, lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		switch key {
		case "stream_load_url":
			cfg.streamLoadURL = value
		case "database":
			cfg.database = value
		case "username":
			cfg.username = value
		case "password":
			cfg.password = value
		case "columns":
			cfg.columns = value
		case "doris_upload_request_timeout_ms":
			duration, err := parseMilliseconds(value)
			if err != nil {
				return demoConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.dorisUploadRequestTimeout = duration
		case "doris_upload_timeout_ms":
			duration, err := parseMilliseconds(value)
			if err != nil {
				return demoConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.dorisUploadTimeout = duration
		case "poll_timeout_ms":
			duration, err := parseMilliseconds(value)
			if err != nil {
				return demoConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.pollTimeout = duration
		case "data_repeats":
			repeats, err := strconv.Atoi(value)
			if err != nil {
				return demoConfig{}, fmt.Errorf("%s:%d: invalid data_repeats value %q", path, lineNo, value)
			}
			if repeats <= 0 {
				return demoConfig{}, fmt.Errorf("%s:%d: data_repeats must be greater than zero", path, lineNo)
			}
			cfg.dataRepeats = repeats
		case "row":
			cfg.rows = append(cfg.rows, value)
		default:
			return demoConfig{}, fmt.Errorf("%s:%d: unknown key %q", path, lineNo, key)
		}
	}
	if err := scanner.Err(); err != nil {
		return demoConfig{}, err
	}

	if strings.TrimSpace(cfg.streamLoadURL) == "" {
		return demoConfig{}, errors.New("stream_load_url is required")
	}
	if strings.TrimSpace(cfg.database) == "" {
		cfg.database = deriveDatabase(cfg.streamLoadURL)
	}
	if strings.TrimSpace(cfg.database) == "" {
		return demoConfig{}, errors.New("database is required when it cannot be derived from stream_load_url")
	}
	if strings.TrimSpace(cfg.columns) == "" {
		return demoConfig{}, errors.New("columns is required")
	}
	if len(cfg.rows) == 0 {
		return demoConfig{}, errors.New("at least one row is required")
	}

	cfg.client = newDemoHTTPClient(cfg.dorisUploadRequestTimeout)
	return cfg, nil
}

func newDemoHTTPClient(timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 10 {
			return errors.New("too many redirects")
		}
		if auth := via[len(via)-1].Header.Get("Authorization"); auth != "" && req.Header.Get("Authorization") == "" {
			req.Header.Set("Authorization", auth)
		}
		return nil
	}
	return client
}

func expandRows(rows []string, repeats int) []string {
	if repeats <= 1 {
		expanded := make([]string, 0, len(rows))
		for i, row := range rows {
			expanded = append(expanded, strings.ReplaceAll(row, "{ROW_ID}", strconv.Itoa(i+1)))
		}
		return expanded
	}
	expanded := make([]string, 0, len(rows)*repeats)
	rowID := 1
	for i := 0; i < repeats; i++ {
		for _, row := range rows {
			expanded = append(expanded, strings.ReplaceAll(row, "{ROW_ID}", strconv.Itoa(rowID)))
			rowID++
		}
	}
	return expanded
}

func parseMilliseconds(value string) (time.Duration, error) {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid millisecond value %q", value)
	}
	if n <= 0 {
		return 0, fmt.Errorf("millisecond value must be greater than zero")
	}
	return time.Duration(n) * time.Millisecond, nil
}

func retryBackoffDelay(attempt int) time.Duration {
	delay := 1 * time.Second
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= 4*time.Second {
			return 4 * time.Second
		}
	}
	return delay
}

func sendCSVBatch(cfg demoConfig, label string, rows []string) (*streamLoadResponse, error) {
	body := strings.Join(rows, "\n")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, cfg.streamLoadURL, bytes.NewBufferString(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/csv")
	req.Header.Set("label", label)
	req.Header.Set("columns", cfg.columns)
	req.Header.Set("format", "csv")
	req.Header.Set("column_separator", ",")
	req.Header.Set("enclose", `"`)
	req.Header.Set("Expect", "100-continue")
	if cfg.username != "" || cfg.password != "" {
		req.SetBasicAuth(cfg.username, cfg.password)
	}

	resp, err := cfg.client.Do(req)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, classifyTransportError(err)
	}

	var parsed streamLoadResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, fmt.Errorf("invalid stream load response: %s", strings.TrimSpace(string(payload)))
	}

	if isHTTPSuccess(resp.StatusCode, &parsed) {
		return &parsed, nil
	}

	switch parsed.Status {
	case "Label Already Exists":
		if strings.EqualFold(parsed.ExistingJobStatus, "RUNNING") {
			return nil, &streamLoadError{message: parsed.Message, ambiguous: true}
		}
		return nil, &streamLoadError{message: parsed.Message}
	case "Fail":
	}

	return nil, classifyResponseError(resp.StatusCode, parsed.Message)
}

func waitForLabel(cfg demoConfig, label string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	backoff := 500 * time.Millisecond
	attempt := 0

	for {
		attempt++
		state, err := getLoadState(cfg, label)
		if err == nil {
			fmt.Printf("poll attempt %d: label=%s state=%s\n", attempt, label, state)
			switch strings.ToUpper(state) {
			case "VISIBLE", "COMMITTED", "ABORTED", "UNKNOWN":
				return state, nil
			}
		} else {
			fmt.Printf("poll attempt %d: label=%s error=%v\n", attempt, label, err)
		}

		if time.Now().After(deadline) {
			if err != nil {
				return "", err
			}
			return "", fmt.Errorf("label %s did not reach a terminal state before timeout; last state=%s", label, state)
		}

		time.Sleep(backoff)
		backoff *= 2
		if backoff > 4*time.Second {
			backoff = 4 * time.Second
		}
	}
}

func getLoadState(cfg demoConfig, label string) (string, error) {
	endpoint, err := loadStateURL(cfg.streamLoadURL, cfg.database, label)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if cfg.username != "" || cfg.password != "" {
		req.SetBasicAuth(cfg.username, cfg.password)
	}

	resp, err := cfg.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var parsed loadStateResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("invalid get_load_state response: %s", strings.TrimSpace(string(body)))
	}
	if state := strings.TrimSpace(parsed.Data); state != "" {
		return state, nil
	}
	return "", fmt.Errorf("get_load_state failed: msg=%s code=%s", parsed.Msg, parsed.Code)
}

func loadStateURL(streamLoadURL, database, label string) (string, error) {
	base, err := url.Parse(streamLoadURL)
	if err != nil {
		return "", err
	}
	base.Path = path.Join("/", "api", database, "get_load_state")
	query := base.Query()
	query.Set("label", label)
	base.RawQuery = query.Encode()
	return base.String(), nil
}

func deriveDatabase(streamLoadURL string) string {
	base, err := url.Parse(streamLoadURL)
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
		return &streamLoadError{message: err.Error(), retriable: true}
	}
	return &streamLoadError{message: err.Error(), ambiguous: true}
}

func classifyResponseError(statusCode int, message string) error {
	return &streamLoadError{
		message:   message,
		retriable: statusCode == http.StatusRequestTimeout || statusCode == http.StatusTooManyRequests || statusCode >= 500,
	}
}

func isHTTPSuccess(statusCode int, response *streamLoadResponse) bool {
	if statusCode < 200 || statusCode >= 300 {
		return false
	}
	switch response.Status {
	case "", "Success", "Publish Timeout":
		return true
	case "Label Already Exists":
		return strings.EqualFold(response.ExistingJobStatus, "FINISHED")
	default:
		return false
	}
}

func generateLabel() string {
	return fmt.Sprintf("normal_demo_%d", time.Now().UnixNano())
}

type streamLoadError struct {
	message   string
	retriable bool
	ambiguous bool
	newLabel  bool
}

func (e *streamLoadError) Error() string {
	return e.message
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
