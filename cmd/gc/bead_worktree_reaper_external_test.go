package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func addExternalReapWorktree(t *testing.T, rigRoot, parent, beadID string) string {
	t.Helper()
	worktreePath := filepath.Join(parent, beadID)
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		t.Fatalf("mkdir external worktree parent: %v", err)
	}
	mustGit(t, rigRoot, "worktree", "add", "-b", "external-"+beadID, worktreePath)
	backdateWorktreeGitFile(t, worktreePath, 24*time.Hour)
	return worktreePath
}

func TestReapClosedBeadWorktrees_ReapsRegisteredExternalWorktreeFromClosedBeadMetadata(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	worktreePath := addExternalReapWorktree(t, rigRoot, filepath.Join(t.TempDir(), "formula-worktrees"), "ga-ext001")
	store := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "ga-ext001",
		Status: "closed",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey: worktreePath,
		},
	}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &stderr)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-ext001" {
		t.Fatalf("Reaped = %+v, want exactly ga-ext001\nstderr:\n%s", report.Reaped, stderr.String())
	}
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("external worktree %s still present after reap (stat err=%v)", worktreePath, err)
	}
}

func TestReapClosedBeadWorktrees_RejectsUnsafeExternalCandidates(t *testing.T) {
	t.Run("rig root", func(t *testing.T) {
		cityPath, rigRoot := initReapRig(t)
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID:     "ga-root01",
			Status: "closed",
			Metadata: map[string]string{
				beadmeta.WorkDirMetadataKey: rigRoot,
			},
		}}, nil)
		injectLiveness(t, liveWorktreeState{scanned: true})

		report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &bytes.Buffer{})

		if len(report.Reaped) != 0 || len(report.Protected) != 0 {
			t.Fatalf("rig root considered for reaping: %+v", report)
		}
		if _, err := os.Stat(rigRoot); err != nil {
			t.Fatalf("rig root was removed or damaged: %v", err)
		}
	})

	t.Run("plain directory", func(t *testing.T) {
		cityPath, rigRoot := initReapRig(t)
		plainDir := filepath.Join(t.TempDir(), "ga-plain01")
		if err := os.MkdirAll(plainDir, 0o755); err != nil {
			t.Fatalf("mkdir plain directory: %v", err)
		}
		store := beads.NewMemStoreFrom(1, []beads.Bead{{
			ID:     "ga-plain01",
			Status: "closed",
			Metadata: map[string]string{
				beadmeta.LegacyWorkDirMetadataKey: plainDir,
			},
		}}, nil)
		injectLiveness(t, liveWorktreeState{scanned: true})

		report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &bytes.Buffer{})

		if len(report.Reaped) != 0 || len(report.Protected) != 0 {
			t.Fatalf("plain directory considered for reaping: %+v", report)
		}
		if _, err := os.Stat(plainDir); err != nil {
			t.Fatalf("plain directory was removed: %v", err)
		}
	})

	t.Run("registered external worktree without metadata", func(t *testing.T) {
		cityPath, rigRoot := initReapRig(t)
		worktreePath := addExternalReapWorktree(t, rigRoot, filepath.Join(t.TempDir(), "unowned"), "ga-unowned1")
		store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-unowned1", Status: "closed"}}, nil)
		injectLiveness(t, liveWorktreeState{scanned: true})

		report := reapClosedBeadWorktrees(cityPath, reapTestConfig(rigRoot), map[string]beads.Store{reapTestRigName: store}, nil, false, events.Discard, nil, &bytes.Buffer{})

		if len(report.Reaped) != 0 || len(report.Protected) != 0 {
			t.Fatalf("unowned external worktree considered for reaping: %+v", report)
		}
		if _, err := os.Stat(worktreePath); err != nil {
			t.Fatalf("unowned external worktree was removed: %v", err)
		}
	})
}
