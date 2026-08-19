package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestTraceStartStopStatusOfflineFallback(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "mayor")
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdTraceStart("repo/polecat", "15m", false, string(TraceModeDetail), &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceStart = %d; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "armed manual repo/polecat") {
		t.Fatalf("start output = %q, want armed confirmation", got)
	}

	status, _, err := traceStatusLocal(cityDir)
	if err != nil {
		t.Fatalf("traceStatusLocal: %v", err)
	}
	if status == nil {
		t.Fatal("traceStatusLocal returned nil status")
	}
	if len(status.ActiveArms) != 1 {
		t.Fatalf("active arms = %d, want 1", len(status.ActiveArms))
	}
	arm := status.ActiveArms[0]
	if arm.ScopeValue != "repo/polecat" {
		t.Fatalf("scope_value = %q, want repo/polecat", arm.ScopeValue)
	}
	if arm.Level != TraceModeDetail {
		t.Fatalf("level = %q, want detail", arm.Level)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdTraceStatusWithJSON(false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceStatusWithJSON = %d; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "Head seq: 0") || !strings.Contains(got, "repo/polecat") {
		t.Fatalf("status output = %q, want head_seq and arm info", got)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdTraceStatusWithJSON(true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceStatusWithJSON = %d; stderr=%s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var statusJSON traceStatusResultJSON
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &statusJSON); err != nil {
		t.Fatalf("unmarshal trace status JSON: %v; output=%s", err, stdout.String())
	}
	if statusJSON.SchemaVersion != "1" || statusJSON.HeadSeq != 0 || len(statusJSON.ActiveArms) != 1 {
		t.Fatalf("trace status JSON = %+v, want schema version, head seq, active arm", statusJSON)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdTraceStop("repo/polecat", false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceStop = %d; stderr=%s", code, stderr.String())
	}
	status, _, err = traceStatusLocal(cityDir)
	if err != nil {
		t.Fatalf("traceStatusLocal after stop: %v", err)
	}
	if status == nil {
		t.Fatal("traceStatusLocal after stop returned nil status")
	}
	if len(status.ActiveArms) != 0 {
		t.Fatalf("active arms after stop = %d, want 0", len(status.ActiveArms))
	}
}

func TestTraceStatusJSONEmptyArmsConformsToSchema(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "mayor")
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdTraceStatusWithJSON(true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceStatusWithJSON = %d; stderr=%s", code, stderr.String())
	}
	validateJSONResultSchema(t, []string{"trace", "status"}, stdout.Bytes())
}

func TestTraceStatusHeadSeqPrefersStatusSnapshot(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "mayor")

	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("newSessionReconcilerTraceStore: %v", err)
	}
	rec := newTraceRecord(TraceRecordDecision)
	rec.TraceID = "cycle-1"
	rec.TickID = "tick-1"
	rec.RecordID = "record-1"
	rec.Ts = time.Now().UTC()
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{rec}, TraceDurabilityMetadata); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close trace store: %v", err)
	}
	storedHead, err := traceHeadSeq(traceCityRuntimeDir(cityDir))
	if err != nil {
		t.Fatalf("traceHeadSeq: %v", err)
	}
	if storedHead == 42 {
		t.Fatal("stored head unexpectedly matches socket snapshot fixture")
	}

	head, err := traceStatusHeadSeq(traceStatusJSON{HeadSeq: 42}, cityDir)
	if err != nil {
		t.Fatalf("traceStatusHeadSeq with snapshot head: %v", err)
	}
	if head != 42 {
		t.Fatalf("head seq = %d, want socket snapshot 42", head)
	}
	head, err = traceStatusHeadSeq(traceStatusJSON{}, cityDir)
	if err != nil {
		t.Fatalf("traceStatusHeadSeq fallback: %v", err)
	}
	if head != storedHead {
		t.Fatalf("fallback head seq = %d, want stored head %d", head, storedHead)
	}
}

func TestTraceControllerSocketCommands(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime"), 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}

	startReq := traceControlRequest{
		Action:         "start",
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     "repo/polecat",
		Source:         TraceArmSourceManual,
		Level:          TraceModeDetail,
		For:            "10m",
		ActorKind:      "cli",
		CommandSummary: traceCommandSummary("trace.start", "repo/polecat", "10m", false),
	}
	pokeCh1 := make(chan struct{}, 1)
	startReply := sendTraceSocketCommand(t, cityDir, "trace-arm", startReq, pokeCh1)
	if !startReply.OK {
		t.Fatalf("start reply error: %s", startReply.Error)
	}
	if startReply.Status == nil || len(startReply.Status.ActiveArms) != 1 {
		t.Fatalf("start reply status = %#v", startReply.Status)
	}
	select {
	case <-pokeCh1:
	default:
		t.Fatal("expected pokeCh to be signaled on start")
	}

	pokeCh2 := make(chan struct{}, 1)
	statusReply := sendTraceStatusSocketCommand(t, cityDir, pokeCh2)
	if !statusReply.OK {
		t.Fatalf("status reply error: %s", statusReply.Error)
	}
	if statusReply.Status == nil || len(statusReply.Status.ActiveArms) != 1 {
		t.Fatalf("status reply = %#v", statusReply.Status)
	}
	statusPayload, err := json.Marshal(statusReply.Status)
	if err != nil {
		t.Fatalf("marshal status reply: %v", err)
	}
	if bytes.Contains(statusPayload, []byte(`"arms"`)) {
		t.Fatalf("status reply JSON = %s, must not carry legacy arms alias", statusPayload)
	}
	select {
	case <-pokeCh2:
		t.Fatal("did not expect pokeCh to be signaled on status")
	default:
	}

	stopReq := traceControlRequest{
		Action:         "stop",
		ScopeType:      TraceArmScopeTemplate,
		ScopeValue:     "repo/polecat",
		Source:         TraceArmSourceManual,
		All:            false,
		ActorKind:      "cli",
		CommandSummary: traceCommandSummary("trace.stop", "repo/polecat", "", false),
	}
	pokeCh3 := make(chan struct{}, 1)
	stopReply := sendTraceSocketCommand(t, cityDir, "trace-stop", stopReq, pokeCh3)
	if !stopReply.OK {
		t.Fatalf("stop reply error: %s", stopReply.Error)
	}
	if stopReply.Status == nil || len(stopReply.Status.ActiveArms) != 0 {
		t.Fatalf("stop reply status = %#v", stopReply.Status)
	}
	stopPayload, err := json.Marshal(stopReply.Status)
	if err != nil {
		t.Fatalf("marshal stop status reply: %v", err)
	}
	if bytes.Contains(stopPayload, []byte(`"arms"`)) {
		t.Fatalf("stop status reply JSON = %s, must not carry legacy arms alias", stopPayload)
	}
	select {
	case <-pokeCh3:
	default:
		t.Fatal("expected pokeCh to be signaled on stop")
	}
}

func TestTraceControllerSocketInvalidRequestDoesNotPoke(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck

	cityDir := t.TempDir()
	convergenceReqCh := make(chan convergenceRequest, 1)
	pokeCh := make(chan struct{}, 1)
	controlDispatcherCh := make(chan struct{}, 1)

	done := make(chan struct{})
	go func() {
		handleControllerConn(server, cityDir, controllerHostingStandalone, func() {}, nil, nil, nil, convergenceReqCh, pokeCh, controlDispatcherCh)
		close(done)
	}()

	if _, err := fmt.Fprintln(client, "trace-arm:{not-json}"); err != nil {
		t.Fatalf("write invalid trace-arm: %v", err)
	}
	reply := readTraceSocketReply(t, client)
	if reply.OK {
		t.Fatal("invalid trace-arm unexpectedly succeeded")
	}
	select {
	case <-pokeCh:
		t.Fatal("invalid trace-arm should not poke controller")
	default:
	}

	client.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("controller socket handler did not exit")
	}
}

func TestTraceShowAndReasonsWithoutTemplateFilter(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "mayor")
	t.Setenv("GC_CITY", cityDir)

	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("newSessionReconcilerTraceStore: %v", err)
	}
	defer store.Close() //nolint:errcheck

	rec := newTraceRecord(TraceRecordDecision)
	rec.TraceID = "cycle-1"
	rec.TickID = "tick-1"
	rec.RecordID = "record-1"
	rec.Template = "repo/polecat"
	rec.SessionName = "polecat-1"
	rec.SiteCode = TraceSiteReconcilerWakeDecision
	rec.ReasonCode = TraceReasonIdle
	rec.OutcomeCode = TraceOutcomeApplied
	rec.Ts = time.Now().UTC()
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{rec}, TraceDurabilityMetadata); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdTraceShow("", "", "", "", "", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceShow = %d; stderr=%s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var showJSON traceShowResultJSON
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &showJSON); err != nil {
		t.Fatalf("unmarshal trace show JSON: %v; output=%s", err, stdout.String())
	}
	foundTemplate := false
	for _, record := range showJSON.Records {
		if record.Template == "repo/polecat" {
			foundTemplate = true
			break
		}
	}
	if showJSON.SchemaVersion != "1" || showJSON.Count != len(showJSON.Records) || !foundTemplate {
		t.Fatalf("trace show JSON = %+v, want repo/polecat", showJSON)
	}
	validateJSONResultSchema(t, []string{"trace", "show"}, stdout.Bytes())
	assertTraceShowSchemaRecordProperty(t, "template")
	assertTraceShowSchemaRecordProperty(t, "session_name")

	stdout.Reset()
	stderr.Reset()
	if code := cmdTraceReasons("", "", &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceReasons = %d; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, string(TraceReasonIdle)) {
		t.Fatalf("trace reasons output = %q, want idle reason", got)
	}
}

func TestTraceShowRecoversCommittedRecordsAcrossMalformedSegment(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "mayor")
	t.Setenv("GC_CITY", cityDir)

	now := time.Now().UTC()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("newSessionReconcilerTraceStore: %v", err)
	}
	old := newTraceRecord(TraceRecordDecision)
	old.TraceID = "cycle-before-gap"
	old.TickID = "tick-before-gap"
	old.RecordID = "record-before-gap"
	old.Template = "repo/polecat"
	old.Ts = now.Add(-2 * time.Hour)
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{old}, TraceDurabilityMetadata); err != nil {
		t.Fatalf("append committed prefix: %v", err)
	}
	if err := store.rotateSegment(now); err != nil {
		t.Fatalf("rotate trace segment: %v", err)
	}
	later := old
	later.TraceID = "cycle-after-gap"
	later.TickID = "tick-after-gap"
	later.RecordID = "record-after-gap"
	later.Ts = now
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{later}, TraceDurabilityMetadata); err != nil {
		t.Fatalf("append later segment: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close trace store: %v", err)
	}

	root := traceCityRuntimeDir(cityDir)
	segments, err := filepath.Glob(filepath.Join(root, sessionReconcilerTraceSegments, "*", "*", "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("glob trace segments: %v", err)
	}
	if len(segments) != 2 {
		t.Fatalf("trace segments = %d, want 2", len(segments))
	}
	if err := appendMalformedTraceLine(segments[0]); err != nil {
		t.Fatalf("append malformed trace line: %v", err)
	}

	records, err := ReadTraceRecords(root, TraceFilter{})
	if !beads.IsPartialResult(err) {
		t.Fatalf("ReadTraceRecords error = %v, want PartialResultError", err)
	}
	var gapErr *traceReadGapError
	if !errors.As(err, &gapErr) {
		t.Fatalf("ReadTraceRecords error = %v, want traceReadGapError", err)
	}
	if gapErr.Segments != 1 {
		t.Fatalf("gap segments = %d, want 1", gapErr.Segments)
	}
	if len(records) != 4 {
		t.Fatalf("ReadTraceRecords returned %d records, want two records and two commits", len(records))
	}
	traceIDs := make(map[string]bool)
	for _, record := range records {
		traceIDs[record.TraceID] = true
	}
	if !traceIDs[old.TraceID] || !traceIDs[later.TraceID] {
		t.Fatalf("ReadTraceRecords trace IDs = %v, want records on both sides of gap", traceIDs)
	}

	quarantined, err := filepath.Glob(filepath.Join(root, sessionReconcilerTraceQuarantine, "*"))
	if err != nil {
		t.Fatalf("glob quarantined trace suffixes: %v", err)
	}
	if len(quarantined) != 1 {
		t.Fatalf("quarantined trace suffixes = %d, want 1", len(quarantined))
	}

	var stdout, stderr bytes.Buffer
	if err := appendMalformedTraceLine(segments[0]); err != nil {
		t.Fatalf("append malformed trace line for trace show: %v", err)
	}

	if code := cmdTraceShow("repo/polecat", "1h", "", "", "", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceShow = %d; stderr=%s", code, stderr.String())
	}
	var showJSON traceShowResultJSON
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &showJSON); err != nil {
		t.Fatalf("unmarshal trace show JSON: %v; output=%s", err, stdout.String())
	}
	if len(showJSON.Gaps) != 1 {
		t.Fatalf("trace show gaps = %+v, want exactly one", showJSON.Gaps)
	}
	if showJSON.Gaps[0].Code != traceGapCodeSegmentSkipped || showJSON.Gaps[0].Segments != 1 {
		t.Fatalf("trace show gap = %+v, want one skipped segment", showJSON.Gaps[0])
	}
	if len(showJSON.Records) != 1 {
		t.Fatalf("trace show records = %+v, want later record after --since filter", showJSON.Records)
	}
	if showJSON.Records[0].TraceID != later.TraceID {
		t.Fatalf("trace show record trace_id = %q, want %q", showJSON.Records[0].TraceID, later.TraceID)
	}

	records, err = ReadTraceRecords(root, TraceFilter{})
	if err != nil {
		t.Fatalf("second ReadTraceRecords: %v", err)
	}
	validateJSONResultSchema(t, []string{"trace", "show"}, stdout.Bytes())
	if len(records) != 4 {
		t.Fatalf("second ReadTraceRecords returned %d records, want recovered prefix and later segment", len(records))
	}
}

func appendMalformedTraceLine(path string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, sessionReconcilerTraceOwnerFilePerm)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(f, "{malformed-json}\n"); err != nil {
		f.Close() //nolint:errcheck
		return err
	}
	return f.Close()
}

func TestReadTraceRecordsNeverRepairsActiveSegment(t *testing.T) {
	cityDir := t.TempDir()
	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("newSessionReconcilerTraceStore: %v", err)
	}
	defer store.Close() //nolint:errcheck

	record := newTraceRecord(TraceRecordDecision)
	record.TraceID = "active-cycle"
	record.TickID = "active-tick"
	record.RecordID = "active-record"
	record.Ts = time.Now().UTC()
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{record}, TraceDurabilityMetadata); err != nil {
		t.Fatalf("append active batch: %v", err)
	}
	activePath := store.currentPath
	if err := appendMalformedTraceLine(activePath); err != nil {
		t.Fatalf("append malformed active line: %v", err)
	}
	before, err := os.Stat(activePath)
	if err != nil {
		t.Fatalf("stat active segment before read: %v", err)
	}

	_, err = ReadTraceRecords(traceCityRuntimeDir(cityDir), TraceFilter{})
	if !beads.IsPartialResult(err) {
		t.Fatalf("ReadTraceRecords error = %v, want PartialResultError", err)
	}
	after, err := os.Stat(activePath)
	if err != nil {
		t.Fatalf("active segment was moved during live read: %v", err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("active segment size = %d after read, want unchanged %d", after.Size(), before.Size())
	}
	quarantined, err := filepath.Glob(filepath.Join(traceCityRuntimeDir(cityDir), sessionReconcilerTraceQuarantine, "*"))
	if err != nil {
		t.Fatalf("glob trace quarantine: %v", err)
	}
	if len(quarantined) != 0 {
		t.Fatalf("active segment produced %d quarantine files, want none", len(quarantined))
	}

	next := record
	next.RecordID = "active-record-after-read"
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{next}, TraceDurabilityMetadata); err != nil {
		t.Fatalf("append after live read: %v", err)
	}
	appended, err := os.Stat(activePath)
	if err != nil {
		t.Fatalf("stat active segment after append: %v", err)
	}
	if appended.Size() <= after.Size() {
		t.Fatalf("active segment did not grow after append: before=%d after=%d", after.Size(), appended.Size())
	}
}

func TestTraceShowTextIncludesPopulatedRecordSummary(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "mayor")
	t.Setenv("GC_CITY", cityDir)

	store, err := newSessionReconcilerTraceStore(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("newSessionReconcilerTraceStore: %v", err)
	}
	defer store.Close() //nolint:errcheck

	rec := newTraceRecord(TraceRecordDecision)
	rec.TraceID = "cycle-1"
	rec.TickID = "tick-1"
	rec.RecordID = "record-1"
	rec.Template = "repo/polecat"
	rec.SessionName = "polecat-1"
	rec.SiteCode = TraceSiteReconcilerWakeDecision
	rec.ReasonCode = TraceReasonIdle
	rec.OutcomeCode = TraceOutcomeApplied
	rec.Ts = time.Now().UTC()
	if err := store.AppendBatch([]SessionReconcilerTraceRecord{rec}, TraceDurabilityMetadata); err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdTraceShow("", "", "", "", "", "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceShow = %d; stderr=%s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"template=repo/polecat", "session=polecat-1", string(TraceReasonIdle)} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want %q", out, want)
		}
	}
}

func TestTraceShowJSONEmptyRecordsConformsToSchema(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "mayor")
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdTraceShow("", "", "", "", "", "", true, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceShow = %d; stderr=%s", code, stderr.String())
	}
	validateJSONResultSchema(t, []string{"trace", "show"}, stdout.Bytes())
}

func TestTraceShowEmptyTextReportsNoRecords(t *testing.T) {
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "mayor")
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdTraceShow("", "", "", "", "", "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdTraceShow = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No trace records found") {
		t.Fatalf("stdout = %q, want empty trace message", stdout.String())
	}
}

func sendTraceSocketCommand(t *testing.T, cityDir, command string, req traceControlRequest, pokeCh chan struct{}) traceControlReply {
	t.Helper()
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck

	convergenceReqCh := make(chan convergenceRequest, 1)
	controlDispatcherCh := make(chan struct{}, 1)

	done := make(chan struct{})
	go func() {
		handleControllerConn(server, cityDir, controllerHostingStandalone, func() {}, nil, nil, nil, convergenceReqCh, pokeCh, controlDispatcherCh)
		close(done)
	}()

	payload, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if _, err := fmt.Fprintf(client, "%s:%s\n", command, payload); err != nil {
		t.Fatalf("write command: %v", err)
	}
	reply := readTraceSocketReply(t, client)
	client.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("controller socket handler did not exit")
	}
	return reply
}

func sendTraceStatusSocketCommand(t *testing.T, cityDir string, pokeCh chan struct{}) traceControlReply {
	t.Helper()
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck

	convergenceReqCh := make(chan convergenceRequest, 1)
	controlDispatcherCh := make(chan struct{}, 1)

	done := make(chan struct{})
	go func() {
		handleControllerConn(server, cityDir, controllerHostingStandalone, func() {}, nil, nil, nil, convergenceReqCh, pokeCh, controlDispatcherCh)
		close(done)
	}()

	if _, err := fmt.Fprintln(client, "trace-status"); err != nil {
		t.Fatalf("write status command: %v", err)
	}
	reply := readTraceSocketReply(t, client)
	client.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("controller socket handler did not exit")
	}
	return reply
}

func readTraceSocketReply(t *testing.T, conn net.Conn) traceControlReply {
	t.Helper()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			t.Fatalf("read reply: %v", err)
		}
		t.Fatal("read reply: connection closed")
	}
	var reply traceControlReply
	if err := json.Unmarshal(scanner.Bytes(), &reply); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return reply
}

func assertTraceShowSchemaRecordProperty(t *testing.T, name string) {
	t.Helper()
	rawSchema, err := readBuiltinSchema([]string{"trace", "show"}, jsonSchemaResultRole)
	if err != nil {
		t.Fatalf("read trace show result schema: %v", err)
	}
	var schema struct {
		Properties struct {
			Records struct {
				Items struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"items"`
			} `json:"records"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		t.Fatalf("unmarshal trace show result schema: %v", err)
	}
	if _, ok := schema.Properties.Records.Items.Properties[name]; !ok {
		t.Fatalf("trace show result schema missing record property %q", name)
	}
}
