package main

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func orderLaneTestRuntime(od orderDispatcher) *CityRuntime {
	now := time.Now()
	return &CityRuntime{
		od:                                 od,
		orderSweepWatchdogLast:             now,
		orderTrackingRetentionWatchdogLast: now,
		nudgeMailSweepWatchdogLast:         now,
		wispIndexMigrationApplied:          true,
		stderr:                             io.Discard,
	}
}

func TestOrderDispatchLaneCoalescesPendingTicks(t *testing.T) {
	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	secondDone := make(chan struct{})
	var calls atomic.Int32
	od := &recordingOrderDispatcher{
		onDispatch: func(context.Context, string, time.Time) {
			switch calls.Add(1) {
			case 1:
				close(firstStarted)
				<-firstRelease
			case 2:
				close(secondDone)
			}
		},
	}
	cr := orderLaneTestRuntime(od)
	lane := cr.orderDispatchLaneOf()
	lane.request(context.Background(), "/city")
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first order pass did not start")
	}
	for range 10 {
		lane.request(context.Background(), "/city")
	}
	close(firstRelease)
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("coalesced order pass did not run")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !lane.pauseAndWait(waitCtx) {
		t.Fatal("order lane did not become idle")
	}
	lane.resume()
	if got := calls.Load(); got != 2 {
		t.Fatalf("order passes = %d, want 2 for one active and ten coalesced requests", got)
	}
}

func TestOrderDispatchLanePauseCancelsActiveEvaluation(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	od := &recordingOrderDispatcher{
		onDispatch: func(ctx context.Context, _ string, _ time.Time) {
			close(started)
			<-ctx.Done()
			close(canceled)
		},
	}
	cr := orderLaneTestRuntime(od)
	lane := cr.orderDispatchLaneOf()
	lane.request(context.Background(), "/city")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("order pass did not start")
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !lane.pauseAndWait(waitCtx) {
		t.Fatal("paused order lane did not cancel and drain its active evaluation")
	}
	lane.resume()
	select {
	case <-canceled:
	default:
		t.Fatal("order evaluation did not observe cancellation")
	}
}

func TestOrderDispatchLaneCancelsDispatcherInstalledAfterTimedOutStop(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	fresh := &recordingOrderDispatcher{}
	var cr *CityRuntime
	old := &recordingOrderDispatcher{
		onDispatch: func(context.Context, string, time.Time) {
			close(started)
			<-release
			cr.replaceOrderDispatcher(fresh)
		},
	}
	cr = orderLaneTestRuntime(old)
	lane := cr.orderDispatchLaneOf()
	lane.request(context.Background(), "/city")
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("order pass did not start")
	}

	expired, cancelExpired := context.WithTimeout(context.Background(), time.Millisecond)
	if lane.stopAndWait(expired) {
		cancelExpired()
		t.Fatal("stop unexpectedly drained a context-unaware order scan")
	}
	cancelExpired()
	cr.stopOrderDispatchers()

	close(release)
	waitCtx, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if !lane.stopAndWait(waitCtx) {
		t.Fatal("order lane did not finish after releasing the scan")
	}
	if got := old.cancelCalls.Load(); got != 1 {
		t.Fatalf("retired dispatcher cancel calls = %d, want 1", got)
	}
	if got := fresh.cancelCalls.Load(); got != 1 {
		t.Fatalf("late replacement cancel calls = %d, want 1", got)
	}
}
