package dispatch

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestProcessControlSerializesMutationsPerWorkflowRoot(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	store := &blockingControlLeaseStore{
		MemStore:        beads.NewMemStore(),
		leaseAcquired:   make(chan struct{}),
		continueAcquire: make(chan struct{}),
	}
	root, control := createCanceledLeaseTestControl(t, store)
	otherControl := createLeaseTestControlForRoot(t, store, root.ID)

	firstDone := make(chan error, 1)
	go func() {
		result, err := ProcessControl(store, control, ProcessOptions{
			controlLeaseNow:   func() time.Time { return now },
			controlLeaseOwner: func() (string, error) { return "controller-first", nil },
		})
		if err == nil && (!result.Processed || result.Action != "canceled-workflow") {
			err = errors.New("first ProcessControl did not execute the canceled-workflow transition")
		}
		firstDone <- err
	}()

	select {
	case <-store.leaseAcquired:
	case <-time.After(2 * time.Second):
		t.Fatal("first ProcessControl did not acquire the execution lease")
	}

	for _, contender := range []struct {
		name    string
		control beads.Bead
		owner   string
	}{
		{name: "same control", control: control, owner: "controller-second"},
		{name: "different control in same root", control: otherControl, owner: "controller-third"},
	} {
		result, err := ProcessControl(store, contender.control, ProcessOptions{
			controlLeaseNow:   func() time.Time { return now },
			controlLeaseOwner: func() (string, error) { return contender.owner, nil },
		})
		if !errors.Is(err, ErrControlPending) {
			t.Fatalf("%s contention error = %v, want %v", contender.name, err, ErrControlPending)
		}
		if result != (ControlResult{}) {
			t.Fatalf("%s contention result = %+v, want no processing", contender.name, result)
		}
	}
	if got := store.transitionCalls.Load(); got != 0 {
		t.Fatalf("control transitions before holder continued = %d, want 0", got)
	}

	close(store.continueAcquire)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first ProcessControl: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first ProcessControl did not finish after acquisition continued")
	}
	if got := store.transitionCalls.Load(); got != 1 {
		t.Fatalf("control transition count = %d, want exactly 1", got)
	}
	if got := mustGetBead(t, store, otherControl.ID).Status; got != "open" {
		t.Fatalf("different control status = %q, want open after root contention", got)
	}
	rootAfter := mustGetBead(t, store.MemStore, root.ID)
	if got := rootAfter.Metadata[controlExecutionLeaseMetadataKey]; got != "" {
		t.Fatalf("released root lease = %q, want empty", got)
	}
}

func TestProcessControlReclaimsExpiredRootLease(t *testing.T) {
	now := time.Date(2026, 8, 21, 13, 0, 0, 0, time.UTC)
	store := beads.NewMemStore()
	root, control := createCanceledLeaseTestControl(t, store)
	expired, err := encodeControlExecutionLease(controlExecutionLeaseValue{
		Owner:     "dead-controller",
		ExpiresAt: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("encode expired lease: %v", err)
	}
	if err := store.SetMetadata(root.ID, controlExecutionLeaseMetadataKey, expired); err != nil {
		t.Fatalf("seed expired root lease: %v", err)
	}
	control = mustGetBead(t, store, control.ID)

	result, err := ProcessControl(store, control, ProcessOptions{
		controlLeaseNow:   func() time.Time { return now },
		controlLeaseOwner: func() (string, error) { return "replacement-controller", nil },
	})
	if err != nil {
		t.Fatalf("ProcessControl with expired lease: %v", err)
	}
	if !result.Processed || result.Action != "canceled-workflow" {
		t.Fatalf("result = %+v, want canceled-workflow", result)
	}
	rootAfter := mustGetBead(t, store, root.ID)
	if got := rootAfter.Metadata[controlExecutionLeaseMetadataKey]; got != "" {
		t.Fatalf("released reclaimed lease = %q, want empty", got)
	}
}

func TestProcessControlLiveRootLeaseLeavesTransitionPending(t *testing.T) {
	now := time.Date(2026, 8, 21, 14, 0, 0, 0, time.UTC)
	store := &countingControlCloseStore{MemStore: beads.NewMemStore()}
	root, control := createCanceledLeaseTestControl(t, store)
	live, err := encodeControlExecutionLease(controlExecutionLeaseValue{
		Owner:     "live-controller",
		ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("encode live lease: %v", err)
	}
	if err := store.SetMetadata(root.ID, controlExecutionLeaseMetadataKey, live); err != nil {
		t.Fatalf("seed live root lease: %v", err)
	}
	control = mustGetBead(t, store, control.ID)

	result, err := ProcessControl(store, control, ProcessOptions{
		controlLeaseNow:   func() time.Time { return now },
		controlLeaseOwner: func() (string, error) { return "contending-controller", nil },
	})
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl error = %v, want %v", err, ErrControlPending)
	}
	if result != (ControlResult{}) {
		t.Fatalf("result = %+v, want no processing", result)
	}
	if got := store.transitionCalls.Load(); got != 0 {
		t.Fatalf("control transition count = %d, want 0", got)
	}
	controlAfter := mustGetBead(t, store, control.ID)
	if controlAfter.Status != "open" {
		t.Fatalf("control status = %q, want open", controlAfter.Status)
	}
	rootAfter := mustGetBead(t, store, root.ID)
	if got := rootAfter.Metadata[controlExecutionLeaseMetadataKey]; got != live {
		t.Fatalf("live root lease changed to %q, want exact original %q", got, live)
	}
}

func TestProcessControlReleaseCannotClearSuccessor(t *testing.T) {
	now := time.Date(2026, 8, 21, 15, 0, 0, 0, time.UTC)
	successor, err := encodeControlExecutionLease(controlExecutionLeaseValue{
		Owner:     "successor-controller",
		ExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("encode successor lease: %v", err)
	}
	store := &successorOnControlLeaseReleaseStore{
		MemStore:  beads.NewMemStore(),
		successor: successor,
	}
	root, control := createCanceledLeaseTestControl(t, store)

	result, err := ProcessControl(store, control, ProcessOptions{
		controlLeaseNow:   func() time.Time { return now },
		controlLeaseOwner: func() (string, error) { return "finishing-controller", nil },
	})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if !result.Processed || result.Action != "canceled-workflow" {
		t.Fatalf("result = %+v, want canceled-workflow", result)
	}
	rootAfter := mustGetBead(t, store, root.ID)
	if got := rootAfter.Metadata[controlExecutionLeaseMetadataKey]; got != successor {
		t.Fatalf("root lease after predecessor release = %q, want successor %q", got, successor)
	}
}

func TestProcessControlRenewsRootLeaseDuringLongTransition(t *testing.T) {
	initialNow := time.Date(2026, 8, 21, 16, 0, 0, 0, time.UTC)
	var nowNanos atomic.Int64
	nowNanos.Store(initialNow.UnixNano())
	ticks := make(chan time.Time)
	store := &renewingControlLeaseStore{
		MemStore:           beads.NewMemStore(),
		transitionStarted:  make(chan struct{}),
		continueTransition: make(chan struct{}),
		renewed:            make(chan struct{}),
	}
	root, control := createCanceledLeaseTestControl(t, store)
	store.rootID = root.ID

	done := make(chan error, 1)
	go func() {
		_, err := ProcessControl(store, control, ProcessOptions{
			controlLeaseNow: func() time.Time {
				return time.Unix(0, nowNanos.Load()).UTC()
			},
			controlLeaseOwner: func() (string, error) { return "renewing-controller", nil },
			controlLeaseRenew: ticks,
		})
		done <- err
	}()

	select {
	case <-store.transitionStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("ProcessControl did not enter the held transition")
	}
	advancedNow := initialNow.Add(controlExecutionLeaseDuration / 2)
	nowNanos.Store(advancedNow.UnixNano())
	ticks <- advancedNow
	select {
	case <-store.renewed:
	case <-time.After(2 * time.Second):
		t.Fatal("execution lease was not renewed while transition was held")
	}

	rootDuring := mustGetBead(t, store.MemStore, root.ID)
	lease, err := decodeControlExecutionLease(rootDuring.Metadata[controlExecutionLeaseMetadataKey])
	if err != nil {
		t.Fatalf("decode renewed lease: %v", err)
	}
	if want := advancedNow.Add(controlExecutionLeaseDuration); !lease.ExpiresAt.Equal(want) {
		t.Fatalf("renewed expiration = %s, want %s", lease.ExpiresAt, want)
	}

	close(store.continueTransition)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ProcessControl after renewal: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ProcessControl did not finish after transition continued")
	}
}

func TestProcessControlFailsClosedWithoutMetadataCAS(t *testing.T) {
	t.Run("unsupported capability", func(t *testing.T) {
		backing := beads.NewMemStore()
		_, control := createCanceledLeaseTestControl(t, backing)
		store := storeWithoutControlLeaseCAS{Store: backing}

		result, err := ProcessControl(store, control, ProcessOptions{})
		if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
			t.Fatalf("ProcessControl error = %v, want %v", err, beads.ErrConditionalWriteUnsupported)
		}
		if result != (ControlResult{}) {
			t.Fatalf("result = %+v, want no processing", result)
		}
		if got := mustGetBead(t, backing, control.ID).Status; got != "open" {
			t.Fatalf("control status = %q, want open", got)
		}
	})

	t.Run("CAS transport error", func(t *testing.T) {
		sentinel := errors.New("ambiguous CAS transport failure")
		store := &failingControlLeaseCASStore{MemStore: beads.NewMemStore(), err: sentinel}
		_, control := createCanceledLeaseTestControl(t, store)

		result, err := ProcessControl(store, control, ProcessOptions{})
		if !errors.Is(err, sentinel) {
			t.Fatalf("ProcessControl error = %v, want %v", err, sentinel)
		}
		if result != (ControlResult{}) {
			t.Fatalf("result = %+v, want no processing", result)
		}
		if got := mustGetBead(t, store, control.ID).Status; got != "open" {
			t.Fatalf("control status = %q, want open", got)
		}
	})
}

func TestProcessControlFailsClosedWhenWorkflowRootIsMissing(t *testing.T) {
	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{
		Title:  "control with missing root",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindRetry,
			beadmeta.RootBeadIDMetadataKey:   "missing-root",
			beadmeta.RootStoreRefMetadataKey: "rig:test",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}

	result, err := ProcessControl(store, control, ProcessOptions{})
	if !errors.Is(err, ErrControlGraphMalformed) {
		t.Fatalf("ProcessControl error = %v, want %v", err, ErrControlGraphMalformed)
	}
	if result != (ControlResult{}) {
		t.Fatalf("result = %+v, want no processing", result)
	}
	if got := mustGetBead(t, store, control.ID).Status; got != "open" {
		t.Fatalf("control status = %q, want open", got)
	}
}

func createCanceledLeaseTestControl(t *testing.T, store beads.Store) (beads.Bead, beads.Bead) {
	t.Helper()
	root, err := store.Create(beads.Bead{
		Title:  "lease test workflow",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.CancelRequestedMetadataKey: "2026-08-21T00:00:00Z",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	control := createLeaseTestControlForRoot(t, store, root.ID)
	return root, control
}

func createLeaseTestControlForRoot(t *testing.T, store beads.Store, rootID string) beads.Bead {
	t.Helper()
	control, err := store.Create(beads.Bead{
		Title:  "lease test control",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindRetry,
			beadmeta.RootBeadIDMetadataKey:   rootID,
			beadmeta.RootStoreRefMetadataKey: "rig:test",
		},
	})
	if err != nil {
		t.Fatalf("create control: %v", err)
	}
	return control
}

type blockingControlLeaseStore struct {
	*beads.MemStore
	leaseAcquired   chan struct{}
	continueAcquire chan struct{}
	acquireOnce     sync.Once
	transitionCalls atomic.Int64
}

func (s *blockingControlLeaseStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	swapped, err := s.MemStore.CompareAndSetMetadataKey(id, key, expected, next)
	if err == nil && swapped && key == controlExecutionLeaseMetadataKey && expected == "" && next != "" {
		s.acquireOnce.Do(func() {
			close(s.leaseAcquired)
			<-s.continueAcquire
		})
	}
	return swapped, err
}

func (s *blockingControlLeaseStore) Update(id string, opts beads.UpdateOpts) error {
	if opts.Status != nil {
		s.transitionCalls.Add(1)
	}
	return s.MemStore.Update(id, opts)
}

type countingControlCloseStore struct {
	*beads.MemStore
	transitionCalls atomic.Int64
}

func (s *countingControlCloseStore) Update(id string, opts beads.UpdateOpts) error {
	if opts.Status != nil {
		s.transitionCalls.Add(1)
	}
	return s.MemStore.Update(id, opts)
}

type successorOnControlLeaseReleaseStore struct {
	*beads.MemStore
	successor string
	once      sync.Once
}

func (s *successorOnControlLeaseReleaseStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if key == controlExecutionLeaseMetadataKey && expected != "" && next == "" {
		var injectErr error
		s.once.Do(func() {
			var swapped bool
			swapped, injectErr = s.MemStore.CompareAndSetMetadataKey(id, key, expected, s.successor)
			if injectErr == nil && !swapped {
				injectErr = errors.New("could not install successor execution lease")
			}
		})
		if injectErr != nil {
			return false, injectErr
		}
	}
	return s.MemStore.CompareAndSetMetadataKey(id, key, expected, next)
}

type renewingControlLeaseStore struct {
	*beads.MemStore
	rootID             string
	rootGets           atomic.Int64
	transitionStarted  chan struct{}
	continueTransition chan struct{}
	renewed            chan struct{}
	transitionOnce     sync.Once
	renewOnce          sync.Once
}

func (s *renewingControlLeaseStore) Get(id string) (beads.Bead, error) {
	bead, err := s.MemStore.Get(id)
	if err == nil && id == s.rootID && s.rootGets.Add(1) == 2 {
		s.transitionOnce.Do(func() {
			close(s.transitionStarted)
			<-s.continueTransition
		})
	}
	return bead, err
}

func (s *renewingControlLeaseStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	swapped, err := s.MemStore.CompareAndSetMetadataKey(id, key, expected, next)
	if err == nil && swapped && key == controlExecutionLeaseMetadataKey && expected != "" && next != "" {
		s.renewOnce.Do(func() { close(s.renewed) })
	}
	return swapped, err
}

type storeWithoutControlLeaseCAS struct {
	beads.Store
}

type failingControlLeaseCASStore struct {
	*beads.MemStore
	err error
}

func (s *failingControlLeaseCASStore) CompareAndSetMetadataKey(_, key, _, next string) (bool, error) {
	if key == controlExecutionLeaseMetadataKey && next != "" {
		return false, s.err
	}
	return false, errors.New("unexpected metadata CAS")
}

// Existing behavioral test stores intentionally wrap a MemStore to inject an
// unrelated failure or observation. Preserve the backing CAS capability so the
// root lease reaches the behavior each wrapper is testing.
func (s *createObservingStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s *failOnceCreateStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s closeErrorStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s statusUpdateNoopCloseStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s *scopeSnapshotQueryGuardStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}
