package events

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type samplePayload struct {
	A string `json:"a"`
}

func (samplePayload) IsEventPayload() {}

func TestRegisterAndLookup(t *testing.T) {
	const evt = "test.register.lookup"
	// Clean up after the test to avoid polluting global registry.
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})

	RegisterPayload(evt, samplePayload{})
	got, ok := LookupPayload(evt)
	if !ok {
		t.Fatalf("expected registered event %q to be found", evt)
	}
	if _, ok := got.(samplePayload); !ok {
		t.Fatalf("expected samplePayload, got %T", got)
	}
}

func TestDecodePayloadRegistered(t *testing.T) {
	const evt = "test.decode.registered"
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})
	RegisterPayload(evt, samplePayload{})

	raw := json.RawMessage(`{"a":"hello"}`)
	got, registered, err := DecodePayload(evt, raw)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if !registered {
		t.Fatalf("expected registered=true")
	}
	sp, ok := got.(samplePayload)
	if !ok {
		t.Fatalf("expected samplePayload, got %T", got)
	}
	if sp.A != "hello" {
		t.Fatalf("A = %q, want hello", sp.A)
	}
}

func TestDecodePayloadUnregistered(t *testing.T) {
	raw := json.RawMessage(`{"anything":true}`)
	got, registered, err := DecodePayload("test.never.registered", raw)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if registered {
		t.Fatalf("expected registered=false")
	}
	if got != nil {
		t.Fatalf("expected nil payload, got %v", got)
	}
}

func TestDecodePayloadEmptyBytesZeroValue(t *testing.T) {
	const evt = "test.decode.empty"
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})
	RegisterPayload(evt, NoPayload{})

	got, registered, err := DecodePayload(evt, nil)
	if err != nil {
		t.Fatalf("decode err: %v", err)
	}
	if !registered {
		t.Fatalf("expected registered=true")
	}
	if _, ok := got.(NoPayload); !ok {
		t.Fatalf("expected NoPayload, got %T", got)
	}
}

func TestRegisterConflictPanics(t *testing.T) {
	const evt = "test.conflict"
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})
	RegisterPayload(evt, samplePayload{})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on conflicting re-registration")
		}
	}()
	RegisterPayload(evt, NoPayload{})
}

func TestRegisterSameTypeIdempotent(t *testing.T) {
	const evt = "test.idempotent"
	t.Cleanup(func() {
		payloadRegistryMu.Lock()
		delete(payloadRegistry, evt)
		payloadRegistryMu.Unlock()
	})
	RegisterPayload(evt, samplePayload{})
	// Second call with same type must not panic.
	RegisterPayload(evt, samplePayload{})
}

func TestWorktreeLifecyclePayloadsAreKnownTypedAndCredentialFree(t *testing.T) {
	t.Parallel()

	tests := []struct {
		eventType string
		payload   Payload
		keys      []string
	}{
		{
			eventType: WorktreeCreated,
			payload: WorktreeCreatedPayload{
				ID:      "candidate-42",
				Owner:   "workflow-42",
				Rig:     "gascity",
				Path:    "/city/gascity/worktrees/candidate-42",
				HeadSHA: "created-sha",
			},
			keys: []string{"id", "owner", "rig", "path", "head_sha"},
		},
		{
			eventType: WorktreePublished,
			payload: WorktreePublishedPayload{
				ID:      "candidate-42",
				Owner:   "workflow-42",
				Rig:     "gascity",
				Path:    "/city/gascity/worktrees/candidate-42",
				Ref:     "refs/code-storage/candidate-42",
				HeadSHA: "published-sha",
			},
			keys: []string{"id", "owner", "rig", "path", "ref", "head_sha"},
		},
		{
			eventType: WorktreeReclaimSkipped,
			payload: WorktreeReclaimSkippedPayload{
				ID:      "candidate-42",
				Owner:   "workflow-42",
				Rig:     "gascity",
				Path:    "/city/gascity/worktrees/candidate-42",
				HeadSHA: "preserved-sha",
				Reason:  "worktree is live",
				DryRun:  true,
			},
			keys: []string{"id", "owner", "rig", "path", "head_sha", "reason", "dry_run"},
		},
		{
			eventType: WorktreeReclaimed,
			payload: WorktreeReclaimedPayload{
				ID:      "candidate-42",
				Owner:   "workflow-42",
				Rig:     "gascity",
				Path:    "/city/gascity/worktrees/candidate-42",
				HeadSHA: "reclaimed-sha",
				DryRun:  false,
			},
			keys: []string{"id", "owner", "rig", "path", "head_sha", "dry_run"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.eventType, func(t *testing.T) {
			t.Parallel()

			if !slices.Contains(KnownEventTypes, tt.eventType) {
				t.Fatalf("%q is missing from KnownEventTypes", tt.eventType)
			}
			sample, ok := LookupPayload(tt.eventType)
			if !ok {
				t.Fatalf("%q has no registered payload", tt.eventType)
			}
			if reflect.TypeOf(sample) != reflect.TypeOf(tt.payload) {
				t.Fatalf("%q registered payload is %T, want %T", tt.eventType, sample, tt.payload)
			}

			raw, err := json.Marshal(tt.payload)
			if err != nil {
				t.Fatalf("marshal %q payload: %v", tt.eventType, err)
			}
			decoded, typed, err := DecodePayload(tt.eventType, raw)
			if err != nil {
				t.Fatalf("DecodePayload(%q): %v", tt.eventType, err)
			}
			if !typed {
				t.Fatalf("DecodePayload(%q) reported no registered type", tt.eventType)
			}
			if !reflect.DeepEqual(decoded, tt.payload) {
				t.Fatalf("%q round-trip = %+v, want %+v", tt.eventType, decoded, tt.payload)
			}

			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatalf("decode %q JSON fields: %v", tt.eventType, err)
			}
			if len(fields) != len(tt.keys) {
				t.Fatalf("%q JSON fields = %v, want exactly %v", tt.eventType, fields, tt.keys)
			}
			for _, key := range tt.keys {
				if _, ok := fields[key]; !ok {
					t.Errorf("%q payload is missing JSON field %q", tt.eventType, key)
				}
			}
			for key := range fields {
				lower := strings.ToLower(key)
				for _, forbidden := range []string{"credential", "password", "private", "secret", "token"} {
					if strings.Contains(lower, forbidden) {
						t.Errorf("%q payload carries credential-like JSON field %q", tt.eventType, key)
					}
				}
			}
		})
	}
}

func TestLegacyBeadWorktreePayloadRegistrationsRemainTyped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		eventType string
		want      Payload
	}{
		{eventType: BeadWorktreeReaped, want: BeadWorktreeReapedPayload{}},
		{eventType: BeadWorktreeReapSkipped, want: BeadWorktreeReapSkippedPayload{}},
	}
	for _, tt := range tests {
		got, ok := LookupPayload(tt.eventType)
		if !ok {
			t.Errorf("legacy event %q has no registered payload", tt.eventType)
			continue
		}
		if reflect.TypeOf(got) != reflect.TypeOf(tt.want) {
			t.Errorf("legacy event %q registered payload is %T, want %T", tt.eventType, got, tt.want)
		}
	}
}
