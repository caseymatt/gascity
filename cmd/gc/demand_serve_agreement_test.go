package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// T-A: THE agreement property.
//
// Any row the controller counts as demand for template T must, in the same store
// state, be servable to a T-worker and acceptable to that worker's claim
// matcher. Counted-by-one is the defect — in either direction. A row counted but
// not servable spawns a seat that reads empty, drains, and is counted again next
// tick; a row servable but not counted is work no seat is ever minted for.

const agreementTemplate = "rig/worker"

func agreementConfig() *config.City {
	max := 3
	// "solo" is capped at one session, so it is a singleton rather than a pool:
	// NormalizePoolRouteTarget must not collapse a suffix against it.
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "rig", Path: "/tmp/rig"}},
		Agents: []config.Agent{
			{Name: "worker", Dir: "rig", MinActiveSessions: intPtr(0), MaxActiveSessions: &max},
			{Name: "solo", Dir: "rig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
		},
	}
}

// agreementRow is one persisted row plus what the two sides must say about it.
type agreementRow struct {
	name string
	bead beads.Bead
	// wantServable is the shared verdict: counted by demand AND servable to a
	// T-worker. There is deliberately only ONE field — that IS the property.
	wantServable bool
	// wantRewrittenTo is the route the same-tick pass must persist, or "" when
	// the pass must leave the row alone.
	wantRewrittenTo string
}

func agreementRows() []agreementRow {
	routed := func(id, target string) beads.Bead {
		return beads.Bead{ID: id, Status: "open", Type: "task",
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: target}}
	}
	return []agreementRow{
		{name: "canonical base route", bead: routed("a-1", agreementTemplate), wantServable: true},
		{
			name: "slot-suffixed route, valid slot", bead: routed("a-2", agreementTemplate+"-2"),
			wantServable: true, wantRewrittenTo: agreementTemplate,
		},
		{name: "slot-suffixed route, out of range", bead: routed("a-3", agreementTemplate+"-9"), wantServable: false},
		{name: "slot-suffixed route, non-pool agent", bead: routed("a-4", "rig/solo-1"), wantServable: false},
		{name: "unknown target", bead: routed("a-5", "rig/nobody"), wantServable: false},
		{
			name: "workflow root routed by run_target only",
			bead: beads.Bead{ID: "a-6", Status: "open", Type: "task", Metadata: map[string]string{
				beadmeta.KindMetadataKey:      beadmeta.KindWorkflow,
				beadmeta.RunTargetMetadataKey: agreementTemplate,
			}},
			wantServable: true,
		},
		{
			name: "routed epic",
			bead: beads.Bead{ID: "a-7", Status: "open", Type: "epic",
				Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate}},
			wantServable: false,
		},
		{
			name: "assigned row",
			bead: beads.Bead{ID: "a-9", Status: "open", Type: "task", Assignee: "someone",
				Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate}},
			wantServable: false,
		},
	}
}

// holdLabelAgreementRows is generated from the label set itself, so a new
// dispatch hold cannot be added without this property covering it.
func holdLabelAgreementRows() []agreementRow {
	rows := make([]agreementRow, 0, len(beadmeta.DispatchHoldLabels))
	for i, label := range beadmeta.DispatchHoldLabels {
		rows = append(rows, agreementRow{
			name: "routed bead held by " + label,
			bead: beads.Bead{
				ID: "hold-" + string(rune('a'+i)), Status: "open", Type: "task",
				Labels:   []string{label},
				Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate},
			},
			wantServable: false,
		})
	}
	return rows
}

// TestDemandAndClaimAgreeOnEveryRowForm is the property itself. Both sides are
// asked about the same row and must answer the same.
func TestDemandAndClaimAgreeOnEveryRowForm(t *testing.T) {
	cfg := agreementConfig()
	templates := map[string]struct{}{agreementTemplate: {}}
	routeTargets := hookClaimRouteTargets(agreementTemplate)

	for _, row := range append(agreementRows(), holdLabelAgreementRows()...) {
		t.Run(row.name, func(t *testing.T) {
			// The same-tick pass runs before demand is counted, so the property
			// is asserted over the POST-rewrite row.
			bead := row.bead
			if rewritten := routeCollapseRewriteTarget(cfg, bead.Metadata[beadmeta.RoutedToMetadataKey]); rewritten != "" {
				meta := map[string]string{}
				for k, v := range bead.Metadata {
					meta[k] = v
				}
				meta[beadmeta.RoutedToMetadataKey] = rewritten
				bead.Metadata = meta
			}

			_, counted := demandServableForTemplates(cfg, bead, templates)
			claimable := demandRowServable(bead) && hookClaimMatchesRoute(bead, routeTargets)

			if counted != row.wantServable {
				t.Errorf("counted by demand = %v, want %v", counted, row.wantServable)
			}
			if claimable != row.wantServable {
				t.Errorf("acceptable to the claim matcher = %v, want %v", claimable, row.wantServable)
			}
			if counted != claimable {
				t.Fatalf("AGREEMENT VIOLATED: demand=%v claim=%v for %s — a row counted by exactly one side is the defect",
					counted, claimable, bead.ID)
			}
		})
	}
}

// TestTierThreeServeRulesMatchTheGeneratedQuery pins the predicate's rule source
// against the flags the worker actually runs. The shell is rendered FROM the
// descriptor, so this fails if either the rendering or the rule set is edited
// alone — the same discipline the query builder already applies to its own two
// forms.
func TestTierThreeServeRulesMatchTheGeneratedQuery(t *testing.T) {
	agent := config.Agent{Name: "worker", Dir: "rig"}
	query := agent.EffectiveWorkQueryFor(config.QueryTopology{})
	rules := config.PoolDemandServeRulesForQuery()

	if rules.RequireUnassigned && !strings.Contains(query, "--unassigned") {
		t.Error("rules require unassigned but the generated query does not pass --unassigned")
	}
	for _, typ := range rules.ExcludeTypes {
		if !strings.Contains(query, "--exclude-type="+typ) {
			t.Errorf("rules exclude type %q but the generated query does not", typ)
		}
	}
	for _, label := range rules.ExcludeLabels {
		if !strings.Contains(query, `--exclude-label "`+label+`"`) {
			t.Errorf("rules exclude label %q but the generated query does not", label)
		}
	}
	// And the reverse: every exclude-type/-label the query carries must be a
	// declared rule, or the controller is blind to it.
	for _, flag := range []string{"--exclude-type=", "--exclude-label "} {
		for _, got := range queryFlagValues(query, flag) {
			if !ruleDeclares(rules, flag, got) {
				t.Errorf("the generated query carries %s%s but PoolDemandServeRules does not declare it", flag, got)
			}
		}
	}
}

func queryFlagValues(query, flag string) []string {
	var out []string
	for _, part := range strings.Split(query, flag)[1:] {
		value := strings.TrimSpace(part)
		value = strings.Trim(strings.Fields(value)[0], `"`)
		out = append(out, value)
	}
	return out
}

func ruleDeclares(rules config.PoolDemandServeRules, flag, value string) bool {
	declared := rules.ExcludeTypes
	if flag == "--exclude-label " {
		declared = rules.ExcludeLabels
	}
	for _, d := range declared {
		if d == value {
			return true
		}
	}
	return false
}

// TestSlotSuffixCollapseIsPersistedForClaimableFormsOnly is the store-backed
// half: the pass rewrites the collapsible route and nothing else.
//
// Controls, each failing differently from a predicate bug: an out-of-range slot
// is untouched (pins the collapse BOUNDS, which are NormalizePoolRouteTarget's,
// not this pass's), and a held or epic row is untouched (the pass owns route
// FORM only — excluding those rows is the predicate's job, and a pass that
// "helpfully" rewrote them would be erasing routing).
func TestSlotSuffixCollapseIsPersistedForClaimableFormsOnly(t *testing.T) {
	cfg := agreementConfig()
	cfg.Rigs[0].Path = filepath.Join(t.TempDir(), "rig")
	store := beads.NewMemStore()

	var seeded []beads.Bead
	var stores []beads.Store
	want := map[string]string{}
	for _, row := range append(agreementRows(), holdLabelAgreementRows()...) {
		created, err := store.Create(beads.Bead{
			Title:    row.name,
			Type:     row.bead.Type,
			Labels:   row.bead.Labels,
			Assignee: row.bead.Assignee,
			Metadata: row.bead.Metadata,
		})
		if err != nil {
			t.Fatalf("seeding %q: %v", row.name, err)
		}
		if row.bead.Assignee != "" {
			assignee := row.bead.Assignee
			if err := store.Update(created.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
				t.Fatalf("assigning %q: %v", row.name, err)
			}
			created, _ = store.Get(created.ID)
		}
		seeded = append(seeded, created)
		stores = append(stores, store)
		route := row.bead.Metadata[beadmeta.RoutedToMetadataKey]
		if row.wantRewrittenTo != "" {
			want[created.ID] = row.wantRewrittenTo
		} else {
			want[created.ID] = route
		}
	}

	var stderr bytes.Buffer
	collapseSlotSuffixedRoutedWork(cfg, seeded, stores, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("collapse reported errors: %s", stderr.String())
	}

	for id, wantRoute := range want {
		got, err := store.Get(id)
		if err != nil {
			t.Fatalf("re-reading %s: %v", id, err)
		}
		if gotRoute := strings.TrimSpace(got.Metadata[beadmeta.RoutedToMetadataKey]); gotRoute != wantRoute {
			t.Errorf("%s (%s): route = %q, want %q", id, got.Title, gotRoute, wantRoute)
		}
	}
}

// TestSlotSuffixCollapseIsIdempotent: steady state performs no writes, so the
// pass cannot become a per-tick write amplifier on the open-routed backlog.
func TestSlotSuffixCollapseIsIdempotent(t *testing.T) {
	cfg := agreementConfig()
	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{Title: "slot routed", Type: "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: agreementTemplate + "-2"}})
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	counting := &countingUpdateStore{Store: store}
	var stderr bytes.Buffer
	collapseSlotSuffixedRoutedWork(cfg, []beads.Bead{created}, []beads.Store{counting}, &stderr)
	if counting.updates != 1 {
		t.Fatalf("updates on the first pass = %d, want 1", counting.updates)
	}

	rewritten, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("re-reading: %v", err)
	}
	collapseSlotSuffixedRoutedWork(cfg, []beads.Bead{rewritten}, []beads.Store{counting}, &stderr)
	if counting.updates != 1 {
		t.Fatalf("updates after the row is already canonical = %d, want no second write", counting.updates)
	}
}

type countingUpdateStore struct {
	beads.Store
	updates int
}

func (s *countingUpdateStore) Update(id string, opts beads.UpdateOpts) error {
	s.updates++
	return s.Store.Update(id, opts)
}

// TestGoPredicateAndGeneratedQueryAgreeRowByRow is the reader-agreement
// invariant made permanent, and it is the operator's framing made mechanical:
// there is ONE eligibility semantics with two representations — the Go form the
// controller's demand loop consumes, and the flag form the worker's generated
// Tier-3 query runs. This row takes the tier STRAIGHT OUT of the generated
// query, parses it through the real `gc ready` flag surface, runs the real
// serving filter over the corpus, and requires the same verdict per row.
//
// Edit one representation without the other and this fails. That is the point:
// a shared predicate that is only shared by convention is a predicate that
// drifts back apart, which is how the demand loop became a divergent copy in the
// first place.
func TestGoPredicateAndGeneratedQueryAgreeRowByRow(t *testing.T) {
	cfg := agreementConfig()
	templates := map[string]struct{}{agreementTemplate: {}}
	agent := config.Agent{Name: "worker", Dir: "rig"}
	query := agent.EffectiveWorkQueryFor(config.QueryTopology{})
	args := tierThreeReaderArgs(t, query, agreementTemplate)

	opts, metaWant := parseReadyArgsForTest(t, args)
	legacyOpts, legacyMetaWant := parseReadyArgsForTest(t, tierThreeLegacyReaderArgs(t, query, agreementTemplate))
	assertLegacyTierFilterUnchanged(t, query)

	for _, row := range append(agreementRows(), holdLabelAgreementRows()...) {
		t.Run(row.name, func(t *testing.T) {
			bead := postCanonicalizeBead(cfg, row.bead)

			_, counted := demandServableForTemplates(cfg, bead, templates)
			served := len(filterReadyBeads([]beads.Bead{bead}, opts, metaWant)) == 1 ||
				legacyWorkflowTierServes(bead, legacyOpts, legacyMetaWant)

			if counted != served {
				t.Fatalf("AGREEMENT VIOLATED for %s: the Go demand predicate says %v, the generated pool-demand query form says %v",
					bead.ID, counted, served)
			}
			if counted != row.wantServable {
				t.Fatalf("both representations say %v, want %v", counted, row.wantServable)
			}
		})
	}
}

// legacyWorkflowTierServes evaluates the generated query's LEGACY workflow-root
// tier: its own reader flags plus the jq post-filter the builder pipes the
// result through. That filter keeps only rows whose gc.routed_to is empty, which
// is the same gate the Go side applies in routedToAndLegacyWorkflowCandidates —
// run_target is consulted only when there is no canonical route. Mirroring one
// jq clause here is the single restatement in this conformance, and
// assertLegacyTierFilterUnchanged is what keeps it honest.
func legacyWorkflowTierServes(bead beads.Bead, opts readyOpts, metaWant []metadataFieldFilter) bool {
	if len(filterReadyBeads([]beads.Bead{bead}, opts, metaWant)) != 1 {
		return false
	}
	return strings.TrimSpace(bead.Metadata[beadmeta.RoutedToMetadataKey]) == ""
}

// assertLegacyTierFilterUnchanged pins the jq clause legacyWorkflowTierServes
// mirrors. If the builder's post-filter changes, this fails instead of leaving
// the mirror quietly wrong.
func assertLegacyTierFilterUnchanged(t *testing.T, query string) {
	t.Helper()
	const wantSelect = `select((.metadata["gc.routed_to"] // "") == "")`
	if !strings.Contains(query, wantSelect) {
		t.Fatalf("the legacy workflow tier no longer post-filters with %s; the Go mirror in legacyWorkflowTierServes is now unpinned:\n%s", wantSelect, query)
	}
}

// tierThreeLegacyReaderArgs slices the legacy workflow-root tier's reader
// invocation out of the generated query, the compat path that serves a root
// stamped with gc.run_target before canonical route stamping shipped.
func tierThreeLegacyReaderArgs(t *testing.T, query, target string) []string {
	t.Helper()
	return readerArgsForMarker(t, query, `--metadata-field "`+beadmeta.RunTargetMetadataKey+`=$target"`, target)
}

// postCanonicalizeBead applies the same-tick route collapse, so the corpus is
// compared in the state the tick actually leaves it in.
func postCanonicalizeBead(cfg *config.City, bead beads.Bead) beads.Bead {
	rewritten := routeCollapseRewriteTarget(cfg, bead.Metadata[beadmeta.RoutedToMetadataKey])
	if rewritten == "" {
		return bead
	}
	meta := map[string]string{}
	for k, v := range bead.Metadata {
		meta[k] = v
	}
	meta[beadmeta.RoutedToMetadataKey] = rewritten
	bead.Metadata = meta
	return bead
}

// tierThreeReaderArgs slices the routed pool-demand tier's reader invocation out
// of the generated work query and returns it as argv with $target bound. It
// fails loudly rather than falling back: if the tier cannot be located, the
// conformance below would silently degrade into testing nothing.
func tierThreeReaderArgs(t *testing.T, query, target string) []string {
	t.Helper()
	return readerArgsForMarker(t, query, `--metadata-field "`+beadmeta.RoutedToMetadataKey+`=$target"`, target)
}

// readerArgsForMarker returns the argv of the reader invocation carrying marker.
// It fails loudly rather than falling back: a conformance that cannot find its
// subject would silently degrade into testing nothing.
func readerArgsForMarker(t *testing.T, query, marker, target string) []string {
	t.Helper()
	idx := strings.Index(query, marker)
	if idx < 0 {
		t.Fatalf("tier carrying %s not found in the generated query:\n%s", marker, query)
	}
	head := strings.LastIndex(query[:idx], "ready ")
	if head < 0 {
		t.Fatalf("no reader command precedes %s:\n%s", marker, query)
	}
	rest := query[head+len("ready "):]
	end := strings.Index(rest, " --json")
	if end < 0 {
		t.Fatalf("tier carrying %s has no --json terminator:\n%s", marker, query)
	}
	return splitShellWords(strings.ReplaceAll(rest[:end], "$target", target))
}

// splitShellWords is the minimal double-quote-aware tokenizer the generated
// query's flag section needs (the builders quote metadata and label values and
// nothing else).
func splitShellWords(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ' ' && !quoted:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// parseReadyArgsForTest parses argv through the REAL `gc ready` flag surface and
// the real metadata-filter parser, so the conformance runs the production
// serving semantics rather than a test's reading of them.
func parseReadyArgsForTest(t *testing.T, args []string) (readyOpts, []metadataFieldFilter) {
	t.Helper()
	var opts readyOpts
	var includeEphemeral, jsonOut bool
	cmd := &cobra.Command{Use: "ready"}
	registerReadyFlags(cmd, &opts, &includeEphemeral, &jsonOut)
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("parsing the generated tier's flags %v: %v", args, err)
	}
	if len(opts.metadataFields) == 0 {
		t.Fatalf("parsed no metadata filters from %v; the conformance would pass vacuously", args)
	}
	metaWant, err := parseMetadataFieldFilters(opts.metadataFields)
	if err != nil {
		t.Fatalf("parsing metadata filters: %v", err)
	}
	return opts, metaWant
}
