// Package runstatus projects a workflow root and its members into explicit,
// independent lifecycle surfaces. It intentionally does not infer merge success
// from workflow closure or publish success.
package runstatus

import (
	"slices"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// OwnerOpen means the workflow root is open and not owner-held.
	OwnerOpen = "open"
	// OwnerHeld means the workflow root remains under explicit owner authority.
	OwnerHeld = "owner_held"
	// OwnerAwaitingClose means workflow controls completed but an owner-held root remains open.
	OwnerAwaitingClose = "awaiting_owner_close"
	// OwnerClosed means the workflow root is closed.
	OwnerClosed = "closed"

	// ControlsNotStarted means the projection found no control beads.
	ControlsNotStarted = "not_started"
	// ControlsActive means at least one control bead remains non-terminal.
	ControlsActive = "active"
	// ControlsBlocked means at least one control bead is blocked or quarantined.
	ControlsBlocked = "blocked"
	// ControlsFailed means a terminal control records a failing outcome.
	ControlsFailed = "failed"
	// ControlsComplete means every control bead closed without a failing outcome.
	ControlsComplete = "complete"

	// DeliveryPending means no implementation or final delivery artifact is recorded.
	DeliveryPending = "pending"
	// DeliveryImplementationRecorded means implementation evidence exists but no final report is recorded.
	DeliveryImplementationRecorded = "implementation_recorded"
	// DeliveryDelivered means a final report path is recorded.
	DeliveryDelivered = "delivered"

	// PublishPending means the workflow has not recorded a publish result.
	PublishPending = "pending"
	// MergeUnreported means the workflow has not recorded a merge result.
	MergeUnreported = "unreported"
)

// Status is the aggregate status of one workflow run.
type Status struct {
	SchemaVersion string          `json:"schema_version"`
	OK            bool            `json:"ok"`
	RootID        string          `json:"root_id"`
	Title         string          `json:"title"`
	Formula       string          `json:"formula,omitempty"`
	Owner         OwnerLifecycle  `json:"owner_lifecycle"`
	Controls      ControlProgress `json:"workflow_control"`
	Delivery      DeliveryStatus  `json:"delivery"`
	Publish       PublishResult   `json:"publish"`
	Merge         MergeResult     `json:"merge"`
}

// OwnerLifecycle reports the root's persisted owner-held lifecycle separately
// from workflow-control completion.
type OwnerLifecycle struct {
	Status             string `json:"status"`
	RootStatus         string `json:"root_status"`
	Owned              bool   `json:"owned"`
	AwaitingOwnerClose bool   `json:"awaiting_owner_close"`
}

// ControlProgress reports only controller-executed infrastructure nodes.
type ControlProgress struct {
	Status     string   `json:"status"`
	Total      int      `json:"total"`
	Open       int      `json:"open"`
	Closed     int      `json:"closed"`
	Blocked    int      `json:"blocked"`
	Failed     int      `json:"failed"`
	OpenIDs    []string `json:"open_ids"`
	BlockedIDs []string `json:"blocked_ids"`
	FailedIDs  []string `json:"failed_ids"`
}

// DeliveryStatus reports implementation and final-report evidence without
// treating publication or merge as implied.
type DeliveryStatus struct {
	Status                    string `json:"status"`
	Outcome                   string `json:"outcome,omitempty"`
	ImplementationSummaryPath string `json:"implementation_summary_path,omitempty"`
	FinalReportPath           string `json:"final_report_path,omitempty"`
}

// PublishResult reports the pack-authored publish result fields verbatim.
type PublishResult struct {
	Status            string `json:"status"`
	Action            string `json:"action,omitempty"`
	Reason            string `json:"reason,omitempty"`
	ArtifactPath      string `json:"artifact_path,omitempty"`
	RemoteStatus      string `json:"remote_status,omitempty"`
	PublishedCommit   string `json:"published_commit,omitempty"`
	PullRequestURL    string `json:"pull_request_url,omitempty"`
	PullRequestNumber string `json:"pull_request_number,omitempty"`
}

// MergeResult reports merge or pull-request evidence independently of publish.
type MergeResult struct {
	Status         string `json:"status"`
	Commit         string `json:"commit,omitempty"`
	Target         string `json:"target,omitempty"`
	PullRequestURL string `json:"pull_request_url,omitempty"`
}

// Build derives status from a workflow root and the beads whose
// gc.root_bead_id points at it. Root metadata takes precedence over member
// metadata because terminal pack stages are required to copy their result to
// the workflow root; member lookup preserves visibility for older runs.
func Build(root beads.Bead, members []beads.Bead) Status {
	controls := buildControlProgress(members)
	owner := buildOwnerLifecycle(root, controls)
	lookup := newMetadataLookup(root, members)
	return Status{
		SchemaVersion: "1",
		OK:            true,
		RootID:        root.ID,
		Title:         root.Title,
		Formula:       firstNonEmpty(root.Metadata[beadmeta.FormulaNameMetadataKey], root.Metadata[beadmeta.FormulaMetadataKey]),
		Owner:         owner,
		Controls:      controls,
		Delivery: DeliveryStatus{
			Status:                    deliveryState(lookup.value(beadmeta.BuildImplementationSummaryPathMetadataKey), lookup.value(beadmeta.BuildFinalReportPathMetadataKey)),
			Outcome:                   lookup.value(beadmeta.OutcomeMetadataKey),
			ImplementationSummaryPath: lookup.value(beadmeta.BuildImplementationSummaryPathMetadataKey),
			FinalReportPath:           lookup.value(beadmeta.BuildFinalReportPathMetadataKey),
		},
		Publish: PublishResult{
			Status:            firstNonEmpty(lookup.value(beadmeta.BuildPublishStatusMetadataKey), PublishPending),
			Action:            lookup.value(beadmeta.BuildPublishActionMetadataKey),
			Reason:            lookup.value(beadmeta.BuildPublishReasonMetadataKey),
			ArtifactPath:      lookup.value(beadmeta.BuildPublishArtifactPathMetadataKey),
			RemoteStatus:      lookup.value(beadmeta.BuildPublishRemoteStatusMetadataKey),
			PublishedCommit:   lookup.value(beadmeta.BuildPublishedCommitMetadataKey),
			PullRequestURL:    lookup.value("pr_url"),
			PullRequestNumber: lookup.value("pr_number"),
		},
		Merge: MergeResult{
			Status:         firstNonEmpty(lookup.value("merge_result"), MergeUnreported),
			Commit:         lookup.value("merged_sha"),
			Target:         lookup.value("merged_target"),
			PullRequestURL: lookup.value("pr_url"),
		},
	}
}

func buildOwnerLifecycle(root beads.Bead, controls ControlProgress) OwnerLifecycle {
	owned := slices.Contains(root.Labels, "owned")
	status := OwnerOpen
	awaiting := false
	switch {
	case root.Status == "closed":
		status = OwnerClosed
	case owned && controls.Total > 0 && controls.Open == 0:
		status = OwnerAwaitingClose
		awaiting = true
	case owned:
		status = OwnerHeld
	}
	return OwnerLifecycle{Status: status, RootStatus: root.Status, Owned: owned, AwaitingOwnerClose: awaiting}
}

func buildControlProgress(members []beads.Bead) ControlProgress {
	result := ControlProgress{
		Status:     ControlsNotStarted,
		OpenIDs:    []string{},
		BlockedIDs: []string{},
		FailedIDs:  []string{},
	}
	for _, member := range members {
		if !beadmeta.IsControlKind(member.Metadata[beadmeta.KindMetadataKey]) {
			continue
		}
		result.Total++
		if member.Status == "closed" {
			result.Closed++
		} else {
			result.Open++
			result.OpenIDs = append(result.OpenIDs, member.ID)
		}
		if member.Status == "blocked" || member.Status == "quarantined" {
			result.Blocked++
			result.BlockedIDs = append(result.BlockedIDs, member.ID)
		}
		if member.Metadata[beadmeta.OutcomeMetadataKey] == beadmeta.OutcomeFail {
			result.Failed++
			result.FailedIDs = append(result.FailedIDs, member.ID)
		}
	}
	sort.Strings(result.OpenIDs)
	sort.Strings(result.BlockedIDs)
	sort.Strings(result.FailedIDs)
	switch {
	case result.Total == 0:
		result.Status = ControlsNotStarted
	case result.Failed > 0:
		result.Status = ControlsFailed
	case result.Blocked > 0:
		result.Status = ControlsBlocked
	case result.Open > 0:
		result.Status = ControlsActive
	default:
		result.Status = ControlsComplete
	}
	return result
}

func deliveryState(implementationSummary, finalReport string) string {
	if strings.TrimSpace(finalReport) != "" {
		return DeliveryDelivered
	}
	if strings.TrimSpace(implementationSummary) != "" {
		return DeliveryImplementationRecorded
	}
	return DeliveryPending
}

type metadataLookup struct {
	beads []beads.Bead
}

func newMetadataLookup(root beads.Bead, members []beads.Bead) metadataLookup {
	ordered := append([]beads.Bead(nil), members...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].UpdatedAt.Equal(ordered[j].UpdatedAt) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].UpdatedAt.After(ordered[j].UpdatedAt)
	})
	return metadataLookup{beads: append([]beads.Bead{root}, ordered...)}
}

func (l metadataLookup) value(key string) string {
	for _, bead := range l.beads {
		if value := strings.TrimSpace(bead.Metadata[key]); value != "" {
			return value
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
