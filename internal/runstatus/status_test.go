package runstatus

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestBuildDistinguishesOwnerControlDeliveryPublishAndMerge(t *testing.T) {
	root := beads.Bead{
		ID:     "gc-root",
		Title:  "Build product",
		Status: "open",
		Labels: []string{"owned"},
		Metadata: map[string]string{
			beadmeta.FormulaNameMetadataKey:        "build-basic",
			"gc.build.implementation_summary_path": "/rig/.gc/artifacts/implementation-summary.md",
			"gc.build.final_report_path":           "/rig/.gc/artifacts/factory-run.md",
			"gc.build.publish_status":              "published",
			"gc.build.publish_action":              "pr",
			"gc.build.publish_artifact_path":       "/rig/.gc/artifacts/publish.md",
			"gc.build.publish_reason":              "pr_opened",
			"gc.build.publish_remote_status":       "origin_present",
			"merge_result":                         "pull_request",
			"pr_url":                               "https://example.test/pull/42",
		},
	}
	members := []beads.Bead{
		{ID: "gc-ralph", Status: "closed", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindRalph, beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}},
		{ID: "gc-finalize", Status: "closed", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflowFinalize, beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}},
		{ID: "gc-task", Status: "closed", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindTask}},
	}

	got := Build(root, members)
	if got.Owner.Status != OwnerAwaitingClose || !got.Owner.Owned || !got.Owner.AwaitingOwnerClose {
		t.Fatalf("owner = %+v", got.Owner)
	}
	if got.Controls.Status != ControlsComplete || got.Controls.Total != 2 || got.Controls.Closed != 2 || got.Controls.Open != 0 {
		t.Fatalf("controls = %+v", got.Controls)
	}
	if got.Delivery.Status != DeliveryDelivered || got.Delivery.FinalReportPath != "/rig/.gc/artifacts/factory-run.md" {
		t.Fatalf("delivery = %+v", got.Delivery)
	}
	if got.Publish.Status != "published" || got.Publish.Action != "pr" || got.Publish.Reason != "pr_opened" {
		t.Fatalf("publish = %+v", got.Publish)
	}
	if got.Merge.Status != "pull_request" || got.Merge.PullRequestURL != "https://example.test/pull/42" {
		t.Fatalf("merge = %+v", got.Merge)
	}
}

func TestBuildReportsActiveControlsWithoutConflatingDelivery(t *testing.T) {
	root := beads.Bead{ID: "gc-root", Status: "open", Labels: []string{"owned"}, Metadata: map[string]string{}}
	members := []beads.Bead{
		{ID: "gc-closed", Status: "closed", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindRalph, beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}},
		{ID: "gc-open", Status: "open", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflowFinalize}},
	}

	got := Build(root, members)
	if got.Owner.Status != OwnerHeld || got.Owner.AwaitingOwnerClose {
		t.Fatalf("owner = %+v", got.Owner)
	}
	if got.Controls.Status != ControlsActive || got.Controls.Total != 2 || got.Controls.Closed != 1 || got.Controls.Open != 1 {
		t.Fatalf("controls = %+v", got.Controls)
	}
	if !reflect.DeepEqual(got.Controls.OpenIDs, []string{"gc-open"}) {
		t.Fatalf("open control ids = %v", got.Controls.OpenIDs)
	}
	if got.Delivery.Status != DeliveryPending || got.Publish.Status != PublishPending || got.Merge.Status != MergeUnreported {
		t.Fatalf("delivery=%+v publish=%+v merge=%+v", got.Delivery, got.Publish, got.Merge)
	}
}

func TestBuildReportsControlFailureAndClosedOwner(t *testing.T) {
	root := beads.Bead{ID: "gc-root", Status: "closed", Labels: []string{"owned"}, Metadata: map[string]string{
		"gc.build.publish_status": "noop",
		"gc.build.publish_action": "noop",
		"gc.build.publish_reason": "push=false_open_pr=false",
	}}
	members := []beads.Bead{
		{ID: "gc-failed", Status: "closed", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindRalph, beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail}},
	}

	got := Build(root, members)
	if got.Owner.Status != OwnerClosed || got.Owner.AwaitingOwnerClose {
		t.Fatalf("owner = %+v", got.Owner)
	}
	if got.Controls.Status != ControlsFailed || !reflect.DeepEqual(got.Controls.FailedIDs, []string{"gc-failed"}) {
		t.Fatalf("controls = %+v", got.Controls)
	}
	if got.Publish.Status != "noop" || got.Merge.Status != MergeUnreported {
		t.Fatalf("publish=%+v merge=%+v", got.Publish, got.Merge)
	}
}

func TestBuildReportsFailedTerminalControlsAwaitingOwnerClose(t *testing.T) {
	root := beads.Bead{ID: "gc-root", Status: "open", Labels: []string{"owned"}, Metadata: map[string]string{}}
	members := []beads.Bead{
		{ID: "gc-failed", Status: "closed", Metadata: map[string]string{
			beadmeta.KindMetadataKey:    beadmeta.KindRalph,
			beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail,
		}},
		{ID: "gc-finalize", Status: "closed", Metadata: map[string]string{
			beadmeta.KindMetadataKey:    beadmeta.KindWorkflowFinalize,
			beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail,
		}},
	}

	got := Build(root, members)
	if got.Owner.Status != OwnerAwaitingClose || !got.Owner.AwaitingOwnerClose {
		t.Fatalf("owner = %+v, want awaiting owner close", got.Owner)
	}
	if got.Controls.Status != ControlsFailed || got.Controls.Open != 0 || got.Controls.Closed != 2 {
		t.Fatalf("controls = %+v, want terminal failure", got.Controls)
	}
}

func TestBuildReadsTerminalResultsFromWorkflowMembersWhenRootLacksCopies(t *testing.T) {
	root := beads.Bead{ID: "gc-root", Status: "open", Metadata: map[string]string{}}
	members := []beads.Bead{
		{ID: "gc-publish", Status: "closed", Metadata: map[string]string{
			"gc.build.publish_status": "failed",
			"gc.build.publish_action": "failed",
			"gc.build.publish_reason": "remote_rejected",
		}},
		{ID: "gc-merge", Status: "closed", Metadata: map[string]string{
			"merge_result":  "merged",
			"merged_sha":    "abc123",
			"merged_target": "main",
		}},
	}

	got := Build(root, members)
	if got.Publish.Status != "failed" || got.Publish.Reason != "remote_rejected" {
		t.Fatalf("publish = %+v", got.Publish)
	}
	if got.Merge.Status != "merged" || got.Merge.Commit != "abc123" || got.Merge.Target != "main" {
		t.Fatalf("merge = %+v", got.Merge)
	}
}
