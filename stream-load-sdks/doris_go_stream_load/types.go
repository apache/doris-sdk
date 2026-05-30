package dorisstreamload

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrClientClosed = errors.New("client is closed")
	ErrQueueFull    = errors.New("queue is full")
	ErrSendTooLarge = errors.New("send exceeds batch bytes limit")
)

type DeliveryCallback func(DeliveryResult)

type DeliveryResult struct {
	Err        error
	Attempts   int
	StatusCode int
	Response   *StreamLoadResponse
	StartedAt  time.Time
	FinishedAt time.Time
}

func (r DeliveryResult) Success() bool {
	return r.Err == nil
}

type ClientStats struct {
	StartedAt time.Time

	TotalWorkers int
	IdleWorkers  int
	BusyWorkers  int

	TotalLoadJobs int64
	ErrorJobs     int64
	ErrorRate     float64

	AverageLoadTime time.Duration
	P50LoadTime     time.Duration
	P90LoadTime     time.Duration
	P99LoadTime     time.Duration
	P999LoadTime    time.Duration

	AverageRetries float64

	TotalBytesSent   int64
	AverageLoadSize  float64
	AverageBytesRate float64

	RecordsSent        int64
	AverageRecordsRate float64

	TotalUploadAttempts int64
}

const maxLoadDurations = 10000

type clientStatsCollector struct {
	startedAt time.Time

	busyWorkers         atomic.Int64
	totalLoadJobs       atomic.Int64
	errorJobs           atomic.Int64
	totalRetries        atomic.Int64
	totalBytesSent      atomic.Int64
	totalRecordsSent    atomic.Int64
	totalUploadAttempts atomic.Int64
	totalLoadTimeNanos  atomic.Int64

	mu            sync.Mutex
	loadDurations []time.Duration
	durIdx        int
}

func newClientStatsCollector(startedAt time.Time) *clientStatsCollector {
	return &clientStatsCollector{
		startedAt:     startedAt,
		loadDurations: make([]time.Duration, 0, maxLoadDurations),
	}
}

func (s *clientStatsCollector) changeBusyWorkers(delta int64) {
	s.busyWorkers.Add(delta)
}

func (s *clientStatsCollector) recordUploadAttempt(bytes int, records int) {
	s.totalUploadAttempts.Add(1)
	s.totalBytesSent.Add(int64(bytes))
	s.totalRecordsSent.Add(int64(records))
}

func (s *clientStatsCollector) recordCompletion(result DeliveryResult) {
	s.totalLoadJobs.Add(1)
	if result.Err != nil {
		s.errorJobs.Add(1)
	}
	if result.Attempts > 1 {
		s.totalRetries.Add(int64(result.Attempts - 1))
	}
	if !result.StartedAt.IsZero() && !result.FinishedAt.IsZero() && !result.FinishedAt.Before(result.StartedAt) {
		duration := result.FinishedAt.Sub(result.StartedAt)
		s.totalLoadTimeNanos.Add(duration.Nanoseconds())
		s.mu.Lock()
		if len(s.loadDurations) < maxLoadDurations {
			s.loadDurations = append(s.loadDurations, duration)
		} else {
			s.loadDurations[s.durIdx] = duration
			s.durIdx = (s.durIdx + 1) % maxLoadDurations
		}
		s.mu.Unlock()
	}
}

func (s *clientStatsCollector) snapshot(totalWorkers int) ClientStats {
	totalJobs := s.totalLoadJobs.Load()
	errorJobs := s.errorJobs.Load()
	busyWorkers := int(s.busyWorkers.Load())
	if busyWorkers < 0 {
		busyWorkers = 0
	}
	if busyWorkers > totalWorkers {
		busyWorkers = totalWorkers
	}

	elapsed := time.Since(s.startedAt).Seconds()
	if elapsed <= 0 {
		elapsed = 1
	}

	stats := ClientStats{
		StartedAt:           s.startedAt,
		TotalWorkers:        totalWorkers,
		BusyWorkers:         busyWorkers,
		IdleWorkers:         totalWorkers - busyWorkers,
		TotalLoadJobs:       totalJobs,
		ErrorJobs:           errorJobs,
		AverageRetries:      0,
		TotalBytesSent:      s.totalBytesSent.Load(),
		AverageBytesRate:    float64(s.totalBytesSent.Load()) / elapsed,
		RecordsSent:         s.totalRecordsSent.Load(),
		AverageRecordsRate:  float64(s.totalRecordsSent.Load()) / elapsed,
		TotalUploadAttempts: s.totalUploadAttempts.Load(),
	}
	if stats.TotalUploadAttempts > 0 {
		stats.AverageLoadSize = float64(stats.TotalBytesSent) / float64(stats.TotalUploadAttempts)
	}
	if totalJobs > 0 {
		stats.ErrorRate = float64(errorJobs) / float64(totalJobs)
		stats.AverageRetries = float64(s.totalRetries.Load()) / float64(totalJobs)
		stats.AverageLoadTime = time.Duration(s.totalLoadTimeNanos.Load() / totalJobs)
	}

	s.mu.Lock()
	if len(s.loadDurations) > 0 {
		durations := append([]time.Duration(nil), s.loadDurations...)
		s.mu.Unlock()
		slices.Sort(durations)
		stats.P50LoadTime = percentileDuration(durations, 0.50)
		stats.P90LoadTime = percentileDuration(durations, 0.90)
		stats.P99LoadTime = percentileDuration(durations, 0.99)
		stats.P999LoadTime = percentileDuration(durations, 0.999)
	} else {
		s.mu.Unlock()
	}

	return stats
}

func percentileDuration(values []time.Duration, quantile float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	if quantile <= 0 {
		return values[0]
	}
	if quantile >= 1 {
		return values[len(values)-1]
	}
	index := int(float64(len(values)-1) * quantile)
	if index < 0 {
		index = 0
	}
	if index >= len(values) {
		index = len(values) - 1
	}
	return values[index]
}

type batchCompletion struct {
	mu     sync.Mutex
	done   chan struct{}
	result DeliveryResult
	set    bool
}

func newBatchCompletion() *batchCompletion {
	return &batchCompletion{done: make(chan struct{})}
}

func (c *batchCompletion) complete(result DeliveryResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.set {
		return
	}
	c.result = result
	c.set = true
	close(c.done)
}

type Handle struct {
	mu sync.Mutex

	assigned chan struct{}

	completion *batchCompletion
	done       chan struct{}
	bridged    bool
}

func newHandle() *Handle {
	return &Handle{assigned: make(chan struct{})}
}

func (h *Handle) Done() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.done == nil {
		h.done = make(chan struct{})
		h.startDoneBridgeLocked()
	}
	return h.done
}

func (h *Handle) IsDone() bool {
	select {
	case <-h.assigned:
	default:
		return false
	}

	select {
	case <-h.completion.done:
		return true
	default:
		return false
	}
}

func (h *Handle) Result() (DeliveryResult, bool) {
	select {
	case <-h.assigned:
	default:
		return DeliveryResult{}, false
	}

	h.mu.Lock()
	completion := h.completion
	h.mu.Unlock()
	select {
	case <-completion.done:
		return completion.result, true
	default:
		return DeliveryResult{}, false
	}
}

func (h *Handle) Wait() DeliveryResult {
	<-h.assigned
	h.mu.Lock()
	completion := h.completion
	h.mu.Unlock()
	<-completion.done
	return completion.result
}

func (h *Handle) WaitContext(ctx context.Context) (DeliveryResult, error) {
	select {
	case <-h.assigned:
	case <-ctx.Done():
		return DeliveryResult{}, ctx.Err()
	}

	h.mu.Lock()
	completion := h.completion
	h.mu.Unlock()
	select {
	case <-completion.done:
		return completion.result, nil
	case <-ctx.Done():
		select {
		case <-completion.done:
			return completion.result, nil
		default:
			return DeliveryResult{}, ctx.Err()
		}
	}
}

func (h *Handle) attach(completion *batchCompletion) {
	h.mu.Lock()
	if h.completion != nil {
		h.mu.Unlock()
		return
	}
	h.completion = completion
	assigned := h.assigned
	h.startDoneBridgeLocked()
	h.mu.Unlock()
	close(assigned)
}

func (h *Handle) startDoneBridgeLocked() {
	if h.done == nil || h.bridged {
		return
	}
	done := h.done
	assigned := h.assigned
	h.bridged = true
	go func() {
		<-assigned
		h.mu.Lock()
		completion := h.completion
		h.mu.Unlock()
		<-completion.done
		close(done)
	}()
}

type SendOption func(*sendOptions)

type sendOptions struct {
	callback DeliveryCallback
}

func WithCallback(callback DeliveryCallback) SendOption {
	return func(opts *sendOptions) {
		opts.callback = callback
	}
}
