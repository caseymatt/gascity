package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func makeClosedOrderTrackingBeads(n int) []beads.Bead {
	return makeClosedOrderTrackingBeadsAt(n, time.Time{})
}

func makeClosedOrderTrackingBeadsAt(n int, closedAt time.Time) []beads.Bead {
	out := make([]beads.Bead, n)
	for i := range out {
		out[i] = beads.Bead{
			ID:        fmt.Sprintf("ot-%04d", i),
			Status:    "closed",
			Labels:    []string{labelOrderTracking},
			CreatedAt: closedAt,
			UpdatedAt: closedAt,
		}
	}
	return out
}

type orderTrackingRetentionListErrorStore struct {
	beads.Store
}

func (orderTrackingRetentionListErrorStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, fmt.Errorf("retention state unavailable")
}

func TestOrderTrackingRetentionCheck_OKWithBoundedEligibleCountBelowThreshold(t *testing.T) {
	// The retain-last floor keeps 10 of the 509 old legacy runs, leaving 499
	// deletion-eligible records.
	store := beads.NewMemStoreFrom(600, makeClosedOrderTrackingBeads(509), nil)
	check := newOrderTrackingRetentionCheck("/city", nil, func(string) (beads.Store, error) { return store, nil })

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK at 499 eligible beads: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "499 deletion-eligible") {
		t.Fatalf("Message = %q, want actual eligible count 499", res.Message)
	}
}

func TestOrderTrackingRetentionCheck_WarningAtEligibleThreshold(t *testing.T) {
	// The retain-last floor leaves exactly 500 of these 510 old runs eligible.
	store := beads.NewMemStoreFrom(600, makeClosedOrderTrackingBeads(510), nil)
	check := newOrderTrackingRetentionCheck("/city", nil, func(string) (beads.Store, error) { return store, nil })

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning at 500 eligible beads: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "500 deletion-eligible") {
		t.Errorf("Message %q missing eligible count 500", res.Message)
	}
	if res.FixHint == "" {
		t.Error("FixHint is empty for actionable retention lag")
	}
}

func TestOrderTrackingRetentionCheck_WarningAboveEligibleThreshold(t *testing.T) {
	store := beads.NewMemStoreFrom(700, makeClosedOrderTrackingBeads(610), nil)
	check := newOrderTrackingRetentionCheck("/city", nil, func(string) (beads.Store, error) { return store, nil })

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning above eligible threshold: %s", res.Status, res.Message)
	}
}

func TestOrderTrackingRetentionCheck_CapsEligibleCountAtContractLimit(t *testing.T) {
	// 511 old runs minus the retain-last floor leaves 501 eligible records, the
	// bounded status contract's lower-bound display point.
	store := beads.NewMemStoreFrom(700, makeClosedOrderTrackingBeads(511), nil)
	check := newOrderTrackingRetentionCheck("/city", nil, func(string) (beads.Store, error) { return store, nil })

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning at count cap: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "≥501 deletion-eligible") {
		t.Fatalf("Message = %q, want bounded ≥501 eligible-count format", res.Message)
	}
}

func TestOrderTrackingRetentionCheck_RecentHistoryDoesNotWarn(t *testing.T) {
	store := beads.NewMemStoreFrom(700, makeClosedOrderTrackingBeadsAt(600, time.Now()), nil)
	check := newOrderTrackingRetentionCheck("/city", nil, func(string) (beads.Store, error) { return store, nil })

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK for normal recent history: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "0 deletion-eligible") {
		t.Fatalf("Message = %q, want zero eligible records rather than raw history volume", res.Message)
	}
}

func TestOrderTrackingRetentionCheck_UsesConfiguredTTL(t *testing.T) {
	store := beads.NewMemStoreFrom(20, makeClosedOrderTrackingBeadsAt(11, time.Now().Add(-2*time.Hour)), nil)
	cfg := &config.City{Beads: config.BeadsConfig{Policies: map[string]config.BeadPolicyConfig{
		orderTrackingBeadPolicyName: {DeleteAfterClose: "1h"},
	}}}
	check := newOrderTrackingRetentionCheck("/city", cfg, func(string) (beads.Store, error) { return store, nil })

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK for one eligible record: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "1 deletion-eligible") {
		t.Fatalf("Message = %q, want eligibility derived from configured 1h TTL", res.Message)
	}
}

func TestOrderTrackingRetentionCheck_OKWhenNoStore(t *testing.T) {
	check := newOrderTrackingRetentionCheck("", nil, nil)
	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK (no store configured means no beads): %s", res.Status, res.Message)
	}
}

func TestOrderTrackingRetentionCheck_WarningOnStoreOpenError(t *testing.T) {
	check := newOrderTrackingRetentionCheck("/city", nil, func(string) (beads.Store, error) {
		return nil, fmt.Errorf("store unreachable")
	})
	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning on store open error: %s", res.Status, res.Message)
	}
	if res.Severity != doctor.SeverityAdvisory {
		t.Fatalf("Severity = %v, want Advisory (observability only): %s", res.Severity, res.Message)
	}
}

func TestOrderTrackingRetentionCheck_WarningWhenEligibilityCannotBeCounted(t *testing.T) {
	store := orderTrackingRetentionListErrorStore{Store: beads.NewMemStore()}
	check := newOrderTrackingRetentionCheck("/city", nil, func(string) (beads.Store, error) {
		return store, nil
	})

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning when authoritative retention state is unavailable: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "retention state unavailable") {
		t.Fatalf("Message = %q, want the eligibility read failure surfaced", res.Message)
	}
}

func TestOrderTrackingRetentionCheck_CheckMetadata(t *testing.T) {
	check := newOrderTrackingRetentionCheck("/city", nil, func(string) (beads.Store, error) {
		return beads.NewMemStore(), nil
	})
	if check.Name() != "order-tracking-retention" {
		t.Errorf("Name() = %q, want order-tracking-retention", check.Name())
	}
	if check.CanFix() {
		t.Error("CanFix() = true, want false (read-only observability check)")
	}
	if check.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
}

func TestOrderTrackingRetentionCheck_RegisteredInBuildDoctorChecks(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{}
	withHealthyStorePreflight(t)
	checks := buildDoctorChecks(cityPath, cfg, nil, buildDoctorChecksOpts{
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})
	for _, c := range checks {
		if c.Name() == "order-tracking-retention" {
			return
		}
	}
	t.Fatal("order-tracking-retention check not found in buildDoctorChecks output")
}
