package config

import (
	"strings"
	"testing"
)

const fakeBDReadyOutcomes = `#!/bin/sh
case "$FAKE_BD_MODE:$*" in
  assigned-all-fail:*ready*) printf 'bd: unreadable store TOKEN=assigned-secret\n' >&2; exit 23 ;;
  assigned-later:*ready*--assignee=first*) printf 'bd: unreadable store TOKEN=assigned-secret\n' >&2; exit 23 ;;
  assigned-later:*ready*--assignee=second*) printf '[{"id":"assigned-2"}]'; exit 0 ;;
  routed-all-fail:*ready*) printf 'bd: unreadable store TOKEN=routed-secret\n' >&2; exit 24 ;;
  routed-later:*ready*gc.routed_to=*) printf 'bd: unreadable store TOKEN=routed-secret\n' >&2; exit 24 ;;
  routed-later:*ready*gc.run_target=*) printf '[{"id":"routed-2"}]'; exit 0 ;;
  *) printf '[]'; exit 0 ;;
esac
`

func TestSingleStoreAssignedReadyReaderOutcomes(t *testing.T) {
	a := &Agent{Name: "worker"}
	command := a.EffectiveAssignedReadyQueryFor(singleStoreTopology())

	tests := []struct {
		name       string
		mode       string
		env        map[string]string
		wantExit   bool
		wantStdout string
	}{
		{
			name:     "all reads failed",
			mode:     "assigned-all-fail",
			env:      map[string]string{"GC_SESSION_ID": "first"},
			wantExit: true,
		},
		{
			name:       "successful empty read",
			mode:       "assigned-empty",
			env:        map[string]string{"GC_SESSION_ID": "first"},
			wantStdout: "[]",
		},
		{
			name:       "later identity succeeds with work",
			mode:       "assigned-later",
			env:        map[string]string{"GC_SESSION_ID": "first", "GC_SESSION_NAME": "second"},
			wantStdout: "assigned-2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.env["FAKE_BD_MODE"] = tt.mode
			res := runGeneratedQueryWithBD(t, command, tt.env, fakeGCReadyFails, fakeBDReadyOutcomes)
			assertSingleStoreReaderOutcome(t, res, tt.wantExit, tt.wantStdout, "assigned-secret")
		})
	}
}

func TestSingleStoreCompleteWorkQueryReportsAssignedReadyFailureBeforeOriginExit(t *testing.T) {
	a := &Agent{Name: "worker"}
	res := runGeneratedQueryWithBD(t, a.EffectiveWorkQueryFor(singleStoreTopology()), map[string]string{
		"FAKE_BD_MODE":      "assigned-all-fail",
		"GC_SESSION_ID":     "first",
		"GC_SESSION_ORIGIN": "external",
	}, fakeGCReadyFails, fakeBDReadyOutcomes)
	assertSingleStoreReaderOutcome(t, res, true, "", "assigned-secret")
}

func TestSingleStoreRoutedPoolReaderOutcomes(t *testing.T) {
	a := &Agent{Name: "worker"}
	command := a.EffectiveRoutedPoolQueryFor(singleStoreTopology())

	tests := []struct {
		name       string
		mode       string
		wantExit   bool
		wantStdout string
	}{
		{name: "all reads failed", mode: "routed-all-fail", wantExit: true},
		{name: "successful empty reads", mode: "routed-empty", wantStdout: "[]"},
		{name: "later migration tier succeeds with work", mode: "routed-later", wantStdout: "routed-2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runGeneratedQueryWithBD(t, command, map[string]string{
				"FAKE_BD_MODE":      tt.mode,
				"GC_SESSION_ORIGIN": "ephemeral",
			}, fakeGCReadyFails, fakeBDReadyOutcomes)
			assertSingleStoreReaderOutcome(t, res, tt.wantExit, tt.wantStdout, "routed-secret")
		})
	}
}

func assertSingleStoreReaderOutcome(t *testing.T, res generatedQueryResult, wantExit bool, wantStdout, secret string) {
	t.Helper()
	if wantExit {
		if res.exit == 0 {
			t.Fatalf("generated query exited 0 and printed %q after every applicable ready read failed", res.stdout)
		}
		if strings.Contains(res.stdout, "[]") {
			t.Errorf("generated query stdout = %q after reader failure; [] launders the failure as no work", res.stdout)
		}
		if !strings.Contains(res.stderr, "ready reader failed") {
			t.Errorf("generated query stderr = %q, want a safe reader failure diagnosis", res.stderr)
		}
		if strings.Contains(res.stderr, secret) {
			t.Errorf("generated query stderr exposed the fake reader secret: %q", res.stderr)
		}
		return
	}
	if res.exit != 0 {
		t.Fatalf("generated query exited %d over a later successful read (stderr=%q)", res.exit, res.stderr)
	}
	if !strings.Contains(res.stdout, wantStdout) {
		t.Errorf("generated query stdout = %q, want it to contain %q", res.stdout, wantStdout)
	}
}
