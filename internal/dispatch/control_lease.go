package dispatch

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

const (
	controlExecutionLeaseMetadataKey = "gc.control_execution_lease"
	controlExecutionLeaseDuration    = 2 * time.Minute
	controlExecutionLeaseRenewEvery  = controlExecutionLeaseDuration / 3
)

type controlExecutionLeaseValue struct {
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expires_at"`
}

// controlExecutionLease owns one exact metadata value on a workflow root. The
// exact value is the fence: renewal and release may replace only the value this
// invocation most recently installed.
type controlExecutionLease struct {
	store    beads.Store
	rootID   string
	owner    string
	now      func() time.Time
	renew    <-chan time.Time
	cancel   func()
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	mu       sync.Mutex
	value    string
	renewErr error
}

func controlExecutionRoot(store beads.Store, control beads.Bead) (beads.Bead, error) {
	rootID := strings.TrimSpace(control.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" {
		return beads.Bead{}, fmt.Errorf("%s: missing %s: %w", control.ID, beadmeta.RootBeadIDMetadataKey, ErrControlGraphMalformed)
	}
	root, err := store.Get(rootID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, fmt.Errorf("%s: workflow root %s not found: %w", control.ID, rootID, ErrControlGraphMalformed)
		}
		return beads.Bead{}, fmt.Errorf("%s: loading workflow root %s for execution lease: %w", control.ID, rootID, err)
	}
	return root, nil
}

func acquireControlExecutionLease(store beads.Store, root beads.Bead, opts ProcessOptions, cancel func()) (*controlExecutionLease, bool, error) {
	now := time.Now
	if opts.controlLeaseNow != nil {
		now = opts.controlLeaseNow
	}
	ownerFactory := newControlExecutionLeaseOwner
	if opts.controlLeaseOwner != nil {
		ownerFactory = opts.controlLeaseOwner
	}
	owner, err := ownerFactory()
	if err != nil {
		return nil, false, fmt.Errorf("creating lease owner: %w", err)
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, false, fmt.Errorf("creating lease owner: empty owner")
	}

	observed := root.Metadata[controlExecutionLeaseMetadataKey]
	observedAt := now().UTC()
	if observed != "" {
		current, err := decodeControlExecutionLease(observed)
		if err != nil {
			return nil, false, fmt.Errorf("root %s has invalid execution lease: %w", root.ID, err)
		}
		if current.ExpiresAt.After(observedAt) {
			return nil, false, nil
		}
	}

	next, err := encodeControlExecutionLease(controlExecutionLeaseValue{
		Owner:     owner,
		ExpiresAt: observedAt.Add(controlExecutionLeaseDuration),
	})
	if err != nil {
		return nil, false, err
	}
	outcome, err := beads.ApplyMetadataCAS(store, root.ID, controlExecutionLeaseMetadataKey, observed, next)
	if err != nil {
		return nil, false, err
	}
	// Only the caller whose CAS actually swapped becomes the executor. Treat an
	// already-next classification as contention too: independent invocations
	// must never share ownership even if a faulty owner seam collides.
	if outcome != beads.MetadataCASSwapped {
		return nil, false, nil
	}

	lease := &controlExecutionLease{
		store:  store,
		rootID: root.ID,
		owner:  owner,
		now:    now,
		renew:  opts.controlLeaseRenew,
		cancel: cancel,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		value:  next,
	}
	go lease.renewLoop()
	return lease, true, nil
}

func (l *controlExecutionLease) renewLoop() {
	defer close(l.done)
	if l.renew != nil {
		for {
			select {
			case <-l.stop:
				return
			case _, ok := <-l.renew:
				if !ok || !l.renewOnce() {
					return
				}
			}
		}
	}

	ticker := time.NewTicker(controlExecutionLeaseRenewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			if !l.renewOnce() {
				return
			}
		}
	}
}

func (l *controlExecutionLease) renewOnce() bool {
	l.mu.Lock()
	expected := l.value
	l.mu.Unlock()

	next, err := encodeControlExecutionLease(controlExecutionLeaseValue{
		Owner:     l.owner,
		ExpiresAt: l.now().UTC().Add(controlExecutionLeaseDuration),
	})
	if err != nil {
		l.failRenewal(err)
		return false
	}
	outcome, err := beads.ApplyMetadataCAS(l.store, l.rootID, controlExecutionLeaseMetadataKey, expected, next)
	if err != nil {
		l.failRenewal(fmt.Errorf("renewing root %s execution lease: %w", l.rootID, err))
		return false
	}
	switch outcome {
	case beads.MetadataCASSwapped, beads.MetadataCASAlreadyNext:
		l.mu.Lock()
		l.value = next
		l.mu.Unlock()
		return true
	default:
		l.failRenewal(fmt.Errorf("workflow root %s execution lease was replaced: %w", l.rootID, ErrControlPending))
		return false
	}
}

func (l *controlExecutionLease) failRenewal(err error) {
	l.mu.Lock()
	if l.renewErr == nil {
		l.renewErr = err
	}
	l.mu.Unlock()
	if l.cancel != nil {
		l.cancel()
	}
}

func (l *controlExecutionLease) release() error {
	l.stopOnce.Do(func() { close(l.stop) })
	<-l.done

	l.mu.Lock()
	expected := l.value
	renewErr := l.renewErr
	l.mu.Unlock()

	outcome, err := beads.ApplyMetadataCAS(l.store, l.rootID, controlExecutionLeaseMetadataKey, expected, "")
	if err != nil {
		return errors.Join(renewErr, err)
	}
	// Conflict means the exact value we owned is gone. A successor owns the key,
	// so release is intentionally a no-op rather than an unconditional clear.
	switch outcome {
	case beads.MetadataCASSwapped, beads.MetadataCASAlreadyNext, beads.MetadataCASConflict:
		return renewErr
	default:
		return errors.Join(renewErr, fmt.Errorf("unexpected metadata CAS outcome %q", outcome))
	}
}

func newControlExecutionLeaseOwner() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func encodeControlExecutionLease(value controlExecutionLeaseValue) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encoding control execution lease: %w", err)
	}
	return string(encoded), nil
}

func decodeControlExecutionLease(encoded string) (controlExecutionLeaseValue, error) {
	var value controlExecutionLeaseValue
	if err := json.Unmarshal([]byte(encoded), &value); err != nil {
		return controlExecutionLeaseValue{}, fmt.Errorf("decoding control execution lease: %w", err)
	}
	value.Owner = strings.TrimSpace(value.Owner)
	if value.Owner == "" {
		return controlExecutionLeaseValue{}, fmt.Errorf("control execution lease has empty owner")
	}
	if value.ExpiresAt.IsZero() {
		return controlExecutionLeaseValue{}, fmt.Errorf("control execution lease has zero expiration")
	}
	value.ExpiresAt = value.ExpiresAt.UTC()
	return value, nil
}
