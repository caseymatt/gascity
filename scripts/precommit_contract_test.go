package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreCommitFormatterPreservesFileMode(t *testing.T) {
	repoRoot := repoRoot(t)
	binDir := t.TempDir()
	fakeLint := filepath.Join(binDir, "golangci-lint")
	writeExecutable(t, fakeLint, `#!/usr/bin/env bash
set -euo pipefail
if [ "$#" -ne 2 ] || [ "$1" != "fmt" ] || [ "$2" != "--stdin" ]; then
  echo "unexpected golangci-lint args: $*" >&2
  exit 2
fi
cat
printf '\n'
`)

	source := filepath.Join(t.TempDir(), "needs\nformat.go")
	if err := os.WriteFile(source, []byte("package main"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	cmd := exec.Command(filepath.Join(repoRoot, "scripts", "precommit-format-staged-go"))
	cmd.Dir = repoRoot
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
	}
	cmd.Stdin = strings.NewReader(source + "\x00")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("precommit formatter failed: %v\n%s", err, out)
	}

	info, err := os.Stat(source)
	if err != nil {
		t.Fatalf("stat formatted source: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("formatted source mode = %o, want 644", got)
	}
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read formatted source: %v", err)
	}
	if string(content) != "package main\n" {
		t.Fatalf("formatted content = %q, want package main with newline", content)
	}
}

func TestTestFastParallelUsesSanitizedEnvironmentAndMachineAwareConcurrency(t *testing.T) {
	repoRoot := repoRoot(t)
	baseEnv := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "LOCAL_TEST_JOBS=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_CPUS=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_MEMORY_KIB=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_MEMINFO=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_PROC_CGROUP=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_CGROUP_ROOT=") ||
			strings.HasPrefix(entry, "GC_PUSH_GATE_NO_CAP=") ||
			strings.HasPrefix(entry, "PUSH_GATE_MAX_CONCURRENT=") ||
			strings.HasPrefix(entry, "PUSH_GATE_MAX_WAIT_SECONDS=") ||
			strings.HasPrefix(entry, "PUSH_GATE_POLL_SECONDS=") ||
			strings.HasPrefix(entry, "PUSH_GATE_UNRELATED_SENTINEL=") ||
			strings.HasPrefix(entry, "GC_TEST_LOCAL_LOADAVG=") {
			continue
		}
		baseEnv = append(baseEnv, entry)
	}
	tests := []struct {
		name      string
		cpus      string
		memoryKiB string
		makeArgs  []string
		wantJobs  string
		cgroup    string
		limit     string
		current   string
	}{
		{name: "large host uses automatic ceiling", cpus: "192", memoryKiB: "536870912", wantJobs: "16"},
		{name: "memory constrains fanout", cpus: "16", memoryKiB: "12582912", wantJobs: "3"},
		{name: "cpu constrains fanout", cpus: "2", memoryKiB: "67108864", wantJobs: "2"},
		{name: "small machine still runs one job", cpus: "8", memoryKiB: "2097152", wantJobs: "1"},
		{name: "unknown memory preserves safe fallback", cpus: "64", memoryKiB: "0", wantJobs: "3"},
		{name: "nested cgroup v2 ancestor constrains fanout", cpus: "16", wantJobs: "3", cgroup: "v2", limit: "12884901888", current: "0"},
		{name: "nested cgroup v1 ancestor constrains fanout", cpus: "16", wantJobs: "2", cgroup: "v1", limit: "8589934592", current: "0"},
		{name: "hybrid cgroup falls through to v1 memory controller", cpus: "16", wantJobs: "3", cgroup: "hybrid", limit: "12884901888", current: "0"},
		{name: "exhausted cgroup forces one job", cpus: "16", wantJobs: "1", cgroup: "v2", limit: "4294967296", current: "4294967296"},
		{name: "explicit override wins", cpus: "192", memoryKiB: "536870912", makeArgs: []string{"LOCAL_TEST_JOBS=7"}, wantJobs: "7"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := append([]string{"-n"}, tt.makeArgs...)
			args = append(args, "test-fast-parallel")
			cmd := exec.Command("make", args...)
			cmd.Dir = repoRoot
			// This table exercises the cpu/memory/cgroup axes only; pin loadavg=0
			// so a live host's real /proc/loadavg can't shrink the expected job
			// count out from under an unrelated case (ga-04m84s).
			cmd.Env = append(append([]string(nil), baseEnv...),
				"GC_TEST_LOCAL_CPUS="+tt.cpus,
				"GC_TEST_LOCAL_LOADAVG=0",
				"GC_PUSH_GATE_NO_CAP=1",
				"PUSH_GATE_MAX_CONCURRENT=7",
				"PUSH_GATE_MAX_WAIT_SECONDS=13",
				"PUSH_GATE_POLL_SECONDS=2",
				"PUSH_GATE_UNRELATED_SENTINEL=must-not-leak",
			)
			if tt.memoryKiB != "" {
				cmd.Env = append(cmd.Env, "GC_TEST_LOCAL_MEMORY_KIB="+tt.memoryKiB)
			}
			if tt.cgroup != "" {
				cmd.Env = append(cmd.Env, localTestCgroupEnv(t, tt.cgroup, tt.limit, tt.current)...)
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make -n test-fast-parallel failed: %v\n%s", err, out)
			}
			command := string(out)
			if !strings.Contains(command, "env -i") {
				t.Fatalf("test-fast-parallel recipe should use TEST_ENV env -i wrapper:\n%s", command)
			}
			if !strings.Contains(command, "./scripts/test-local-parallel fast") {
				t.Fatalf("test-fast-parallel recipe should still dispatch the sharded fast runner:\n%s", command)
			}
			wantJobAssignment := " LOCAL_TEST_JOBS=" + tt.wantJobs + " CMD_GC_PROCESS_TOTAL="
			if !strings.Contains(command, wantJobAssignment) {
				t.Fatalf("test-fast-parallel job count should be %s:\n%s", tt.wantJobs, command)
			}
			for _, key := range []string{
				"GC_PUSH_GATE_NO_CAP",
				"PUSH_GATE_MAX_CONCURRENT",
				"PUSH_GATE_MAX_WAIT_SECONDS",
				"PUSH_GATE_POLL_SECONDS",
			} {
				wantForwarding := key + `="${` + key + `-}"`
				if !strings.Contains(command, wantForwarding) {
					t.Fatalf("test-fast-parallel should forward %s through TEST_ENV:\n%s", key, command)
				}
			}
			if strings.Contains(command, "PUSH_GATE_UNRELATED_SENTINEL") {
				t.Fatalf("test-fast-parallel must keep unrelated ambient variables out of TEST_ENV:\n%s", command)
			}
		})
	}
}

func localTestCgroupEnv(t *testing.T, version, limit, current string) []string {
	t.Helper()
	root := t.TempDir()
	cgroupRoot := filepath.Join(root, "cgroup")
	procCgroup := filepath.Join(root, "proc-self-cgroup")
	meminfo := filepath.Join(root, "meminfo")
	writeTestFile(t, meminfo, "MemAvailable: 67108864 kB\n")

	var controllerRoot, procLine, limitFile, currentFile string
	switch version {
	case "v2":
		controllerRoot = cgroupRoot
		procLine = "0::/parent/child\n"
		limitFile = "memory.max"
		currentFile = "memory.current"
	case "v1":
		controllerRoot = filepath.Join(cgroupRoot, "memory")
		procLine = "5:memory:/parent/child\n"
		limitFile = "memory.limit_in_bytes"
		currentFile = "memory.usage_in_bytes"
	case "hybrid":
		controllerRoot = filepath.Join(cgroupRoot, "memory")
		procLine = "0::/unified/child\n5:memory:/parent/child\n"
		limitFile = "memory.limit_in_bytes"
		currentFile = "memory.usage_in_bytes"
	default:
		t.Fatalf("unsupported cgroup fixture version %q", version)
	}

	writeTestFile(t, procCgroup, procLine)
	if err := os.MkdirAll(filepath.Join(controllerRoot, "parent", "child"), 0o755); err != nil {
		t.Fatalf("create nested cgroup fixture: %v", err)
	}
	writeTestFile(t, filepath.Join(controllerRoot, "parent", limitFile), limit+"\n")
	writeTestFile(t, filepath.Join(controllerRoot, "parent", currentFile), current+"\n")

	return []string{
		"GC_TEST_LOCAL_MEMINFO=" + meminfo,
		"GC_TEST_LOCAL_PROC_CGROUP=" + procCgroup,
		"GC_TEST_LOCAL_CGROUP_ROOT=" + cgroupRoot,
	}
}

func TestPrePushUsesStableChangedSurfaceSelection(t *testing.T) {
	repoRoot := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(repoRoot, ".githooks", "pre-push"))
	if err != nil {
		t.Fatalf("read pre-push hook: %v", err)
	}
	content := string(script)
	if strings.Contains(content, "test-fast-parallel") {
		t.Fatal("pre-push hook must not run the full untagged repository sweep")
	}
	for _, required := range []string{
		"make test-affected",
		"LINT_CHANGED_SCOPE=tracked",
		`LINT_CHANGED_REF="$base_sha"`,
		`LINT_CHANGED_HEAD="$head_sha"`,
		`base_sha="$remote_sha"`,
		"git merge-base",
		"assert_bead_still_claimed",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("pre-push hook is missing changed-surface contract %q", required)
		}
	}
}

func TestPrePushPassesExactStableRangeToFocusedTests(t *testing.T) {
	productionRoot := repoRoot(t)
	repo := t.TempDir()
	writeTestFile(t, filepath.Join(repo, "main.go"), "package main\n")
	runGitFixtureCommands(t, repo, os.Environ(),
		"git init -q -b main",
		"git config user.email pre-push@example.invalid",
		"git config user.name pre-push-test",
		"git add main.go",
		"git commit -qm baseline",
	)
	base := strings.TrimSpace(runGitFixtureCommands(t, repo, os.Environ(), "git rev-parse HEAD"))
	writeTestFile(t, filepath.Join(repo, "main.go"), "package main\n\nfunc changed() {}\n")
	runGitFixtureCommands(t, repo, os.Environ(),
		"git add main.go",
		"git commit -qm changed",
	)
	head := strings.TrimSpace(runGitFixtureCommands(t, repo, os.Environ(), "git rev-parse HEAD"))
	runGitFixtureCommands(t, repo, os.Environ(),
		"git update-ref refs/remotes/origin/main "+base,
		"git symbolic-ref refs/remotes/origin/HEAD refs/remotes/origin/main",
	)

	hookBody, err := os.ReadFile(filepath.Join(productionRoot, ".githooks", "pre-push"))
	if err != nil {
		t.Fatalf("read production pre-push hook: %v", err)
	}
	for _, directory := range []string{".githooks", "scripts"} {
		if err := os.MkdirAll(filepath.Join(repo, directory), 0o755); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
	}
	hook := filepath.Join(repo, ".githooks", "pre-push")
	writeExecutable(t, hook, string(hookBody))
	writeExecutable(t, filepath.Join(repo, "scripts", "push-ownership-guard.sh"), `#!/usr/bin/env bash
assert_bead_still_claimed() { return 0; }
`)
	binDir := t.TempDir()
	makeLog := filepath.Join(binDir, "make.log")
	writeExecutable(t, filepath.Join(binDir, "make"), `#!/usr/bin/env bash
set -euo pipefail
printf '%s|%s|%s|%s\n' "$LINT_CHANGED_SCOPE" "$LINT_CHANGED_REF" "$LINT_CHANGED_HEAD" "$*" >> "$PRE_PUSH_MAKE_LOG"
`)
	env := append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PRE_PUSH_MAKE_LOG="+makeLog,
	)
	zero := strings.Repeat("0", 40)
	tests := []struct {
		name      string
		remoteSHA string
	}{
		{name: "existing branch", remoteSHA: base},
		{name: "new branch", remoteSHA: zero},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(makeLog, nil, 0o644); err != nil {
				t.Fatalf("reset make log: %v", err)
			}
			cmd := exec.Command(hook, "origin", "unused")
			cmd.Dir = repo
			cmd.Env = env
			cmd.Stdin = strings.NewReader("refs/heads/topic " + head + " refs/heads/topic " + tt.remoteSHA + "\n")
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("run pre-push hook: %v\n%s", err, output)
			}
			logged, err := os.ReadFile(makeLog)
			if err != nil {
				t.Fatalf("read make log: %v", err)
			}
			want := "tracked|" + base + "|" + head + "|test-affected\n"
			if string(logged) != want {
				t.Fatalf("focused test range = %q, want %q", logged, want)
			}
		})
	}
}

func TestPreCommitRegeneratesDashboardClientOnSpecChange(t *testing.T) {
	repoRoot := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(repoRoot, ".githooks", "pre-commit"))
	if err != nil {
		t.Fatalf("read pre-commit hook: %v", err)
	}
	content := string(script)

	npmBlockStart := strings.Index(content, "if command -v npm")
	if npmBlockStart < 0 {
		t.Fatal("pre-commit hook must guard dashboard regeneration on npm availability")
	}
	npmBlock := content[npmBlockStart:]

	genClientIdx := strings.Index(npmBlock, "npm run generate:client")
	if genClientIdx < 0 {
		t.Fatal("pre-commit hook must run 'npm run generate:client' when internal/api/openapi.json changes — " +
			"make dashboard-check only builds and typechecks against whatever client is already on disk, it never " +
			"regenerates it (that's make dashboard-ci's job, which the hook never calls). A spec-only commit " +
			"currently ships a stale generated TS client (see PR #4627, #4607)")
	}

	dashboardCheckIdx := strings.Index(npmBlock, "make dashboard-check")
	if dashboardCheckIdx < 0 {
		t.Fatal("pre-commit hook must still run make dashboard-check dashboard-smoke")
	}
	if genClientIdx > dashboardCheckIdx {
		t.Fatal("pre-commit hook must regenerate the dashboard client BEFORE typecheck/build, so a client that " +
			"doesn't match the new spec fails typecheck immediately instead of silently building against stale types")
	}

	clientAddNeedle := "git add -- internal/api/dashboardspa/web/shared/src/generated/gc-supervisor-client"
	genClientAddIdx := strings.Index(npmBlock, clientAddNeedle)
	if genClientAddIdx < 0 {
		t.Fatal("pre-commit hook must stage the regenerated dashboard client so a spec-only commit includes it")
	}
	if genClientAddIdx < genClientIdx {
		t.Fatal("pre-commit hook must stage the generated client after regenerating it, not before")
	}

	if strings.Contains(content, "regenerate the TS types, typecheck, and rebuild") {
		t.Fatal("pre-commit hook's dashboard block comment must not claim it regenerates the TS types unless it " +
			"actually calls npm run generate:client")
	}

	if !strings.Contains(content, `echo "warning: npm not on PATH`) {
		t.Fatal("pre-commit hook must still warn and no-op cleanly when npm is not on PATH")
	}
}

func TestPreCommitReachesDashboardBlockWhenOnlySpecFileStaged(t *testing.T) {
	repoRoot := repoRoot(t)
	hookPath := filepath.Join(repoRoot, ".githooks", "pre-commit")

	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	specPath := filepath.Join(tmpRepo, "internal", "api", "openapi.json")
	clientPath := filepath.Join(tmpRepo, "internal", "api", "dashboardspa", "web", "shared", "src", "generated", "gc-supervisor-client")
	goClientPath := filepath.Join(tmpRepo, "internal", "api", "genclient", "client_gen.go")
	distPath := filepath.Join(tmpRepo, "internal", "api", "dashboardspa", "dist", "placeholder")

	runGit("init")
	writeTestFile(t, specPath, "{}\n")
	writeTestFile(t, clientPath, "placeholder\n")
	writeTestFile(t, goClientPath, "placeholder\n")
	writeTestFile(t, distPath, "placeholder\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	// Stage ONLY a change to openapi.json -- no .go, web-src, or doc files
	// are staged, matching the reviewer's criterion-2 repro scenario.
	writeTestFile(t, specPath, `{"changed":true}`+"\n")
	runGit("add", "internal/api/openapi.json")

	binDir := t.TempDir()
	npmLog := filepath.Join(binDir, "npm.log")
	writeExecutable(t, filepath.Join(binDir, "npm"), `#!/usr/bin/env bash
set -euo pipefail
echo "$*" >> "`+npmLog+`"
exit 0
`)
	writeExecutable(t, filepath.Join(binDir, "go"), `#!/usr/bin/env bash
exit 0
`)
	// Stub make: this test verifies the control-flow reaches the dashboard
	// block at all (the reviewer's criterion-2 gap), not the real
	// dashboard-check/dashboard-smoke targets, which need the full repo.
	writeExecutable(t, filepath.Join(binDir, "make"), `#!/usr/bin/env bash
exit 0
`)

	cmd := exec.Command("bash", hookPath)
	cmd.Dir = tmpRepo
	cmd.Env = []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pre-commit hook failed: %v\n%s", err, out)
	}

	logContent, readErr := os.ReadFile(npmLog)
	if readErr != nil {
		t.Fatalf("pre-commit hook exited early and never invoked npm when only internal/api/openapi.json was "+
			"staged -- the go/web/docs early guard must not skip a spec-only commit (hook output: %s)", out)
	}
	if !strings.Contains(string(logContent), "generate:client") {
		t.Fatalf("pre-commit hook must run 'npm run generate:client' when only internal/api/openapi.json is "+
			"staged, got npm invocations:\n%s", logContent)
	}
}

func TestPreCommitFailsClosedWhenSpecStagedButNpmAbsent(t *testing.T) {
	repoRoot := repoRoot(t)
	hookPath := filepath.Join(repoRoot, ".githooks", "pre-commit")

	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	specPath := filepath.Join(tmpRepo, "internal", "api", "openapi.json")
	clientPath := filepath.Join(tmpRepo, "internal", "api", "genclient", "client_gen.go")

	runGit("init")
	writeTestFile(t, specPath, "{}\n")
	writeTestFile(t, clientPath, "placeholder\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	// Stage ONLY a change to openapi.json -- same repro shape as
	// TestPreCommitReachesDashboardBlockWhenOnlySpecFileStaged, but this
	// time npm itself is unreachable on PATH.
	writeTestFile(t, specPath, `{"changed":true}`+"\n")
	runGit("add", "internal/api/openapi.json")

	cmd := exec.Command("bash", hookPath)
	cmd.Dir = tmpRepo
	cmd.Env = []string{
		"PATH=" + restrictedPathWithoutNpm(t, map[string]string{
			"go":   "#!/usr/bin/env bash\nexit 0\n",
			"make": "#!/usr/bin/env bash\nexit 0\n",
		}),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("pre-commit hook must fail when internal/api/openapi.json is staged and npm is not on PATH "+
			"-- the generated TS client can't be regenerated, so the commit would silently ship a stale "+
			"client with no enforcement until CI runs. Hook exited 0, output:\n%s", out)
	}
	if !strings.Contains(string(out), "npm ci") || !strings.Contains(string(out), "generate:client") {
		t.Fatalf("pre-commit hook's npm-absent+spec-staged failure must name the exact recovery command "+
			"(cd internal/api/dashboardspa/web && npm ci && npm run generate:client), got:\n%s", out)
	}
}

func TestPreCommitUnrelatedGoUsesAffectedVetWithoutGenerators(t *testing.T) {
	repoRoot := repoRoot(t)
	hookPath := filepath.Join(repoRoot, ".githooks", "pre-commit")

	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	goFilePath := filepath.Join(tmpRepo, "unrelated", "unrelated.go")
	formatStagedGoPath := filepath.Join(tmpRepo, "scripts", "precommit-format-staged-go")
	if err := os.MkdirAll(filepath.Dir(formatStagedGoPath), 0o755); err != nil {
		t.Fatalf("create scripts directory: %v", err)
	}
	runGit("init")
	writeTestFile(t, goFilePath, "package unrelated\n\nfunc Value() int { return 1 }\n")
	writeExecutable(t, formatStagedGoPath, "#!/usr/bin/env bash\nexit 0\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	writeTestFile(t, goFilePath, "package unrelated\n\nfunc Value() int { return 2 }\n")
	runGit("add", "unrelated/unrelated.go")

	makeLog := filepath.Join(t.TempDir(), "make.log")
	makeStub := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "` + makeLog + `"
`
	cmd := exec.Command("bash", hookPath)
	cmd.Dir = tmpRepo
	cmd.Env = []string{
		"PATH=" + restrictedPathWithoutNpm(t, map[string]string{
			"make": makeStub,
		}),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pre-commit hook failed for unrelated Go change: %v\n%s", err, out)
	}

	logContent, err := os.ReadFile(makeLog)
	if err != nil {
		t.Fatalf("read make log: %v", err)
	}
	log := string(logContent)
	for _, want := range []string{
		"lint-changed LINT_CHANGED_SCOPE=staged",
		"vet-affected LINT_CHANGED_SCOPE=staged",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("unrelated Go change did not run %q:\n%s", want, log)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		if line == "vet" || strings.HasPrefix(line, "vet ") {
			t.Errorf("unrelated Go change ran full make vet instead of vet-affected:\n%s", log)
		}
	}
	for _, forbidden := range []string{"genspec", "genschema", "genclient", "dashboard-check"} {
		if strings.Contains(log, forbidden) || strings.Contains(string(out), forbidden) {
			t.Errorf("unrelated Go change unexpectedly ran %s (make log: %s; hook output: %s)", forbidden, log, out)
		}
	}
}

func TestPreCommitDoesNotEnterDashboardForDocsOnlyChange(t *testing.T) {
	repoRoot := repoRoot(t)
	hookPath := filepath.Join(repoRoot, ".githooks", "pre-commit")

	tmpRepo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpRepo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.invalid",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	docPath := filepath.Join(tmpRepo, "README.md")

	runGit("init")
	writeTestFile(t, docPath, "hello\n")
	runGit("add", "-A")
	runGit("commit", "-m", "init")

	// A docs-only change exercises check-docs, but is not a dashboard input.
	// npm is deliberately absent so entering the dashboard branch is observable.
	writeTestFile(t, docPath, "hello again\n")
	runGit("add", "README.md")

	cmd := exec.Command("bash", hookPath)
	cmd.Dir = tmpRepo
	cmd.Env = []string{
		"PATH=" + restrictedPathWithoutNpm(t, map[string]string{
			"make": "#!/usr/bin/env bash\nexit 0\n",
		}),
		"HOME=" + t.TempDir(),
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pre-commit hook must succeed when npm is absent for a docs-only change: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "npm not on PATH") {
		t.Fatalf("pre-commit hook must not enter the dashboard branch for docs-only changes, got:\n%s", out)
	}
}

func TestPreCommitScopesGeneratorsAndStagesEveryOutput(t *testing.T) {
	hookPath := filepath.Join(repoRoot(t), ".githooks", "pre-commit")
	script, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read pre-commit hook: %v", err)
	}
	content := string(script)

	for _, trigger := range []string{
		"--name-only -z --no-renames --diff-filter=ACM",
		"generator-affected LINT_CHANGED_SCOPE=staged",
		"./cmd/genspec",
		"./cmd/genschema",
		"internal/api/dashboardspa/web/",
		"cmd/gen-client/",
	} {
		if !strings.Contains(content, trigger) {
			t.Errorf("pre-commit hook is missing dependency-aware, delete-safe trigger %q", trigger)
		}
	}
	selector, err := os.ReadFile(filepath.Join(repoRoot(t), "scripts", "ci-static-select"))
	if err != nil {
		t.Fatalf("read static selector: %v", err)
	}
	selectorContent := string(selector)
	for _, contract := range []string{
		`"-deps"`,
		`{"go.mod", "go.sum"}`,
		"direct_selected",
		"validate_changed_package_dir",
		"GENERATOR_RUNTIME_DIRS",
		"GENERATOR_OWNED_OUTPUTS",
		"generator-affected: selecting all generators",
	} {
		if !strings.Contains(selectorContent, contract) {
			t.Errorf("generator ownership selector is missing fail-safe dependency contract %q", contract)
		}
	}
	if !strings.Contains(content, "--diff-filter=ACM -- '*.go'") {
		t.Error("pre-commit formatting must remain limited to existing added/copied/modified Go files")
	}
	if !strings.Contains(content, "make vet-affected LINT_CHANGED_SCOPE=staged") {
		t.Error("pre-commit hook must vet the staged affected-package graph")
	}
	for _, forbidden := range []string{"\nmake vet\n", "\n  make vet\n"} {
		if strings.Contains(content, forbidden) {
			t.Error("pre-commit hook must not run unconditional full make vet")
		}
	}

	block := func(start, end string) string {
		t.Helper()
		startIndex := strings.Index(content, start)
		if startIndex < 0 {
			t.Fatalf("pre-commit hook is missing block start %q", start)
		}
		endIndex := strings.Index(content[startIndex:], end)
		if endIndex < 0 {
			t.Fatalf("pre-commit hook block %q is missing terminator %q", start, end)
		}
		return content[startIndex : startIndex+endIndex]
	}

	genspecBlock := block(`if [ "$run_genspec" = true ]`, "# Re-read the index after genspec")
	for _, output := range []string{
		"internal/api/openapi.json",
		"docs/reference/schema/openapi.json",
		"docs/reference/schema/openapi.txt",
		"docs/reference/schema/events.json",
		"docs/reference/schema/events.txt",
	} {
		if !strings.Contains(genspecBlock, output) {
			t.Errorf("genspec output %s is not staged", output)
		}
	}

	clientBlock := block(`if [ "$run_genclient" = true ]`, `if [ "$run_genschema" = true ]`)
	for _, want := range []string{"go generate ./internal/api/genclient", "git add -- internal/api/genclient/client_gen.go"} {
		if !strings.Contains(clientBlock, want) {
			t.Errorf("spec-changed client block is missing %q", want)
		}
	}

	genschemaBlock := block(`if [ "$run_genschema" = true ]`, "# The selector reads the complete staged diff")
	for _, output := range []string{
		"docs/reference/schema/city-schema.json",
		"docs/reference/schema/city-schema.txt",
		"docs/reference/schema/pack-schema.json",
		"docs/reference/schema/pack-schema.txt",
		"docs/reference/config.md",
		"docs/reference/cli.md",
	} {
		if !strings.Contains(genschemaBlock, output) {
			t.Errorf("genschema output %s is not staged", output)
		}
	}
	genschemaIndex := strings.Index(content, `if [ "$run_genschema" = true ]`)
	docsSnapshotIndex := strings.Index(content, "# Re-read after generation")
	checkDocsIndex := strings.Index(content, "make check-docs")
	if genschemaIndex < 0 || docsSnapshotIndex < genschemaIndex || checkDocsIndex < docsSnapshotIndex {
		t.Error("pre-commit hook must re-read and validate staged docs after running generators")
	}
}

// restrictedPathWithoutNpm builds a PATH containing only symlinks to the
// real bash and git (plus any provided stub scripts), guaranteeing npm is
// unreachable regardless of what's installed on the test host -- falling
// back to the ambient PATH would make these tests flaky on any machine
// that actually has npm installed.
func restrictedPathWithoutNpm(t *testing.T, stubs map[string]string) string {
	t.Helper()
	binDir := t.TempDir()
	for _, name := range []string{"bash", "git", "xargs"} {
		realPath, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("resolve real %s on test host PATH: %v", name, err)
		}
		if err := os.Symlink(realPath, filepath.Join(binDir, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	for name, script := range stubs {
		writeExecutable(t, filepath.Join(binDir, name), script)
	}
	return binDir
}

func TestNativeDoltliteBeadsTargetRunsTaggedSuite(t *testing.T) {
	repoRoot := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	if err := validateNativeDoltliteMakefile(string(makefile)); err != nil {
		t.Fatalf("test-native-doltlite-beads recipe: %v", err)
	}

	cmd := exec.Command("make", "-n", "test-native-doltlite-beads")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n test-native-doltlite-beads failed: %v\n%s", err, out)
	}
	command := string(out)
	if err := validateNativeDoltliteDryRun(command); err != nil {
		t.Fatalf("make -n test-native-doltlite-beads output: %v", err)
	}
	for _, want := range []string{
		"CGO_ENABLED=0",
		"-tags gascity_native_beads",
		"-run '^TestDoltlite'",
		"./internal/beads",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("test-native-doltlite-beads recipe missing %q:\n%s", want, command)
		}
	}
	for _, banned := range []string{
		"CGO_ENABLED=1",
		"cgo,gascity_native_beads",
	} {
		if strings.Contains(command, banned) {
			t.Fatalf("test-native-doltlite-beads recipe must not contain %q (doltlite store now uses pure-Go modernc):\n%s", banned, command)
		}
	}
	assertNativeDoltliteBeadsSelectionMatchesTaggedOwners(t, repoRoot)
}

func TestLocalParallelAllowlistIncludesObservableEnv(t *testing.T) {
	repoRoot := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "test-local-parallel"))
	if err != nil {
		t.Fatalf("read test-local-parallel: %v", err)
	}
	content := string(script)
	for _, key := range []string{"OBSERVABLE_TEST_LOG", "OBSERVABLE_FAILURE_LINES"} {
		if !strings.Contains(content, key+"=") {
			t.Fatalf("test-local-parallel job env should pass through %s", key)
		}
	}
	for _, key := range []string{"GC_CITY", "GC_HOME", "GC_SESSION_ID"} {
		if strings.Contains(content, key+"=") {
			t.Fatalf("test-local-parallel job env must not pass through live session env %s", key)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(wd)
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
