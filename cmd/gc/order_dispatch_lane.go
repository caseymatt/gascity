package main

import (
	"context"
	"sync"
)

type orderDispatchRequest struct {
	ctx      context.Context
	cityRoot string
}

// orderDispatchLane keeps slow order evaluation off the serialized controller
// tick. It owns only scheduling: the dispatcher continues to own store handles,
// in-flight actions, and their drain barrier.
type orderDispatchLane struct {
	mu sync.Mutex

	owner   *CityRuntime
	pending *orderDispatchRequest
	active  bool
	paused  bool
	stopped bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func newOrderDispatchLane(owner *CityRuntime) *orderDispatchLane {
	return &orderDispatchLane{owner: owner}
}

func (l *orderDispatchLane) request(ctx context.Context, cityRoot string) {
	if l == nil || l.owner == nil || ctx.Err() != nil {
		return
	}
	l.mu.Lock()
	if l.paused || l.stopped {
		l.mu.Unlock()
		return
	}
	request := &orderDispatchRequest{ctx: ctx, cityRoot: cityRoot}
	l.pending = request
	if l.active {
		l.mu.Unlock()
		return
	}
	l.active = true
	l.done = make(chan struct{})
	l.mu.Unlock()

	go l.run()
}

func (l *orderDispatchLane) run() {
	for {
		l.mu.Lock()
		request := l.pending
		l.pending = nil
		if request == nil || l.paused || l.stopped {
			l.active = false
			l.cancel = nil
			close(l.done)
			l.mu.Unlock()
			return
		}
		runCtx, cancel := context.WithCancel(request.ctx)
		l.cancel = cancel
		l.mu.Unlock()

		l.owner.safeTick(func() {
			l.owner.dispatchOrders(runCtx, request.cityRoot)
		}, "order-dispatch-lane")
		cancel()

		l.mu.Lock()
		l.cancel = nil
		l.mu.Unlock()
	}
}

// pauseAndWait prevents a stale dispatcher pass from racing config reload.
// resume must be called after every successful pause, including a timed-out one.
func (l *orderDispatchLane) pauseAndWait(ctx context.Context) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	l.paused = true
	l.pending = nil
	if l.cancel != nil {
		l.cancel()
	}
	if !l.active {
		l.mu.Unlock()
		return true
	}
	done := l.done
	l.mu.Unlock()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (l *orderDispatchLane) resume() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.stopped {
		l.paused = false
	}
	l.mu.Unlock()
}

func (l *orderDispatchLane) stopAndWait(ctx context.Context) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	l.stopped = true
	l.paused = true
	l.pending = nil
	if l.cancel != nil {
		l.cancel()
	}
	if !l.active {
		l.mu.Unlock()
		return true
	}
	done := l.done
	l.mu.Unlock()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
