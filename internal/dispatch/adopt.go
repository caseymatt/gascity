package dispatch

import (
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

var fullCommitRE = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// AdoptAttemptOptions supplies the reviewed source evidence and audit identity
// for adopting an externally completed result into an open Ralph attempt.
type AdoptAttemptOptions struct {
	SourceID  string
	Actor     string
	Reason    string
	AdoptedAt time.Time
	FS        fsys.FS
}

// AdoptAttemptResult identifies the attempt changed by AdoptRalphAttempt.
type AdoptAttemptResult struct {
	ControlID      string `json:"control_id"`
	AttemptID      string `json:"attempt_id"`
	SourceID       string `json:"source_id"`
	AlreadyApplied bool   `json:"already_applied"`
}

// AdoptRalphAttempt binds reviewed evidence from a terminal drain source to the
// unique unowned attempt of an open Ralph control. attemptStore owns the
// control, workflow root, and attempt; sourceStore owns the reviewed source and
// may be a distinct work-class store. It never closes the control, workflow
// root, drain, or source. One atomic attempt-store transaction revalidates the
// current control, root, attempt, and source evidence before recording the audit
// fields and closing only the attempt.
func AdoptRalphAttempt(attemptStore, sourceStore beads.Store, controlID string, opts AdoptAttemptOptions) (AdoptAttemptResult, error) {
	if attemptStore == nil {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: nil attempt store")
	}
	if sourceStore == nil {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: nil source store")
	}
	controlID = strings.TrimSpace(controlID)
	if controlID == "" {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: control id is required")
	}
	if err := validateAdoptAttemptOptions(opts); err != nil {
		return AdoptAttemptResult{}, err
	}
	if !beads.StoreSupportsAtomicTx(attemptStore) {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: store %T does not support atomic transactions", attemptStore)
	}

	control, err := attemptStore.Get(controlID)
	if err != nil {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: reading control %q: %w", controlID, err)
	}
	rootID, err := adoptionControlRootID(control)
	if err != nil {
		return AdoptAttemptResult{}, err
	}
	root, err := attemptStore.Get(rootID)
	if err != nil {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: reading workflow root %q: %w", rootID, err)
	}
	sourceID := strings.TrimSpace(opts.SourceID)
	source, err := sourceStore.Get(sourceID)
	if err != nil {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: reading source %q: %w", sourceID, err)
	}
	if err := validateAdoptionSource(root, source); err != nil {
		return AdoptAttemptResult{}, err
	}

	attempts, err := attemptStore.ListByMetadata(map[string]string{beadmeta.LogicalBeadIDMetadataKey: control.ID}, 0, beads.IncludeClosed, beads.WithBothTiers)
	if err != nil {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: listing attempts for control %q: %w", control.ID, err)
	}
	matching := filterAdoptionAttempts(attempts, control)
	attempt, priorApplied := matchingAdoptedAttempt(matching, source)
	if !priorApplied {
		live := liveAdoptionAttempts(matching)
		if len(live) == 0 {
			return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: control %q has no open attempt", control.ID)
		}
		if len(live) > 1 {
			ids := make([]string, 0, len(live))
			for _, candidate := range live {
				ids = append(ids, candidate.ID)
			}
			sort.Strings(ids)
			return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: control %q has ambiguous open attempts: %s", control.ID, strings.Join(ids, ", "))
		}
		attempt = live[0]
	}
	if !priorApplied && control.Status == "closed" {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: control %q is already closed without this adoption", control.ID)
	}
	if strings.TrimSpace(attempt.Assignee) != "" {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: attempt %q is still assigned to %q", attempt.ID, attempt.Assignee)
	}
	if claimed := strings.TrimSpace(attempt.Metadata[beadmeta.AdoptionSourceIDMetadataKey]); claimed != "" && claimed != source.ID {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: attempt %q is already claimed by source %q", attempt.ID, claimed)
	}
	if err := validateAdoptionEvidence(opts.FS, root, attempt, source); err != nil {
		return AdoptAttemptResult{}, err
	}

	alreadyApplied := false
	committedSource := source
	if err := attemptStore.Tx("adopt reviewed workflow attempt", func(tx beads.Tx) error {
		currentControl, err := tx.Get(control.ID)
		if err != nil {
			return fmt.Errorf("reading current control: %w", err)
		}
		currentRootID, err := adoptionControlRootID(currentControl)
		if err != nil {
			return err
		}
		if currentRootID != rootID {
			return fmt.Errorf("control root changed concurrently from %q to %q", rootID, currentRootID)
		}
		currentRoot, err := tx.Get(currentRootID)
		if err != nil {
			return fmt.Errorf("reading current workflow root: %w", err)
		}
		currentAttempt, err := tx.Get(attempt.ID)
		if err != nil {
			return fmt.Errorf("reading current attempt: %w", err)
		}
		if !adoptionAttemptMatchesControl(currentAttempt, currentControl) {
			return fmt.Errorf("attempt no longer belongs to control %q", currentControl.ID)
		}
		currentSource, err := adoptionSourceForTransaction(tx, attemptStore, sourceStore, sourceID)
		if err != nil {
			return err
		}
		if err := validateAdoptionSource(currentRoot, currentSource); err != nil {
			return err
		}
		if strings.TrimSpace(currentAttempt.Assignee) != "" {
			return fmt.Errorf("attempt was assigned concurrently to %q", currentAttempt.Assignee)
		}
		if claimed := strings.TrimSpace(currentAttempt.Metadata[beadmeta.AdoptionSourceIDMetadataKey]); claimed != "" && claimed != currentSource.ID {
			return fmt.Errorf("attempt is already claimed by source %q", claimed)
		}
		if err := validateAdoptionEvidence(opts.FS, currentRoot, currentAttempt, currentSource); err != nil {
			return err
		}
		if currentAttempt.Status == "closed" {
			if !adoptionEvidenceMatches(currentAttempt, currentSource) {
				return fmt.Errorf("attempt closed concurrently without the reviewed evidence")
			}
			alreadyApplied = true
			committedSource = currentSource
			return nil
		}
		if currentControl.Status == "closed" {
			return fmt.Errorf("control %q closed concurrently before adoption", currentControl.ID)
		}
		if err := tx.SetMetadataBatch(currentAttempt.ID, adoptionMetadata(currentSource, opts)); err != nil {
			return fmt.Errorf("recording adoption evidence: %w", err)
		}
		if err := tx.Close(currentAttempt.ID); err != nil {
			return fmt.Errorf("closing adopted attempt: %w", err)
		}
		if !sameAdoptionStore(attemptStore, sourceStore) {
			verifiedSource, err := sourceStore.Get(sourceID)
			if err != nil {
				return fmt.Errorf("re-reading source before commit: %w", err)
			}
			if !adoptionSourceSnapshotEqual(currentSource, verifiedSource) {
				return fmt.Errorf("source %q changed concurrently during adoption", sourceID)
			}
		}
		committedSource = currentSource
		return nil
	}); err != nil {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: committing attempt %q: %w", attempt.ID, err)
	}
	if alreadyApplied {
		return AdoptAttemptResult{ControlID: control.ID, AttemptID: attempt.ID, SourceID: committedSource.ID, AlreadyApplied: true}, nil
	}

	committed, err := attemptStore.Get(attempt.ID)
	if err != nil {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: verifying attempt %q: %w", attempt.ID, err)
	}
	if committed.Status != "closed" || !adoptionEvidenceMatches(committed, committedSource) {
		return AdoptAttemptResult{}, fmt.Errorf("adopt ralph attempt: attempt %q did not persist the reviewed terminal evidence", attempt.ID)
	}
	return AdoptAttemptResult{ControlID: control.ID, AttemptID: attempt.ID, SourceID: committedSource.ID}, nil
}

func adoptionControlRootID(control beads.Bead) (string, error) {
	if control.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindRalph {
		return "", fmt.Errorf("adopt ralph attempt: bead %q has gc.kind=%q, want %q", control.ID, control.Metadata[beadmeta.KindMetadataKey], beadmeta.KindRalph)
	}
	rootID := strings.TrimSpace(control.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" {
		return "", fmt.Errorf("adopt ralph attempt: control %q is missing %s", control.ID, beadmeta.RootBeadIDMetadataKey)
	}
	return rootID, nil
}

func adoptionSourceForTransaction(tx beads.Tx, attemptStore, sourceStore beads.Store, sourceID string) (beads.Bead, error) {
	var (
		source beads.Bead
		err    error
	)
	if sameAdoptionStore(attemptStore, sourceStore) {
		source, err = tx.Get(sourceID)
	} else {
		source, err = sourceStore.Get(sourceID)
	}
	if err != nil {
		return beads.Bead{}, fmt.Errorf("reading current source %q: %w", sourceID, err)
	}
	return source, nil
}

func sameAdoptionStore(left, right beads.Store) bool {
	leftType := reflect.TypeOf(left)
	rightType := reflect.TypeOf(right)
	if leftType == nil || leftType != rightType || !leftType.Comparable() {
		return false
	}
	return left == right
}

func adoptionSourceSnapshotEqual(left, right beads.Bead) bool {
	return left.ID == right.ID &&
		left.Status == right.Status &&
		left.UpdatedAt.Equal(right.UpdatedAt) &&
		left.Metadata[beadmeta.OutcomeMetadataKey] == right.Metadata[beadmeta.OutcomeMetadataKey] &&
		adoptionWorkDir(left) == adoptionWorkDir(right) &&
		left.Metadata[beadmeta.ImplementationCommitMetadataKey] == right.Metadata[beadmeta.ImplementationCommitMetadataKey] &&
		left.Metadata[beadmeta.ImplementationSummaryPathMetadataKey] == right.Metadata[beadmeta.ImplementationSummaryPathMetadataKey]
}

func validateAdoptAttemptOptions(opts AdoptAttemptOptions) error {
	if strings.TrimSpace(opts.SourceID) == "" {
		return fmt.Errorf("adopt ralph attempt: source id is required")
	}
	if strings.TrimSpace(opts.Actor) == "" {
		return fmt.Errorf("adopt ralph attempt: actor is required")
	}
	if strings.TrimSpace(opts.Reason) == "" {
		return fmt.Errorf("adopt ralph attempt: reason is required")
	}
	if opts.AdoptedAt.IsZero() {
		return fmt.Errorf("adopt ralph attempt: adoption time is required")
	}
	if opts.FS == nil {
		return fmt.Errorf("adopt ralph attempt: filesystem is required")
	}
	return nil
}

func validateAdoptionSource(root, source beads.Bead) error {
	if source.Status != "closed" {
		return fmt.Errorf("adopt ralph attempt: source %q must be closed, got %q", source.ID, source.Status)
	}
	if source.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		return fmt.Errorf("adopt ralph attempt: source %q outcome is %q, want %q", source.ID, source.Metadata[beadmeta.OutcomeMetadataKey], beadmeta.OutcomePass)
	}
	if got := strings.TrimSpace(root.Metadata[beadmeta.DrainMemberIDMetadataKey]); got != source.ID {
		return fmt.Errorf("adopt ralph attempt: workflow root %q %s=%q does not name source %q", root.ID, beadmeta.DrainMemberIDMetadataKey, got, source.ID)
	}
	return nil
}

func filterAdoptionAttempts(attempts []beads.Bead, control beads.Bead) []beads.Bead {
	out := make([]beads.Bead, 0, len(attempts))
	for _, attempt := range attempts {
		if adoptionAttemptMatchesControl(attempt, control) {
			out = append(out, attempt)
		}
	}
	return out
}

func adoptionAttemptMatchesControl(attempt, control beads.Bead) bool {
	if strings.TrimSpace(attempt.Metadata[beadmeta.LogicalBeadIDMetadataKey]) != control.ID {
		return false
	}
	if attempt.Metadata[beadmeta.RootBeadIDMetadataKey] != control.Metadata[beadmeta.RootBeadIDMetadataKey] {
		return false
	}
	attemptNum, err := strconv.Atoi(strings.TrimSpace(attempt.Metadata[beadmeta.AttemptMetadataKey]))
	if err != nil || attemptNum < 1 {
		return false
	}
	if controlFor := strings.TrimSpace(attempt.Metadata[beadmeta.ControlForMetadataKey]); controlFor != "" {
		return controlIdentitySet(control)[controlFor]
	}
	return strings.TrimSpace(attempt.Metadata[beadmeta.RalphStepIDMetadataKey]) == strings.TrimSpace(control.Metadata[beadmeta.StepIDMetadataKey])
}

func liveAdoptionAttempts(attempts []beads.Bead) []beads.Bead {
	out := make([]beads.Bead, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.Status != "closed" {
			out = append(out, attempt)
		}
	}
	return out
}

func matchingAdoptedAttempt(attempts []beads.Bead, source beads.Bead) (beads.Bead, bool) {
	for _, attempt := range attempts {
		if attempt.Status == "closed" && adoptionEvidenceMatches(attempt, source) {
			return attempt, true
		}
	}
	return beads.Bead{}, false
}

func validateAdoptionEvidence(filesystem fsys.FS, root, attempt, source beads.Bead) error {
	commit := strings.TrimSpace(source.Metadata[beadmeta.ImplementationCommitMetadataKey])
	if commit == "" {
		return fmt.Errorf("adopt ralph attempt: source %q is missing %s", source.ID, beadmeta.ImplementationCommitMetadataKey)
	}
	if !fullCommitRE.MatchString(commit) {
		return fmt.Errorf("adopt ralph attempt: source %q %s must be a full 40 hexadecimal commit", source.ID, beadmeta.ImplementationCommitMetadataKey)
	}
	sourceWorkDir := adoptionWorkDir(source)
	attemptWorkDir := adoptionWorkDir(attempt)
	if sourceWorkDir == "" || attemptWorkDir == "" || filepath.Clean(sourceWorkDir) != filepath.Clean(attemptWorkDir) {
		return fmt.Errorf("adopt ralph attempt: source %q and attempt %q must name the same absolute work directory", source.ID, attempt.ID)
	}
	if !filepath.IsAbs(sourceWorkDir) {
		return fmt.Errorf("adopt ralph attempt: source work directory must be absolute")
	}
	summary := strings.TrimSpace(source.Metadata[beadmeta.ImplementationSummaryPathMetadataKey])
	if summary == "" {
		return fmt.Errorf("adopt ralph attempt: source %q is missing %s", source.ID, beadmeta.ImplementationSummaryPathMetadataKey)
	}
	if !filepath.IsAbs(summary) {
		return fmt.Errorf("adopt ralph attempt: source %q summary path must be absolute", source.ID)
	}
	trustedRoots := []string{sourceWorkDir, adoptionWorkDir(root)}
	if !pathWithinAny(summary, trustedRoots) {
		return fmt.Errorf("adopt ralph attempt: summary evidence %q escapes trusted work roots", summary)
	}
	info, err := filesystem.Lstat(summary)
	if err != nil {
		return fmt.Errorf("adopt ralph attempt: reading summary evidence %q: %w", summary, err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("adopt ralph attempt: summary evidence %q must be a non-empty regular file", summary)
	}
	return nil
}

func adoptionWorkDir(bead beads.Bead) string {
	if value := strings.TrimSpace(bead.Metadata[beadmeta.WorkDirMetadataKey]); value != "" {
		return value
	}
	return strings.TrimSpace(bead.Metadata[beadmeta.LegacyWorkDirMetadataKey])
}

func adoptionMetadata(source beads.Bead, opts AdoptAttemptOptions) map[string]string {
	return map[string]string{
		beadmeta.AdoptedAtMetadataKey:                 opts.AdoptedAt.UTC().Format(time.RFC3339Nano),
		beadmeta.AdoptionActorMetadataKey:             strings.TrimSpace(opts.Actor),
		beadmeta.AdoptionReasonMetadataKey:            strings.TrimSpace(opts.Reason),
		beadmeta.AdoptionSourceIDMetadataKey:          source.ID,
		beadmeta.AdoptionSourceUpdatedAtMetadataKey:   source.UpdatedAt.UTC().Format(time.RFC3339Nano),
		beadmeta.ImplementationCommitMetadataKey:      strings.TrimSpace(source.Metadata[beadmeta.ImplementationCommitMetadataKey]),
		beadmeta.ImplementationSummaryPathMetadataKey: strings.TrimSpace(source.Metadata[beadmeta.ImplementationSummaryPathMetadataKey]),
		beadmeta.OutcomeMetadataKey:                   beadmeta.OutcomePass,
		"close_reason":                                fmt.Sprintf("adopted reviewed source %s: %s", source.ID, strings.TrimSpace(opts.Reason)),
	}
}

func adoptionEvidenceMatches(attempt, source beads.Bead) bool {
	return attempt.Metadata[beadmeta.AdoptionSourceIDMetadataKey] == source.ID &&
		attempt.Metadata[beadmeta.OutcomeMetadataKey] == beadmeta.OutcomePass &&
		attempt.Metadata[beadmeta.ImplementationCommitMetadataKey] == strings.TrimSpace(source.Metadata[beadmeta.ImplementationCommitMetadataKey]) &&
		attempt.Metadata[beadmeta.ImplementationSummaryPathMetadataKey] == strings.TrimSpace(source.Metadata[beadmeta.ImplementationSummaryPathMetadataKey])
}
