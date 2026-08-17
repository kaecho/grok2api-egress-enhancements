package main

import (
	"bytes"
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
	keys := interceptAuthKeys(req.Metadata, req.RequestID)
	rememberStreamChunk(req.Body, req.ChunkIndex, keys...)
	return okEnvelope(missingThinkingStreamDecision(req, keys, req.Model, req.RequestedModel))
}

func handleResponseIntercept(request []byte) ([]byte, error) {
	var req pluginapi.ResponseInterceptRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode response intercept request: %w", err)
	}
	keys := interceptAuthKeys(req.Metadata, req.RequestID)
	if len(keys) > 0 && len(req.Body) > 0 {
		rememberRequestHint(requestHint{
			StreamSeen:        true,
			StreamHasThinking: streamChunkHasThinking(req.Body),
		}, keys...)
	}
	if interceptLooksLikeXAI(keys, req.Model, req.RequestedModel) {
		if body, blocked := missingThinkingResponseBody(req.Body, keys); blocked {
			return okEnvelope(pluginapi.ResponseInterceptResponse{Body: body})
		}
	}
	return okEnvelope(pluginapi.ResponseInterceptResponse{})
}

func interceptThinkingOn(hint requestHint, ok bool) bool {
	if ok && hint.ThinkingKnown {
		return hint.ThinkingRequested
	}
	return true
}

func missingThinkingShouldBlock(keys []string) (requestHint, bool) {
	hint, ok := recentRequestHint(keys...)
	if !interceptThinkingOn(hint, ok) || hint.Multimodal {
		return hint, false
	}
	return hint, true
}

func interceptLooksLikeXAI(keys []string, models ...string) bool {
	for _, model := range models {
		if looksLikeXAIUsage("", "", model) {
			return true
		}
	}
	for _, key := range keys {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "xai") || strings.Contains(lower, "grok") {
			return true
		}
	}
	return false
}

func missingThinkingStreamDecision(req pluginapi.StreamChunkInterceptRequest, keys []string, models ...string) pluginapi.StreamChunkInterceptResponse {
	if !interceptLooksLikeXAI(keys, append(models, req.Model, req.RequestedModel)...) {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	if req.ChunkIndex < 0 || len(req.Body) == 0 {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	hint, watch := missingThinkingShouldBlock(keys)
	if !watch {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	if hint.DegradeBlocked {
		return pluginapi.StreamChunkInterceptResponse{DropChunk: true}
	}
	hasThinking := hint.StreamHasThinking || streamChunkHasThinking(req.Body)
	if hasThinking {
		return pluginapi.StreamChunkInterceptResponse{}
	}
	if streamLooksFinished(req.Body) {
		rememberRequestHint(requestHint{DegradeBlocked: true}, keys...)
		disableMissingThinkingFromIntercept(keys, streamChunkUsageTokens(req.Body))
		return pluginapi.StreamChunkInterceptResponse{Body: missingThinkingSSEError()}
	}
	if streamChunkHasVisibleContent(req.Body) {
		return pluginapi.StreamChunkInterceptResponse{DropChunk: true}
	}
	return pluginapi.StreamChunkInterceptResponse{}
}

func missingThinkingResponseBody(body []byte, keys []string) ([]byte, bool) {
	hint, watch := missingThinkingShouldBlock(keys)
	if !watch || hint.StreamHasThinking || streamChunkHasThinking(body) {
		return nil, false
	}
	rememberRequestHint(requestHint{DegradeBlocked: true}, keys...)
	disableMissingThinkingFromIntercept(keys, streamChunkUsageTokens(body))
	return missingThinkingJSONError(), true
}

func disableMissingThinkingFromIntercept(keys []string, outputTokens int64) {
	ensureStore()
	if store == nil {
		return
	}
	pol := store.policy()
	if !pol.ThinkingGuard {
		return
	}
	if outputTokens <= 0 || (pol.MinOutputTokens > 0 && outputTokens < pol.MinOutputTokens) {
		return
	}
	authID := ""
	if hint, ok := recentRequestHint(keys...); ok {
		authID = hint.AuthKey
	}
	if authID == "" && len(keys) > 0 {
		authID = keys[0]
	}
	disableObservedAuth(store, qualityResult{
		Classification: "hard",
		HasThinking:    false,
		AuthID:         authID,
		AuthLabel:      authID,
		OutputTokens:   outputTokens,
		Error:          "响应缺少 thinking_content（降智）",
	}, "响应缺少 thinking_content（降智）")
}

func streamLooksFinished(body []byte) bool {
	if bytes.Contains(body, []byte("[DONE]")) {
		return true
	}
	payload := sseJSONPayload(body)
	if len(payload) == 0 {
		return false
	}
	var raw map[string]any
	if json.Unmarshal(payload, &raw) != nil {
		return false
	}
	typ := strings.ToLower(firstString(raw, "type", "Type"))
	if strings.Contains(typ, "completed") || strings.HasSuffix(typ, ".done") || typ == "response.completed" {
		return true
	}
	if _, ok := raw["usage"]; ok {
		return true
	}
	if choices, ok := raw["choices"].([]any); ok {
		for _, c := range choices {
			cm, _ := c.(map[string]any)
			if cm == nil {
				continue
			}
			if reason := firstString(cm, "finish_reason", "finishReason"); reason != "" && reason != "null" {
				return true
			}
		}
	}
	return false
}

func streamChunkUsageTokens(body []byte) int64 {
	payload := sseJSONPayload(body)
	if len(payload) == 0 {
		return 0
	}
	var raw map[string]any
	if json.Unmarshal(payload, &raw) != nil {
		return 0
	}
	if usage, ok := raw["usage"].(map[string]any); ok {
		return outputTokensFromUsage(usage)
	}
	return 0
}

func streamChunkHasVisibleContent(body []byte) bool {
	payload := sseJSONPayload(body)
	if len(payload) == 0 {
		return bytes.Contains(bytes.TrimSpace(body), []byte("{"))
	}
	var raw map[string]any
	if json.Unmarshal(payload, &raw) != nil {
		return false
	}
	if choices, ok := raw["choices"].([]any); ok {
		for _, c := range choices {
			cm, _ := c.(map[string]any)
			if cm == nil {
				continue
			}
			if delta, ok := cm["delta"].(map[string]any); ok && thinkingFieldNonEmpty(delta["content"]) {
				return true
			}
			if msg, ok := cm["message"].(map[string]any); ok && thinkingFieldNonEmpty(msg["content"]) {
				return true
			}
		}
	}
	return thinkingFieldNonEmpty(raw["output_text"]) || thinkingFieldNonEmpty(raw["delta"])
}

func missingThinkingJSONError() []byte {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "egress_auth_degraded",
			"message": "响应缺少 thinking_content（降智）",
		},
	})
	return body
}

func missingThinkingSSEError() []byte {
	return []byte("data: " + string(missingThinkingJSONError()) + "\n\ndata: [DONE]\n\n")
}
