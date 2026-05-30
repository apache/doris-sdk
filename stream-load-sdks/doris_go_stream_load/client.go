package dorisstreamload

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"strings"
	"sync"
	"time"
)

type queueItem struct {
	payload  string
	byteSize int
	handle   *Handle
}

type queuedSubmission struct {
	items              []*queueItem
	standaloneByteSize int
	appendByteSize     int
	callback           DeliveryCallback
}

type deliveryBatch struct {
	label       string
	mode        Mode
	items       []*queueItem
	byteSize    int
	createdAt   time.Time
	completion  *batchCompletion
	hasCallback bool
	submissions []*queuedSubmission
	csvRows     []string
	jsonRecords []string
}

func (b *deliveryBatch) len() int {
	return len(b.items)
}

func (b *deliveryBatch) canAccept(item *queueItem, cfg Config) bool {
	if b == nil || len(b.items) == 0 {
		return true
	}
	if cfg.BatchBytes > 0 && b.byteSize >= cfg.BatchBytes {
		return false
	}
	return true
}

func (b *deliveryBatch) addSubmission(submission *queuedSubmission) {
	if submission.callback != nil {
		b.hasCallback = true
	}
	b.submissions = append(b.submissions, submission)
}

func (b *deliveryBatch) add(item *queueItem, cfg Config) {
	if len(b.items) == 0 {
		b.label = generateLabel(cfg.LabelPrefix)
		b.mode = cfg.Mode
		b.createdAt = time.Now()
		b.completion = newBatchCompletion()
	}
	b.items = append(b.items, item)
	item.handle.attach(b.completion)

	switch cfg.Mode {
	case ModeCSV:
		if len(b.csvRows) > 0 {
			b.byteSize++
		}
		b.byteSize += len(item.payload)
		b.csvRows = append(b.csvRows, item.payload)
	case ModeJSON:
		if len(b.jsonRecords) == 0 {
			b.byteSize = 2 + len(item.payload)
		} else {
			b.byteSize += 1 + len(item.payload)
		}
		b.jsonRecords = append(b.jsonRecords, item.payload)
	}
}

func (b *deliveryBatch) estimatedByteSizeWith(item *queueItem, cfg Config) int {
	switch cfg.Mode {
	case ModeCSV:
		rows := len(b.csvRows)
		size := b.byteSize
		if rows == 0 {
			size = 0
		}
		if item != nil {
			if rows > 0 {
				size++
			}
			size += len(item.payload)
		}
		if item == nil {
			size = joinCSVRowsByteSize(b.csvRows)
		}
		return size
	case ModeJSON:
		records := b.jsonRecords
		if item != nil {
			records = append(append([]string(nil), records...), item.payload)
		}
		return joinJSONRecordsByteSize(records)
	default:
		return 0
	}
}

func (b *deliveryBatch) encodeBody() ([]byte, string, error) {
	switch b.mode {
	case ModeCSV:
		return []byte(strings.Join(b.csvRows, "\n")), "text/csv", nil
	case ModeJSON:
		var builder strings.Builder
		builder.Grow(joinJSONRecordsByteSize(b.jsonRecords))
		builder.WriteByte('[')
		for i, record := range b.jsonRecords {
			if i > 0 {
				builder.WriteByte(',')
			}
			builder.WriteString(record)
		}
		builder.WriteByte(']')
		return []byte(builder.String()), "application/json", nil
	default:
		return nil, "", fmt.Errorf("unsupported mode %q", b.mode)
	}
}

func (b *deliveryBatch) headerFormat() string {
	switch b.mode {
	case ModeCSV:
		return "csv"
	case ModeJSON:
		return "json"
	default:
		return ""
	}
}

type Client struct {
	cfg Config

	validator recordValidator

	intake   *requestQueue
	dispatch chan *deliveryBatch
	sender   sender
	stats    *clientStatsCollector

	wg sync.WaitGroup

	closeOnce sync.Once
	closing   chan struct{}
	closed    chan struct{}

	mu       sync.RWMutex
	isClosed bool
}

func NewClient(cfg Config) (*Client, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	httpClient, err := buildHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	cfg.HTTPClient = httpClient
	var sender sender = &httpSender{cfg: cfg}
	if cfg.FakeSend {
		sender = &fakeSuccessSender{delay: cfg.FakeSendDelay}
	}
	c := newClientWithSender(cfg, sender)
	if cfg.StreamLoadURL != "" && !streamLoadURLHasSuffix(cfg.StreamLoadURL) {
		c.warnf("stream load url %q does not end with _stream_load; this may not be a valid Doris stream load endpoint", cfg.StreamLoadURL)
	}
	return c, nil
}

func newClientWithSender(cfg Config, s sender) *Client {
	c := &Client{
		cfg:       cfg,
		validator: newRecordValidator(cfg),
		intake:    newRequestQueue(cfg.MaxQueueSize),
		dispatch:  make(chan *deliveryBatch, cfg.MaxUploadQueueSize),
		sender:    s,
		stats:     newClientStatsCollector(time.Now()),
		closing:   make(chan struct{}),
		closed:    make(chan struct{}),
	}
	c.wg.Add(1)
	go c.runBatcher()
	for i := 0; i < cfg.DorisUploadWorkers; i++ {
		c.wg.Add(1)
		go c.runWorker(i)
	}
	return c
}

func (c *Client) Send(record string) (*Handle, error) {
	return c.SendBatch([]string{record})
}

func (c *Client) SendBatch(records []string) (*Handle, error) {
	ctx := context.Background()
	var cancel context.CancelFunc
	if c.cfg.MaxQueueWaitTime > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.cfg.MaxQueueWaitTime)
		defer cancel()
	}
	return c.sendRecords(ctx, records)
}

func (c *Client) SendBatchContext(ctx context.Context, records []string) (*Handle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return c.sendRecords(ctx, records)
}

func (c *Client) SendWithCallback(callback DeliveryCallback, record string) (*Handle, error) {
	return c.SendBatchWithCallback(callback, []string{record})
}

func (c *Client) SendBatchWithCallback(callback DeliveryCallback, records []string) (*Handle, error) {
	ctx := context.Background()
	var cancel context.CancelFunc
	if c.cfg.MaxQueueWaitTime > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.cfg.MaxQueueWaitTime)
		defer cancel()
	}
	return c.sendRecords(ctx, records, WithCallback(callback))
}

func (c *Client) SendRecord(records ...string) (*Handle, error) {
	return c.SendBatch(records)
}

func (c *Client) SendRecordContext(ctx context.Context, records ...string) (*Handle, error) {
	return c.SendBatchContext(ctx, records)
}

func (c *Client) SendRecordWithCallback(callback DeliveryCallback, records ...string) (*Handle, error) {
	return c.SendBatchWithCallback(callback, records)
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.isClosed = true
		close(c.closing)
		c.mu.Unlock()

		c.intake.Close()

		c.wg.Wait()
		close(c.closed)
	})
	return nil
}

func (c *Client) Closed() <-chan struct{} {
	return c.closed
}

func (c *Client) BufferedRecords() int {
	return c.intake.Len()
}

func (c *Client) Stats() ClientStats {
	if c.stats == nil {
		return ClientStats{}
	}
	return c.stats.snapshot(c.cfg.DorisUploadWorkers)
}

func (c *Client) sendRecords(ctx context.Context, records []string, opts ...SendOption) (*Handle, error) {
	if len(records) == 0 {
		return nil, errors.New("at least one record is required")
	}

	sendOpts := sendOptions{}
	for _, opt := range opts {
		opt(&sendOpts)
	}

	items := make([]*queueItem, 0, len(records))
	handle := newHandle()
	payloadBytes := 0
	for _, record := range records {
		item, err := c.prepareItem(record)
		if err != nil {
			return nil, err
		}
		item.handle = handle
		items = append(items, item)
		payloadBytes += item.byteSize
	}
	submission := newQueuedSubmission(c.cfg.Mode, items, payloadBytes, sendOpts.callback)
	if c.cfg.BatchBytes > 0 && submission.standaloneByteSize > c.cfg.BatchBytes {
		return nil, ErrSendTooLarge
	}

	c.mu.RLock()
	if c.isClosed {
		c.mu.RUnlock()
		return nil, ErrClientClosed
	}
	c.mu.RUnlock()

	if err := c.intake.Enqueue(ctx, submission); err != nil {
		return nil, err
	}
	for _, item := range items {
		c.logf(LogLevelDebug, "message enqueued: mode=%s bytes=%d", c.cfg.Mode, item.byteSize)
	}
	return handle, nil
}

func newQueuedSubmission(mode Mode, items []*queueItem, payloadBytes int, callback DeliveryCallback) *queuedSubmission {
	count := len(items)
	if count == 0 {
		return &queuedSubmission{}
	}

	submission := &queuedSubmission{items: items, callback: callback}
	switch mode {
	case ModeCSV:
		submission.standaloneByteSize = payloadBytes + count - 1
		submission.appendByteSize = payloadBytes + count
	case ModeJSON:
		submission.standaloneByteSize = payloadBytes + count + 1
		submission.appendByteSize = payloadBytes + count
	default:
		submission.standaloneByteSize = payloadBytes
		submission.appendByteSize = payloadBytes
	}
	return submission
}

func (c *Client) prepareItem(record string) (*queueItem, error) {
	if strings.TrimSpace(record) == "" {
		return nil, errors.New("record cannot be empty")
	}

	validator := c.validator
	if validator == nil {
		validator = newRecordValidator(c.cfg)
	}
	if err := validator.validate(record); err != nil {
		return nil, err
	}

	item := &queueItem{
		payload:  record,
		byteSize: len(record),
	}

	switch c.cfg.Mode {
	case ModeCSV:
	case ModeJSON:
	default:
		return nil, fmt.Errorf("unsupported mode %q", c.cfg.Mode)
	}

	return item, nil
}

func (c *Client) runBatcher() {
	defer c.wg.Done()

	var current *deliveryBatch

	flush := func() {
		if current == nil || current.len() == 0 {
			return
		}
		c.logf(LogLevelDebug, "batch sealed: label=%s items=%d bytes=%d mode=%s", current.label, current.len(), current.byteSize, c.cfg.Mode)
		c.dispatch <- current
		current = nil
	}

	addItem := func(item *queueItem) {
		if current == nil {
			current = &deliveryBatch{}
		}
		current.add(item, c.cfg)
	}

	addSubmission := func(submission *queuedSubmission) {
		if current == nil {
			current = &deliveryBatch{}
		}
		if current.len() > 0 && c.cfg.BatchBytes > 0 && current.byteSize+submission.appendByteSize > c.cfg.BatchBytes {
			flush()
			current = &deliveryBatch{}
		}
		current.addSubmission(submission)
		for _, item := range submission.items {
			addItem(item)
		}
		if c.cfg.BatchBytes > 0 && current.byteSize >= c.cfg.BatchBytes {
			flush()
		}
	}

	for {
		submissions, _, ok := c.intake.DequeueBatch(c.cfg.BatchBytes)
		if !ok {
			flush()
			close(c.dispatch)
			return
		}
		for _, submission := range submissions {
			addSubmission(submission)
		}

	drain:
		for {
			if current == nil || current.len() == 0 {
				break drain
			}
			lingerRemaining := c.cfg.Linger - time.Since(current.createdAt)
			if lingerRemaining <= 0 {
				c.logf(LogLevelDebug, "batch linger reached")
				flush()
				break drain
			}
			remaining := c.cfg.BatchBytes - current.byteSize
			if c.cfg.BatchBytes > 0 && remaining <= 0 {
				flush()
				break drain
			}
			submissions, _, ok, timedOut := c.intake.DequeueBatchWait(remaining, lingerRemaining)
			if !ok {
				flush()
				close(c.dispatch)
				return
			}
			if timedOut {
				c.logf(LogLevelDebug, "batch linger reached")
				flush()
				break drain
			}
			for _, submission := range submissions {
				addSubmission(submission)
			}
		}
	}
}

func (c *Client) runWorker(id int) {
	defer c.wg.Done()
	for batch := range c.dispatch {
		c.stats.changeBusyWorkers(1)
		c.logf(LogLevelDebug, "worker=%d send batch label=%s items=%d bytes=%d", id, batch.label, batch.len(), batch.byteSize)
		c.deliverBatch(batch)
		c.stats.changeBusyWorkers(-1)
	}
}

func (c *Client) deliverBatch(batch *deliveryBatch) {
	var (
		outcome sendOutcome
		err     error
	)

	started := time.Now()
	attempts := 0
	var retryDeadline time.Time

	for {
		if attempts > 0 && !retryDeadline.IsZero() && time.Now().After(retryDeadline) {
			result := DeliveryResult{
				Err:        &streamLoadError{StatusCode: outcome.statusCode, Message: fmt.Sprintf("upload did not conclude within doris upload timeout %s", c.cfg.DorisUploadTimeout)},
				Attempts:   attempts,
				StatusCode: outcome.statusCode,
				Response:   outcome.response,
				StartedAt:  started,
				FinishedAt: time.Now(),
			}
			c.completeBatch(batch, result)
			return
		}

		attempts++
		c.stats.recordUploadAttempt(batch.byteSize, batch.len())
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.DorisUploadRequestTimeout)
		outcome, err = c.sender.Send(ctx, batch)
		cancel()

		if err == nil {
			result := DeliveryResult{
				Attempts:   attempts,
				StatusCode: outcome.statusCode,
				Response:   outcome.response,
				StartedAt:  started,
				FinishedAt: time.Now(),
			}
			c.completeBatch(batch, result)
			return
		}

		retriable := isRetriable(err)
		ambiguous := isAmbiguous(err)
		c.logf(LogLevelDebug, "send error: attempts=%d retriable=%t err=%v", attempts, retriable, err)
		if ambiguous {
			pollResult, pollErr := c.pollLabelUntilConclusion(batch.label, started, attempts)
			if pollErr == nil {
				c.completeBatch(batch, pollResult)
				return
			}
			err = pollErr
			retriable = isRetriable(pollErr)
			if retriable {
				// Generate a fresh label: the old one is now registered in Doris
				// (as ABORTED) and cannot be reused.
				batch.label = generateLabel(c.cfg.LabelPrefix)
			}
		}
		if !retriable {
			result := DeliveryResult{
				Err:        err,
				Attempts:   attempts,
				StatusCode: outcome.statusCode,
				Response:   outcome.response,
				StartedAt:  started,
				FinishedAt: time.Now(),
			}
			c.completeBatch(batch, result)
			return
		}

		if retryDeadline.IsZero() {
			retryDeadline = time.Now().Add(c.cfg.DorisUploadTimeout)
		}
		if time.Now().After(retryDeadline) {
			result := DeliveryResult{
				Err:        &streamLoadError{StatusCode: outcome.statusCode, Message: fmt.Sprintf("upload did not conclude within doris upload timeout %s", c.cfg.DorisUploadTimeout)},
				Attempts:   attempts,
				StatusCode: outcome.statusCode,
				Response:   outcome.response,
				StartedAt:  started,
				FinishedAt: time.Now(),
			}
			c.completeBatch(batch, result)
			return
		}

		backoff := c.retryBackoffDelay(attempts)
		if remaining := time.Until(retryDeadline); backoff > remaining {
			backoff = remaining
		}
		c.logf(LogLevelDebug, "retry scheduled: attempts=%d backoff=%s", attempts, backoff)
		time.Sleep(backoff)
	}
}

func (c *Client) completeBatch(batch *deliveryBatch, result DeliveryResult) {
	c.stats.recordCompletion(result)
	batch.completion.complete(result)
	if !batch.hasCallback {
		return
	}
	for _, submission := range batch.submissions {
		c.invokeCallback(submission.callback, result)
	}
}

func (c *Client) invokeCallback(callback DeliveryCallback, result DeliveryResult) {
	if callback == nil {
		return
	}

	started := time.Now()
	callback(result)
	if elapsed := time.Since(started); elapsed > c.cfg.SlowCallbackWarn {
		c.logf(LogLevelInfo, "callback took %s", elapsed)
	}
}

func (c *Client) retryBackoffDelay(attempt int) time.Duration {
	delay := uploadRetryInitialBackoff
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= uploadRetryMaxBackoff {
			return uploadRetryMaxBackoff
		}
	}
	return delay
}

func (c *Client) logf(level LogLevel, format string, args ...any) {
	if c.cfg.Logger == nil || level > c.cfg.LogLevel {
		return
	}
	c.cfg.Logger.Printf(format, args...)
}

func (c *Client) warnf(format string, args ...any) {
	if c.cfg.Logger != nil {
		c.cfg.Logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func isRetriable(err error) bool {
	var loadErr *streamLoadError
	if errors.As(err, &loadErr) {
		return loadErr.Retriable
	}
	return false
}

func isAmbiguous(err error) bool {
	var loadErr *streamLoadError
	if errors.As(err, &loadErr) {
		return loadErr.Ambiguous
	}
	return false
}

func joinCSVRowsByteSize(rows []string) int {
	if len(rows) == 0 {
		return 0
	}
	size := 0
	for i, row := range rows {
		if i > 0 {
			size++
		}
		size += len(row)
	}
	return size
}

func joinJSONRecordsByteSize(records []string) int {
	if len(records) == 0 {
		return 2
	}
	size := 2
	for i, record := range records {
		if i > 0 {
			size++
		}
		size += len(record)
	}
	return size
}

type recordValidator interface {
	validate(record string) error
}

type noOpRecordValidator struct{}

func (noOpRecordValidator) validate(string) error {
	return nil
}

func newRecordValidator(cfg Config) recordValidator {
	if cfg.Validation == ValidateNone {
		return noOpRecordValidator{}
	}
	switch cfg.Mode {
	case ModeCSV:
		return newCSVRecordValidator(len(cfg.Columns))
	case ModeJSON:
		return newJSONRecordValidator(cfg.Columns, cfg.Validation == ValidateStrict)
	default:
		return noOpRecordValidator{}
	}
}

type csvRecordValidator struct {
	expectedColumns int
	pool            sync.Pool
}

func newCSVRecordValidator(expectedColumns int) *csvRecordValidator {
	validator := &csvRecordValidator{expectedColumns: expectedColumns}
	validator.pool.New = func() any {
		return newCSVRecordParser(expectedColumns)
	}
	return validator
}

func (v *csvRecordValidator) validate(record string) error {
	parser := v.pool.Get().(*csvRecordParser)
	err := parser.validate(record)
	if err == nil {
		v.pool.Put(parser)
	}
	return err
}

type csvRecordParser struct {
	source strings.Reader
	reader *csv.Reader
}

func newCSVRecordParser(expectedColumns int) *csvRecordParser {
	parser := &csvRecordParser{}
	parser.reader = csv.NewReader(&parser.source)
	parser.reader.FieldsPerRecord = expectedColumns
	parser.reader.ReuseRecord = true
	return parser
}

func (p *csvRecordParser) validate(record string) error {
	p.source.Reset(record)

	row, err := p.reader.Read()
	if err != nil {
		return fmt.Errorf("invalid csv record: %w", err)
	}
	if len(row) != p.reader.FieldsPerRecord {
		return fmt.Errorf("invalid csv record: expected %d columns, got %d", p.reader.FieldsPerRecord, len(row))
	}
	if _, err := p.reader.Read(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("invalid csv record: expected exactly one row")
		}
		return fmt.Errorf("invalid csv record: %w", err)
	}
	return nil
}

type jsonRecordValidator struct {
	strict    bool
	columns   []string
	columnSet map[string]struct{}
}

func newJSONRecordValidator(columns []string, strict bool) *jsonRecordValidator {
	validator := &jsonRecordValidator{strict: strict}
	if strict {
		validator.columns = append([]string(nil), columns...)
		validator.columnSet = make(map[string]struct{}, len(columns))
		for _, column := range columns {
			validator.columnSet[column] = struct{}{}
		}
	}
	return validator
}

func (v *jsonRecordValidator) validate(record string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(record), &object); err != nil {
		return fmt.Errorf("invalid json record payload: %w", err)
	}
	if object == nil {
		return errors.New("invalid json object payload: expected JSON object")
	}
	if !v.strict {
		return nil
	}

	if len(object) != len(v.columns) {
		return fmt.Errorf("invalid json object payload: expected exactly %d columns, got %d", len(v.columns), len(object))
	}
	for _, column := range v.columns {
		if _, ok := object[column]; !ok {
			return fmt.Errorf("invalid json object payload: missing column %q", column)
		}
	}
	for key := range object {
		if _, ok := v.columnSet[key]; !ok {
			return fmt.Errorf("invalid json object payload: unexpected column %q", key)
		}
	}
	return nil
}

func (c *Client) pollLabelUntilConclusion(label string, started time.Time, attempts int) (DeliveryResult, error) {
	deadline := time.Now().Add(c.cfg.StatusPollTimeout)
	backoff := statusPollInitialBackoff
	for {
		ctx, cancel := context.WithTimeout(context.Background(), c.cfg.DorisUploadRequestTimeout)
		state, err := c.sender.PollLabel(ctx, label)
		cancel()
		if err == nil {
			switch strings.ToUpper(state.State) {
			case "VISIBLE", "COMMITTED":
				return DeliveryResult{
					Attempts:   attempts,
					StatusCode: state.StatusCode,
					Response: &StreamLoadResponse{
						Label:   label,
						Status:  "Success",
						Message: state.State,
					},
					StartedAt:  started,
					FinishedAt: time.Now(),
				}, nil
			case "ABORTED":
				// Doris aborted the transaction (e.g. connection was cut before the
				// request completed). The data was not loaded, so it is safe to retry
				// with a fresh label.
				return DeliveryResult{}, &streamLoadError{
					StatusCode: state.StatusCode,
					Message:    fmt.Sprintf("load label %s concluded as ABORTED", label),
					Retriable:  true,
					Ambiguous:  false,
				}
			case "PREPARE", "PRECOMMITTED":
			case "UNKNOWN":
				// Label not found in Doris — the transaction was never registered.
				// Data was not loaded; retry with a new label rather than polling
				// indefinitely for a state that will never change.
				return DeliveryResult{}, &streamLoadError{
					StatusCode: state.StatusCode,
					Message:    fmt.Sprintf("load label %s not found in Doris (state=UNKNOWN)", label),
					Retriable:  true,
					Ambiguous:  false,
				}
			default:
			}
		}

		if time.Now().After(deadline) {
			if err != nil {
				return DeliveryResult{}, err
			}
			return DeliveryResult{}, &streamLoadError{
				StatusCode: state.StatusCode,
				Message:    fmt.Sprintf("load label %s did not reach a terminal state before poll timeout; last state=%s", label, state.State),
				Retriable:  false,
				Ambiguous:  true,
			}
		}

		time.Sleep(backoff)
		backoff = nextBackoff(backoff, statusPollMaxBackoff)
	}
}

func nextBackoff(current, max time.Duration) time.Duration {
	if current <= 0 {
		return 0
	}
	next := current * 2
	if max > 0 && next > max {
		return max
	}
	return next
}

func generateLabel(prefix string) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	var suffix strings.Builder
	suffix.Grow(12)
	limit := big.NewInt(int64(len(letters)))
	for i := 0; i < 12; i++ {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
		}
		suffix.WriteByte(letters[n.Int64()])
	}
	return fmt.Sprintf("%s_%d_%s", prefix, time.Now().UnixNano(), suffix.String())
}
