package dispatch

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestFilterFailedDependencyCandidatesClosesOrdinaryWorkButPreservesControls(t *testing.T) {
	base := beads.NewMemStore()
	store := metadataCASOnlyStore{Store: base, writer: base}
	if _, ok := beads.ConditionalWriterFor(store); ok {
		t.Fatal("fixture unexpectedly exposes revision-conditional writes")
	}
	if _, ok := beads.MetadataCASWriterFor(store); !ok {
		t.Fatal("fixture must expose metadata CAS")
	}
	root := createFailedDependencyBead(t, store, beads.Bead{
		Title: "workflow", Type: "epic", Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		},
	})
	failed := createFailedDependencyBead(t, store, beads.Bead{
		Title: "failed prerequisite", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:    root.ID,
			beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
			beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
			beadmeta.FailureReasonMetadataKey: "invalid_spec",
			beadmeta.FailureOwnerMetadataKey:  "source-system",
		},
	})
	passed := createFailedDependencyBead(t, store, beads.Bead{
		Title: "passed prerequisite", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
		},
	})
	closeFailedDependencyBead(t, store, failed.ID)
	closeFailedDependencyBead(t, store, passed.ID)

	downstream := createFailedDependencyBead(t, store, beads.Bead{
		Title: "ordinary downstream", Type: "task", Status: "open",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	sibling := createFailedDependencyBead(t, store, beads.Bead{
		Title: "ordinary success sibling", Type: "task", Status: "open",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	addFailedDependencyEdge(t, store, downstream.ID, failed.ID)
	addFailedDependencyEdge(t, store, sibling.ID, passed.ID)

	candidates := []beads.Bead{downstream, sibling}
	controlIDs := make(map[string]bool, len(beadmeta.ControlKinds))
	for _, kind := range beadmeta.ControlKinds {
		control := createFailedDependencyBead(t, store, beads.Bead{
			Title: kind, Type: "task", Status: "open",
			Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: root.ID,
				beadmeta.KindMetadataKey:       kind,
			},
		})
		addFailedDependencyEdge(t, store, control.ID, failed.ID)
		candidates = append(candidates, control)
		controlIDs[control.ID] = true
	}

	kept, err := FilterFailedDependencyCandidates(store, candidates)
	if err != nil {
		t.Fatalf("FilterFailedDependencyCandidates: %v", err)
	}
	gotIDs := make([]string, 0, len(kept))
	for _, candidate := range kept {
		gotIDs = append(gotIDs, candidate.ID)
	}
	wantIDs := []string{sibling.ID}
	for _, candidate := range candidates[2:] {
		wantIDs = append(wantIDs, candidate.ID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("kept ids = %v, want success sibling and all controls %v", gotIDs, wantIDs)
	}

	closed := getFailedDependencyBead(t, store, downstream.ID)
	for key, want := range map[string]string{
		beadmeta.OutcomeMetadataKey:        beadmeta.OutcomeFail,
		beadmeta.FailureSubjectMetadataKey: failed.ID,
		beadmeta.FailureReasonMetadataKey:  "invalid_spec",
		beadmeta.FailureClassMetadataKey:   beadmeta.FailureClassHard,
		beadmeta.FailureOwnerMetadataKey:   "source-system",
	} {
		if got := closed.Metadata[key]; got != want {
			t.Errorf("downstream %s = %q, want %q", key, got, want)
		}
	}
	if closed.Status != "closed" {
		t.Errorf("downstream status = %q, want closed", closed.Status)
	}
	for id := range controlIDs {
		if got := getFailedDependencyBead(t, store, id).Status; got != "open" {
			t.Errorf("failure-handling control %s status = %q, want open", id, got)
		}
	}
}

func TestFailedDependencyPropagationConvergesAndLetsRootFinalizeFailed(t *testing.T) {
	store := beads.NewMemStore()
	root := createFailedDependencyBead(t, store, beads.Bead{
		Title: "workflow", Type: "epic", Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		},
	})
	upstream := createFailedDependencyBead(t, store, beads.Bead{
		Title: "failed source", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:    root.ID,
			beadmeta.OutcomeMetadataKey:       beadmeta.OutcomeFail,
			beadmeta.FailureClassMetadataKey:  beadmeta.FailureClassHard,
			beadmeta.FailureReasonMetadataKey: "source_contract_failed",
		},
	})
	closeFailedDependencyBead(t, store, upstream.ID)
	middle := createFailedDependencyBead(t, store, beads.Bead{
		Title: "futile summarize", Type: "task", Status: "open",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	leaf := createFailedDependencyBead(t, store, beads.Bead{
		Title: "futile review", Type: "task", Status: "open",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	finalizer := createFailedDependencyBead(t, store, beads.Bead{
		Title: "finalize", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.KindMetadataKey:       beadmeta.KindWorkflowFinalize,
		},
	})
	addFailedDependencyEdge(t, store, middle.ID, upstream.ID)
	addFailedDependencyEdge(t, store, leaf.ID, middle.ID)
	addFailedDependencyEdge(t, store, finalizer.ID, leaf.ID)

	kept, err := FilterFailedDependencyCandidates(store, []beads.Bead{middle})
	if err != nil || len(kept) != 0 {
		t.Fatalf("middle gate = kept %v err %v, want terminal suppression", kept, err)
	}
	middle = getFailedDependencyBead(t, store, middle.ID)
	kept, err = FilterFailedDependencyCandidates(store, []beads.Bead{leaf})
	if err != nil || len(kept) != 0 {
		t.Fatalf("leaf gate = kept %v err %v, want terminal suppression", kept, err)
	}
	leaf = getFailedDependencyBead(t, store, leaf.ID)
	for _, propagated := range []beads.Bead{middle, leaf} {
		if got := propagated.Metadata[beadmeta.FailureSubjectMetadataKey]; got != upstream.ID {
			t.Errorf("%s failure subject = %q, want original upstream %q", propagated.ID, got, upstream.ID)
		}
		if got := propagated.Metadata[beadmeta.FailureReasonMetadataKey]; got != "source_contract_failed" {
			t.Errorf("%s failure reason = %q, want original detail", propagated.ID, got)
		}
	}

	kept, err = FilterFailedDependencyCandidates(store, []beads.Bead{finalizer})
	if err != nil {
		t.Fatalf("finalizer gate: %v", err)
	}
	if len(kept) != 1 || kept[0].ID != finalizer.ID {
		t.Fatalf("finalizer kept = %+v, want failure-handling control %s", kept, finalizer.ID)
	}
	result, err := ProcessControl(store, finalizer, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl(finalizer): %v", err)
	}
	if !result.Processed || result.Action != "workflow-fail" {
		t.Fatalf("finalizer result = %+v, want processed workflow-fail", result)
	}
	root = getFailedDependencyBead(t, store, root.ID)
	if root.Status != "closed" || root.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail {
		t.Fatalf("root = status %q outcome %q, want closed/fail", root.Status, root.Metadata[beadmeta.OutcomeMetadataKey])
	}
}

func TestFilterFailedDependencyCandidatesFailsClosedOnDependencyReadError(t *testing.T) {
	base := beads.NewMemStore()
	candidate := createFailedDependencyBead(t, base, beads.Bead{
		Title: "ordinary graph work", Type: "task", Status: "open",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "root"},
	})
	store := failedDependencyReadErrorStore{Store: base}
	if kept, err := FilterFailedDependencyCandidates(store, []beads.Bead{candidate}); err == nil || kept != nil {
		t.Fatalf("gate = kept %v err %v, want no exposed candidate and read error", kept, err)
	}
	if got := getFailedDependencyBead(t, base, candidate.ID).Status; got != "open" {
		t.Fatalf("candidate status = %q, want unchanged open after failed read", got)
	}
}

type failedDependencyReadErrorStore struct {
	beads.Store
}

func (failedDependencyReadErrorStore) DepList(string, string) ([]beads.Dep, error) {
	return nil, errors.New("dependency store unavailable")
}

type metadataCASOnlyStore struct {
	beads.Store
	writer beads.MetadataCASWriter
}

func (s metadataCASOnlyStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	return s.writer.CompareAndSetMetadataKey(id, key, expected, next)
}

func createFailedDependencyBead(t *testing.T, store beads.Store, bead beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(bead)
	if err != nil {
		t.Fatalf("create %q: %v", bead.Title, err)
	}
	return created
}

func closeFailedDependencyBead(t *testing.T, store beads.Store, id string) {
	t.Helper()
	if err := store.Close(id); err != nil {
		t.Fatalf("close %s: %v", id, err)
	}
}

func addFailedDependencyEdge(t *testing.T, store beads.Store, issueID, blockerID string) {
	t.Helper()
	if err := store.DepAdd(issueID, blockerID, "blocks"); err != nil {
		t.Fatalf("add dependency %s -> %s: %v", issueID, blockerID, err)
	}
}

func getFailedDependencyBead(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return bead
}
