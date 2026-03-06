package websocket_client

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/tools"
	"github.com/sipeed/picoclaw/pkg/utils"
)

const (
	commandNameSkillInstall = "skill.install"
	defaultDedupTTL         = 5 * time.Minute
	defaultInstallTimeout   = 2 * time.Minute
	defaultMaxConcurrent    = 1
)

type eventSender func(ctx context.Context, eventName, content string, metadata map[string]string) error

type installCommandPayload struct {
	RequestID string `json:"request_id,omitempty"`
	Slug      string `json:"slug"`
	Registry  string `json:"registry,omitempty"`
	Version   string `json:"version,omitempty"`
	Force     bool   `json:"force,omitempty"`
	Timeout   int    `json:"timeout_sec,omitempty"`
}

type installRequestState struct {
	inProgress bool
	finishedAt time.Time
	eventName  string
	content    string
	metadata   map[string]string
}

type skillInstallCommandHandler struct {
	installer *tools.InstallSkillTool

	maxConcurrent  int
	installTimeout time.Duration
	dedupTTL       time.Duration
	defaultReg     string
	allowedRegs    map[string]struct{}

	sem      chan struct{}
	mu       sync.Mutex
	requests map[string]installRequestState
}

func newSkillInstallCommandHandler(cfg config.WSCommandsConfig, installer *tools.InstallSkillTool) *skillInstallCommandHandler {
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrent
	}

	installTimeout := time.Duration(cfg.InstallTimeout) * time.Second
	if installTimeout <= 0 {
		installTimeout = defaultInstallTimeout
	}

	dedupTTL := time.Duration(cfg.DedupTTL) * time.Second
	if dedupTTL <= 0 {
		dedupTTL = defaultDedupTTL
	}

	allowed := make(map[string]struct{}, len(cfg.AllowedRegistries))
	for _, reg := range cfg.AllowedRegistries {
		reg = strings.TrimSpace(reg)
		if reg == "" {
			continue
		}
		allowed[reg] = struct{}{}
	}

	return &skillInstallCommandHandler{
		installer:      installer,
		maxConcurrent:  maxConcurrent,
		installTimeout: installTimeout,
		dedupTTL:       dedupTTL,
		defaultReg:     strings.TrimSpace(cfg.DefaultRegistry),
		allowedRegs:    allowed,
		sem:            make(chan struct{}, maxConcurrent),
		requests:       make(map[string]installRequestState),
	}
}

func (h *skillInstallCommandHandler) Handle(ctx context.Context, inMsg InboundWSMessage, send eventSender) error {
	if inMsg.Metadata["command_name"] != commandNameSkillInstall {
		return send(ctx, "skill.install.failed", "unsupported command", map[string]string{
			"error_code": "unsupported_command",
		})
	}

	payload, err := parseInstallPayload(inMsg)
	if err != nil {
		return send(ctx, "skill.install.failed", err.Error(), map[string]string{
			"error_code": "invalid_command_payload",
		})
	}

	if payload.Registry == "" {
		payload.Registry = h.defaultReg
	}
	if payload.Registry == "" {
		return send(ctx, "skill.install.failed", "registry is required", map[string]string{
			"request_id": payload.RequestID,
			"slug":       payload.Slug,
			"error_code": "registry_required",
		})
	}

	if len(h.allowedRegs) > 0 {
		if _, ok := h.allowedRegs[payload.Registry]; !ok {
			return send(ctx, "skill.install.failed", "registry is not allowed", map[string]string{
				"request_id": payload.RequestID,
				"slug":       payload.Slug,
				"registry":   payload.Registry,
				"error_code": "registry_not_allowed",
			})
		}
	}

	if err := utils.ValidateSkillIdentifier(payload.Slug); err != nil {
		return send(ctx, "skill.install.failed", "invalid slug", map[string]string{
			"request_id": payload.RequestID,
			"slug":       payload.Slug,
			"error_code": "invalid_slug",
		})
	}
	if err := utils.ValidateSkillIdentifier(payload.Registry); err != nil {
		return send(ctx, "skill.install.failed", "invalid registry", map[string]string{
			"request_id": payload.RequestID,
			"slug":       payload.Slug,
			"registry":   payload.Registry,
			"error_code": "invalid_registry",
		})
	}

	accepted, terminalReplay, inProgress := h.registerRequest(payload.RequestID)
	if !accepted {
		if inProgress {
			return send(ctx, "skill.install.accepted", "request already in progress", map[string]string{
				"request_id":           payload.RequestID,
				"slug":                 payload.Slug,
				"registry":             payload.Registry,
				"already_in_progress":  "true",
				"max_concurrent_limit": strconv.Itoa(h.maxConcurrent),
			})
		}

		replayMeta := cloneMap(terminalReplay.metadata)
		replayMeta["request_id"] = payload.RequestID
		replayMeta["replayed"] = "true"
		return send(ctx, terminalReplay.eventName, terminalReplay.content, replayMeta)
	}

	if err := send(ctx, "skill.install.accepted", "skill install request accepted", map[string]string{
		"request_id": payload.RequestID,
		"slug":       payload.Slug,
		"registry":   payload.Registry,
	}); err != nil {
		h.markRequestFinished(payload.RequestID, "skill.install.failed", err.Error(), map[string]string{
			"request_id": payload.RequestID,
			"slug":       payload.Slug,
			"registry":   payload.Registry,
			"error_code": "send_event_failed",
		})
		return err
	}

	go h.executeInstall(ctx, payload, send)
	return nil
}

func (h *skillInstallCommandHandler) executeInstall(parentCtx context.Context, payload installCommandPayload, send eventSender) {
	start := time.Now()

	select {
	case h.sem <- struct{}{}:
	case <-parentCtx.Done():
		h.markRequestFinished(payload.RequestID, "skill.install.failed", "command canceled before scheduling", map[string]string{
			"request_id": payload.RequestID,
			"slug":       payload.Slug,
			"registry":   payload.Registry,
			"error_code": "request_canceled",
		})
		return
	}
	defer func() { <-h.sem }()

	_ = send(parentCtx, "skill.install.started", "skill installation started", map[string]string{
		"request_id": payload.RequestID,
		"slug":       payload.Slug,
		"registry":   payload.Registry,
	})

	timeout := h.installTimeout
	if payload.Timeout > 0 {
		requested := time.Duration(payload.Timeout) * time.Second
		if requested < timeout {
			timeout = requested
		}
	}

	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	result := h.installer.Execute(ctx, map[string]any{
		"slug":     payload.Slug,
		"registry": payload.Registry,
		"version":  payload.Version,
		"force":    payload.Force,
	})

	duration := strconv.FormatInt(time.Since(start).Milliseconds(), 10)
	if result.IsError {
		meta := map[string]string{
			"request_id":  payload.RequestID,
			"slug":        payload.Slug,
			"registry":    payload.Registry,
			"version":     payload.Version,
			"duration_ms": duration,
			"error_code":  "install_failed",
		}
		h.markRequestFinished(payload.RequestID, "skill.install.failed", result.ForLLM, meta)
		_ = send(parentCtx, "skill.install.failed", result.ForLLM, meta)
		return
	}

	meta := map[string]string{
		"request_id":  payload.RequestID,
		"slug":        payload.Slug,
		"registry":    payload.Registry,
		"version":     payload.Version,
		"duration_ms": duration,
	}
	h.markRequestFinished(payload.RequestID, "skill.install.succeeded", result.ForLLM, meta)
	_ = send(parentCtx, "skill.install.succeeded", result.ForLLM, meta)
}

func (h *skillInstallCommandHandler) registerRequest(requestID string) (accepted bool, replay installRequestState, inProgress bool) {
	now := time.Now()

	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanupExpiredLocked(now)

	if current, ok := h.requests[requestID]; ok {
		if current.inProgress {
			return false, installRequestState{}, true
		}
		return false, current, false
	}

	h.requests[requestID] = installRequestState{
		inProgress: true,
	}
	return true, installRequestState{}, false
}

func (h *skillInstallCommandHandler) markRequestFinished(
	requestID string,
	eventName string,
	content string,
	metadata map[string]string,
) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.requests[requestID] = installRequestState{
		inProgress: false,
		finishedAt: time.Now(),
		eventName:  eventName,
		content:    content,
		metadata:   cloneMap(metadata),
	}
}

func (h *skillInstallCommandHandler) cleanupExpiredLocked(now time.Time) {
	for reqID, state := range h.requests {
		if state.inProgress {
			continue
		}
		if now.Sub(state.finishedAt) > h.dedupTTL {
			delete(h.requests, reqID)
		}
	}
}

func parseInstallPayload(inMsg InboundWSMessage) (installCommandPayload, error) {
	var payload installCommandPayload
	if strings.TrimSpace(inMsg.Content) == "" {
		return payload, fmt.Errorf("empty command payload")
	}

	if err := json.Unmarshal([]byte(inMsg.Content), &payload); err != nil {
		return payload, fmt.Errorf("payload must be valid JSON: %w", err)
	}

	if payload.RequestID == "" {
		payload.RequestID = strings.TrimSpace(inMsg.Metadata["request_id"])
	}
	if payload.RequestID == "" {
		return payload, fmt.Errorf("request_id is required")
	}
	if payload.Slug == "" {
		return payload, fmt.Errorf("slug is required")
	}
	return payload, nil
}

func cloneMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(input))
	for k, v := range input {
		out[k] = v
	}
	return out
}
