package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runstatus"
)

func TestRunStatusCommandReportsIndependentLifecycleSurfaces(t *testing.T) {
	root := beads.Bead{ID: "gc-root", Title: "Build product", Status: "open", Labels: []string{"owned"}, Metadata: map[string]string{
		beadmeta.FormulaNameMetadataKey: "build-basic",
		"gc.build.final_report_path":    "/rig/factory-run.md",
		"gc.build.publish_status":       "published",
		"gc.build.publish_action":       "pr",
		"merge_result":                  "pull_request",
		"pr_url":                        "https://example.test/pull/42",
	}}
	members := []beads.Bead{{ID: "gc-finalize", Status: "closed", Metadata: map[string]string{
		beadmeta.KindMetadataKey:    beadmeta.KindWorkflowFinalize,
		beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass,
	}}}
	loader := func(rootID string) (beads.Bead, []beads.Bead, error) {
		if rootID != root.ID {
			t.Fatalf("root id = %q", rootID)
		}
		return root, members, nil
	}

	var stdout, stderr bytes.Buffer
	cmd := newRunStatusCmdWith(&stdout, &stderr, loader)
	cmd.SetArgs([]string{root.ID, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc run status: %v\nstderr=%s", err, stderr.String())
	}
	var got runstatus.Status
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", stdout.String(), err)
	}
	if got.RootID != root.ID || got.Owner.Status != runstatus.OwnerAwaitingClose || got.Controls.Status != runstatus.ControlsComplete {
		t.Fatalf("status = %+v", got)
	}
	if got.Delivery.Status != runstatus.DeliveryDelivered || got.Publish.Status != "published" || got.Merge.Status != "pull_request" {
		t.Fatalf("delivery=%+v publish=%+v merge=%+v", got.Delivery, got.Publish, got.Merge)
	}
}

func TestRunStatusCommandPlainTextNamesEveryLifecycleSurface(t *testing.T) {
	loader := func(string) (beads.Bead, []beads.Bead, error) {
		return beads.Bead{ID: "gc-root", Title: "Build", Status: "open", Metadata: map[string]string{}}, nil, nil
	}
	var stdout, stderr bytes.Buffer
	cmd := newRunStatusCmdWith(&stdout, &stderr, loader)
	cmd.SetArgs([]string{"gc-root"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Owner lifecycle:", "Workflow control:", "Delivery:", "Publish:", "Merge:"} {
		if !strings.Contains(stdout.String(), label) {
			t.Fatalf("output %q missing %q", stdout.String(), label)
		}
	}
}

func TestRunStatusCommandPropagatesLoadFailure(t *testing.T) {
	loader := func(string) (beads.Bead, []beads.Bead, error) {
		return beads.Bead{}, nil, errors.New("store unavailable")
	}
	var stdout, stderr bytes.Buffer
	cmd := newRunStatusCmdWith(&stdout, &stderr, loader)
	cmd.SetArgs([]string{"gc-root", "--json"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Fatalf("error = %v", err)
	}
}
