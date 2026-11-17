// Package signals handles signals that may arrive any number of times.
package signals

import (
	"context"
	"os"
	"os/signal"
	"sync"
)

// OnEach calls handle every time sig arrives, until ctx is done. sig is
// caught from the moment OnEach returns; the goroutine watching it is tracked
// by handlers.
func OnEach(ctx context.Context, handlers *sync.WaitGroup, sig os.Signal, handle func()) {
	arrived, stop := signal.NotifyContext(ctx, sig)
	handlers.Go(func() {
		for {
			<-arrived.Done()
			if ctx.Err() != nil {
				stop()
				return
			}
			// The next arrival is caught before this one is handled and before
			// its registration is dropped, so the signal is never left to its
			// default action in between.
			next, stopNext := signal.NotifyContext(ctx, sig)
			stop()
			handle()
			arrived, stop = next, stopNext
		}
	})
}
