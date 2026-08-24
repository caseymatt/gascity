package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/google/uuid"
)

func TestWorktreeCommandSurfaceHasFourVerbsAndNoForce(t *testing.T) {
	cmd := newWorktreeCmd(io.Discard, io.Discard)
	want := map[string]bool{"create": false, "publish": false, "reclaim": false, "list": false}
	for _, child := range cmd.Commands() {
		if _, ok := want[child.Name()]; !ok {
			continue
		}
		want[child.Name()] = true
		if child.Flags().Lookup("force") != nil {
			t.Fatalf("gc worktree %s unexpectedly exposes --force", child.Name())
		}
	}
	for verb, found := range want {
		if !found {
			t.Errorf("gc worktree %s is not registered", verb)
		}
	}
}

func TestWorktreeSubcommandsDeclareJSONSupport(t *testing.T) {
	for _, verb := range []string{"create", "publish", "reclaim", "list"} {
		t.Run(verb, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runWithRootCommandOptions(
				[]string{"worktree", verb, "--json-schema"},
				&stdout,
				&stderr,
				rootCommandOptions{},
			)
			if code != 0 {
				t.Fatalf("gc worktree %s --json-schema exited %d: stdout=%s stderr=%s", verb, code, stdout.String(), stderr.String())
			}
			var manifest jsonSchemaManifest
			if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
				t.Fatalf("decode manifest: %v\n%s", err, stdout.String())
			}
			if !manifest.JSONSupported {
				t.Fatalf("gc worktree %s does not declare JSON support: %s", verb, stdout.String())
			}
			if got := strings.Join(manifest.Command, " "); got != "worktree "+verb {
				t.Fatalf("manifest command = %q, want %q", got, "worktree "+verb)
			}
			if !json.Valid(manifest.Schemas[jsonSchemaResultRole]) {
				t.Fatalf("gc worktree %s result schema is missing or invalid: %s", verb, stdout.String())
			}
			compileJSONSchema(
				t,
				"gc://schemas/worktree/"+verb+"/result.schema.json",
				manifest.Schemas[jsonSchemaResultRole],
			)
		})
	}
}

func TestWorktreeJSONExecutesThroughProductionRoot(t *testing.T) {
	cityPath, rig, _ := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	stubWorktreeTestLiveness(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_JSON_CONTRACT_STRICT", "1")

	cityTOML := fmt.Sprintf(
		"[workspace]\nname = \"worktree-json-test\"\n\n[[rigs]]\nname = %q\npath = %q\nprefix = \"wt\"\n",
		rig.Name,
		rig.Root,
	)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	runJSON := func(args ...string) []byte {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("gc %s exited %d: stdout=%s stderr=%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
		}
		if strings.Contains(stdout.String(), "json_unsupported") {
			t.Fatalf("gc %s was rejected by the JSON contract: %s", strings.Join(args, " "), stdout.String())
		}
		return append([]byte(nil), stdout.Bytes()...)
	}

	scope := []string{"--city", cityPath, "--rig", rig.Name, "worktree"}
	createPath := filepath.Join(rig.Root, "worktrees", "root-json")
	createArgs := append(append([]string(nil), scope...), "create", "root-json", "--owner", "root-test", "--path", createPath, "--base", "HEAD", "--json")
	var created worktreeListEntry
	if err := json.Unmarshal(runJSON(createArgs...), &created); err != nil {
		t.Fatalf("decode create result: %v", err)
	}
	if created.ID != "root-json" || created.Owner != "root-test" || created.HeadSHA == "" {
		t.Fatalf("create result = %+v", created)
	}

	publishArgs := append(append([]string(nil), scope...), "publish", created.ID, "--json")
	var published worktreeListEntry
	if err := json.Unmarshal(runJSON(publishArgs...), &published); err != nil {
		t.Fatalf("decode publish result: %v", err)
	}
	if !published.Published || published.PublishedSHA != created.HeadSHA || published.PublishedRef == "" {
		t.Fatalf("publish result = %+v", published)
	}

	listArgs := append(append([]string(nil), scope...), "list", created.ID, "--json")
	var listed []worktreeListEntry
	if err := json.Unmarshal(runJSON(listArgs...), &listed); err != nil {
		t.Fatalf("decode list result: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID || !listed[0].Published {
		t.Fatalf("list result = %+v", listed)
	}

	plainArgs := append(append([]string(nil), scope...), "list", created.ID)
	var plainStdout, plainStderr bytes.Buffer
	if code := run(plainArgs, &plainStdout, &plainStderr); code != 0 {
		t.Fatalf("plain gc %s exited %d: stdout=%s stderr=%s", strings.Join(plainArgs, " "), code, plainStdout.String(), plainStderr.String())
	}
	if !strings.Contains(plainStdout.String(), "ID") || !strings.Contains(plainStdout.String(), created.ID) {
		t.Fatalf("plain list output is not usable: %q", plainStdout.String())
	}

	reclaimArgs := append(append([]string(nil), scope...), "reclaim", created.ID, "--json")
	var reclaimed worktreeReclaimResult
	if err := json.Unmarshal(runJSON(reclaimArgs...), &reclaimed); err != nil {
		t.Fatalf("decode reclaim result: %v", err)
	}
	if !reclaimed.Reclaimed || reclaimed.ID != created.ID || reclaimed.PublishedSHA != published.PublishedSHA {
		t.Fatalf("reclaim result = %+v", reclaimed)
	}
}

func TestWorktreeMalformedRegistryFailsClosed(t *testing.T) {
	cityPath, _, cfg := newWorktreeTestRig(t)
	registryPath := worktreeRegistryFilePaths(cityPath).Registry
	if err := os.MkdirAll(filepath.Dir(registryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(registryPath, []byte(`{"version":1,"entries":[`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := listRegisteredWorktrees(context.Background(), cityPath, cfg, "", "")
	if err == nil || !strings.Contains(err.Error(), "malformed worktree registry") {
		t.Fatalf("list error = %v, want malformed-registry failure", err)
	}
}

func TestWorktreeCreateRejectsDuplicateIDAndPath(t *testing.T) {
	cityPath, rig, _ := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	first := createWorktreeTestEntry(t, cityPath, rig, "item-1", "one")

	_, err := createRegisteredWorktree(context.Background(), cityPath, rig, worktreeCreateOptions{
		ID: "item-1", Owner: "other-owner", Path: "two", Base: "HEAD", Attempt: 1,
	}, events.Discard)
	if err == nil || !strings.Contains(err.Error(), "already registered with different create parameters") {
		t.Fatalf("duplicate id error = %v", err)
	}

	_, err = createRegisteredWorktree(context.Background(), cityPath, rig, worktreeCreateOptions{
		ID: "item-2", Owner: "owner-2", Path: first.Path, Base: "HEAD", Attempt: 1,
	}, events.Discard)
	if err == nil || !strings.Contains(err.Error(), "already registered to id") {
		t.Fatalf("duplicate path error = %v", err)
	}
}

func TestWorktreeCreateIsIdempotentOnlyForExactRegisteredRequest(t *testing.T) {
	cityPath, rig, _ := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	requestLog := filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv("GC_WORKTREE_TEST_REQUEST_LOG", requestLog)
	opts := worktreeCreateOptions{ID: "same", Owner: "owner", Path: "same", Base: "HEAD", Attempt: 1}
	first, err := createRegisteredWorktree(context.Background(), cityPath, rig, opts, events.Discard)
	if err != nil {
		t.Fatal(err)
	}
	second, err := createRegisteredWorktree(context.Background(), cityPath, rig, opts, events.Discard)
	if err != nil {
		t.Fatalf("exact retry: %v", err)
	}
	if first != second {
		t.Fatalf("idempotent retry changed entry:\nfirst=%+v\nsecond=%+v", first, second)
	}
	data, err := os.ReadFile(requestLog)
	if err != nil {
		t.Fatal(err)
	}
	var registerCalls int
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, `"registerAttempt"`) {
			registerCalls++
		}
	}
	if registerCalls != 1 {
		t.Fatalf("helper register calls = %d, want exactly one; requests=%s", registerCalls, data)
	}
}

func TestWorktreeCreateRegisterFailureCompensatesAndSanitizesDiagnostic(t *testing.T) {
	cityPath, rig, _ := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, true)
	t.Setenv("GC_WORKTREE_TEST_SECRET", "known-secret-value")
	path := filepath.Join(rig.Root, "worktrees", "failed")
	cargoTarget := filepath.Join(rig.Root, "worktrees", ".cargo-targets", "failed", "attempt-1")

	_, err := createRegisteredWorktree(context.Background(), cityPath, rig, worktreeCreateOptions{
		ID: "failed", Owner: "owner", Path: path, Base: "HEAD", Attempt: 1,
	}, events.Discard)
	if err == nil {
		t.Fatal("create unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "register failed with exit code 23") ||
		!strings.Contains(err.Error(), "remote rejected lifecycle") {
		t.Fatalf("register error lost actionable diagnostic: %v", err)
	}
	for _, secret := range []string{
		"known-secret-value",
		"super-secret-token",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJzZWNyZXQifQ.signature123",
		"abcdefgh.ijklmnop.",
		"private-key-material",
		"authorization-secret",
		"opaque-authorization-secret",
		"github-authorization-secret",
		"proxy-secret",
		"https://user:stdout-secret@example.invalid/attached",
		"https://user:password@example.invalid/repository",
		"https://example.invalid/archive?X-Amz-Credential=credential-value&X-Amz-Signature=signature-value",
	} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("helper error leaked %q: %v", secret, err)
		}
	}
	if len(err.Error()) > codeStorageDiagnosticLimit+200 {
		t.Fatalf("helper error length = %d, want bounded diagnostic", len(err.Error()))
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("compensated path stat error = %v, want not exist", statErr)
	}
	if _, statErr := os.Stat(cargoTarget); !os.IsNotExist(statErr) {
		t.Fatalf("compensated cargo target stat error = %v, want not exist", statErr)
	}
	owned, listErr := rigOwnsWorktreePath(rig.Root, path)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if owned {
		t.Fatal("failed checkout remains in git worktree registry")
	}
	registryPath := worktreeRegistryFilePaths(cityPath).Registry
	registry, loadErr := loadWorktreeRegistry(registryPath)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(registry.Entries) != 0 {
		t.Fatalf("registry entries = %+v, want empty", registry.Entries)
	}
	if _, statErr := os.Stat(registryPath); !os.IsNotExist(statErr) {
		t.Fatalf("registry metadata stat error = %v, want not exist", statErr)
	}
}

func TestWorktreeCreateRejectsInvalidRegisterAcknowledgement(t *testing.T) {
	for _, test := range []struct {
		name string
		env  string
	}{
		{name: "unknown field", env: "GC_WORKTREE_TEST_REGISTER_EXTRA"},
		{name: "mismatched request", env: "GC_WORKTREE_TEST_REGISTER_MISMATCH"},
		{name: "trailing output", env: "GC_WORKTREE_TEST_REGISTER_TRAILING"},
		{name: "overflowed output", env: "GC_WORKTREE_TEST_REGISTER_OVERFLOW"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cityPath, rig, _ := newWorktreeTestRig(t)
			installWorktreeTestHelper(t, false)
			t.Setenv(test.env, "true")
			path := filepath.Join(rig.Root, "worktrees", "invalid-register")

			_, err := createRegisteredWorktree(context.Background(), cityPath, rig, worktreeCreateOptions{
				ID: "invalid-register", Owner: "owner", Path: path, Base: "HEAD", Attempt: 1,
			}, events.Discard)
			if err == nil || !strings.Contains(err.Error(), "register returned") ||
				!strings.Contains(err.Error(), "output redacted") {
				t.Fatalf("invalid register acknowledgment error = %v", err)
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("compensated path stat error = %v, want not exist", statErr)
			}
			assertWorktreeTestRegistered(t, cityPath, "invalid-register", false)
		})
	}
}

func TestWorktreeCreateAcceptsCompleteRegisterFrameAboveDiagnosticLimit(t *testing.T) {
	cityPath, rig, _ := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	t.Setenv("GC_WORKTREE_TEST_REGISTER_PADDING", "true")
	path := filepath.Join(rig.Root, "worktrees", "large-register-frame")

	if _, err := createRegisteredWorktree(context.Background(), cityPath, rig, worktreeCreateOptions{
		ID: "large-register-frame", Owner: "owner", Path: path, Base: "HEAD", Attempt: 1,
	}, events.Discard); err != nil {
		t.Fatal(err)
	}
	assertWorktreeTestRegistered(t, cityPath, "large-register-frame", true)
}

func TestWorktreeSignerRequestsContainCanonicalLifecycleIdentity(t *testing.T) {
	cityPath, rig, _ := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	requestLog := filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv("GC_WORKTREE_TEST_REQUEST_LOG", requestLog)
	t.Setenv("PIERRE_PRIVATE_KEY", "must-not-reach-helper")
	startedAt := time.Now().UTC()

	created := createWorktreeTestEntry(t, cityPath, rig, "request-fields", "request-fields")
	if _, err := publishRegisteredWorktree(context.Background(), cityPath, created.ID, events.Discard); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(requestLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("helper requests = %d, want register and publish: %s", len(lines), data)
	}
	for i, operation := range []string{"registerAttempt", "publishAttempt"} {
		var record struct {
			Argv                []string        `json:"argv"`
			Request             json.RawMessage `json:"request"`
			HasPierrePrivateKey bool            `json:"has_pierre_private_key"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &record); err != nil {
			t.Fatalf("decode helper request %d: %v", i, err)
		}
		if record.HasPierrePrivateKey {
			t.Fatalf("%s helper inherited PIERRE_PRIVATE_KEY", operation)
		}
		var request codeStorageRequest
		if err := json.Unmarshal(record.Request, &request); err != nil {
			t.Fatalf("decode %s request: %v", operation, err)
		}
		var requestFields map[string]json.RawMessage
		if err := json.Unmarshal(record.Request, &requestFields); err != nil {
			t.Fatalf("decode %s request fields: %v", operation, err)
		}
		wantFields := []string{
			"version", "operation", "requestId", "expiresAt", "lifecycleId", "owner",
			"rig", "rigRoot", "worktreePath", "attempt", "base", "branch", "headSha",
		}
		if len(requestFields) != len(wantFields) {
			t.Fatalf("%s request fields = %v, want exactly %v", operation, requestFields, wantFields)
		}
		for _, field := range wantFields {
			if _, ok := requestFields[field]; !ok {
				t.Fatalf("%s request missing field %q: %s", operation, field, record.Request)
			}
		}
		wantVerb := strings.TrimSuffix(operation, "Attempt")
		if got := strings.Join(record.Argv, " "); got != wantVerb+" --json-request" {
			t.Fatalf("%s argv = %q", operation, got)
		}
		if request.Version != 1 || request.Operation != operation ||
			request.LifecycleID != created.ID || request.Owner != created.Owner ||
			request.Rig != created.Rig || request.RigRoot != created.RigRoot ||
			request.Attempt != created.Attempt || request.Base != created.Base ||
			request.Branch != created.Branch || request.WorktreePath != created.Path ||
			request.HeadSHA != created.HeadSHA {
			t.Fatalf("%s request = %+v, want canonical entry %+v", operation, request, created)
		}
		requestID, err := uuid.Parse(request.RequestID)
		if err != nil || requestID.Version() != 4 {
			t.Fatalf("%s requestId = %q, want UUIDv4", operation, request.RequestID)
		}
		expiresAt, err := time.Parse(time.RFC3339Nano, request.ExpiresAt)
		if err != nil {
			t.Fatalf("%s expiresAt = %q: %v", operation, request.ExpiresAt, err)
		}
		if !expiresAt.After(startedAt) || expiresAt.After(time.Now().UTC().Add(30*time.Second)) {
			t.Fatalf("%s expiresAt = %s, want a future expiry no more than 30s ahead", operation, expiresAt)
		}
	}
}

func TestWorktreeCreateUnavailableHelperCompensatesFilesystemMutation(t *testing.T) {
	cityPath, rig, _ := newWorktreeTestRig(t)
	t.Setenv("GC_CODE_STORAGE_HELPER", filepath.Join(t.TempDir(), "super-secret-helper-path"))
	path := filepath.Join(rig.Root, "worktrees", "unavailable")
	worktreesRoot := filepath.Join(rig.Root, "worktrees")
	cargoTarget := filepath.Join(worktreesRoot, ".cargo-targets", "unavailable", "attempt-1")
	cargoHome := filepath.Join(rig.Root, ".gc", "cache", "cargo-home")

	_, err := createRegisteredWorktree(context.Background(), cityPath, rig, worktreeCreateOptions{
		ID: "unavailable", Owner: "owner", Path: path, Base: "HEAD", Attempt: 1,
	}, events.Discard)
	if err == nil || !strings.Contains(err.Error(), "register could not start (details redacted)") {
		t.Fatalf("unavailable helper error = %v", err)
	}
	if strings.Contains(err.Error(), "super-secret-helper-path") {
		t.Fatalf("unavailable helper leaked path: %v", err)
	}
	for _, provisional := range []string{worktreesRoot, filepath.Join(rig.Root, ".gc"), path, cargoTarget, cargoHome} {
		if _, statErr := os.Stat(provisional); !os.IsNotExist(statErr) {
			t.Fatalf("unavailable preflight created %s: %v", provisional, statErr)
		}
	}
	assertWorktreeTestRegistered(t, cityPath, "unavailable", false)
}

func TestWorktreePublishRecordsExactHelperSHA(t *testing.T) {
	cityPath, rig, _ := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	created := createWorktreeTestEntry(t, cityPath, rig, "publish", "publish")

	published, err := publishRegisteredWorktree(context.Background(), cityPath, created.ID, events.Discard)
	if err != nil {
		t.Fatal(err)
	}
	wantHead, err := git.New(created.Path).HeadCtx(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !published.Published || published.HeadSHA != wantHead || published.PublishedSHA != wantHead {
		t.Fatalf("published entry = %+v, want exact HEAD %s", published, wantHead)
	}
	if published.PublishedRef != "sessions/publish/attempt/1" || published.PublishedAt == "" {
		t.Fatalf("publication metadata = %+v", published)
	}
	registry, err := loadWorktreeRegistry(worktreeRegistryFilePaths(cityPath).Registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Entries) != 1 || registry.Entries[0] != published {
		t.Fatalf("registry entries = %+v, want published entry", registry.Entries)
	}
}

func TestWorktreePublishAcceptsAlreadyUpToDateReply(t *testing.T) {
	cityPath, rig, _ := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	created := createWorktreeTestEntry(t, cityPath, rig, "already-pushed", "already-pushed")
	t.Setenv("GC_WORKTREE_TEST_REPLY_ALREADY_UP_TO_DATE", "true")

	published, err := publishRegisteredWorktree(context.Background(), cityPath, created.ID, events.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !published.Published || published.PublishedRef == "" || published.PublishedSHA != created.HeadSHA {
		t.Fatalf("published entry = %+v, want idempotent already-up-to-date success", published)
	}
}

func TestWorktreePublishRejectsHeadChangeDuringRPC(t *testing.T) {
	cityPath, rig, _ := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	created := createWorktreeTestEntry(t, cityPath, rig, "head-race", "head-race")
	t.Setenv("GC_WORKTREE_TEST_ADVANCE_HEAD", "true")

	_, err := publishRegisteredWorktree(context.Background(), cityPath, created.ID, events.Discard)
	if err == nil || !strings.Contains(err.Error(), "HEAD changed while") {
		t.Fatalf("publish error = %v, want concurrent HEAD change rejection", err)
	}
	registry, loadErr := loadWorktreeRegistry(worktreeRegistryFilePaths(cityPath).Registry)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(registry.Entries) != 1 || registry.Entries[0] != created {
		t.Fatalf("registry entries = %+v, want unchanged unpublished entry %+v", registry.Entries, created)
	}
}

func TestWorktreePublishRejectsInvalidHelperReplyWithoutPublishing(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		value     string
		wantError string
	}{
		{
			name:      "mismatched worktree",
			env:       "GC_WORKTREE_TEST_REPLY_WORKTREE",
			value:     "/not/the/registered/worktree",
			wantError: "mismatched worktree",
		},
		{
			name:      "mismatched ref",
			env:       "GC_WORKTREE_TEST_REPLY_REF",
			value:     "sessions/someone-else/attempt/1",
			wantError: "mismatched ref",
		},
		{
			name:      "mismatched head SHA",
			env:       "GC_WORKTREE_TEST_REPLY_HEAD_SHA",
			value:     "0000000000000000000000000000000000000000",
			wantError: "mismatched SHA",
		},
		{
			name:      "push refused",
			env:       "GC_WORKTREE_TEST_REPLY_PUSHED",
			value:     "false",
			wantError: "refused to push",
		},
		{
			name:      "unknown field",
			env:       "GC_WORKTREE_TEST_REPLY_EXTRA",
			value:     "true",
			wantError: "invalid JSON",
		},
		{
			name:      "trailing output",
			env:       "GC_WORKTREE_TEST_REPLY_TRAILING",
			value:     "true",
			wantError: "trailing data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath, rig, _ := newWorktreeTestRig(t)
			installWorktreeTestHelper(t, false)
			created := createWorktreeTestEntry(t, cityPath, rig, "reject", "reject")
			t.Setenv(tt.env, tt.value)

			_, err := publishRegisteredWorktree(context.Background(), cityPath, created.ID, events.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("publish error = %v, want %q", err, tt.wantError)
			}
			if strings.Contains(err.Error(), "super-secret-helper-output") {
				t.Fatalf("publish error leaked helper output: %v", err)
			}

			registry, loadErr := loadWorktreeRegistry(worktreeRegistryFilePaths(cityPath).Registry)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if len(registry.Entries) != 1 || registry.Entries[0] != created {
				t.Fatalf("registry entries = %+v, want unchanged unpublished entry %+v", registry.Entries, created)
			}
		})
	}
}

func TestWorktreeReclaimDeniesDirtyCheckout(t *testing.T) {
	cityPath, rig, cfg := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	stubWorktreeTestLiveness(t)
	entry := createWorktreeTestEntry(t, cityPath, rig, "dirty", "dirty")
	if _, err := publishRegisteredWorktree(context.Background(), cityPath, entry.ID, events.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry.Path, "untracked.txt"), []byte("do not lose\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := reclaimRegisteredWorktree(context.Background(), cityPath, cfg, entry.ID, "", false, events.Discard)
	if err == nil || result.Reclaimed || result.Reclaimable {
		t.Fatalf("dirty reclaim = (%+v, %v), want preserved denial", result, err)
	}
	if !strings.Contains(result.Reason, "uncommitted work") {
		t.Fatalf("reason = %q", result.Reason)
	}
	if _, statErr := os.Stat(entry.Path); statErr != nil {
		t.Fatalf("dirty checkout was removed: %v", statErr)
	}
	assertWorktreeTestRegistered(t, cityPath, entry.ID, true)
}

func TestWorktreeReclaimSucceedsAtExactPublishedSHA(t *testing.T) {
	cityPath, rig, cfg := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	stubWorktreeTestLiveness(t)
	entry := createWorktreeTestEntry(t, cityPath, rig, "exact", "exact")
	if _, err := publishRegisteredWorktree(context.Background(), cityPath, entry.ID, events.Discard); err != nil {
		t.Fatal(err)
	}

	result, err := reclaimRegisteredWorktree(context.Background(), cityPath, cfg, entry.ID, "", false, events.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reclaimed || !result.Reclaimable || result.Reason != "" {
		t.Fatalf("reclaim result = %+v", result)
	}
	if _, statErr := os.Stat(entry.Path); !os.IsNotExist(statErr) {
		t.Fatalf("reclaimed path stat error = %v, want not exist", statErr)
	}
	assertWorktreeTestRegistered(t, cityPath, entry.ID, false)
	if _, statErr := os.Stat(entry.CargoTargetDir); !os.IsNotExist(statErr) {
		t.Fatalf("per-attempt cargo target stat error = %v, want not exist", statErr)
	}
	if _, statErr := os.Stat(filepath.Dir(entry.CargoTargetDir)); !os.IsNotExist(statErr) {
		t.Fatalf("empty cargo target id parent stat error = %v, want not exist", statErr)
	}
	if _, statErr := os.Stat(entry.CargoHome); statErr != nil {
		t.Fatalf("reclaim removed shared cargo home: %v", statErr)
	}
}

func TestWorktreeReclaimUsesPromotedAncestryFallback(t *testing.T) {
	cityPath, rig, cfg := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	stubWorktreeTestLiveness(t)
	entry := createWorktreeTestEntry(t, cityPath, rig, "ancestry", "ancestry")
	if _, err := publishRegisteredWorktree(context.Background(), cityPath, entry.ID, events.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry.Path, "next.txt"), []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, entry.Path, "add", "next.txt")
	worktreeTestGit(t, entry.Path, "commit", "-m", "candidate head")
	candidateHead := worktreeTestGitOutput(t, entry.Path, "rev-parse", "HEAD")
	tree := worktreeTestGitOutput(t, entry.Path, "rev-parse", "HEAD^{tree}")
	promotedSHA := worktreeTestGitWithInput(t, rig.Root, "promoted\n", "commit-tree", tree, "-p", candidateHead)

	result, err := reclaimRegisteredWorktree(context.Background(), cityPath, cfg, entry.ID, promotedSHA, false, events.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Reclaimed || result.HeadSHA != candidateHead {
		t.Fatalf("ancestry reclaim result = %+v, want head %s", result, candidateHead)
	}
	assertWorktreeTestRegistered(t, cityPath, entry.ID, false)
}

func TestWorktreeReclaimDryRunPreservesCheckoutAndRegistry(t *testing.T) {
	cityPath, rig, cfg := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	stubWorktreeTestLiveness(t)
	entry := createWorktreeTestEntry(t, cityPath, rig, "dry-run", "dry-run")
	if _, err := publishRegisteredWorktree(context.Background(), cityPath, entry.ID, events.Discard); err != nil {
		t.Fatal(err)
	}

	result, err := reclaimRegisteredWorktree(context.Background(), cityPath, cfg, entry.ID, "", true, events.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reclaimed || !result.Reclaimable || !result.DryRun || result.Reason != "dry-run: would reclaim" {
		t.Fatalf("dry-run result = %+v", result)
	}
	if _, statErr := os.Stat(entry.Path); statErr != nil {
		t.Fatalf("dry-run removed checkout: %v", statErr)
	}
	assertWorktreeTestRegistered(t, cityPath, entry.ID, true)
	if _, statErr := os.Stat(entry.CargoTargetDir); statErr != nil {
		t.Fatalf("dry-run removed per-attempt cargo target: %v", statErr)
	}
}

func TestWorktreeReclaimCacheValidationFailurePreservesCheckoutAndRegistry(t *testing.T) {
	cityPath, rig, cfg := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	stubWorktreeTestLiveness(t)
	entry := createWorktreeTestEntry(t, cityPath, rig, "cache-symlink", "cache-symlink")
	if _, err := publishRegisteredWorktree(context.Background(), cityPath, entry.ID, events.Discard); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(entry.CargoTargetDir); err != nil {
		t.Fatal(err)
	}
	escape := t.TempDir()
	sentinel := filepath.Join(escape, "sentinel")
	if err := os.WriteFile(sentinel, []byte("preserve\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(escape, entry.CargoTargetDir); err != nil {
		t.Fatal(err)
	}

	result, err := reclaimRegisteredWorktree(context.Background(), cityPath, cfg, entry.ID, "", false, events.Discard)
	if err == nil || result.Reclaimed || result.Reclaimable {
		t.Fatalf("cache validation reclaim = (%+v, %v), want preserved failure", result, err)
	}
	if !strings.Contains(result.Reason, "cargo target cleanup failed") {
		t.Fatalf("reason = %q", result.Reason)
	}
	if _, statErr := os.Stat(entry.Path); statErr != nil {
		t.Fatalf("cache validation failure removed checkout: %v", statErr)
	}
	if contents, readErr := os.ReadFile(sentinel); readErr != nil || string(contents) != "preserve\n" {
		t.Fatalf("cache validation failure disturbed escape target: contents=%q err=%v", contents, readErr)
	}
	assertWorktreeTestRegistered(t, cityPath, entry.ID, true)
}

func TestWorktreeListReportsSizePublicationAndReasons(t *testing.T) {
	cityPath, rig, cfg := newWorktreeTestRig(t)
	installWorktreeTestHelper(t, false)
	stubWorktreeTestLiveness(t)
	published := createWorktreeTestEntry(t, cityPath, rig, "published", "published")
	if _, err := publishRegisteredWorktree(context.Background(), cityPath, published.ID, events.Discard); err != nil {
		t.Fatal(err)
	}
	unpublished := createWorktreeTestEntry(t, cityPath, rig, "unpublished", "unpublished")
	dirty := createWorktreeTestEntry(t, cityPath, rig, "dirty-list", "dirty-list")
	if err := os.WriteFile(filepath.Join(dirty.Path, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rows, err := listRegisteredWorktrees(context.Background(), cityPath, cfg, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("list rows = %+v", rows)
	}
	byID := make(map[string]worktreeListEntry, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
		if row.SizeBytes <= 0 {
			t.Errorf("%s size_bytes = %d, want positive", row.ID, row.SizeBytes)
		}
	}
	if row := byID[published.ID]; !row.Published || !row.Reclaimable || row.Reason != "" {
		t.Errorf("published row = %+v", row)
	}
	if row := byID[unpublished.ID]; row.Reclaimable || !strings.Contains(row.Reason, "no verified publication") {
		t.Errorf("unpublished row = %+v", row)
	}
	if row := byID[dirty.ID]; row.Reclaimable || !strings.Contains(row.Reason, "uncommitted work") {
		t.Errorf("dirty row = %+v", row)
	}
}

func TestWorktreeJSONUsesPinnedSnakeCaseFields(t *testing.T) {
	entry := worktreeRegistryEntry{
		ID: "id", Owner: "owner", Rig: "rig", RigRoot: "/rig", Path: "/rig/worktrees/id",
		Attempt: 1, Base: "base", Branch: "HEAD", HeadSHA: "abc", CreatedAt: "time",
		CargoTargetDir: "/rig/worktrees/.cargo-targets/id/attempt-1", CargoHome: "/rig/.gc/cache/cargo-home",
	}
	var stdout bytes.Buffer
	if err := writeWorktreeEntryOutput(&stdout, worktreeListEntry{worktreeRegistryEntry: entry}, true); err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"id", "owner", "rig", "rig_root", "path", "attempt", "base", "branch", "head_sha", "created_at", "cargo_target_dir", "cargo_home", "published", "published_ref", "published_sha", "published_at", "size_bytes", "reclaimable", "reason"} {
		if _, ok := object[key]; !ok {
			t.Errorf("create JSON missing %q: %s", key, stdout.String())
		}
	}
	for _, forbidden := range []string{"rigRoot", "headSha", "cargoTargetDir", "cargoHome"} {
		if _, ok := object[forbidden]; ok {
			t.Errorf("create JSON contains camelCase field %q", forbidden)
		}
	}
}

func newWorktreeTestRig(t *testing.T) (string, worktreeRig, *config.City) {
	t.Helper()
	root := t.TempDir()
	cityPath := filepath.Join(root, "city")
	rigRoot := filepath.Join(root, "rig")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, rigRoot, "init", "-b", "main")
	worktreeTestGit(t, rigRoot, "config", "user.name", "Worktree Test")
	worktreeTestGit(t, rigRoot, "config", "user.email", "worktree@example.invalid")
	if err := os.WriteFile(filepath.Join(rigRoot, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	worktreeTestGit(t, rigRoot, "add", "seed.txt")
	worktreeTestGit(t, rigRoot, "commit", "-m", "seed")
	rig := worktreeRig{Name: "test-rig", Root: rigRoot}
	cfg := &config.City{Rigs: []config.Rig{{Name: rig.Name, Path: rigRoot}}}
	return cityPath, rig, cfg
}

func installWorktreeTestHelper(t *testing.T, failRegister bool) string {
	t.Helper()
	helper := filepath.Join(t.TempDir(), "gc-code-storage")
	fail := "False"
	if failRegister {
		fail = "True"
	}
	body := `#!/usr/bin/env python3
import json
import os
import subprocess
import sys

if len(sys.argv) != 3 or sys.argv[2] != "--json-request" or sys.argv[1] not in ("register", "publish"):
    raise SystemExit(64)
request = json.loads(sys.stdin.read())
log_path = os.environ.get("GC_WORKTREE_TEST_REQUEST_LOG")
if log_path:
    with open(log_path, "a", encoding="utf-8") as log:
        json.dump({
            "argv": sys.argv[1:],
            "request": request,
            "has_pierre_private_key": "PIERRE_PRIVATE_KEY" in os.environ,
        }, log, separators=(",", ":"))
        log.write("\n")
if sys.argv[1] == "register":
    if ` + fail + `:
        sys.stderr.write("""remote rejected lifecycle
credential=super-secret-token
jwt=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJzZWNyZXQifQ.signature123
jwt_truncated=abcdefgh.ijklmnop.
auth=opaque-authorization-secret
Authorization: token github-authorization-secret
url=https://user:password@example.invalid/repository
signed=https://example.invalid/archive?X-Amz-Credential=credential-value&X-Amz-Signature=signature-value
Authorization: Bearer authorization-secret
Proxy-Authorization: Basic proxy-secret
tenant=""" + os.environ.get("GC_WORKTREE_TEST_SECRET", "") + "\n")
        sys.stderr.write("-----BEGIN PRIVATE KEY-----\n" + ("private-key-material" * 1024))
        print("https://user:stdout-secret@example.invalid/attached")
        raise SystemExit(23)
    reply = {
        "version": request["version"],
        "requestId": request["requestId"],
        "ok": True,
        "operation": "registerAttempt",
        "lifecycleId": request["lifecycleId"],
        "owner": request["owner"],
        "rig": request["rig"],
        "rigRoot": request["rigRoot"],
        "worktreePath": request["worktreePath"],
        "attempt": request["attempt"],
        "base": request["base"],
        "branch": request["branch"],
        "headSha": request["headSha"],
    }
    if os.environ.get("GC_WORKTREE_TEST_REGISTER_EXTRA"):
        reply["unexpected"] = "super-secret-helper-output"
    if os.environ.get("GC_WORKTREE_TEST_REGISTER_MISMATCH"):
        reply["requestId"] = "123e4567-e89b-42d3-a456-426614174999"
    encoded = json.dumps(reply, separators=(",", ":"))
    if os.environ.get("GC_WORKTREE_TEST_REGISTER_PADDING"):
        encoded = (" " * 9000) + encoded
    sys.stdout.write(encoded + "\n")
    if os.environ.get("GC_WORKTREE_TEST_REGISTER_OVERFLOW"):
        sys.stdout.write((" " * (33 * 1024)) + '{"unexpected":true}\n')
    if os.environ.get("GC_WORKTREE_TEST_REGISTER_TRAILING"):
        print("super-secret-helper-output")
    raise SystemExit(0)

worktree = os.environ.get("GC_WORKTREE_TEST_REPLY_WORKTREE", request["worktreePath"])
ref = os.environ.get(
    "GC_WORKTREE_TEST_REPLY_REF",
    f'sessions/{request["lifecycleId"]}/attempt/{request["attempt"]}',
)
head = os.environ.get("GC_WORKTREE_TEST_REPLY_HEAD_SHA", request["headSha"])
reply = {
    "worktree": worktree,
    "ref": ref,
    "head_sha": head,
    "pushed": os.environ.get("GC_WORKTREE_TEST_REPLY_PUSHED", "true") == "true",
    "already_up_to_date": os.environ.get(
        "GC_WORKTREE_TEST_REPLY_ALREADY_UP_TO_DATE", "false"
    ) == "true",
}
if os.environ.get("GC_WORKTREE_TEST_REPLY_EXTRA"):
    reply["unexpected"] = "super-secret-helper-output"
if os.environ.get("GC_WORKTREE_TEST_ADVANCE_HEAD"):
    subprocess.run(
        ["git", "-C", request["worktreePath"], "commit", "--allow-empty", "-m", "advance during publish"],
        check=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
print(json.dumps(reply, separators=(",", ":")))
if os.environ.get("GC_WORKTREE_TEST_REPLY_TRAILING"):
    print("super-secret-helper-output")
`
	if err := os.WriteFile(helper, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CODE_STORAGE_HELPER", helper)
	return helper
}

func createWorktreeTestEntry(t *testing.T, cityPath string, rig worktreeRig, id, relativePath string) worktreeRegistryEntry {
	t.Helper()
	entry, err := createRegisteredWorktree(context.Background(), cityPath, rig, worktreeCreateOptions{
		ID: id, Owner: "owner-" + id, Path: relativePath, Base: "HEAD", Attempt: 1,
	}, events.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func stubWorktreeTestLiveness(t *testing.T) {
	t.Helper()
	previous := collectLiveWorktreeStateFn
	collectLiveWorktreeStateFn = func() liveWorktreeState {
		return liveWorktreeState{scanned: true, source: liveScanSourceProc}
	}
	t.Cleanup(func() { collectLiveWorktreeStateFn = previous })
}

func assertWorktreeTestRegistered(t *testing.T, cityPath, id string, want bool) {
	t.Helper()
	registry, err := loadWorktreeRegistry(worktreeRegistryFilePaths(cityPath).Registry)
	if err != nil {
		t.Fatal(err)
	}
	_, got := findWorktreeRegistryEntry(registry, id)
	if got != want {
		t.Fatalf("registered(%s) = %t, want %t; entries=%+v", id, got, want, registry.Entries)
	}
}

func worktreeTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	_ = worktreeTestGitOutput(t, dir, args...)
}

func worktreeTestGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath="}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func worktreeTestGitWithInput(t *testing.T, dir, input string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath="}, args...)...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
