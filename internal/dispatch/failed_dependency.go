package dispatch

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// FilterFailedDependencyCandidates removes ordinary graph work whose blocking
// dependency terminated with gc.outcome=fail. Such a dependency is structurally
// resolved to the beads backend, but it is not a successful prerequisite: the
// candidate is terminally failed instead of being handed to an agent.
//
// Control kinds are deliberately transparent to this gate. Retry, drain,
// scope-check, and workflow-finalize controls exist to interpret terminal
// outcomes, so treating their failed blockers like ordinary prerequisites would
// disable the workflow's failure-handling path.
func FilterFailedDependencyCandidates(store beads.Store, candidates []beads.Bead) ([]beads.Bead, error) {
	if store == nil {
		return nil, fmt.Errorf("failed-dependency gate: store is nil")
	}
	kept := make([]beads.Bead, 0, len(candidates))
	for _, candidate := range candidates {
		closed, err := closeAfterFailedDependency(store, candidate)
		if err != nil {
			return nil, fmt.Errorf("failed-dependency gate for %s: %w", candidate.ID, err)
		}
		if !closed {
			kept = append(kept, candidate)
		}
	}
	return kept, nil
}

func closeAfterFailedDependency(store beads.Store, candidate beads.Bead) (bool, error) {
	if !ordinaryGraphCandidate(candidate) {
		return false, nil
	}
	failure, found, err := failedBlockingDependency(store, candidate.ID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}

	// gc.outcome=fail is both the ordinary terminal outcome and the durable
	// fail marker. NativeDoltStore cannot provide revision-CAS, but it does
	// provide metadata value-CAS, so claiming this empty outcome is atomic
	// across processes on every supported graph store. Once it lands, every
	// ready path continues the same idempotent close and no path emits the row.
	outcome, err := beads.ApplyMetadataCAS(
		store,
		candidate.ID,
		beadmeta.OutcomeMetadataKey,
		"",
		beadmeta.OutcomeFail,
	)
	if err != nil {
		return false, err
	}
	switch outcome {
	case beads.MetadataCASSwapped, beads.MetadataCASAlreadyNext:
		// We own, or are helping finish, the same terminal failure.
	case beads.MetadataCASConflict:
		current, getErr := store.Get(candidate.ID)
		if getErr != nil {
			return false, getErr
		}
		if current.Status == "closed" {
			return true, nil
		}
		return false, fmt.Errorf("terminal outcome marker conflict: current outcome %q", current.Metadata[beadmeta.OutcomeMetadataKey])
	default:
		return false, fmt.Errorf("unsupported metadata CAS outcome %q", outcome)
	}

	status := "closed"
	metadata := failedDependencyMetadata(failure)
	if err := store.Update(candidate.ID, beads.UpdateOpts{Status: &status, Metadata: metadata}); err != nil {
		return false, err
	}
	closed, err := store.Get(candidate.ID)
	if err != nil {
		return false, fmt.Errorf("verifying terminal close: %w", err)
	}
	if closed.Status == "closed" {
		return true, nil
	}
	if err := store.Close(candidate.ID); err != nil {
		return false, err
	}
	return true, nil
}

func ordinaryGraphCandidate(candidate beads.Bead) bool {
	return candidate.Status == "open" &&
		strings.TrimSpace(candidate.Metadata[beadmeta.RootBeadIDMetadataKey]) != "" &&
		!beadmeta.IsControlKind(strings.TrimSpace(candidate.Metadata[beadmeta.KindMetadataKey]))
}

func failedBlockingDependency(store beads.Store, candidateID string) (beads.Bead, bool, error) {
	deps, err := store.DepList(candidateID, "down")
	if err != nil {
		return beads.Bead{}, false, err
	}
	var failed beads.Bead
	found := false
	for _, dep := range deps {
		if !beads.IsReadyBlockingDependencyType(dep.Type) || strings.TrimSpace(dep.DependsOnID) == "" {
			continue
		}
		blocker, err := store.Get(dep.DependsOnID)
		if err != nil {
			return beads.Bead{}, false, err
		}
		if blocker.Status == "closed" &&
			strings.TrimSpace(blocker.Metadata[beadmeta.OutcomeMetadataKey]) == beadmeta.OutcomeFail &&
			(!found || blocker.ID < failed.ID) {
			failed = blocker
			found = true
		}
	}
	if !found {
		return beads.Bead{}, false, nil
	}
	return failed, true, nil
}

func failedDependencyMetadata(blocker beads.Bead) map[string]string {
	subject := strings.TrimSpace(blocker.Metadata[beadmeta.FailureSubjectMetadataKey])
	if subject == "" {
		subject = blocker.ID
	}
	reason := strings.TrimSpace(blocker.Metadata[beadmeta.FailureReasonMetadataKey])
	if reason == "" {
		reason = "failed_dependency"
	}
	metadata := map[string]string{
		beadmeta.OutcomeMetadataKey:        beadmeta.OutcomeFail,
		beadmeta.FailureSubjectMetadataKey: subject,
		beadmeta.FailureReasonMetadataKey:  reason,
	}
	for _, key := range []string{beadmeta.FailureClassMetadataKey, beadmeta.FailureOwnerMetadataKey} {
		if value := strings.TrimSpace(blocker.Metadata[key]); value != "" {
			metadata[key] = value
		}
	}
	return metadata
}
