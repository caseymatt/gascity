package scripts_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrePushGatePreflightsAndReusesExactTreeReceipt(t *testing.T) {
	fixture := newPrePushGateFixture(t)
	remoteSHA := fixture.remoteMainSHA(t)
	localSHA := fixture.commitGoChange(t, "func Value() int { return 2 }\n")
	input := prePushInput(localSHA, remoteSHA, "refs/heads/main")

	fixture.runHook(t, input, fixture.remotePath, "go version go1.test")
	fixture.runHook(t, input, fixture.remotePath, "go version go1.test")
	if got := fixture.makeCalls(t); got != 1 {
		t.Fatalf("identical tree/toolchain gate executions = %d, want 1", got)
	}
	if got := fixture.guardCalls(t); got != 2 {
		t.Fatalf("ownership guard executions = %d, want 2", got)
	}

	fixture.runHook(t, input, fixture.remotePath, "go version go2.test")
	if got := fixture.makeCalls(t); got != 2 {
		t.Fatalf("gate executions after toolchain change = %d, want 2", got)
	}

	newLocalSHA := fixture.commitGoChange(t, "func Value() int { return 3 }\n")
	fixture.runHook(t, prePushInput(newLocalSHA, remoteSHA, "refs/heads/main"), fixture.remotePath, "go version go2.test")
	if got := fixture.makeCalls(t); got != 3 {
		t.Fatalf("gate executions after tree change = %d, want 3", got)
	}
}

func TestPrePushGateRejectsUnavailableRemoteBeforeOwnershipAndTests(t *testing.T) {
	fixture := newPrePushGateFixture(t)
	remoteSHA := fixture.remoteMainSHA(t)
	localSHA := fixture.commitGoChange(t, "func Value() int { return 2 }\n")
	missingRemote := filepath.Join(t.TempDir(), "missing.git")

	output, err := fixture.hookCommand(
		prePushInput(localSHA, remoteSHA, "refs/heads/main"),
		missingRemote,
		"go version go1.test",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("pre-push hook passed an unavailable publication route:\n%s", output)
	}
	if !strings.Contains(string(output), "preflight") {
		t.Fatalf("pre-push failure did not identify publication preflight:\n%s", output)
	}
	if got := fixture.guardCalls(t); got != 0 {
		t.Fatalf("ownership guard ran %d time(s) after preflight failure, want 0", got)
	}
	if got := fixture.makeCalls(t); got != 0 {
		t.Fatalf("test gate ran %d time(s) after preflight failure, want 0", got)
	}
}

func TestPrePushGateRejectsMissingWorkflowScopeBeforeOwnershipAndTests(t *testing.T) {
	fixture := newPrePushGateFixture(t)
	remoteSHA := fixture.remoteMainSHA(t)
	writeTestFile(t, filepath.Join(fixture.repoPath, ".github", "workflows", "check.yml"), "name: check\n")
	localSHA := fixture.commitGoChange(t, "func Value() int { return 2 }\n")

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fixture.binPath, "git"), fmt.Sprintf(`#!/bin/sh
case "${1-}" in
  fetch) exit 0 ;;
  rev-list) printf 'workflow-commit\n'; exit 0 ;;
  push) exit 0 ;;
esac
exec %q "$@"
`, realGit))
	writeExecutable(t, filepath.Join(fixture.binPath, "gh"), "#!/bin/sh\nprintf 'gist, read:org, repo\\n'\n")

	output, err := fixture.hookCommand(
		prePushInput(localSHA, remoteSHA, "refs/heads/main"),
		"https://github.com/example/repository.git",
		"go version go1.test",
	).CombinedOutput()
	if err == nil {
		t.Fatalf("pre-push hook passed a workflow push without OAuth workflow scope:\n%s", output)
	}
	if !strings.Contains(string(output), "workflow") || !strings.Contains(string(output), "preflight") {
		t.Fatalf("pre-push failure did not identify workflow-scope preflight:\n%s", output)
	}
	if got := fixture.guardCalls(t); got != 0 {
		t.Fatalf("ownership guard ran %d time(s) after workflow preflight failure, want 0", got)
	}
	if got := fixture.makeCalls(t); got != 0 {
		t.Fatalf("test gate ran %d time(s) after workflow preflight failure, want 0", got)
	}
}

func TestPrePushNewBranchUsesConfiguredBaseInsteadOfAssumingGoChanged(t *testing.T) {
	fixture := newPrePushGateFixture(t)
	writeTestFile(t, filepath.Join(fixture.repoPath, "README.md"), "documentation only\n")
	localSHA := fixture.commitAll(t, "docs only")

	fixture.runHook(t, prePushInput(localSHA, strings.Repeat("0", 40), "refs/heads/docs"), fixture.remotePath, "go version go1.test")
	if got := fixture.makeCalls(t); got != 0 {
		t.Fatalf("new documentation-only branch gate executions = %d, want 0", got)
	}
	if got := fixture.guardCalls(t); got != 1 {
		t.Fatalf("new branch ownership guard executions = %d, want 1", got)
	}
}

type prePushGateFixture struct {
	repoPath    string
	remotePath  string
	hookPath    string
	binPath     string
	makeLog     string
	guardLog    string
	receiptsDir string
}

func newPrePushGateFixture(t *testing.T) prePushGateFixture {
	t.Helper()
	root := repoRoot(t)
	repoPath := filepath.Join(t.TempDir(), "repo")
	remotePath := filepath.Join(t.TempDir(), "remote.git")
	binPath := t.TempDir()
	makeLog := filepath.Join(t.TempDir(), "make.log")
	guardLog := filepath.Join(t.TempDir(), "guard.log")
	receiptsDir := filepath.Join(t.TempDir(), "receipts")

	if err := os.MkdirAll(filepath.Join(repoPath, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoPath, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyTestFile(t, filepath.Join(root, ".githooks", "pre-push"), filepath.Join(repoPath, ".githooks", "pre-push"))
	copyTestFile(t, filepath.Join(root, "scripts", "push-gate-receipt.sh"), filepath.Join(repoPath, "scripts", "push-gate-receipt.sh"))
	writeExecutable(t, filepath.Join(repoPath, "scripts", "push-ownership-guard.sh"), fmt.Sprintf("#!/bin/sh\nassert_bead_still_claimed() { printf 'guard\\n' >> %q; }\n", guardLog))
	writeExecutable(t, filepath.Join(binPath, "make"), fmt.Sprintf("#!/bin/sh\nprintf 'make %%s\\n' \"$*\" >> %q\n", makeLog))
	writeExecutable(t, filepath.Join(binPath, "go"), "#!/bin/sh\nprintf '%s\\n' \"${PUSH_GATE_GO_VERSION:?}\"\n")

	runGitFixtureCommands(t, repoPath, nil,
		"git init -q -b main",
		"git config user.email push-gate@example.invalid",
		"git config user.name push-gate-test",
	)
	writeTestFile(t, filepath.Join(repoPath, "main.go"), "package fixture\n\nfunc Value() int { return 1 }\n")
	runGitFixtureCommands(t, repoPath, nil, "git add -A", "git commit -qm baseline")
	if output, err := testCommand("git", "init", "--bare", "-q", remotePath).CombinedOutput(); err != nil {
		t.Fatalf("initialize bare remote: %v\n%s", err, output)
	}
	runGitFixtureCommands(t, repoPath, nil,
		"git remote add origin "+shellQuotePushGate(remotePath),
		"git push -q --no-verify origin main",
		"git fetch -q origin main:refs/remotes/origin/main",
	)

	return prePushGateFixture{
		repoPath: repoPath, remotePath: remotePath,
		hookPath: filepath.Join(repoPath, ".githooks", "pre-push"),
		binPath:  binPath, makeLog: makeLog, guardLog: guardLog,
		receiptsDir: receiptsDir,
	}
}

func (f prePushGateFixture) commitGoChange(t *testing.T, body string) string {
	t.Helper()
	writeTestFile(t, filepath.Join(f.repoPath, "main.go"), "package fixture\n\n"+body)
	return f.commitAll(t, "change Go")
}

func (f prePushGateFixture) commitAll(t *testing.T, message string) string {
	t.Helper()
	runGitFixtureCommands(t, f.repoPath, nil, "git add -A", "git commit -qm "+shellQuotePushGate(message))
	return strings.TrimSpace(runGitFixtureCommands(t, f.repoPath, nil, "git rev-parse HEAD"))
}

func (f prePushGateFixture) remoteMainSHA(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(runGitFixtureCommands(t, f.repoPath, nil, "git rev-parse refs/remotes/origin/main"))
}

func (f prePushGateFixture) runHook(t *testing.T, input, remote, goVersion string) {
	t.Helper()
	output, err := f.hookCommand(input, remote, goVersion).CombinedOutput()
	if err != nil {
		t.Fatalf("run pre-push hook: %v\n%s", err, output)
	}
}

func (f prePushGateFixture) hookCommand(input, remote, goVersion string) *exec.Cmd {
	cmd := testCommand("bash", f.hookPath, "origin", remote)
	cmd.Dir = f.repoPath
	cmd.Stdin = strings.NewReader(input)
	cmd.Env = append(os.Environ(),
		"PATH="+f.binPath+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PUSH_GATE_RESULTS_DIR="+f.receiptsDir,
		"PUSH_GATE_GO_VERSION="+goVersion,
		"PUSH_GATE_BASE_REF=refs/remotes/origin/main",
	)
	return cmd
}

func (f prePushGateFixture) makeCalls(t *testing.T) int {
	t.Helper()
	return lineCount(t, f.makeLog)
}

func (f prePushGateFixture) guardCalls(t *testing.T) int {
	t.Helper()
	return lineCount(t, f.guardLog)
}

func prePushInput(localSHA, remoteSHA, remoteRef string) string {
	return fmt.Sprintf("refs/heads/local %s %s %s\n", localSHA, remoteRef, remoteSHA)
}

func lineCount(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(body), "\n")
}

func copyTestFile(t *testing.T, source, destination string) {
	t.Helper()
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read %s: %v", source, err)
	}
	if err := os.WriteFile(destination, body, 0o755); err != nil {
		t.Fatalf("write %s: %v", destination, err)
	}
}

func shellQuotePushGate(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
