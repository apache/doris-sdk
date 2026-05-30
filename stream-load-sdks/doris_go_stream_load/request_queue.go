package dorisstreamload

import (
	"context"
	"sync"
	"time"
)

type requestQueue struct {
	maxRequests int
	items       chan *queuedSubmission
	slots       chan struct{}
	closeC      chan struct{}

	mu      sync.Mutex
	closed  bool
	pending *queuedSubmission
}

func newRequestQueue(maxRequests int) *requestQueue {
	return &requestQueue{
		maxRequests: maxRequests,
		items:       make(chan *queuedSubmission, maxRequests),
		slots:       make(chan struct{}, maxRequests),
		closeC:      make(chan struct{}),
	}
}

func (q *requestQueue) Enqueue(ctx context.Context, submission *queuedSubmission) error {
	if submission == nil || len(submission.items) == 0 {
		return nil
	}
	q.mu.Lock()
	closed := q.closed
	q.mu.Unlock()
	if closed {
		return ErrClientClosed
	}

	select {
	case q.slots <- struct{}{}:
	case <-q.closeC:
		return ErrClientClosed
	case <-ctx.Done():
		return ErrQueueFull
	}

	// Hold the mutex to make "check closed" and "write to items" atomic with
	// respect to Close(). The write is non-blocking because acquiring a slot
	// above guarantees buffer space in q.items.
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		<-q.slots
		return ErrClientClosed
	}
	q.items <- submission
	q.mu.Unlock()
	return nil
}

func (q *requestQueue) DequeueBatch(maxBytes int) ([]*queuedSubmission, int, bool) {
	first, ok := q.waitForItem()
	if !ok {
		return nil, 0, false
	}
	submissions, bytes := q.collectBatch(first, maxBytes)
	return submissions, bytes, true
}

func (q *requestQueue) DequeueBatchWait(maxBytes int, timeout time.Duration) ([]*queuedSubmission, int, bool, bool) {
	if submission, ok := q.tryPendingOrImmediate(); ok {
		submissions, bytes := q.collectBatch(submission, maxBytes)
		return submissions, bytes, true, false
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case submission := <-q.items:
		submissions, bytes := q.collectBatch(submission, maxBytes)
		return submissions, bytes, true, false
	case <-q.closeC:
		if submission, ok := q.tryPendingOrImmediate(); ok {
			submissions, bytes := q.collectBatch(submission, maxBytes)
			return submissions, bytes, true, false
		}
		return nil, 0, false, false
	case <-timer.C:
		return nil, 0, true, true
	}
}

func (q *requestQueue) Close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	close(q.closeC)
	q.mu.Unlock()
}

func (q *requestQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.items)
	if q.pending != nil {
		n++
	}
	return n
}

func (q *requestQueue) waitForItem() (*queuedSubmission, bool) {
	if submission, ok := q.tryPendingOrImmediate(); ok {
		return submission, true
	}

	for {
		select {
		case submission := <-q.items:
			return submission, true
		case <-q.closeC:
			if submission, ok := q.tryPendingOrImmediate(); ok {
				return submission, true
			}
			return nil, false
		}
	}
}

func (q *requestQueue) tryPendingOrImmediate() (*queuedSubmission, bool) {
	q.mu.Lock()
	if q.pending != nil {
		submission := q.pending
		q.pending = nil
		q.mu.Unlock()
		return submission, true
	}
	q.mu.Unlock()

	select {
	case submission := <-q.items:
		return submission, true
	default:
		return nil, false
	}
}

func (q *requestQueue) collectBatch(first *queuedSubmission, maxBytes int) ([]*queuedSubmission, int) {
	submissions := []*queuedSubmission{first}
	bytes := first.standaloneByteSize

	if maxBytes > 0 && bytes >= maxBytes {
		<-q.slots
		return submissions, bytes
	}

	for {
		var next *queuedSubmission
		select {
		case next = <-q.items:
		default:
			for range submissions {
				<-q.slots
			}
			return submissions, bytes
		}

		if maxBytes > 0 && bytes+next.appendByteSize > maxBytes {
			q.mu.Lock()
			q.pending = next
			q.mu.Unlock()
			for range submissions {
				<-q.slots
			}
			return submissions, bytes
		}

		submissions = append(submissions, next)
		bytes += next.appendByteSize
	}
}
