package wrap

import (
	"sync"

	"github.com/110y/muxac/internal/status"
)

// statusQueue hands status changes from the goroutine that reads the program's
// output to the goroutine that records them. Pushing never blocks, so a slow
// database can never stall the program's output, and every change is
// delivered in order.
type statusQueue struct {
	mu      sync.Mutex
	pending []status.Status
	closed  bool
	// wake is signaled whenever pending or closed changes.
	wake chan struct{}
}

func newStatusQueue() *statusQueue {
	return &statusQueue{wake: make(chan struct{}, 1)}
}

// push queues st. It reports false if the queue is already closed.
func (q *statusQueue) push(st status.Status) bool {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return false
	}
	q.pending = append(q.pending, st)
	q.mu.Unlock()
	q.signal()
	return true
}

// close stops accepting statuses. run returns once it has delivered every
// status pushed before close.
func (q *statusQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

// run calls fn with every queued status, in order, until the queue is closed
// and drained.
func (q *statusQueue) run(fn func(status.Status)) {
	for range q.wake {
		for {
			q.mu.Lock()
			batch := q.pending
			q.pending = nil
			closed := q.closed
			q.mu.Unlock()

			for _, st := range batch {
				fn(st)
			}
			if len(batch) > 0 {
				continue
			}
			if closed {
				return
			}
			break
		}
	}
}

func (q *statusQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}
