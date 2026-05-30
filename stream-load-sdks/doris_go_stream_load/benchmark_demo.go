//go:build ignore

package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dorisstreamload "github.com/wushilin/doris_go_stream_load"
)

const defaultConfigPath = "benchmark_demo.conf"

const sampleUseConfig = `# benchmark_demo config
# Lines beginning with # are comments.
# C-style block comments are also ignored.
# Repeat "row =" for each CSV row you want to send.
# Use {ROW_ID} inside a row to inject a 1-based generated row sequence.
# Use {NOW} inside a row to inject the current timestamp in RFC3339 format.

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
# Supported modes: csv, json
mode = csv

# Optional tuning
doris_upload_request_timeout_ms = 300000
queue_timeout_ms = 0
batch_bytes = 1048576
max_buffered_requests = 1000
linger_ms = 5
doris_upload_timeout_ms = 300000
poll_timeout_ms = 300000
report_interval_ms = 3000
threads = 50
sender_workers = 1
# Producer-side submitted batch size. 1 means Send one row at a time.
sender_batch_size = 1
validation = syntax
# Supported wait modes: sync, callback, none
wait_mode = sync
debug = false
fake_send = false
fake_send_delay_ms = 500
data_repeats = 1

row = {NOW},{ROW_ID},login
row = {NOW},{ROW_ID},logout
`

type sampleConfig struct {
	streamLoadURL             string
	database                  string
	username                  string
	password                  string
	columns                   []string
	mode                      dorisstreamload.Mode
	dorisUploadRequestTimeout time.Duration
	queueTimeout              time.Duration
	batchBytes                int
	maxBufferedRequests       int
	linger                    time.Duration
	dorisUploadTimeout        time.Duration
	pollTimeout               time.Duration
	reportInterval            time.Duration
	threads                   int
	senderWorkers             int
	senderBatchSize           int
	validation                dorisstreamload.ValidationMode
	waitMode                  waitMode
	debug                     bool
	fakeSend                  bool
	fakeSendDelay             time.Duration
	fakeSendDelaySet          bool
	dataRepeats               int
	rows                      []string
}

type waitMode string

const (
	waitModeSync     waitMode = "sync"
	waitModeCallback waitMode = "callback"
	waitModeNone     waitMode = "none"
)

func main() {
	configPath, printSample := parseArgs()
	if printSample {
		fmt.Print(sampleUseConfig)
		return
	}

	cfg, err := loadConfigFromFile(configPath)
	if err != nil {
		fatalf("config error: %v", err)
	}

	client, err := dorisstreamload.NewClient(dorisConfig(cfg))
	if err != nil {
		fatalf("client config error: %v", err)
	}

	totalRows := len(cfg.rows) * cfg.dataRepeats
	fmt.Printf("streaming %d rows with %d threads sender_workers=%d mode=%s wait_mode=%s (%d base rows x %d repeats)\n", totalRows, cfg.threads, cfg.senderWorkers, cfg.mode, cfg.waitMode, len(cfg.rows), cfg.dataRepeats)

	var stats streamStats
	stopStats := startStatsReporter(&stats, client, cfg.reportInterval)
	defer stopStats()

	started := time.Now()
	if err := streamRows(cfg, client, &stats); err != nil {
		fatalf("%v", err)
	}
	enqueueElapsed := time.Since(started)

	closeStarted := time.Now()
	if err := client.Close(); err != nil {
		log.Printf("close error: %v", err)
	}
	closeElapsed := time.Since(closeStarted)
	totalElapsed := time.Since(started)

	fmt.Printf("result: success rows=%d enqueue_elapsed=%s drain_elapsed=%s total_elapsed=%s rows_s=%.2f\n",
		totalRows,
		enqueueElapsed.Round(time.Millisecond),
		closeElapsed.Round(time.Millisecond),
		totalElapsed.Round(time.Millisecond),
		float64(totalRows)/totalElapsed.Seconds(),
	)
}

func streamRows(cfg sampleConfig, client *dorisstreamload.Client, stats *streamStats) error {
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	var nextRow atomic.Int64
	var firstLabelPrinted atomic.Bool
	totalRows := len(cfg.rows) * cfg.dataRepeats

	reportErr := func(err error) {
		select {
		case errCh <- err:
			close(stop)
		default:
		}
	}

	for workerID := 0; workerID < cfg.threads; workerID++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}

				if err := sendNextBatch(cfg, client, stats, &firstLabelPrinted, reportErr, &nextRow, totalRows); err != nil {
					reportErr(err)
					return
				}
				if nextRow.Load() >= int64(totalRows) {
					return
				}
			}
		}()
	}

	wg.Wait()

	if cfg.waitMode == waitModeCallback {
		waitForCallbacks(stats, stop, int64(len(cfg.rows)*cfg.dataRepeats))
	}

	select {
	case err := <-errCh:
		return err
	default:
		return nil
	}
}

func waitForCallbacks(stats *streamStats, stop <-chan struct{}, totalRows int64) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		if stats.acked.Load() >= totalRows || stats.inFlight.Load() == 0 {
			return
		}
		select {
		case <-ticker.C:
		case <-stop:
			return
		}
	}
}

func sendNextBatch(cfg sampleConfig, client *dorisstreamload.Client, stats *streamStats, firstLabelPrinted *atomic.Bool, reportErr func(error), nextRow *atomic.Int64, totalRows int) error {
	payloads := make([]string, 0, cfg.senderBatchSize)
	descriptions := make([]string, 0, cfg.senderBatchSize)

	for len(payloads) < cfg.senderBatchSize {
		rowID := int(nextRow.Add(1))
		if rowID > totalRows {
			if len(payloads) == 0 {
				return nil
			}
			break
		}
		templateIndex := (rowID - 1) % len(cfg.rows)
		rowTemplate := cfg.rows[templateIndex]
		row := expandRow(rowTemplate, rowID, time.Now())
		payload, err := payloadForMode(cfg, row)
		if err != nil {
			return fmt.Errorf("row %d encode failed: %w", rowID, err)
		}
		payloads = append(payloads, payload)
		descriptions = append(descriptions, fmt.Sprintf("row %d", rowID))
	}

	if len(payloads) == 0 {
		return nil
	}

	for start := 0; start < len(payloads); start += cfg.senderBatchSize {
		end := start + cfg.senderBatchSize
		if end > len(payloads) {
			end = len(payloads)
		}
		if err := sendPayloadBatch(cfg, client, stats, firstLabelPrinted, reportErr, payloads[start:end], descriptions[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func sendPayloadBatch(cfg sampleConfig, client *dorisstreamload.Client, stats *streamStats, firstLabelPrinted *atomic.Bool, reportErr func(error), payloads []string, descriptions []string) error {
	rowCount := len(payloads)
	description := fmt.Sprintf("%s..%s", descriptions[0], descriptions[len(descriptions)-1])
	if len(payloads) == 1 {
		description = descriptions[0]
	}
	if cfg.waitMode == waitModeNone {
		return sendPayloadNone(client, stats, payloads, rowCount, description)
	}
	if cfg.waitMode == waitModeCallback {
		return sendPayloadWithCallback(client, stats, firstLabelPrinted, reportErr, payloads, rowCount, description)
	}
	return sendPayloadSync(client, stats, firstLabelPrinted, payloads, rowCount, description)
}

func sendPayloadSync(client *dorisstreamload.Client, stats *streamStats, firstLabelPrinted *atomic.Bool, payloads []string, rowCount int, description string) error {
	handle, err := client.SendBatch(payloads)
	if err != nil {
		return fmt.Errorf("%s enqueue failed: %w", description, err)
	}
	stats.enqueued.Add(int64(rowCount))
	stats.inFlight.Add(int64(rowCount))

	result := handle.Wait()
	stats.inFlight.Add(-int64(rowCount))
	if result.Err != nil {
		return fmt.Errorf("%s failed after %d attempt(s): %w", description, result.Attempts, result.Err)
	}
	stats.acked.Add(int64(rowCount))

	if result.Response != nil && firstLabelPrinted.CompareAndSwap(false, true) {
		fmt.Printf("first stream load label: %s\n", result.Response.Label)
	}

	return nil
}

func sendPayloadWithCallback(client *dorisstreamload.Client, stats *streamStats, firstLabelPrinted *atomic.Bool, reportErr func(error), payloads []string, rowCount int, description string) error {
	callback := func(result dorisstreamload.DeliveryResult) {
		stats.inFlight.Add(-int64(rowCount))
		if result.Err != nil {
			reportErr(fmt.Errorf("%s failed after %d attempt(s): %w", description, result.Attempts, result.Err))
			return
		}
		stats.acked.Add(int64(rowCount))
		if result.Response != nil && firstLabelPrinted.CompareAndSwap(false, true) {
			fmt.Printf("first stream load label: %s\n", result.Response.Label)
		}
	}

	if _, err := client.SendBatchWithCallback(callback, payloads); err != nil {
		return fmt.Errorf("%s enqueue failed: %w", description, err)
	}
	stats.enqueued.Add(int64(rowCount))
	stats.inFlight.Add(int64(rowCount))
	return nil
}

func sendPayloadNone(client *dorisstreamload.Client, stats *streamStats, payloads []string, rowCount int, description string) error {
	if _, err := client.SendBatch(payloads); err != nil {
		return fmt.Errorf("%s enqueue failed: %w", description, err)
	}
	stats.enqueued.Add(int64(rowCount))
	stats.inFlight.Add(int64(rowCount))
	return nil
}

type streamStats struct {
	inFlight atomic.Int64
	acked    atomic.Int64
	enqueued atomic.Int64
}

type statsSnapshot struct {
	at       time.Time
	acked    int64
	enqueued int64
}

func startStatsReporter(stats *streamStats, client *dorisstreamload.Client, interval time.Duration) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	ticker := time.NewTicker(interval)
	last := currentStatsSnapshot(stats)

	go func() {
		defer ticker.Stop()
		defer close(stopped)
		for {
			select {
			case <-ticker.C:
				last = printStats(stats, client, last)
			case <-done:
				printStats(stats, client, last)
				return
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}

func currentStatsSnapshot(stats *streamStats) statsSnapshot {
	return statsSnapshot{
		at:       time.Now(),
		acked:    stats.acked.Load(),
		enqueued: stats.enqueued.Load(),
	}
}

func printStats(stats *streamStats, client *dorisstreamload.Client, last statsSnapshot) statsSnapshot {
	now := currentStatsSnapshot(stats)
	elapsed := now.at.Sub(last.at).Seconds()
	if elapsed <= 0 {
		elapsed = 1
	}

	sentRowsPerSecond := float64(now.enqueued-last.enqueued) / elapsed
	ackedRowsPerSecond := float64(now.acked-last.acked) / elapsed

	fmt.Printf("stats: in_flight=%d acked=%d enqueued=%d queue_requests=%d sent_rows_s=%.2f ack_rows_s=%.2f\n",
		stats.inFlight.Load(),
		now.acked,
		now.enqueued,
		client.BufferedRecords(),
		sentRowsPerSecond,
		ackedRowsPerSecond,
	)
	clientStats := client.Stats()
	fmt.Printf("client: workers=%d idle=%d busy=%d jobs=%d errors=%d error_rate=%.4f avg_load=%s p50=%s p90=%s p99=%s p999=%s avg_retries=%.2f bytes=%d avg_load_size=%.2f bytes_s=%.2f records=%d records_s=%.2f attempts=%d\n",
		clientStats.TotalWorkers,
		clientStats.IdleWorkers,
		clientStats.BusyWorkers,
		clientStats.TotalLoadJobs,
		clientStats.ErrorJobs,
		clientStats.ErrorRate,
		clientStats.AverageLoadTime.Round(time.Millisecond),
		clientStats.P50LoadTime.Round(time.Millisecond),
		clientStats.P90LoadTime.Round(time.Millisecond),
		clientStats.P99LoadTime.Round(time.Millisecond),
		clientStats.P999LoadTime.Round(time.Millisecond),
		clientStats.AverageRetries,
		clientStats.TotalBytesSent,
		clientStats.AverageLoadSize,
		clientStats.AverageBytesRate,
		clientStats.RecordsSent,
		clientStats.AverageRecordsRate,
		clientStats.TotalUploadAttempts,
	)
	return now
}

func parseArgs() (configPath string, printSample bool) {
	flag.Usage = func() {
		printHelp(flag.CommandLine.Output())
	}

	flag.StringVar(&configPath, "config", defaultConfigPath, "Path to config file")
	flag.BoolVar(&printSample, "sample", false, "Print a sample config file and exit")
	flag.Parse()
	return configPath, printSample
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, "benchmark_demo measures dorisstreamload batching, callbacks, queueing, and stats.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  go run benchmark_demo.go")
	fmt.Fprintln(w, "  go run benchmark_demo.go --config benchmark_demo.conf")
	fmt.Fprintln(w, "  go run benchmark_demo.go --sample")
	fmt.Fprintln(w, "  go run benchmark_demo.go --help")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Flags:")
	flag.CommandLine.SetOutput(w)
	flag.PrintDefaults()
}

func loadConfigFromFile(path string) (sampleConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return sampleConfig{}, err
	}
	defer file.Close()

	cfg := sampleConfig{
		dorisUploadRequestTimeout: 300 * time.Second,
		queueTimeout:              0,
		batchBytes:                0,
		maxBufferedRequests:       0,
		linger:                    0,
		dorisUploadTimeout:        300 * time.Second,
		pollTimeout:               300 * time.Second,
		reportInterval:            3 * time.Second,
		threads:                   50,
		senderWorkers:             1,
		senderBatchSize:           1,
		validation:                dorisstreamload.ValidateSyntax,
		waitMode:                  waitModeSync,
		fakeSendDelay:             500 * time.Millisecond,
		dataRepeats:               1,
		mode:                      dorisstreamload.ModeCSV,
	}

	scanner := bufio.NewScanner(file)
	lineNo := 0
	inBlockComment := false
	seenKeys := make(map[string]int)
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
			return sampleConfig{}, fmt.Errorf("%s:%d: expected key = value", path, lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if key != "row" {
			if firstSeenLine, ok := seenKeys[key]; ok {
				return sampleConfig{}, fmt.Errorf("%s:%d: duplicate key %q (first defined at line %d)", path, lineNo, key, firstSeenLine)
			}
			seenKeys[key] = lineNo
		}

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
			cfg.columns = parseColumns(value)
		case "mode":
			mode, err := parseMode(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.mode = mode
		case "doris_upload_request_timeout_ms":
			duration, err := parseMilliseconds(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.dorisUploadRequestTimeout = duration
		case "queue_timeout_ms":
			duration, err := parseNonNegativeMilliseconds(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.queueTimeout = duration
		case "batch_bytes":
			batchBytes, err := parseNonNegativeInt("batch_bytes", value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.batchBytes = batchBytes
		case "max_buffered_requests":
			maxBufferedRequests, err := parseNonNegativeInt("max_buffered_requests", value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.maxBufferedRequests = maxBufferedRequests
		case "linger_ms":
			duration, err := parseNonNegativeMilliseconds(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.linger = duration
		case "doris_upload_timeout_ms":
			duration, err := parseMilliseconds(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.dorisUploadTimeout = duration
		case "poll_timeout_ms":
			duration, err := parseMilliseconds(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.pollTimeout = duration
		case "report_interval_ms":
			duration, err := parseMilliseconds(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.reportInterval = duration
		case "threads":
			threads, err := strconv.Atoi(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: invalid threads value %q", path, lineNo, value)
			}
			if threads <= 0 {
				return sampleConfig{}, fmt.Errorf("%s:%d: threads must be greater than zero", path, lineNo)
			}
			cfg.threads = threads
		case "sender_workers":
			senderWorkers, err := strconv.Atoi(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: invalid sender_workers value %q", path, lineNo, value)
			}
			if senderWorkers <= 0 {
				return sampleConfig{}, fmt.Errorf("%s:%d: sender_workers must be greater than zero", path, lineNo)
			}
			cfg.senderWorkers = senderWorkers
		case "sender_batch_size":
			senderBatchSize, err := strconv.Atoi(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: invalid sender_batch_size value %q", path, lineNo, value)
			}
			if senderBatchSize <= 0 {
				return sampleConfig{}, fmt.Errorf("%s:%d: sender_batch_size must be greater than zero", path, lineNo)
			}
			cfg.senderBatchSize = senderBatchSize
		case "validation":
			validation, err := parseValidationMode(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.validation = validation
		case "wait_mode":
			waitMode, err := parseWaitMode(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.waitMode = waitMode
		case "debug":
			debug, err := parseBool(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.debug = debug
		case "fake_send":
			fakeSend, err := parseBool(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.fakeSend = fakeSend
		case "fake_send_delay_ms":
			duration, err := parseNonNegativeMilliseconds(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: %w", path, lineNo, err)
			}
			cfg.fakeSendDelay = duration
			cfg.fakeSendDelaySet = true
		case "data_repeats":
			repeats, err := strconv.Atoi(value)
			if err != nil {
				return sampleConfig{}, fmt.Errorf("%s:%d: invalid data_repeats value %q", path, lineNo, value)
			}
			if repeats <= 0 {
				return sampleConfig{}, fmt.Errorf("%s:%d: data_repeats must be greater than zero", path, lineNo)
			}
			cfg.dataRepeats = repeats
		case "row":
			cfg.rows = append(cfg.rows, value)
		default:
			return sampleConfig{}, fmt.Errorf("%s:%d: unknown key %q", path, lineNo, key)
		}
	}
	if err := scanner.Err(); err != nil {
		return sampleConfig{}, err
	}

	if strings.TrimSpace(cfg.streamLoadURL) == "" {
		return sampleConfig{}, errors.New("stream_load_url is required")
	}
	if len(cfg.columns) == 0 {
		return sampleConfig{}, errors.New("columns is required")
	}
	if len(cfg.rows) == 0 {
		return sampleConfig{}, errors.New("at least one row is required")
	}

	return cfg, nil
}

func dorisConfig(cfg sampleConfig) dorisstreamload.Config {
	logLevel := dorisstreamload.LogLevelInfo
	if cfg.debug {
		logLevel = dorisstreamload.LogLevelDebug
	}

	dorisCfg := dorisstreamload.Config{
		StreamLoadURL:             cfg.streamLoadURL,
		Database:                  cfg.database,
		Columns:                   cfg.columns,
		Mode:                      cfg.mode,
		DorisUploadRequestTimeout: cfg.dorisUploadRequestTimeout,
		MaxQueueWaitTime:          cfg.queueTimeout,
		BatchBytes:                cfg.batchBytes,
		MaxQueueSize:              cfg.maxBufferedRequests,
		Linger:                    cfg.linger,
		DorisUploadTimeout:        cfg.dorisUploadTimeout,
		StatusPollTimeout:         cfg.pollTimeout,
		DorisUploadWorkers:        cfg.senderWorkers,
		Validation:                cfg.validation,
		FakeSend:                  cfg.fakeSend,
		FakeSendDelay:             cfg.fakeSendDelay,
		FakeSendDelaySet:          cfg.fakeSendDelaySet,
		LogLevel:                  logLevel,
		LogLevelSet:               true,
		Logger:                    log.Default(),
		HTTPClient:                newSampleHTTPClient(cfg.dorisUploadRequestTimeout),
	}
	if cfg.username != "" || cfg.password != "" {
		dorisCfg.AuthenticationType = dorisstreamload.AuthenticationBasic
		dorisCfg.AuthenticationToken = cfg.username + ":" + cfg.password
	}
	return dorisCfg
}

func newSampleHTTPClient(timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 10 {
			return errors.New("too many redirects")
		}
		if len(via) == 1 {
			if auth := via[0].Header.Get("Authorization"); auth != "" && req.Header.Get("Authorization") == "" {
				req.Header.Set("Authorization", auth)
			}
		}
		return nil
	}
	return client
}

func parseColumns(value string) []string {
	parts := strings.Split(value, ",")
	columns := make([]string, 0, len(parts))
	for _, part := range parts {
		column := strings.TrimSpace(part)
		if column != "" {
			columns = append(columns, column)
		}
	}
	return columns
}

func expandRow(row string, rowID int, now time.Time) string {
	row = strings.ReplaceAll(row, "{ROW_ID}", strconv.Itoa(rowID))
	row = strings.ReplaceAll(row, "{NOW}", now.Local().Format(time.RFC3339))
	return row
}

func parseMode(value string) (dorisstreamload.Mode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "csv":
		return dorisstreamload.ModeCSV, nil
	case "json":
		return dorisstreamload.ModeJSON, nil
	default:
		return "", fmt.Errorf("invalid mode %q; expected csv or json", value)
	}
}

func parseValidationMode(value string) (dorisstreamload.ValidationMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(dorisstreamload.ValidateSyntax):
		return dorisstreamload.ValidateSyntax, nil
	case string(dorisstreamload.ValidateNone):
		return dorisstreamload.ValidateNone, nil
	case string(dorisstreamload.ValidateStrict):
		return dorisstreamload.ValidateStrict, nil
	default:
		return "", fmt.Errorf("invalid validation %q; expected none, syntax, or strict", value)
	}
}

func parseWaitMode(value string) (waitMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(waitModeSync):
		return waitModeSync, nil
	case string(waitModeCallback):
		return waitModeCallback, nil
	case string(waitModeNone):
		return waitModeNone, nil
	default:
		return "", fmt.Errorf("invalid wait_mode %q; expected sync, callback, or none", value)
	}
}

func parseBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "y", "on":
		return true, nil
	case "false", "0", "no", "n", "off", "":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean value %q", value)
	}
}

func payloadForMode(cfg sampleConfig, csvRow string) (string, error) {
	switch cfg.mode {
	case dorisstreamload.ModeCSV:
		return csvRow, nil
	case dorisstreamload.ModeJSON:
		return jsonPayload(cfg.columns, csvRow)
	default:
		return "", fmt.Errorf("unsupported mode %q", cfg.mode)
	}
}

func jsonPayload(columns []string, csvRow string) (string, error) {
	record, err := jsonRecord(columns, csvRow)
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func jsonRecord(columns []string, csvRow string) (map[string]string, error) {
	fields, err := parseCSVRow(csvRow)
	if err != nil {
		return nil, err
	}
	if len(fields) != len(columns) {
		return nil, fmt.Errorf("csv row has %d field(s), but columns has %d", len(fields), len(columns))
	}

	record := make(map[string]string, len(columns))
	for i, column := range columns {
		record[column] = fields[i]
	}
	return record, nil
}

func parseCSVRow(row string) ([]string, error) {
	reader := csv.NewReader(strings.NewReader(row))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	fields, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("invalid csv row: %w", err)
	}
	if _, err := reader.Read(); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("csv row must contain exactly one record")
		}
		return nil, fmt.Errorf("invalid csv row: %w", err)
	}
	return fields, nil
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

func parseNonNegativeMilliseconds(value string) (time.Duration, error) {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid millisecond value %q", value)
	}
	if n < 0 {
		return 0, fmt.Errorf("millisecond value cannot be negative")
	}
	return time.Duration(n) * time.Millisecond, nil
}

func parseNonNegativeInt(field, value string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid %s value %q", field, value)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s cannot be negative", field)
	}
	return n, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
