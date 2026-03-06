package websocket_client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/skills"
	"github.com/sipeed/picoclaw/pkg/tools"
)

type fakeRegistry struct {
	name  string
	delay time.Duration
	fail  bool
}

func (r *fakeRegistry) Name() string { return r.name }

func (r *fakeRegistry) Search(_ context.Context, _ string, _ int) ([]skills.SearchResult, error) {
	return nil, nil
}

func (r *fakeRegistry) GetSkillMeta(_ context.Context, slug string) (*skills.SkillMeta, error) {
	return &skills.SkillMeta{Slug: slug, LatestVersion: "1.0.0", RegistryName: r.name}, nil
}

func (r *fakeRegistry) DownloadAndInstall(ctx context.Context, slug, version, targetDir string) (*skills.InstallResult, error) {
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if r.fail {
		return nil, fmt.Errorf("simulated registry failure for %s", slug)
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(targetDir, "SKILL.md"), []byte("# "+slug), 0o644); err != nil {
		return nil, err
	}
	if version == "" {
		version = "1.0.0"
	}
	return &skills.InstallResult{
		Version: version,
		Summary: "fake skill",
	}, nil
}

type capturedEvent struct {
	name    string
	content string
	meta    map[string]string
}

func makeHandler(t *testing.T, delay time.Duration) *skillInstallCommandHandler {
	t.Helper()
	return makeHandlerWithOpts(t, delay, false, []string{"fake"}, "")
}

func makeHandlerWithOpts(t *testing.T, delay time.Duration, fail bool, allowed []string, defaultReg string) *skillInstallCommandHandler {
	t.Helper()
	rm := skills.NewRegistryManager()
	rm.AddRegistry(&fakeRegistry{name: "fake", delay: delay, fail: fail})
	installer := tools.NewInstallSkillTool(rm, t.TempDir())
	return newSkillInstallCommandHandler(config.WSCommandsConfig{
		Enabled:           true,
		MaxConcurrent:     1,
		InstallTimeout:    5,
		DedupTTL:          60,
		AllowedRegistries: allowed,
		DefaultRegistry:   defaultReg,
	}, installer)
}

func collectSendFunc() (eventSender, *[]capturedEvent, *sync.Mutex) {
	var (
		mu     sync.Mutex
		events []capturedEvent
	)
	send := func(_ context.Context, name, content string, meta map[string]string) error {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, capturedEvent{name: name, content: content, meta: cloneMap(meta)})
		return nil
	}
	return send, &events, &mu
}

// --- Validation tests ---

func TestSkillInstallCommandHandler_InvalidPayload(t *testing.T) {
	t.Parallel()
	h := makeHandler(t, 0)
	var got capturedEvent
	send := func(_ context.Context, name, content string, meta map[string]string) error {
		got = capturedEvent{name: name, content: content, meta: meta}
		return nil
	}

	err := h.Handle(context.Background(), InboundWSMessage{
		Type:     "command",
		Content:  "not-json",
		Metadata: map[string]string{"command_name": "skill.install", "request_id": "req-1"},
	}, send)
	require.NoError(t, err)
	assert.Equal(t, "skill.install.failed", got.name)
	assert.Equal(t, "invalid_command_payload", got.meta["error_code"])
}

func TestSkillInstallCommandHandler_EmptyPayload(t *testing.T) {
	t.Parallel()
	h := makeHandler(t, 0)
	var got capturedEvent
	send := func(_ context.Context, name, content string, meta map[string]string) error {
		got = capturedEvent{name: name, content: content, meta: meta}
		return nil
	}

	err := h.Handle(context.Background(), InboundWSMessage{
		Type:     "command",
		Content:  "",
		Metadata: map[string]string{"command_name": "skill.install"},
	}, send)
	require.NoError(t, err)
	assert.Equal(t, "skill.install.failed", got.name)
	assert.Equal(t, "invalid_command_payload", got.meta["error_code"])
}

func TestSkillInstallCommandHandler_UnsupportedCommand(t *testing.T) {
	t.Parallel()
	h := makeHandler(t, 0)
	var got capturedEvent
	send := func(_ context.Context, name, content string, meta map[string]string) error {
		got = capturedEvent{name: name, content: content, meta: meta}
		return nil
	}

	err := h.Handle(context.Background(), InboundWSMessage{
		Type:     "command",
		Content:  `{"slug":"x"}`,
		Metadata: map[string]string{"command_name": "skill.uninstall"},
	}, send)
	require.NoError(t, err)
	assert.Equal(t, "skill.install.failed", got.name)
	assert.Equal(t, "unsupported_command", got.meta["error_code"])
}

func TestSkillInstallCommandHandler_MissingRequestID(t *testing.T) {
	t.Parallel()
	h := makeHandler(t, 0)
	var got capturedEvent
	send := func(_ context.Context, name, content string, meta map[string]string) error {
		got = capturedEvent{name: name, content: content, meta: meta}
		return nil
	}

	err := h.Handle(context.Background(), InboundWSMessage{
		Type:     "command",
		Content:  mustJSON(t, installCommandPayload{Slug: "my-skill", Registry: "fake"}),
		Metadata: map[string]string{"command_name": "skill.install"},
	}, send)
	require.NoError(t, err)
	assert.Equal(t, "skill.install.failed", got.name)
	assert.Equal(t, "invalid_command_payload", got.meta["error_code"])
	assert.Contains(t, got.content, "request_id is required")
}

func TestSkillInstallCommandHandler_MissingSlug(t *testing.T) {
	t.Parallel()
	h := makeHandler(t, 0)
	var got capturedEvent
	send := func(_ context.Context, name, content string, meta map[string]string) error {
		got = capturedEvent{name: name, content: content, meta: meta}
		return nil
	}

	err := h.Handle(context.Background(), InboundWSMessage{
		Type:     "command",
		Content:  mustJSON(t, installCommandPayload{RequestID: "req-1", Registry: "fake"}),
		Metadata: map[string]string{"command_name": "skill.install"},
	}, send)
	require.NoError(t, err)
	assert.Equal(t, "skill.install.failed", got.name)
	assert.Equal(t, "invalid_command_payload", got.meta["error_code"])
	assert.Contains(t, got.content, "slug is required")
}

func TestSkillInstallCommandHandler_RegistryRequired(t *testing.T) {
	t.Parallel()
	h := makeHandlerWithOpts(t, 0, false, nil, "")
	var got capturedEvent
	send := func(_ context.Context, name, content string, meta map[string]string) error {
		got = capturedEvent{name: name, content: content, meta: meta}
		return nil
	}

	err := h.Handle(context.Background(), InboundWSMessage{
		Type:     "command",
		Content:  mustJSON(t, installCommandPayload{RequestID: "req-1", Slug: "my-skill"}),
		Metadata: map[string]string{"command_name": "skill.install"},
	}, send)
	require.NoError(t, err)
	assert.Equal(t, "skill.install.failed", got.name)
	assert.Equal(t, "registry_required", got.meta["error_code"])
}

func TestSkillInstallCommandHandler_RegistryNotAllowed(t *testing.T) {
	t.Parallel()
	h := makeHandler(t, 0)
	var got capturedEvent
	send := func(_ context.Context, name, content string, meta map[string]string) error {
		got = capturedEvent{name: name, content: content, meta: meta}
		return nil
	}

	err := h.Handle(context.Background(), InboundWSMessage{
		Type: "command",
		Content: mustJSON(t, installCommandPayload{
			RequestID: "req-1",
			Slug:      "my-skill",
			Registry:  "evil-registry",
		}),
		Metadata: map[string]string{"command_name": "skill.install"},
	}, send)
	require.NoError(t, err)
	assert.Equal(t, "skill.install.failed", got.name)
	assert.Equal(t, "registry_not_allowed", got.meta["error_code"])
}

func TestSkillInstallCommandHandler_InvalidSlug(t *testing.T) {
	t.Parallel()
	h := makeHandler(t, 0)
	var got capturedEvent
	send := func(_ context.Context, name, content string, meta map[string]string) error {
		got = capturedEvent{name: name, content: content, meta: meta}
		return nil
	}

	err := h.Handle(context.Background(), InboundWSMessage{
		Type: "command",
		Content: mustJSON(t, installCommandPayload{
			RequestID: "req-1",
			Slug:      "../traversal",
			Registry:  "fake",
		}),
		Metadata: map[string]string{"command_name": "skill.install"},
	}, send)
	require.NoError(t, err)
	assert.Equal(t, "skill.install.failed", got.name)
	assert.Equal(t, "invalid_slug", got.meta["error_code"])
}

// --- Happy path tests ---

func TestSkillInstallCommandHandler_HappyPath(t *testing.T) {
	t.Parallel()
	h := makeHandler(t, 0)
	send, events, mu := collectSendFunc()

	msg := InboundWSMessage{
		Type: "command",
		Content: mustJSON(t, installCommandPayload{
			RequestID: "req-happy",
			Slug:      "good-skill",
			Registry:  "fake",
		}),
		Metadata: map[string]string{"command_name": "skill.install"},
	}

	require.NoError(t, h.Handle(context.Background(), msg, send))

	// Wait for async install to complete
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, *events, 3)
	assert.Equal(t, "skill.install.accepted", (*events)[0].name)
	assert.Equal(t, "skill.install.started", (*events)[1].name)
	assert.Equal(t, "skill.install.succeeded", (*events)[2].name)
	assert.Equal(t, "req-happy", (*events)[2].meta["request_id"])
	assert.Equal(t, "good-skill", (*events)[2].meta["slug"])
	assert.NotEmpty(t, (*events)[2].meta["duration_ms"])
}

func TestSkillInstallCommandHandler_DefaultRegistry(t *testing.T) {
	t.Parallel()
	h := makeHandlerWithOpts(t, 0, false, nil, "fake")
	send, events, mu := collectSendFunc()

	msg := InboundWSMessage{
		Type: "command",
		Content: mustJSON(t, installCommandPayload{
			RequestID: "req-default-reg",
			Slug:      "default-reg-skill",
		}),
		Metadata: map[string]string{"command_name": "skill.install"},
	}

	require.NoError(t, h.Handle(context.Background(), msg, send))
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, *events, 3)
	assert.Equal(t, "skill.install.succeeded", (*events)[2].name)
	assert.Equal(t, "fake", (*events)[2].meta["registry"])
}

func TestSkillInstallCommandHandler_RequestIDFromMetadata(t *testing.T) {
	t.Parallel()
	h := makeHandler(t, 0)
	send, events, mu := collectSendFunc()

	msg := InboundWSMessage{
		Type:     "command",
		Content:  `{"slug":"meta-skill","registry":"fake"}`,
		Metadata: map[string]string{"command_name": "skill.install", "request_id": "req-from-meta"},
	}

	require.NoError(t, h.Handle(context.Background(), msg, send))
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, *events, 3)
	assert.Equal(t, "req-from-meta", (*events)[0].meta["request_id"])
	assert.Equal(t, "skill.install.succeeded", (*events)[2].name)
}

// --- Idempotency tests ---

func TestSkillInstallCommandHandler_DuplicateInProgress(t *testing.T) {
	t.Parallel()
	h := makeHandler(t, 300*time.Millisecond)
	send, events, mu := collectSendFunc()

	msg := InboundWSMessage{
		Type: "command",
		Content: mustJSON(t, installCommandPayload{
			RequestID: "req-dup",
			Slug:      "demo-skill",
			Registry:  "fake",
		}),
		Metadata: map[string]string{"command_name": "skill.install"},
	}

	// First call: accepted + spawns goroutine
	require.NoError(t, h.Handle(context.Background(), msg, send))
	// Small sleep to let goroutine start and emit "started"
	time.Sleep(50 * time.Millisecond)
	// Second call while first is still running
	require.NoError(t, h.Handle(context.Background(), msg, send))

	// Wait for first install to complete
	time.Sleep(400 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	// Collect event names
	var names []string
	for _, e := range *events {
		names = append(names, e.name)
	}

	// Must contain: accepted, started, accepted(dup), succeeded (order of started vs dup-accepted may vary)
	assert.Contains(t, names, "skill.install.accepted")
	assert.Contains(t, names, "skill.install.started")
	assert.Contains(t, names, "skill.install.succeeded")

	// Find the duplicate-accepted event by its metadata
	var foundDup bool
	for _, e := range *events {
		if e.name == "skill.install.accepted" && e.meta["already_in_progress"] == "true" {
			foundDup = true
			break
		}
	}
	assert.True(t, foundDup, "expected a duplicate-accepted event with already_in_progress=true, events: %v", names)
}

func TestSkillInstallCommandHandler_DedupReplay(t *testing.T) {
	t.Parallel()
	h := makeHandler(t, 0)
	send, events, mu := collectSendFunc()

	msg := InboundWSMessage{
		Type: "command",
		Content: mustJSON(t, installCommandPayload{
			RequestID: "req-replay",
			Slug:      "replay-skill",
			Registry:  "fake",
		}),
		Metadata: map[string]string{"command_name": "skill.install"},
	}

	// First call: runs to completion
	require.NoError(t, h.Handle(context.Background(), msg, send))
	time.Sleep(200 * time.Millisecond)

	// Second call with same request_id after completion: should replay terminal result
	require.NoError(t, h.Handle(context.Background(), msg, send))

	mu.Lock()
	defer mu.Unlock()

	// Last event should be the replayed succeeded with replayed=true
	last := (*events)[len(*events)-1]
	assert.Equal(t, "skill.install.succeeded", last.name)
	assert.Equal(t, "true", last.meta["replayed"])
}

// --- Install failure test ---

func TestSkillInstallCommandHandler_InstallFailure(t *testing.T) {
	t.Parallel()
	h := makeHandlerWithOpts(t, 0, true, []string{"fake"}, "")
	send, events, mu := collectSendFunc()

	msg := InboundWSMessage{
		Type: "command",
		Content: mustJSON(t, installCommandPayload{
			RequestID: "req-fail",
			Slug:      "fail-skill",
			Registry:  "fake",
		}),
		Metadata: map[string]string{"command_name": "skill.install"},
	}

	require.NoError(t, h.Handle(context.Background(), msg, send))
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, *events, 3)
	assert.Equal(t, "skill.install.accepted", (*events)[0].name)
	assert.Equal(t, "skill.install.started", (*events)[1].name)
	assert.Equal(t, "skill.install.failed", (*events)[2].name)
	assert.Equal(t, "install_failed", (*events)[2].meta["error_code"])
	assert.NotEmpty(t, (*events)[2].meta["duration_ms"])
}

// --- parseInstallPayload unit tests ---

func TestParseInstallPayload_ValidJSON(t *testing.T) {
	t.Parallel()
	p, err := parseInstallPayload(InboundWSMessage{
		Content:  `{"request_id":"r1","slug":"s1","registry":"reg1","version":"2.0","force":true,"timeout_sec":10}`,
		Metadata: map[string]string{},
	})
	require.NoError(t, err)
	assert.Equal(t, "r1", p.RequestID)
	assert.Equal(t, "s1", p.Slug)
	assert.Equal(t, "reg1", p.Registry)
	assert.Equal(t, "2.0", p.Version)
	assert.True(t, p.Force)
	assert.Equal(t, 10, p.Timeout)
}

func TestParseInstallPayload_RequestIDFallbackToMetadata(t *testing.T) {
	t.Parallel()
	p, err := parseInstallPayload(InboundWSMessage{
		Content:  `{"slug":"s1","registry":"reg1"}`,
		Metadata: map[string]string{"request_id": "meta-req"},
	})
	require.NoError(t, err)
	assert.Equal(t, "meta-req", p.RequestID)
}

func TestParseInstallPayload_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		meta    map[string]string
		wantErr string
	}{
		{"empty content", "", nil, "empty command payload"},
		{"whitespace only", "   ", nil, "empty command payload"},
		{"invalid json", "{bad", nil, "payload must be valid JSON"},
		{"missing request_id", `{"slug":"s"}`, map[string]string{}, "request_id is required"},
		{"missing slug", `{"request_id":"r"}`, nil, "slug is required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseInstallPayload(InboundWSMessage{Content: tt.content, Metadata: tt.meta})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// --- Helpers ---

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}
