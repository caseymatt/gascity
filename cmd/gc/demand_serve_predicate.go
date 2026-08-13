package main

// The agreement predicate: would a T-worker's own query serve this row?
//
// The pool is a PULL system. The controller does not choose which bead a seat
// picks up; it scales capacity on evidence that work exists, and the worker
// discovers and claims for itself. That only converges if the two readers agree
// about what counts as work for a template — and they did not.
//
// The controller's demand loop counted any ready, unassigned row whose route
// normalized to T. The worker's Tier-3 query serves a strictly smaller set: it
// passes --exclude-type=epic and one --exclude-label per dispatch hold, and it
// matches the route by EXACT string. So three classes of row were permanent
// capacity demand that no worker could ever claim:
//
//   - a routed EPIC (an unassigned parent has no executable spec — the query
//     excludes it deliberately, workquery.go),
//   - a bead parked on hold:mayor / hold:external (parked precisely because the
//     next actor is not this worker),
//   - a route stamped with a live slot suffix, "<base>-N", which the demand side
//     normalizes to <base> and every raw consumer rejects.
//
// Each one spawns a seat, the seat's hook reads empty, it drains, and the
// controller counts the row again on the next tick. Forever.
//
// This file is the one predicate both sides now answer with. The serving rules
// come from internal/config — the same value the shell flags are rendered from —
// so a flag the query gains cannot silently fail to reach the controller. The
// route half mirrors hookClaimMatchesRoute exactly, because that is the function
// that will actually accept or reject the claim.

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// demandServableForTemplates reports the template a row is capacity demand for,
// among the templates a store group is counting, or ok=false when no worker for
// any of them would be served it.
//
// The route candidate is normalized (agentutil.NormalizePoolRouteTarget) only to
// bridge the intra-tick window: the same-tick canonicalize pass persists the
// collapse before this runs, so what is counted here is servable in the store
// THIS tick. Without the collapse, counting only exact forms would leave a "-N"
// row counted by neither side — invisible dead work, the dead-drop
// NormalizePoolRouteTarget exists to close.
func demandServableForTemplates(cfg *config.City, b beads.Bead, templates map[string]struct{}) (string, bool) {
	if !demandRowServable(b) {
		return "", false
	}
	for _, candidate := range controllerDemandRouteCandidates(b) {
		normalized := agentutil.NormalizePoolRouteTarget(cfg, candidate)
		if _, ok := templates[normalized]; ok {
			return normalized, true
		}
	}
	return "", false
}

// demandRowServable applies the route-independent half of the Tier-3 serving
// rules to one row: the exclusions a worker's query enforces regardless of which
// template it is asking for.
func demandRowServable(b beads.Bead) bool {
	rules := config.PoolDemandServeRulesForQuery()
	if rules.RequireUnassigned && strings.TrimSpace(b.Assignee) != "" {
		return false
	}
	beadType := strings.TrimSpace(b.Type)
	for _, excluded := range rules.ExcludeTypes {
		if strings.EqualFold(beadType, excluded) {
			return false
		}
	}
	for _, label := range b.Labels {
		label = strings.TrimSpace(label)
		for _, excluded := range rules.ExcludeLabels {
			// EqualFold, matching isHeldHookCandidate: the hook applies the hold
			// filter again in Go over whatever the query returned, so its
			// comparison is the last word on what a worker actually sees.
			if strings.EqualFold(label, excluded) {
				return false
			}
		}
	}
	return true
}

// routeCollapseRewriteTarget returns the canonical base route a slot-suffixed
// route should be persisted as, or "" when the route is already canonical or is
// not a collapsible slot form.
//
// NormalizePoolRouteTarget owns the decision — base names, non-pool agents,
// unknown agents, non-numeric and out-of-range suffixes are all returned
// unchanged and therefore left alone here. This wrapper adds only the cheap
// pre-filter that keeps the steady-state backlog scan off the per-bead agent
// walk: a collapsible route's local segment ends in "-<digits>".
func routeCollapseRewriteTarget(cfg *config.City, routedTo string) string {
	routedTo = strings.TrimSpace(routedTo)
	if routedTo == "" || !routeHasSlotSuffixShape(routedTo) {
		return ""
	}
	collapsed := agentutil.NormalizePoolRouteTarget(cfg, routedTo)
	if collapsed == "" || collapsed == routedTo {
		return ""
	}
	return collapsed
}

// routeHasSlotSuffixShape reports whether a route's local segment ends in a
// "-<digits>" slot suffix. Pure shape, no config: it is the pre-filter, not the
// decision.
func routeHasSlotSuffixShape(routedTo string) bool {
	_, local := config.ParseQualifiedName(routedTo)
	idx := strings.LastIndexByte(local, '-')
	if idx < 0 || idx == len(local)-1 {
		return false
	}
	for _, r := range local[idx+1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// collapseSlotSuffixedRoutedWork persists the base route for open, unassigned
// rows routed at a live slot suffix, alongside the legacy-bound canonicalization
// that already runs in this pass.
//
// Why persist rather than only normalize on read: the readers that reject "-N"
// are shell, jq and bd flags (the generated query's --metadata-field, the
// claim's raw string compare) and cannot call into Go. One write fixes every raw
// consumer at once — and the alternative, counting only exact forms, would leave
// the row claimable by nobody and visible to nobody.
//
// Idempotent by construction: it writes only when the collapse differs from what
// is persisted, so steady state performs no writes. Same error discipline as its
// sibling — a write failure is logged and skipped, never blocking reconciliation.
func collapseSlotSuffixedRoutedWork(cfg *config.City, workBeads []beads.Bead, workStores []beads.Store, stderr io.Writer) {
	if cfg == nil || len(workBeads) != len(workStores) {
		return
	}
	for i, wb := range workBeads {
		if wb.Status != "open" || strings.TrimSpace(wb.Assignee) != "" {
			continue
		}
		store := workStores[i]
		if store == nil {
			continue
		}
		collapsed := routeCollapseRewriteTarget(cfg, wb.Metadata[beadmeta.RoutedToMetadataKey])
		if collapsed == "" {
			continue
		}
		opts := beads.UpdateOpts{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: collapsed}}
		if err := store.Update(wb.ID, opts); err != nil && stderr != nil {
			fmt.Fprintf(stderr, "collapseSlotSuffixedRoutedWork: %s: %v\n", wb.ID, err) //nolint:errcheck
		}
	}
}
