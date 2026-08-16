package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

type atomicAdoptionStore struct{ *beads.MemStore }

func (atomicAdoptionStore) AtomicTx() bool { return true }

type adoptionTxRaceStore struct {
	atomicAdoptionStore
	beforeTx func()
}

func (s *adoptionTxRaceStore) Tx(commitMsg string, fn func(beads.Tx) error) error {
	if s.beforeTx != nil {
		s.beforeTx()
	}
	return s.atomicAdoptionStore.Tx(commitMsg, fn)
}

type adoptionSourceRaceStore struct {
	beads.Store
	sourceID        string
	sourceReadCount int
	afterCurrentGet func()
}

func (s *adoptionSourceRaceStore) Get(id string) (beads.Bead, error) {
	bead, err := s.Store.Get(id)
	if err == nil && id == s.sourceID {
		s.sourceReadCount++
		if s.sourceReadCount == 2 && s.afterCurrentGet != nil {
			s.afterCurrentGet()
		}
	}
	return bead, err
}

type adoptionFixture struct {
	store   beads.Store
	root    beads.Bead
	control beads.Bead
	attempt beads.Bead
	source  beads.Bead
	summary string
}

func newAdoptionFixture(t *testing.T) adoptionFixture {
	t.Helper()
	return newAdoptionFixtureWithStore(t, atomicAdoptionStore{MemStore: beads.NewMemStore()})
}

func newAdoptionFixtureWithStore(t *testing.T, store beads.Store) adoptionFixture {
	t.Helper()
	rigRoot := t.TempDir()
	workDir := filepath.Join(rigRoot, "worktrees", "item-1")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	summary := filepath.Join(rigRoot, ".gc", "artifacts", "item-summary.md")
	if err := os.MkdirAll(filepath.Dir(summary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(summary, []byte("validated summary\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	source := mustCreateAdoptionBead(t, store, beads.Bead{
		Title:  "completed source",
		Status: "closed",
		Metadata: map[string]string{
			beadmeta.OutcomeMetadataKey:                   beadmeta.OutcomePass,
			beadmeta.WorkDirMetadataKey:                   workDir,
			beadmeta.ImplementationCommitMetadataKey:      strings.Repeat("a", 40),
			beadmeta.ImplementationSummaryPathMetadataKey: summary,
		},
	})
	root := mustCreateAdoptionBead(t, store, beads.Bead{
		Title: "workflow",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:          beadmeta.KindWorkflow,
			beadmeta.DrainMemberIDMetadataKey: source.ID,
			beadmeta.WorkDirMetadataKey:       rigRoot,
		},
	})
	control := mustCreateAdoptionBead(t, store, beads.Bead{
		Title: "implement loop",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindRalph,
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.StepIDMetadataKey:     "implement",
		},
	})
	attempt := mustCreateAdoptionBead(t, store, beads.Bead{
		Title: "implementation attempt",
		Metadata: map[string]string{
			beadmeta.LogicalBeadIDMetadataKey: control.ID,
			beadmeta.RootBeadIDMetadataKey:    root.ID,
			beadmeta.RalphStepIDMetadataKey:   "implement",
			beadmeta.AttemptMetadataKey:       "1",
			beadmeta.WorkDirMetadataKey:       workDir,
		},
	})
	return adoptionFixture{store: store, root: root, control: control, attempt: attempt, source: source, summary: summary}
}

func mustCreateAdoptionBead(t *testing.T, store beads.Store, bead beads.Bead) beads.Bead {
	t.Helper()
	wantedStatus := bead.Status
	bead.Status = ""
	created, err := store.Create(bead)
	if err != nil {
		t.Fatal(err)
	}
	if wantedStatus == "closed" {
		if err := store.Close(created.ID); err != nil {
			t.Fatal(err)
		}
		created, err = store.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	return created
}

func adoptionOptions(f adoptionFixture) AdoptAttemptOptions {
	return AdoptAttemptOptions{
		SourceID:  f.source.ID,
		Actor:     "operator@example.test",
		Reason:    "reviewed external implementation evidence",
		AdoptedAt: time.Date(2026, 8, 16, 1, 2, 3, 0, time.UTC),
		FS:        fsys.OSFS{},
	}
}

func TestAdoptRalphAttemptRecordsEvidenceAndClosesOnlyAttempt(t *testing.T) {
	f := newAdoptionFixture(t)
	result, err := AdoptRalphAttempt(f.store, f.store, f.control.ID, adoptionOptions(f))
	if err != nil {
		t.Fatalf("AdoptRalphAttempt: %v", err)
	}
	if result.AttemptID != f.attempt.ID || result.SourceID != f.source.ID || result.AlreadyApplied {
		t.Fatalf("result = %+v", result)
	}

	attempt, err := f.store.Get(f.attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "closed" {
		t.Fatalf("attempt status = %q, want closed", attempt.Status)
	}
	wantMetadata := map[string]string{
		beadmeta.OutcomeMetadataKey:                   beadmeta.OutcomePass,
		beadmeta.ImplementationCommitMetadataKey:      strings.Repeat("a", 40),
		beadmeta.ImplementationSummaryPathMetadataKey: f.summary,
		beadmeta.AdoptionSourceIDMetadataKey:          f.source.ID,
		beadmeta.AdoptionActorMetadataKey:             "operator@example.test",
		beadmeta.AdoptionReasonMetadataKey:            "reviewed external implementation evidence",
		beadmeta.AdoptionSourceUpdatedAtMetadataKey:   f.source.UpdatedAt.UTC().Format(time.RFC3339Nano),
		beadmeta.AdoptedAtMetadataKey:                 "2026-08-16T01:02:03Z",
	}
	for key, want := range wantMetadata {
		if got := attempt.Metadata[key]; got != want {
			t.Errorf("attempt metadata[%q] = %q, want %q", key, got, want)
		}
	}
	for _, bead := range []beads.Bead{f.root, f.control, f.source} {
		got, getErr := f.store.Get(bead.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Status != bead.Status {
			t.Errorf("%s status = %q, want unchanged %q", bead.ID, got.Status, bead.Status)
		}
	}
}

func TestAdoptRalphAttemptCommitsOnSQLiteStore(t *testing.T) {
	store, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.(*beads.SQLiteStore).CloseStore(); err != nil {
			t.Errorf("CloseStore: %v", err)
		}
	})
	f := newAdoptionFixtureWithStore(t, store)

	if _, err := AdoptRalphAttempt(store, store, f.control.ID, adoptionOptions(f)); err != nil {
		t.Fatalf("AdoptRalphAttempt: %v", err)
	}
	attempt, err := store.Get(f.attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Status != "closed" || !adoptionEvidenceMatches(attempt, f.source) {
		t.Fatalf("committed attempt = %+v, want closed with adopted evidence", attempt)
	}
}

func TestAdoptRalphAttemptIsIdempotentForSameSource(t *testing.T) {
	f := newAdoptionFixture(t)
	opts := adoptionOptions(f)
	if _, err := AdoptRalphAttempt(f.store, f.store, f.control.ID, opts); err != nil {
		t.Fatal(err)
	}
	result, err := AdoptRalphAttempt(f.store, f.store, f.control.ID, opts)
	if err != nil {
		t.Fatalf("repeat adoption: %v", err)
	}
	if !result.AlreadyApplied || result.AttemptID != f.attempt.ID {
		t.Fatalf("repeat result = %+v, want already applied", result)
	}
}

func TestAdoptRalphAttemptRejectsInvalidOrConflictingEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, f adoptionFixture)
		want   string
	}{
		{
			name: "source is not terminal",
			mutate: func(t *testing.T, f adoptionFixture) {
				status := "open"
				mustAdoptionUpdate(t, f.store, f.source.ID, beads.UpdateOpts{Status: &status})
			},
			want: "must be closed",
		},
		{
			name: "source outcome failed",
			mutate: func(t *testing.T, f adoptionFixture) {
				mustAdoptionUpdate(t, f.store, f.source.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail}})
			},
			want: "outcome",
		},
		{
			name: "source does not belong to drain",
			mutate: func(t *testing.T, f adoptionFixture) {
				mustAdoptionUpdate(t, f.store, f.root.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.DrainMemberIDMetadataKey: "other-source"}})
			},
			want: "does not name source",
		},
		{
			name: "missing commit",
			mutate: func(t *testing.T, f adoptionFixture) {
				mustAdoptionUpdate(t, f.store, f.source.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.ImplementationCommitMetadataKey: ""}})
			},
			want: beadmeta.ImplementationCommitMetadataKey,
		},
		{
			name: "invalid commit",
			mutate: func(t *testing.T, f adoptionFixture) {
				mustAdoptionUpdate(t, f.store, f.source.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.ImplementationCommitMetadataKey: "not-a-commit"}})
			},
			want: "40 hexadecimal",
		},
		{
			name: "relative summary",
			mutate: func(t *testing.T, f adoptionFixture) {
				mustAdoptionUpdate(t, f.store, f.source.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.ImplementationSummaryPathMetadataKey: "summary.md"}})
			},
			want: "must be absolute",
		},
		{
			name: "summary outside trusted work roots",
			mutate: func(t *testing.T, f adoptionFixture) {
				outside := filepath.Join(t.TempDir(), "summary.md")
				if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				mustAdoptionUpdate(t, f.store, f.source.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.ImplementationSummaryPathMetadataKey: outside}})
			},
			want: "escapes trusted work roots",
		},
		{
			name: "summary missing",
			mutate: func(t *testing.T, f adoptionFixture) {
				missing := filepath.Join(filepath.Dir(f.summary), "missing.md")
				mustAdoptionUpdate(t, f.store, f.source.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.ImplementationSummaryPathMetadataKey: missing}})
			},
			want: "reading summary evidence",
		},
		{
			name: "attempt still assigned",
			mutate: func(t *testing.T, f adoptionFixture) {
				assignee := "live-worker"
				mustAdoptionUpdate(t, f.store, f.attempt.ID, beads.UpdateOpts{Assignee: &assignee})
			},
			want: "still assigned",
		},
		{
			name: "attempt worktree mismatch",
			mutate: func(t *testing.T, f adoptionFixture) {
				mustAdoptionUpdate(t, f.store, f.attempt.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.WorkDirMetadataKey: filepath.Join(t.TempDir(), "different")}})
			},
			want: "work directory",
		},
		{
			name: "conflicting prior adoption",
			mutate: func(t *testing.T, f adoptionFixture) {
				mustAdoptionUpdate(t, f.store, f.attempt.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.AdoptionSourceIDMetadataKey: "other-source"}})
			},
			want: "already claimed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdoptionFixture(t)
			tc.mutate(t, f)
			_, err := AdoptRalphAttempt(f.store, f.store, f.control.ID, adoptionOptions(f))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
			attempt, getErr := f.store.Get(f.attempt.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if attempt.Status == "closed" {
				t.Fatal("invalid evidence closed the attempt")
			}
		})
	}
}

func TestAdoptRalphAttemptMatchesSimpleAttemptControlLineage(t *testing.T) {
	f := newAdoptionFixture(t)
	mustAdoptionUpdate(t, f.store, f.attempt.ID, beads.UpdateOpts{Metadata: map[string]string{
		beadmeta.RalphStepIDMetadataKey: "",
		beadmeta.ControlForMetadataKey:  f.control.ID,
	}})

	result, err := AdoptRalphAttempt(f.store, f.store, f.control.ID, adoptionOptions(f))
	if err != nil {
		t.Fatalf("AdoptRalphAttempt: %v", err)
	}
	if result.AttemptID != f.attempt.ID {
		t.Fatalf("attempt id = %q, want %q", result.AttemptID, f.attempt.ID)
	}
}

func TestAdoptRalphAttemptReadsSourceFromItsOwningStore(t *testing.T) {
	f := newAdoptionFixture(t)
	sourceStore := atomicAdoptionStore{MemStore: beads.NewMemStore()}
	mustCreateAdoptionBead(t, sourceStore, f.source)
	mustAdoptionUpdate(t, f.store, f.source.ID, beads.UpdateOpts{Metadata: map[string]string{
		beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail,
	}})

	if _, err := AdoptRalphAttempt(f.store, sourceStore, f.control.ID, adoptionOptions(f)); err != nil {
		t.Fatalf("AdoptRalphAttempt with split source store: %v", err)
	}
}

func TestAdoptRalphAttemptRollsBackWhenSplitStoreSourceChanges(t *testing.T) {
	attemptStore, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := attemptStore.(*beads.SQLiteStore).CloseStore(); closeErr != nil {
			t.Errorf("CloseStore: %v", closeErr)
		}
	})
	f := newAdoptionFixtureWithStore(t, attemptStore)
	sourceBase := atomicAdoptionStore{MemStore: beads.NewMemStore()}
	mustCreateAdoptionBead(t, sourceBase, f.source)
	mustAdoptionUpdate(t, f.store, f.source.ID, beads.UpdateOpts{Metadata: map[string]string{
		beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail,
	}})
	sourceStore := &adoptionSourceRaceStore{Store: sourceBase, sourceID: f.source.ID}
	sourceStore.afterCurrentGet = func() {
		mustAdoptionUpdate(t, sourceBase, f.source.ID, beads.UpdateOpts{Metadata: map[string]string{
			beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail,
		}})
	}

	_, err = AdoptRalphAttempt(f.store, sourceStore, f.control.ID, adoptionOptions(f))
	if err == nil || !strings.Contains(err.Error(), "changed concurrently") {
		t.Fatalf("error = %v, want concurrent source change", err)
	}
	attempt, getErr := f.store.Get(f.attempt.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if attempt.Status == "closed" || attempt.Metadata[beadmeta.AdoptionSourceIDMetadataKey] != "" {
		t.Fatalf("failed split-store adoption mutated attempt: %+v", attempt)
	}
}

func TestAdoptRalphAttemptRejectsAmbiguousOpenAttempts(t *testing.T) {
	f := newAdoptionFixture(t)
	mustCreateAdoptionBead(t, f.store, beads.Bead{
		Title: "competing attempt",
		Metadata: map[string]string{
			beadmeta.LogicalBeadIDMetadataKey: f.control.ID,
			beadmeta.RootBeadIDMetadataKey:    f.root.ID,

			beadmeta.RalphStepIDMetadataKey: "implement",
			beadmeta.AttemptMetadataKey:     "2",
			beadmeta.WorkDirMetadataKey:     filepath.Dir(filepath.Dir(f.summary)),
		},
	})
	_, err := AdoptRalphAttempt(f.store, f.store, f.control.ID, adoptionOptions(f))
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error = %v, want ambiguous", err)
	}
}

func TestAdoptRalphAttemptRefusesConcurrentAssignmentWithoutLeavingReservation(t *testing.T) {
	f := newAdoptionFixture(t)
	racer := &adoptionTxRaceStore{atomicAdoptionStore: f.store.(atomicAdoptionStore)}
	racer.beforeTx = func() {
		assignee := "worker-1"
		if err := f.store.Update(f.attempt.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
			t.Fatal(err)
		}
	}

	_, err := AdoptRalphAttempt(racer, racer, f.control.ID, adoptionOptions(f))
	if err == nil || !strings.Contains(err.Error(), "assigned concurrently") {
		t.Fatalf("error = %v", err)
	}
	attempt, getErr := f.store.Get(f.attempt.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if attempt.Status == "closed" || attempt.Assignee != "worker-1" {
		t.Fatalf("attempt = %+v, want open and assigned worker-1", attempt)
	}
	if claimed := attempt.Metadata[beadmeta.AdoptionSourceIDMetadataKey]; claimed != "" {
		t.Fatalf("failed adoption left source reservation %q", claimed)
	}
}

func TestAdoptRalphAttemptRevalidatesRootAndSourceInsideTransaction(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, f adoptionFixture)
		want   string
	}{
		{
			name: "root drain member changed",
			mutate: func(t *testing.T, f adoptionFixture) {
				mustAdoptionUpdate(t, f.store, f.root.ID, beads.UpdateOpts{Metadata: map[string]string{
					beadmeta.DrainMemberIDMetadataKey: "other-source",
				}})
			},
			want: "does not name source",
		},
		{
			name: "source outcome changed",
			mutate: func(t *testing.T, f adoptionFixture) {
				mustAdoptionUpdate(t, f.store, f.source.ID, beads.UpdateOpts{Metadata: map[string]string{
					beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail,
				}})
			},
			want: "outcome",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdoptionFixture(t)
			racer := &adoptionTxRaceStore{atomicAdoptionStore: f.store.(atomicAdoptionStore)}
			racer.beforeTx = func() { tc.mutate(t, f) }

			_, err := AdoptRalphAttempt(racer, racer, f.control.ID, adoptionOptions(f))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
			attempt, getErr := f.store.Get(f.attempt.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if attempt.Status == "closed" || attempt.Metadata[beadmeta.AdoptionSourceIDMetadataKey] != "" {
				t.Fatalf("failed adoption mutated attempt: %+v", attempt)
			}
		})
	}
}

func TestAdoptRalphAttemptRequiresAtomicTransaction(t *testing.T) {
	f := newAdoptionFixture(t)
	withoutAtomic := beads.NewMemStore()
	for _, bead := range []beads.Bead{f.source, f.root, f.control, f.attempt} {
		created, createErr := withoutAtomic.Create(bead)
		if createErr != nil {
			t.Fatal(createErr)
		}
		if bead.Status == "closed" {
			if closeErr := withoutAtomic.Close(created.ID); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
	}
	_, err := AdoptRalphAttempt(withoutAtomic, withoutAtomic, f.control.ID, adoptionOptions(f))
	if err == nil || !strings.Contains(err.Error(), "atomic transactions") {
		t.Fatalf("without atomic transaction error = %v", err)
	}
}

func mustAdoptionUpdate(t *testing.T, store beads.Store, id string, opts beads.UpdateOpts) {
	t.Helper()
	if err := store.Update(id, opts); err != nil {
		t.Fatal(err)
	}
}
