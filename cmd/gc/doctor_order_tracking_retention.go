package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

const (
	orderTrackingRetentionCheckThreshold  = 500
	orderTrackingRetentionCheckCountLimit = 501
)

// orderTrackingRetentionCheck reports the bounded count of closed
// order-tracking beads that the configured retention sweep can delete now. The
// recent-history floor and configured TTL are applied by the same authoritative
// eligibility calculation used by the sweep, so ordinary retained history does
// not become a warning. It is pure observability and never gates.
type orderTrackingRetentionCheck struct {
	cityPath string
	policy   orderTrackingRetentionPolicy
	newStore func(string) (beads.Store, error)
}

// newOrderTrackingRetentionCheck constructs an orderTrackingRetentionCheck.
func newOrderTrackingRetentionCheck(cityPath string, cfg *config.City, newStore func(string) (beads.Store, error)) *orderTrackingRetentionCheck {
	return &orderTrackingRetentionCheck{
		cityPath: cityPath,
		policy:   orderTrackingRetentionPolicyForConfig(cfg),
		newStore: newStore,
	}
}

// Name implements doctor.Check.
func (c *orderTrackingRetentionCheck) Name() string { return "order-tracking-retention" }

// CanFix implements doctor.Check.
func (c *orderTrackingRetentionCheck) CanFix() bool { return false }

// Fix implements doctor.Check.
func (c *orderTrackingRetentionCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible implements doctor.Check.
func (c *orderTrackingRetentionCheck) WarmupEligible() bool { return false }

// Run implements doctor.Check.
func (c *orderTrackingRetentionCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	res := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory}
	if c.newStore == nil || strings.TrimSpace(c.cityPath) == "" {
		res.Status = doctor.StatusOK
		res.Message = "order-tracking retention: no bead store configured"
		return res
	}
	store, err := c.newStore(c.cityPath)
	if err != nil {
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("order-tracking retention unknown: opening city bead store: %v", err)
		return res
	}
	// The city work store holds the pre-cutover backlog; the orders binding
	// holds everything a split city has written since. Counting only one of them
	// reports a healthy 0 on exactly the city whose backlog is growing.
	stores := []beads.Store{store}
	if ordersStore := relocatedOrdersClassStore(c.cityPath, nil); ordersStore != nil && ordersStore != store {
		stores = append(stores, ordersStore)
	}
	eligible, err := countClosedOrderTrackingRetentionEligible(stores, time.Now(), c.policy, nil)
	if err != nil {
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("order-tracking retention unknown: counting deletion-eligible beads: %v", err)
		return res
	}

	countStr := fmt.Sprintf("%d", eligible)
	if eligible >= orderTrackingRetentionCheckCountLimit {
		countStr = fmt.Sprintf("≥%d", orderTrackingRetentionCheckCountLimit)
	}
	if eligible >= orderTrackingRetentionCheckThreshold {
		res.Status = doctor.StatusWarning
		res.Message = fmt.Sprintf("%s deletion-eligible closed order-tracking beads: retention watchdog is behind", countStr)
		res.FixHint = "verify the city controller is running and inspect controller logs for order-tracking retention watchdog errors"
		return res
	}
	res.Status = doctor.StatusOK
	res.Message = fmt.Sprintf("%s deletion-eligible closed order-tracking beads", countStr)
	return res
}
