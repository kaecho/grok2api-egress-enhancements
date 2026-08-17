package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func isXAIProvider(provider string) bool {
	provider = strings.ToLower(strings.TrimSpace(provider))
	return strings.Contains(provider, "xai") || strings.Contains(provider, "grok")
}

func requestIncludesXAI(provider string, providers []string) bool {
	if isXAIProvider(provider) {
		return true
	}
	for _, candidate := range providers {
		if isXAIProvider(candidate) {
			return true
		}
	}
	return false
}

func schedulerCandidateAvailable(candidate pluginapi.SchedulerAuthCandidate) bool {
	status := strings.ToLower(strings.TrimSpace(candidate.Status))
	// Empty status is retained for older CPA hosts. Any explicit lifecycle or
	// cooldown state other than active/ready must stay with CPA's retry logic.
	switch status {
	case "", "active", "ready":
		return true
	case "disabled", "unavailable", "error", "cooling", "cooldown", "pending", "refreshing", "retrying":
		return false
	default:
		// Unknown explicit states are fail-closed; selecting them can bypass CPA's
		// own cooldown scheduler.
		return false
	}
}

func nodeAllowsTraffic(node *nodeRecord) bool {
	return node != nil && node.Enabled && !node.DisabledByGuard
}

// handleSchedulerPick keeps the host scheduler from selecting an auth whose
// proxy node is quarantined. Healthy and unmanaged candidates are left to
// CPA's native strategy (fill-first / round-robin / weighted).
func handleSchedulerPick(request []byte) ([]byte, error) {
	ensureStore()
	var req pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode scheduler request: %w", err)
	}
	if !requestIncludesXAI(req.Provider, req.Providers) {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}

	nodes := store.listNodes()
	byProxy := make(map[string]*nodeRecord, len(nodes))
	for _, node := range nodes {
		if node.ProxyURL != "" {
			byProxy[node.ProxyURL] = node
		}
	}
	cache := refreshAuthProxyCache()
	eligible := make([]string, 0, len(req.Candidates))
	filtered := false
	managed := false
	nonXAIAvailable := false
	for _, candidate := range req.Candidates {
		if !isXAIProvider(candidate.Provider) {
			nonXAIAvailable = true
			continue
		}
		if cachedAuthDisabled(candidate.ID) {
			managed = true
			filtered = true
			continue
		}
		proxy := cache[candidate.ID]
		if proxy == "" {
			continue
		}
		node := byProxy[proxy]
		if node == nil {
			continue
		}
		managed = true
		if !schedulerCandidateAvailable(candidate) {
			continue
		}
		if nodeAllowsTraffic(node) {
			eligible = append(eligible, candidate.ID)
			continue
		}
		filtered = true
	}
	if !managed {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	if len(eligible) == 0 {
		if strings.TrimSpace(req.Provider) == "" && nonXAIAvailable {
			return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
		}
		return errorEnvelope("egress_no_healthy_auth", "没有可用的健康 CPA 出口账号"), nil
	}
	if !filtered {
		return okEnvelope(pluginapi.SchedulerPickResponse{Handled: false})
	}
	// Host ABI cannot exclude candidates then re-run CPA's configured
	// strategy. Keep the host's already-sorted first remaining auth so
	// fill-first / priority order is not replaced by plugin round-robin.
	return okEnvelope(pluginapi.SchedulerPickResponse{Handled: true, AuthID: eligible[0]})
}

// handleRequestInterceptAfterAuth closes the small race between auth selection
// and synchronous quarantine migration. A request selected during that window
// receives a retryable response instead of reaching a known bad egress.
func handleRequestIntercept(request []byte, afterAuth bool) ([]byte, error) {
	ensureStore()
	var req pluginapi.RequestInterceptRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode request interceptor request: %w", err)
	}
	if !afterAuth {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	selected := ""
	if len(req.Metadata) > 0 {
		selected = firstString(req.Metadata, "selected_auth_id", "selectedAuthID", "auth_id", "authID")
	}
	if selected != "" && requestLooksMultimodal(req.Body) {
		rememberMultimodalAuth(selected)
	}
	if selected != "" {
		if requested, known := requestThinkingRequested(req.Body); known {
			rememberRequestHint(requestHint{ThinkingKnown: true, ThinkingRequested: requested}, selected)
		}
		rememberRequestHint(requestHint{AuthKey: selected}, selected, req.RequestID)
	}
	if selected == "" {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	if cachedAuthDisabled(selected) {
		body, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"type":    "egress_auth_disabled",
				"message": "当前账号已被出口守护停用，请重试其他账号",
			},
		})
		return okEnvelope(pluginapi.RequestInterceptResponse{
			Terminate:  true,
			StatusCode: http.StatusServiceUnavailable,
			ResponseHeaders: http.Header{
				"Content-Type": []string{"application/json"},
				"Retry-After":  []string{"1"},
			},
			ResponseBody: body,
		})
	}
	nodeID := resolveNodeIDForAuth(store, selected)
	if nodeID == "" {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	node, ok := store.getNode(nodeID)
	if !ok || !node.DisabledByGuard {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "egress_quarantined",
			"message": "当前账号出口正在隔离迁移，请重试",
		},
	})
	return okEnvelope(pluginapi.RequestInterceptResponse{
		Terminate:  true,
		StatusCode: http.StatusServiceUnavailable,
		ResponseHeaders: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{"1"},
		},
		ResponseBody: body,
	})
}

func interceptAuthKeys(meta map[string]any, extra ...string) []string {
	keys := make([]string, 0, 4)
	if len(meta) > 0 {
		for _, key := range []string{"selected_auth_id", "selectedAuthID", "auth_id", "authID", "auth_index", "authIndex"} {
			if v := firstString(meta, key); v != "" {
				keys = append(keys, v)
			}
		}
	}
	for _, key := range extra {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	// Stream intercepts often only carry RequestID. Recover the auth stashed
	// by after-auth intercept so thinking evidence lands on the usage key.
	if hint, ok := recentRequestHint(keys...); ok && hint.AuthKey != "" {
		keys = append(keys, hint.AuthKey)
	}
	return keys
}

func handleStreamChunkIntercept(request []byte) ([]byte, error) {
	var req pluginapi.StreamChunkInterceptRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode stream chunk intercept request: %w", err)
	}
	rememberStreamChunk(req.Body, req.ChunkIndex, interceptAuthKeys(req.Metadata, req.RequestID)...)
	return okEnvelope(pluginapi.StreamChunkInterceptResponse{})
}

func handleResponseIntercept(request []byte) ([]byte, error) {
	var req pluginapi.ResponseInterceptRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode response intercept request: %w", err)
	}
	if keys := interceptAuthKeys(req.Metadata, req.RequestID); len(keys) > 0 && len(req.Body) > 0 {
		rememberRequestHint(requestHint{
			StreamSeen:        true,
			StreamHasThinking: streamChunkHasThinking(req.Body),
		}, keys...)
	}
	return okEnvelope(pluginapi.ResponseInterceptResponse{})
}
