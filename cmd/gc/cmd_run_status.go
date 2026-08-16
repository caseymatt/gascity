package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runstatus"
	"github.com/spf13/cobra"
)

type runStatusLoadFunc func(string) (beads.Bead, []beads.Bead, error)

func newRunCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Inspect formula run state",
	}
	cmd.AddCommand(newRunStatusCmd(stdout, stderr))
	return cmd
}

func newRunStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	return newRunStatusCmdWith(stdout, stderr, func(rootID string) (beads.Bead, []beads.Bead, error) {
		return loadRunStatusInput(stderr, rootID)
	})
}

func newRunStatusCmdWith(stdout, stderr io.Writer, load runStatusLoadFunc) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status <workflow-root-id>",
		Short: "Show owner, control, delivery, publish, and merge state",
		Long: `Show one formula run without collapsing distinct lifecycle boundaries.

Owner lifecycle reports whether an owned root remains open after its graph is
complete. Workflow control reports the controller-executed control beads.
Delivery reports implementation/final-report evidence. Publish and merge report
only their explicit persisted results; neither is inferred from workflow
closure or from the other surface.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			root, members, err := load(args[0])
			if err != nil {
				return formulaCommandError(stderr, "gc run status", jsonOutput, err)
			}
			status := runstatus.Build(root, members)
			if jsonOutput {
				return json.NewEncoder(stdout).Encode(status)
			}
			if _, err := io.WriteString(stdout, formatRunStatus(status)); err != nil {
				return formulaCommandError(stderr, "gc run status", jsonOutput, fmt.Errorf("writing result: %w", err))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON result")
	return cmd
}

func loadRunStatusInput(stderr io.Writer, rootID string) (beads.Bead, []beads.Bead, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return beads.Bead{}, nil, err
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		return beads.Bead{}, nil, err
	}
	store, _, err := resolveOwningStoreDir(rootID, cfg, cityPath, func(storeDir string) (beads.Store, error) {
		work, openErr := openStoreAtForCity(storeDir, cityPath)
		if openErr != nil {
			return nil, openErr
		}
		return classRoutedStoreForID(cityPath, rootID, work)
	})
	if err != nil {
		return beads.Bead{}, nil, fmt.Errorf("resolving workflow root %q: %w", rootID, err)
	}
	root, err := store.Get(rootID)
	if err != nil {
		return beads.Bead{}, nil, fmt.Errorf("reading workflow root %q: %w", rootID, err)
	}
	if linkedRoot := strings.TrimSpace(root.Metadata[beadmeta.RootBeadIDMetadataKey]); linkedRoot != "" && linkedRoot != root.ID {
		return beads.Bead{}, nil, fmt.Errorf("bead %q is a workflow member of root %q, not a workflow root", root.ID, linkedRoot)
	}
	if kind := strings.TrimSpace(root.Metadata[beadmeta.KindMetadataKey]); kind != "" && kind != beadmeta.KindWorkflow {
		return beads.Bead{}, nil, fmt.Errorf("bead %q has gc.kind=%q, want %q", root.ID, kind, beadmeta.KindWorkflow)
	}
	members, err := store.ListByMetadata(map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}, 0, beads.IncludeClosed, beads.WithBothTiers)
	if err != nil {
		return beads.Bead{}, nil, fmt.Errorf("listing workflow members for %q: %w", root.ID, err)
	}
	return root, members, nil
}

func formatRunStatus(status runstatus.Status) string {
	lines := []string{fmt.Sprintf("Run: %s (%s)", status.Title, status.RootID)}
	if status.Formula != "" {
		lines = append(lines, "Formula: "+status.Formula)
	}
	lines = append(lines,
		fmt.Sprintf("Owner lifecycle: %s (root=%s, owned=%t)", status.Owner.Status, status.Owner.RootStatus, status.Owner.Owned),
		fmt.Sprintf("Workflow control: %s (%d/%d closed)", status.Controls.Status, status.Controls.Closed, status.Controls.Total),
	)
	if len(status.Controls.OpenIDs) > 0 {
		lines = append(lines, "  Open controls: "+strings.Join(status.Controls.OpenIDs, ", "))
	}
	delivery := "Delivery: " + status.Delivery.Status
	if status.Delivery.FinalReportPath != "" {
		delivery += " (" + status.Delivery.FinalReportPath + ")"
	}
	publish := "Publish: " + status.Publish.Status
	if status.Publish.Action != "" {
		publish += " (action=" + status.Publish.Action + ")"
	}
	if status.Publish.Reason != "" {
		publish += " reason=" + status.Publish.Reason
	}
	if status.Publish.PullRequestURL != "" {
		publish += " " + status.Publish.PullRequestURL
	}
	merge := "Merge: " + status.Merge.Status
	if status.Merge.Target != "" {
		merge += " (target=" + status.Merge.Target + ")"
	}
	if status.Merge.PullRequestURL != "" {
		merge += " " + status.Merge.PullRequestURL
	}
	lines = append(lines, delivery, publish, merge)
	return strings.Join(lines, "\n") + "\n"
}
