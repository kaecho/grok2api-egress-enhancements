package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
)

func computeTPS(outputTokens, durationMs, firstTokenMs, minGenerationMs int64) float64 {
	if outputTokens <= 0 || durationMs <= 0 {
		return 0
	}
	denom := durationMs - firstTokenMs
	// Short replies often have firstToken ≈ duration, which blows up TPS and
	// false-triggers hard quarantine. Require a configurable generation window.
	if minGenerationMs <= 0 {
		minGenerationMs = 1000
	}
	if denom < minGenerationMs {
		denom = durationMs
	}
	if denom < minGenerationMs {
		return 0
	}
	// Ignore tiny outputs for hard-class decisions upstream; still return TPS.
	return float64(outputTokens) / (float64(denom) / 1000.0)
}

// authProxyCache maps auth id/index/name → proxy_url.
// Rebuilt from the auth-list cache (no host N+1) after TTL or explicit invalidate.
const authProxyCacheTTL = 60 * time.Second

var (
	authProxyMu    sync.Mutex
	authProxyCache map[string]string
	authProxyAt    time.Time
)

// multimodalUsageTTL is how long a request-body image mark stays attached to
// an auth so the later usage event (which has no request body) can skip the
// thinking-guard false positive. Grok 4.6 vision replies often omit
// reasoning_tokens even when the model did think.
const multimodalUsageTTL = 2 * time.Minute

var (
	requestHintMu     sync.Mutex
	requestHintByAuth map[string]requestHint
)

type requestHint struct {
	Until             time.Time
	Multimodal        bool
	ThinkingRequested bool
	ThinkingKnown     bool
	StreamSeen        bool
	StreamHasThinking bool
	AuthKey           string
}

func rememberRequestHint(hint requestHint, keys ...string) {
	if !hint.Multimodal && !hint.ThinkingKnown && !hint.StreamSeen && strings.TrimSpace(hint.AuthKey) == "" {
		return
	}
	hint.Until = time.Now().Add(multimodalUsageTTL)
	requestHintMu.Lock()
	if requestHintByAuth == nil {
		requestHintByAuth = make(map[string]requestHint)
	}
	for _, key := range authHintKeys(keys...) {
		prev := requestHintByAuth[key]
		if hint.Until.After(prev.Until) {
			prev.Until = hint.Until
		}
		prev.Multimodal = prev.Multimodal || hint.Multimodal
		if hint.ThinkingKnown {
			prev.ThinkingKnown = true
			prev.ThinkingRequested = hint.ThinkingRequested
		}
		if hint.StreamSeen {
			prev.StreamSeen = true
			prev.StreamHasThinking = prev.StreamHasThinking || hint.StreamHasThinking
		}
		if hint.AuthKey != "" && prev.AuthKey == "" {
			prev.AuthKey = hint.AuthKey
		}
		requestHintByAuth[key] = prev
	}
	now := time.Now()
	for key, exp := range requestHintByAuth {
		if now.After(exp.Until) {
			delete(requestHintByAuth, key)
		}
	}
	requestHintMu.Unlock()
}

func recentRequestHint(keys ...string) (requestHint, bool) {
	now := time.Now()
	requestHintMu.Lock()
	defer requestHintMu.Unlock()
	for _, key := range authHintKeys(keys...) {
		if hint, ok := requestHintByAuth[key]; ok && now.Before(hint.Until) {
			return hint, true
		}
	}
	return requestHint{}, false
}

func authHintKeys(keys ...string) []string {
	out := make([]string, 0, len(keys)*3)
	seen := map[string]struct{}{}
	add := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	for _, key := range keys {
		add(key)
		add(filepath.Base(key))
		add(strings.TrimSuffix(filepath.Base(key), ".json"))
	}
	return out
}

func resetRequestHints() {
	requestHintMu.Lock()
	requestHintByAuth = nil
	requestHintMu.Unlock()
}

func rememberMultimodalAuth(keys ...string) {
	rememberRequestHint(requestHint{Multimodal: true}, keys...)
}

func recentMultimodalAuth(keys ...string) bool {
	hint, ok := recentRequestHint(keys...)
	return ok && hint.Multimodal
}

func resetMultimodalAuthMarks() {
	resetRequestHints()
}

// requestLooksMultimodal reports image/vision input from an intercept body.
// Cheap substring scan: usage events do not carry the original request.
func requestLooksMultimodal(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	for _, needle := range [][]byte{
		[]byte(`"image_url"`),
		[]byte(`"imageUrl"`),
		[]byte(`"input_image"`),
		[]byte(`"inputImage"`),
		[]byte(`"inline_data"`),
		[]byte(`"inlineData"`),
		[]byte("data:image/"),
		[]byte(`"type":"image"`),
		[]byte(`"type": "image"`),
		[]byte("image/png"),
		[]byte("image/jpeg"),
		[]byte("image/jpg"),
		[]byte("image/webp"),
		[]byte("image/gif"),
		[]byte("image/heic"),
	} {
		if bytes.Contains(body, needle) {
			return true
		}
	}
	return false
}

// requestThinkingRequested inspects an intercept body for an explicit
// thinking on/off signal. Unknown / omitted config is left unset so Grok
// default-on models still run the missing-thinking guard.
func requestThinkingRequested(body []byte) (requested bool, known bool) {
	if len(body) == 0 {
		return false, false
	}
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return false, false
	}
	effort := firstString(raw, "reasoning_effort", "reasoningEffort", "ReasoningEffort")
	if effort == "" {
		if reasoning, ok := raw["reasoning"].(map[string]any); ok {
			effort = firstString(reasoning, "effort", "Effort")
		}
	}
	if effort != "" {
		return !thinkingEffortDisabled(effort), true
	}
	if thinkingObj, ok := raw["thinking"].(map[string]any); ok {
		kind := firstString(thinkingObj, "type", "Type")
		if kind != "" {
			return !thinkingEffortDisabled(kind), true
		}
	}
	return false, false
}

func thinkingEffortDisabled(effort string) bool {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "off", "disabled", "disable", "0", "false":
		return true
	default:
		return false
	}
}

func usageThinkingRequested(record map[string]any) (requested bool, known bool) {
	if record == nil {
		return false, false
	}
	effort := firstString(record, "ReasoningEffort", "reasoning_effort", "reasoningEffort")
	if effort != "" {
		return !thinkingEffortDisabled(effort), true
	}
	return false, false
}

func invalidateAuthProxyCache() {
	authProxyMu.Lock()
	authProxyAt = time.Time{}
	authProxyMu.Unlock()
}

func refreshAuthProxyCache() map[string]string {
	authProxyMu.Lock()
	if authProxyCache != nil && time.Since(authProxyAt) < authProxyCacheTTL {
		out := authProxyCache
		authProxyMu.Unlock()
		return out
	}
	authProxyMu.Unlock()

	out := map[string]string{}
	if store != nil {
		if idx := store.authProxyIndexSnapshot(); len(idx) > 0 {
			out = idx
		}
	}
	authListMu.Lock()
	warm := authListCache
	authListMu.Unlock()
	for _, a := range warm {
		if a.ProxyURL == "" {
			continue
		}
		for _, key := range authIndexKeys(a) {
			out[key] = a.ProxyURL
		}
	}

	authProxyMu.Lock()
	authProxyCache = out
	authProxyAt = time.Now()
	authProxyMu.Unlock()
	return out
}

// resolveNodeIDForAuth is on the request hot path. It only consults in-memory
// caches — never host.auth.get — so a cache miss yields "" (unmapped) rather
// than blocking the CPA request on N Host RPCs.
func resolveNodeIDForAuth(store *stateStore, authKeys ...string) string {
	if store == nil {
		return ""
	}
	cache := refreshAuthProxyCache()
	var proxy string
	for _, k := range authKeys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if p, ok := cache[k]; ok && p != "" {
			proxy = p
			break
		}
	}
	if proxy == "" {
		return ""
	}
	if id := store.nodeIDByProxy(proxy); id != "" {
		return id
	}
	return ""
}

func classifyTPS(tps float64, soft, hard float64) string {
	if tps <= 0 {
		return "unknown"
	}
	if tps >= hard {
		return "hard"
	}
	if tps >= soft {
		return "soft"
	}
	return "healthy"
}

// thinkingFieldNonEmpty reports whether a delta/message field carries real
// thinking/reasoning text. Empty strings and whitespace-only values do not count.
func thinkingFieldNonEmpty(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t) != ""
	case []any:
		for _, item := range t {
			if thinkingPartNonEmpty(item) {
				return true
			}
		}
	case map[string]any:
		return thinkingPartNonEmpty(t)
	}
	return false
}

func thinkingPartNonEmpty(v any) bool {
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return false
	}
	kind := strings.ToLower(strings.TrimSpace(firstString(m, "type", "Type")))
	switch kind {
	case "thinking", "reasoning", "thought", "reasoning_content", "thinking_content":
		return true
	}
	if thinkingFieldNonEmpty(m["thinking"]) || thinkingFieldNonEmpty(m["text"]) {
		if kind == "" || strings.Contains(kind, "think") || strings.Contains(kind, "reason") {
			return true
		}
	}
	return mapHasThinkingContent(m)
}

// mapHasThinkingContent inspects OpenAI-compatible delta/message maps for the
// thinking markers used by Grok/xAI streams (thinking_content / reasoning_content).
func mapHasThinkingContent(m map[string]any) bool {
	if m == nil {
		return false
	}
	for _, key := range []string{
		"thinking_content", "ThinkingContent", "thinkingContent",
		"reasoning_content", "ReasoningContent", "reasoningContent",
		"thinking", "Thinking",
		"encrypted_content", "encryptedContent", "EncryptedContent",
	} {
		if thinkingFieldNonEmpty(m[key]) {
			return true
		}
	}
	for _, key := range []string{"content", "Content", "parts", "Parts"} {
		switch v := m[key].(type) {
		case []any:
			for _, item := range v {
				if thinkingPartNonEmpty(item) {
					return true
				}
			}
		case map[string]any:
			if thinkingPartNonEmpty(v) {
				return true
			}
		}
	}
	return false
}

func sseJSONPayload(body []byte) []byte {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if data[0] == '{' {
			return data
		}
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '{' {
		return body
	}
	return nil
}

func streamChunkHasThinking(body []byte) bool {
	payload := sseJSONPayload(body)
	if len(payload) == 0 {
		return false
	}
	var raw map[string]any
	if json.Unmarshal(payload, &raw) != nil {
		return false
	}
	if recordHasThinking(raw) {
		return true
	}
	typ := strings.ToLower(firstString(raw, "type", "Type"))
	if strings.Contains(typ, "reason") || strings.Contains(typ, "think") {
		return true
	}
	if item, ok := raw["item"].(map[string]any); ok {
		itemType := strings.ToLower(firstString(item, "type", "Type"))
		if itemType == "reasoning" || strings.Contains(itemType, "think") {
			return true
		}
		if mapHasThinkingContent(item) {
			return true
		}
	}
	if choices, ok := raw["choices"].([]any); ok {
		for _, c := range choices {
			cm, _ := c.(map[string]any)
			if cm == nil {
				continue
			}
			if delta, ok := cm["delta"].(map[string]any); ok && mapHasThinkingContent(delta) {
				return true
			}
			if msg, ok := cm["message"].(map[string]any); ok && mapHasThinkingContent(msg) {
				return true
			}
		}
	}
	return false
}

func rememberStreamChunk(body []byte, chunkIndex int, keys ...string) {
	if chunkIndex < 0 || len(body) == 0 {
		return
	}
	if hint, ok := recentRequestHint(keys...); ok && hint.StreamHasThinking {
		return
	}
	rememberRequestHint(requestHint{
		StreamSeen:        true,
		StreamHasThinking: streamChunkHasThinking(body),
	}, keys...)
}

// recordHasThinking derives a thinking signal from a CPA usage event.
// Prefer explicit thinking/reasoning body fields; fall back to reasoning_tokens > 0
// because passive usage rarely includes full response text.
// A zero or omitted reasoning_tokens field is NOT evidence of missing thinking:
// CPA's xAI usage parser only copies completion/output_tokens_details.reasoning_tokens,
// and Grok /responses traffic often leaves that bucket empty even when the model thought.
func recordHasThinking(record map[string]any) bool {
	if record == nil {
		return false
	}
	if mapHasThinkingContent(record) {
		return true
	}
	for _, nestKey := range []string{"Detail", "detail", "Message", "message", "Response", "response", "Delta", "delta"} {
		if nested, ok := record[nestKey].(map[string]any); ok && mapHasThinkingContent(nested) {
			return true
		}
	}
	// Boolean / presence markers some hosts may attach.
	for _, key := range []string{"has_thinking", "HasThinking", "hasThinking", "has_reasoning", "HasReasoning"} {
		switch v := record[key].(type) {
		case bool:
			if v {
				return true
			}
		case string:
			if strings.EqualFold(strings.TrimSpace(v), "true") || strings.TrimSpace(v) == "1" {
				return true
			}
		}
	}
	if firstInt(record, "reasoning_tokens", "ReasoningTokens", "reasoningTokens") > 0 {
		return true
	}
	for _, nestKey := range []string{"Detail", "detail", "Usage", "usage"} {
		if nested, ok := record[nestKey].(map[string]any); ok {
			if firstInt(nested, "reasoning_tokens", "ReasoningTokens", "reasoningTokens") > 0 {
				return true
			}
		}
	}
	return false
}

// classifyQuality classifies a successful generation sample.
// When ThinkingGuard is on: missing thinking (with enough output) is hard 降智;
// present thinking falls through to the original soft/hard Token/s thresholds.
// When ThinkingGuard is off: behavior matches the original TPS-only path.
func classifyQuality(tps float64, outputTokens int64, hasThinking bool, pol policyConfig) string {
	if outputTokens <= 0 || tps <= 0 {
		return "unknown"
	}
	if pol.MinOutputTokens > 0 && outputTokens < pol.MinOutputTokens {
		// Tiny responses are not enough evidence (original min-output guard).
		return "ignored"
	}
	if pol.ThinkingGuard && !hasThinking {
		// Sufficient output but no thinking_content / reasoning signal → 降智.
		return "hard"
	}
	// Original author logic: soft/hard Token/s thresholds as the quality baseline
	// (also the fallback when thinking is present, or ThinkingGuard is disabled).
	return classifyTPS(tps, pol.SoftTPS, pol.HardTPS)
}

func classifyFailureKind(status int, body string) string {
	lower := strings.ToLower(body)
	if status == http.StatusProxyAuthRequired {
		return "transport_error"
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusConflict || status == http.StatusUnprocessableEntity || status == http.StatusTooManyRequests {
		return "account_error"
	}
	for _, marker := range []string{"invalid token", "expired", "no auth", "quota", "rate limit", "ratelimit", "too many requests", "permission denied", "forbidden"} {
		if strings.Contains(lower, marker) {
			return "account_error"
		}
	}
	for _, marker := range []string{"connection refused", "connection reset", "dial tcp", "timeout", "timed out", "eof", "tls handshake", "no such host", "proxyconnect", "proxy authentication"} {
		if strings.Contains(lower, marker) {
			return "transport_error"
		}
	}
	if status >= 500 && status <= 599 {
		return "upstream_error"
	}
	return "request_error"
}

func maxInt64(values ...int64) int64 {
	var result int64
	for _, value := range values {
		if value > result {
			result = value
		}
	}
	return result
}

// outputTokensFromUsage normalizes CPA/OpenAI-compatible aliases. In CPA's
// xAI usage contract, completion_tokens/output_tokens are aliases and
// reasoning_tokens is a detail bucket, so summing them double-counts output.
func outputTokensFromUsage(usage map[string]any) int64 {
	if usage == nil {
		return 0
	}
	return maxInt64(
		anyInt(usage["completion_tokens"]),
		anyInt(usage["output_tokens"]),
		anyInt(usage["CompletionTokens"]),
		anyInt(usage["OutputTokens"]),
		anyInt(usage["completionTokens"]),
		anyInt(usage["outputTokens"]),
		anyInt(usage["reasoning_tokens"]),
		anyInt(usage["ReasoningTokens"]),
		anyInt(usage["reasoningTokens"]),
	)
}

func httpClientThroughProxy(proxyURL string, timeout time.Duration) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if repaired, ok := repairBareIPv6ProxyURL(proxyURL); ok {
		proxyURL = repaired
	}
	if proxyURL == "" {
		return &http.Client{Timeout: timeout, Transport: proxyutil.NewDirectTransport()}, nil
	}
	transport, mode, err := proxyutil.BuildHTTPTransport(proxyURL)
	if err != nil || transport == nil || mode != proxyutil.ModeProxy {
		if err != nil {
			return nil, fmt.Errorf("代理 URL 无效: %w", err)
		}
		return nil, fmt.Errorf("代理 URL 无效")
	}
	return &http.Client{Timeout: timeout, Transport: transport}, nil
}

// Dual-stack exit-IP endpoints. IPv6-only SOCKS exits cannot CONNECT to
// api.ipify.org (A-only) and return "host unreachable".
var connectivityProbeURLs = []string{
	"https://api64.ipify.org",
	"https://api6.ipify.org",
	"https://api.ipify.org",
	"https://ifconfig.co/ip",
}

func probeConnectivity(proxyURL string) (exitIP string, latencyMs int64, err error) {
	client, err := httpClientThroughProxy(proxyURL, 20*time.Second)
	if err != nil {
		return "", 0, err
	}
	start := time.Now()
	var last error
	for _, target := range connectivityProbeURLs {
		ip, probeErr := fetchExitIP(client, target)
		if probeErr == nil {
			return ip, time.Since(start).Milliseconds(), nil
		}
		last = probeErr
	}
	if last == nil {
		last = fmt.Errorf("连通性失败")
	}
	return "", time.Since(start).Milliseconds(), last
}

func fetchExitIP(client *http.Client, target string) (string, error) {
	if client == nil {
		return "", fmt.Errorf("http client 为空")
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "CPA-egress-guard/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 128))
	ip := strings.TrimSpace(string(body))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("连通性失败 HTTP %d", resp.StatusCode)
	}
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("连通性失败：未返回有效 IP")
	}
	return ip, nil
}

type qualityResult struct {
	Classification  string  `json:"classification"`
	TPS             float64 `json:"tps"`
	OutputTokens    int64   `json:"output_tokens"`
	DurationMs      int64   `json:"duration_ms"`
	FirstTokenMs    int64   `json:"first_token_ms"`
	HasThinking     bool    `json:"has_thinking,omitempty"`
	ExpectedMatched bool    `json:"expected_matched"`
	ProfileID       string  `json:"profile_id,omitempty"`
	ProfileName     string  `json:"profile_name,omitempty"`
	AuthID          string  `json:"auth_id,omitempty"`
	AuthLabel       string  `json:"auth_label,omitempty"`
	ExitIP          string  `json:"exit_ip,omitempty"`
	Error           string  `json:"error,omitempty"`
	ErrorKind       string  `json:"error_kind,omitempty"`
	Model           string  `json:"model,omitempty"`
}

func rotationAllowed(cfg pluginConfig, nodeID string) bool {
	if strings.TrimSpace(cfg.RotationURL) == "" || strings.TrimSpace(nodeID) == "" {
		return false
	}
	for _, allowed := range cfg.RotatableNodeIDs {
		if strings.TrimSpace(allowed) == nodeID {
			return true
		}
	}
	return false
}

func rotateNodeIfConfigured(store *stateStore, node *nodeRecord) (bool, error) {
	if node == nil {
		return false, nil
	}
	value := currentConfig.Load()
	if value == nil {
		return false, nil
	}
	cfg, ok := value.(pluginConfig)
	if !ok || !rotationAllowed(cfg, node.ID) {
		return false, nil
	}
	parsed, err := url.Parse(strings.TrimSpace(cfg.RotationURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return false, fmt.Errorf("换 IP Webhook URL 无效")
	}
	payload, _ := json.Marshal(map[string]any{"nodeId": node.ID, "oldExitIp": node.ExitIP})
	req, err := http.NewRequest(http.MethodPost, parsed.String(), bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if envName := strings.TrimSpace(cfg.RotationTokenEnv); envName != "" {
		if token := strings.TrimSpace(os.Getenv(envName)); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	timeout := time.Duration(cfg.RotationTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("换 IP 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("换 IP 返回 HTTP %d", resp.StatusCode)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return false, fmt.Errorf("换 IP 响应无效")
	}
	newIP := firstString(result, "newExitIp", "new_exit_ip", "exitIp", "exit_ip")
	if newIP == "" || (node.ExitIP != "" && newIP == node.ExitIP) {
		return false, fmt.Errorf("换 IP 未确认出口变化")
	}
	updated, err := store.updateNode(node.ID, func(n *nodeRecord) error {
		n.ExitIP = newIP
		n.LastReason = "已换 IP，等待真实模型复测"
		return nil
	})
	if err != nil || updated == nil {
		return false, fmt.Errorf("保存换 IP 状态失败")
	}
	store.appendEvent(guardEvent{Event: "node_rotated", NodeID: node.ID, NodeName: node.Name, Reason: "Webhook 已确认出口 IP 变化"})
	return true, nil
}

func applyGrokClientHeaders(req *http.Request, auth authFile) {
	// Always force Grok CLI headers — missing X-XAI-Token-Auth yields
	// upstream 401 "x_xai_token_auth=none / no auth context".
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", "0.2.93")
	req.Header.Set("x-grok-client-identifier", "grok-shell")
	req.Header.Set("User-Agent", "CPA-egress-guard/1.0")
	if headers, ok := auth.Raw["headers"].(map[string]any); ok {
		for k, v := range headers {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				req.Header.Set(k, s)
			}
		}
	}
	// Re-assert critical headers after auth map copy (auth may contain empty values).
	if req.Header.Get("X-XAI-Token-Auth") == "" {
		req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	}
	if req.Header.Get("x-grok-client-version") == "" {
		req.Header.Set("x-grok-client-version", "0.2.93")
	}
	if req.Header.Get("x-grok-client-identifier") == "" {
		req.Header.Set("x-grok-client-identifier", "grok-shell")
	}
}

func isAuthExpired(auth authFile) bool {
	exp, _ := auth.Raw["expired"].(string)
	if exp == "" {
		return false
	}
	// accept RFC3339 / RFC3339Nano / trailing Z variants
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, exp); err == nil {
			return time.Now().After(t.Add(-2 * time.Minute))
		}
	}
	// bare "2026-08-02T05:04:09Z" already RFC3339
	if t, err := time.Parse("2006-01-02T15:04:05Z07:00", exp); err == nil {
		return time.Now().After(t.Add(-2 * time.Minute))
	}
	return false
}

// isAccountQuotaExhausted detects free-tier / quota exhaustion on an auth.
// body is normalized to lower-case inside the helper.
func isAccountQuotaExhausted(status int, body string) bool {
	lower := strings.ToLower(body)
	for _, marker := range []string{
		"free-usage-exhausted",
		"free_usage_exhausted",
		"subscription:free-usage",
		"included free usage",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	if status == http.StatusTooManyRequests &&
		(strings.Contains(lower, "quota") || strings.Contains(lower, "usage") || strings.Contains(lower, "rate")) {
		return true
	}
	if strings.Contains(lower, "quota") && strings.Contains(lower, "exhaust") {
		return true
	}
	return false
}

func isAuthErrorRetryable(status int, body string) bool {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return true
	}
	lower := strings.ToLower(body)
	return strings.Contains(lower, "invalid or expired") ||
		strings.Contains(lower, "no auth context") ||
		strings.Contains(lower, "permissiondenied") ||
		strings.Contains(lower, "x_xai_token_auth=none")
}

// shouldRetryProbeWithNextAuth switches credentials on free-usage / account errors.
// Stream instability must NOT go through this path — those hard-quarantine the node.
func shouldRetryProbeWithNextAuth(hasNext bool, status int, body, kind string) bool {
	if !hasNext {
		return false
	}
	if isAccountQuotaExhausted(status, body) {
		return true
	}
	if kind == "account_error" {
		return true
	}
	return isAuthErrorRetryable(status, body)
}

func isProbeTimeoutErr(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "timeout") ||
		strings.Contains(lower, "timed out") ||
		strings.Contains(lower, "deadline exceeded") ||
		strings.Contains(lower, "context deadline")
}

// isProbeUnstableErr marks mid-stream / link instability. Timeouts are also
// unstable, but callers may branch on isProbeTimeoutErr first.
func isProbeUnstableErr(err error) bool {
	if err == nil {
		return false
	}
	if isProbeTimeoutErr(err) {
		return true
	}
	lower := strings.ToLower(err.Error())
	for _, marker := range []string{
		"unexpected eof",
		"eof",
		"connection reset",
		"connection refused",
		"broken pipe",
		"stream error",
		"http2: stream",
		"server closed idle connection",
		"tls:",
		"tls handshake",
		"use of closed network connection",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func probeUnstableResult(base qualityResult, err error, durationMs int64) qualityResult {
	base.Classification = "hard"
	base.ErrorKind = "probe_unstable"
	base.DurationMs = durationMs
	detail := ""
	if err != nil {
		detail = truncate(err.Error(), 80)
	}
	if detail != "" {
		base.Error = "不一定降智，但节点断流不稳定，标记为降智，暂不使用 (" + detail + ")"
	} else {
		base.Error = "不一定降智，但节点断流不稳定，标记为降智，暂不使用"
	}
	return base
}

func authProbeLabel(auth authFile) string {
	if auth.Email != "" {
		return auth.Email
	}
	if auth.Name != "" {
		return auth.Name
	}
	if auth.ID != "" {
		return auth.ID
	}
	return auth.Index
}

func probeQuality(store *stateStore, node *nodeRecord, profileID, preferAuth string) qualityResult {
	pol := store.policy()
	profile := store.resolveProfile(profileID)
	model := strings.TrimSpace(profile.Model)
	if model == "" {
		model = pol.Model
	}
	res := qualityResult{Model: model, ProfileID: profile.ID, ProfileName: profile.Name, ExpectedMatched: true}
	if node == nil || node.ProxyURL == "" {
		res.Classification = "error"
		res.ErrorKind = "request_error"
		res.Error = "节点缺少代理"
		return res
	}

	// connectivity first for exit IP
	if ip, _, errIP := probeConnectivity(node.ProxyURL); errIP == nil {
		res.ExitIP = ip
	}

	candidates, err := listAuthsForNode(node, 8)
	if err != nil {
		candidates = nil
	}
	candidates = mergePreferredAuth(candidates, preferAuth)
	if len(candidates) == 0 {
		res.Classification = "error"
		res.ErrorKind = "no_account"
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Error = "没有可用的 CPA xAI 账号"
		}
		return res
	}

	client, err := httpClientThroughProxy(node.ProxyURL, 90*time.Second)
	if err != nil {
		res.Classification = "error"
		res.ErrorKind = "transport_error"
		res.Error = err.Error()
		return res
	}

	maxTok := pol.MaxOutputTokensProbe
	if profile.MaxOutputTokens > 0 {
		maxTok = profile.MaxOutputTokens
	}
	if maxTok <= 0 {
		maxTok = 256
	}
	temp := profile.Temperature
	if temp == 0 && profile.ID == profileThroughput {
		temp = 0.7
	}
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": profile.Prompt},
		},
		"stream":      true,
		"max_tokens":  maxTok,
		"temperature": temp,
	}
	body, _ := json.Marshal(payload)

	var lastErr string
	for i, auth := range candidates {
		token, _ := auth.Raw["access_token"].(string)
		if strings.TrimSpace(token) == "" {
			lastErr = "账号缺少 access_token"
			continue
		}
		if isAuthExpired(auth) && i+1 < len(candidates) {
			// Prefer non-expired accounts first; last candidate still tried.
			continue
		}
		baseURL, _ := auth.Raw["base_url"].(string)
		if baseURL == "" {
			baseURL = "https://cli-chat-proxy.grok.com/v1"
		}
		baseURL = strings.TrimRight(baseURL, "/")

		req, errReq := http.NewRequest(http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(body))
		if errReq != nil {
			res.Classification = "error"
			res.ErrorKind = "request_error"
			res.Error = "无法创建探测请求"
			return res
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		applyGrokClientHeaders(req, auth)

		start := time.Now()
		resp, errDo := client.Do(req)
		if errDo != nil {
			dur := time.Since(start).Milliseconds()
			// Timeout / mid-stream link instability is a node problem: hard, no auth switch.
			if isProbeTimeoutErr(errDo) || isProbeUnstableErr(errDo) {
				return probeUnstableResult(res, errDo, dur)
			}
			lastErr = "模型探测请求失败: " + truncate(errDo.Error(), 120)
			res.ErrorKind = "transport_error"
			res.DurationMs = dur
			continue
		}

		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			bodyText := string(b)
			msg := fmt.Sprintf("上游 HTTP %d: %s", resp.StatusCode, truncate(bodyText, 160))
			lastErr = msg
			kind := classifyFailureKind(resp.StatusCode, bodyText)
			res.ErrorKind = kind
			res.DurationMs = time.Since(start).Milliseconds()
			hasNext := i+1 < len(candidates)
			if shouldRetryProbeWithNextAuth(hasNext, resp.StatusCode, bodyText, kind) {
				// Account/quota errors belong to the credential, not the egress.
				// Try another account on the same channel before classifying it.
				if isAccountQuotaExhausted(resp.StatusCode, bodyText) {
					log.Printf("quality free-usage exhausted -> retry next auth auth=%s node=%s remain=%d",
						authProbeLabel(auth), node.ID, len(candidates)-(i+1))
				}
				continue
			}
			res.Classification = "error"
			res.Error = msg
			return res
		}

		var (
			firstTokenAt time.Time
			contentLen   int
			usageOut     int64
			usageReason  int64
			hasThinking  bool
			visibleText  strings.Builder
		)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				if data == "[DONE]" {
					break
				}
				continue
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(data), &chunk) != nil {
				continue
			}
			if u, ok := chunk["usage"].(map[string]any); ok {
				usageOut = maxInt64(usageOut, outputTokensFromUsage(u))
				usageReason = maxInt64(usageReason, anyInt(u["reasoning_tokens"]), anyInt(u["reasoningTokens"]))
			}
			choices, _ := chunk["choices"].([]any)
			for _, c := range choices {
				cm, _ := c.(map[string]any)
				delta, _ := cm["delta"].(map[string]any)
				if delta == nil {
					if msg, ok := cm["message"].(map[string]any); ok {
						delta = msg
					}
				}
				if delta == nil {
					continue
				}
				if mapHasThinkingContent(delta) {
					hasThinking = true
				}
				if t, ok := delta["content"].(string); ok && t != "" {
					if firstTokenAt.IsZero() {
						firstTokenAt = time.Now()
					}
					contentLen += len([]rune(t))
					appendCappedRunes(&visibleText, t, maxProbeContentRunes)
				}
				for _, key := range []string{"thinking_content", "reasoning_content", "thinking"} {
					if t, ok := delta[key].(string); ok && t != "" {
						if firstTokenAt.IsZero() {
							firstTokenAt = time.Now()
						}
						contentLen += len([]rune(t))
					}
				}
			}
		}
		_ = resp.Body.Close()
		if scanErr := scanner.Err(); scanErr != nil {
			dur := time.Since(start).Milliseconds()
			// Broken stream while reading SSE: hard-quarantine, do not switch auth.
			if isProbeTimeoutErr(scanErr) || isProbeUnstableErr(scanErr) {
				return probeUnstableResult(res, scanErr, dur)
			}
			lastErr = "模型探测流读取失败: " + truncate(scanErr.Error(), 120)
			res.ErrorKind = "transport_error"
			res.DurationMs = dur
			continue
		}

		duration := time.Since(start)
		res.DurationMs = duration.Milliseconds()
		if !firstTokenAt.IsZero() {
			res.FirstTokenMs = firstTokenAt.Sub(start).Milliseconds()
		}
		// CPA's xAI usage contract treats reasoning tokens as a subset of output
		// tokens. Prefer the authoritative total instead of adding the buckets.
		outTokens := usageOut
		if usageReason > outTokens {
			outTokens = usageReason
		}
		if outTokens <= 0 {
			outTokens = int64(contentLen / 4)
			if outTokens == 0 && contentLen > 0 {
				outTokens = 1
			}
		}
		res.OutputTokens = outTokens
		res.HasThinking = hasThinking
		res.AuthID = firstNonEmpty(auth.ID, auth.Index, auth.Name, auth.Email)
		res.AuthLabel = authProbeLabel(auth)
		res.TPS = computeTPS(outTokens, res.DurationMs, res.FirstTokenMs, pol.MinGenerationMs)
		res.ExpectedMatched = matchExpected(visibleText.String(), profile.ExpectedText, profile.MatchMode)
		res = classifyWithProfile(res, profile, pol)
		if res.Classification == "unknown" && outTokens == 0 {
			lastErr = "探测无输出"
			res.ErrorKind = "no_output"
			continue
		}
		return res
	}

	res.Classification = "error"
	if res.ErrorKind == "" {
		res.ErrorKind = "transport_error"
	}
	if lastErr == "" {
		lastErr = "所有候选账号探测失败"
	}
	res.Error = lastErr
	return res
}

func anyInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		i, _ := t.Int64()
		return i
	default:
		return 0
	}
}

// crossVerifyInflight prevents stacked follow-up probes for the same node
// while a soft/thinking cross-verify is already running.
var (
	crossVerifyMu       sync.Mutex
	crossVerifyInflight = map[string]struct{}{}
)

func beginCrossVerify(nodeID string) bool {
	crossVerifyMu.Lock()
	defer crossVerifyMu.Unlock()
	if _, ok := crossVerifyInflight[nodeID]; ok {
		return false
	}
	crossVerifyInflight[nodeID] = struct{}{}
	return true
}

func endCrossVerify(nodeID string) {
	crossVerifyMu.Lock()
	defer crossVerifyMu.Unlock()
	delete(crossVerifyInflight, nodeID)
}

func scheduleCrossVerifyProbe(store *stateStore, nodeID, name, event, reason string, res qualityResult, source string) {
	if !beginCrossVerify(nodeID) {
		return
	}
	_, _ = store.updateNode(nodeID, func(node *nodeRecord) error {
		// Keep soft-ish UI state while waiting for confirmation; do not quarantine yet.
		if node.LastClassification == "" || node.LastClassification == "hard" {
			node.LastClassification = "soft"
		}
		node.LastReason = reason
		node.LastSource = source
		node.LastObservedAt = float64(time.Now().Unix())
		node.LastOutputTPS = res.TPS
		node.LastOutputTokens = res.OutputTokens
		node.LastDurationMs = res.DurationMs
		node.LastFirstTokenMs = res.FirstTokenMs
		return nil
	})
	store.appendEvent(guardEvent{
		Event:          event,
		NodeID:         nodeID,
		NodeName:       name,
		Classification: "soft",
		OutputTPS:      res.TPS,
		Reason:         reason,
	})
	go func(id string) {
		defer endCrossVerify(id)
		if _, err := runNodeQuality(store, id, "", res.AuthID); err != nil {
			log.Printf("cross-verify probe failed node=%s err=%v", id, err)
		}
	}(nodeID)
}

// missingThinkingHit is a pure 降智 sample (enough output, no thinking), not transport instability.
func missingThinkingHit(res qualityResult, pol policyConfig) bool {
	return pol.ThinkingGuard &&
		res.Classification == "hard" &&
		!res.HasThinking &&
		res.ErrorKind != "probe_unstable"
}

func applyObservation(store *stateStore, nodeID, source string, res qualityResult) {
	pol := store.policy()
	if res.Classification == "error" && res.ErrorKind != "transport_error" {
		res.Classification = "ignored"
	}
	now := float64(time.Now().Unix())
	var (
		doRestore     bool
		doQuarantine  bool
		quarantineWhy string
		nodeCopy      nodeRecord
		scheduleCV    bool
		cvEvent       string
		cvReason      string
	)
	updated, err := store.updateNode(nodeID, func(n *nodeRecord) error {
		if res.Classification == "ignored" {
			n.LastSource = source
			n.LastObservedAt = now
			if source == "active" {
				n.LastProbeAt = now
			}
			if res.ExitIP != "" {
				n.ExitIP = res.ExitIP
			}
			if res.Error != "" {
				n.LastReason = res.Error
			}
			nodeCopy = *n
			return nil
		}
		n.LastClassification = res.Classification
		n.LastOutputTPS = res.TPS
		n.LastFirstTokenMs = res.FirstTokenMs
		n.LastDurationMs = res.DurationMs
		n.LastOutputTokens = res.OutputTokens
		n.LastSource = source
		n.LastObservedAt = now
		if source == "active" {
			n.LastProbeAt = now
		}
		if res.ExitIP != "" {
			n.ExitIP = res.ExitIP
		}
		if res.Error != "" {
			n.LastReason = res.Error
		} else if res.Classification == "healthy" {
			n.LastReason = ""
		}

		isActiveConfirm := source == "active" || source == "cross_verify"
		needMissing := missingThinkingHit(res, pol)

		switch {
		case res.Classification == "healthy":
			n.SoftStrikes = 0
			n.ErrorStrikes = 0
			n.ThinkingStrikes = 0
			if n.DisabledByGuard && (source == "active" || pol.Mode == "passive") {
				n.DisabledByGuard = false
				n.QuarantinedUntil = 0
				doRestore = true
			}

		case needMissing:
			// Missing thinking accumulates on the same egress; healthy/soft/error reset it.
			n.ThinkingStrikes++
			n.SoftStrikes = 0
			threshold := pol.ConsecutiveMissingThinking
			if threshold <= 0 {
				threshold = 1
			}
			if n.ThinkingStrikes < threshold {
				n.LastClassification = "soft"
				n.LastReason = fmt.Sprintf("连续缺少 thinking %d/%d", n.ThinkingStrikes, threshold)
			} else if !n.DisabledByGuard {
				// Reached threshold: either cross-verify (passive) or quarantine now.
				if pol.ThinkingCrossVerify && !isActiveConfirm {
					scheduleCV = true
					cvEvent = "thinking_cross_verify_scheduled"
					cvReason = fmt.Sprintf("连续缺少 thinking %d 次，触发交叉验证探测（可能延迟隔离并增加 Token 消耗）", n.ThinkingStrikes)
					n.LastClassification = "soft"
					n.LastReason = cvReason
					// Keep strikes so a confirming active probe still sees threshold met;
					// active path quarantines immediately without rescheduling.
				} else {
					doQuarantine = true
					if res.Error != "" {
						quarantineWhy = res.Error
					} else {
						quarantineWhy = fmt.Sprintf("连续缺少 thinking_content %d 次（降智）", n.ThinkingStrikes)
					}
				}
			}

		case res.Classification == "soft":
			n.ThinkingStrikes = 0
			n.SoftStrikes++
			if n.SoftStrikes >= pol.ConsecutiveSoft && !n.DisabledByGuard {
				if pol.SoftCrossVerify && !isActiveConfirm {
					scheduleCV = true
					cvEvent = "soft_cross_verify_scheduled"
					cvReason = fmt.Sprintf("连续软阈值 Token/s=%.1f，触发交叉验证探测（可能延迟隔离并增加 Token 消耗）", res.TPS)
					n.LastReason = cvReason
				} else {
					doQuarantine = true
					if res.Error != "" {
						quarantineWhy = res.Error
					} else {
						quarantineWhy = fmt.Sprintf("连续软阈值 Token/s=%.1f", res.TPS)
					}
				}
			}

		case res.Classification == "hard":
			// Hard for non-missing-thinking reasons (e.g. probe_unstable, TPS hard with thinking).
			n.ThinkingStrikes = 0
			if !n.DisabledByGuard {
				doQuarantine = true
				if res.Error != "" {
					quarantineWhy = res.Error
				} else {
					quarantineWhy = fmt.Sprintf("硬阈值 Token/s=%.1f", res.TPS)
				}
			}

		case res.Classification == "error":
			n.ThinkingStrikes = 0
			n.ErrorStrikes++
			if n.ErrorStrikes >= pol.ConsecutiveErrors && !n.DisabledByGuard {
				doQuarantine = true
				quarantineWhy = "连续探测错误: " + res.Error
			}
		}
		nodeCopy = *n
		return nil
	})
	if err != nil || updated == nil {
		store.bumpStat(source, res.Classification, res.OutputTokens)
		return
	}
	// Per-account 降智 stats:
	// - sample: real generation outcomes (healthy/soft/hard), not errors/ignored
	// - degraded: quality-degrade hits for THIS account only (never merge across accounts)
	//   * missing-thinking hard: count on the observed account immediately
	//     (node cross-verify may still defer quarantine; probe may use another account)
	//   * soft/hard TPS outcomes that actually quarantine (or would, if already isolated)
	// Soft cross-verify scheduling must NOT mark degrade yet (avoid +2 on soft).
	if res.AuthID != "" {
		if res.Classification != "error" && res.Classification != "ignored" && res.Classification != "unknown" {
			degraded := false
			reason := res.Error
			if missingThinkingHit(res, pol) {
				degraded = true
				if reason == "" {
					reason = "响应缺少 thinking_content（降智）"
				}
			} else if res.Classification == "hard" && res.ErrorKind != "probe_unstable" {
				// TPS hard (with thinking) or other hard quality — count as degrade when final.
				degraded = true
				if reason == "" {
					reason = fmt.Sprintf("硬阈值 Token/s=%.1f", res.TPS)
				}
			} else if res.Classification == "soft" {
				// Soft only counts as degrade when it reaches action threshold and is not deferred.
				if scheduleCV && cvEvent == "soft_cross_verify_scheduled" {
					degraded = false
				} else if doQuarantine || nodeCopy.DisabledByGuard {
					// Reached consecutive soft threshold (quarantine now or already isolated).
					degraded = true
					if reason == "" {
						reason = fmt.Sprintf("连续软阈值 Token/s=%.1f", res.TPS)
					}
				}
			}
			// Always count the sample for rate denominator when it's a real generation.
			// For soft-below-threshold, degraded=false (suspicious but not yet 降智事件).
			store.recordAuthObservation(res.AuthID, res.AuthLabel, source, nodeCopy.ID, nodeCopy.Name, res.Classification, reason, res.TPS, degraded)
		}
	}
	if res.Classification == "ignored" {
		// Account, quota, upstream and no-account failures are not evidence that
		// the egress is degraded. Keep the observation for diagnostics, but never
		// spend error strikes or quarantine the node for them.
		store.bumpStat(source, "ignored", res.OutputTokens)
		return
	}
	if doRestore {
		store.bumpAction("restored")
		store.appendEvent(guardEvent{Event: "node_restored", NodeID: nodeCopy.ID, NodeName: nodeCopy.Name, Classification: "healthy", OutputTPS: res.TPS})
		go func(nn nodeRecord) { _ = enableAuthsOnNode(&nn) }(nodeCopy)
	}
	if scheduleCV {
		// Stat as soft while waiting; active confirmation will record the final class.
		store.bumpStat(source, "soft", res.OutputTokens)
		scheduleCrossVerifyProbe(store, nodeCopy.ID, nodeCopy.Name, cvEvent, cvReason, res, source)
		return
	}
	if doQuarantine {
		disableObservedAuth(store, res, quarantineWhy)
		quarantineNode(store, nodeCopy.ID, quarantineWhy, res.TPS, res.Classification)
	}
	store.bumpStat(source, res.Classification, res.OutputTokens)
}

func disableObservedAuth(store *stateStore, res qualityResult, reason string) {
	if store == nil || strings.TrimSpace(res.AuthID) == "" {
		return
	}
	if !store.policy().DisableAuthOnHard {
		return
	}
	if strings.TrimSpace(reason) == "" {
		reason = res.Error
	}
	if strings.TrimSpace(reason) == "" {
		reason = "账号质量异常"
	}
	why := "egress-guard 账号降智: " + reason
	if err := disableAuthByID(res.AuthID, why); err != nil {
		store.appendEvent(guardEvent{
			Event:     "account_disable_failed",
			AuthID:    res.AuthID,
			NodeName:  res.AuthLabel,
			Reason:    err.Error(),
			OutputTPS: res.TPS,
		})
		return
	}
	store.appendEvent(guardEvent{
		Event:          "account_disabled",
		AuthID:         res.AuthID,
		NodeName:       res.AuthLabel,
		Reason:         why,
		Classification: res.Classification,
		OutputTPS:      res.TPS,
	})
}

func quarantineNode(store *stateStore, nodeID, reason string, tps float64, class string) {
	pol := store.policy()
	enabledHealthy := 0
	var target *nodeRecord
	for _, o := range store.listNodes() {
		if o.ID == nodeID {
			target = o
			continue
		}
		if o.Enabled && !o.DisabledByGuard {
			enabledHealthy++
		}
	}
	if target == nil {
		return
	}
	if enabledHealthy < pol.MinHealthyNodes {
		store.bumpAction("suppressed")
		store.appendEvent(guardEvent{Event: "quarantine_suppressed", NodeID: target.ID, NodeName: target.Name, Reason: "低于最低健康节点数", OutputTPS: tps})
		_, _ = store.updateNode(nodeID, func(n *nodeRecord) error {
			n.LastReason = "隔离已抑制: " + reason
			return nil
		})
		return
	}
	updated, err := store.updateNode(nodeID, func(n *nodeRecord) error {
		n.DisabledByGuard = true
		n.QuarantinedUntil = float64(time.Now().Add(time.Duration(pol.QuarantineSec) * time.Second).Unix())
		n.LastReason = reason
		return nil
	})
	if err != nil || updated == nil {
		return
	}
	store.bumpAction("quarantined")
	store.appendEvent(guardEvent{Event: "node_quarantined", NodeID: updated.ID, NodeName: updated.Name, Reason: reason, Classification: class, OutputTPS: tps})
	// Move accounts off the bad channel synchronously. The first phase disables
	// them, so no new request can continue using the quarantined egress while
	// migration and post-save verification are in flight.
	if err := migrateAuthsOffNode(store, updated); err != nil {
		store.appendEvent(guardEvent{Event: "accounts_migration_failed", NodeID: updated.ID, NodeName: updated.Name, Reason: err.Error()})
		if pol.DisableAuthOnHard {
			_ = disableAuthsOnNode(store, updated, "egress-guard 降智隔离: "+reason)
		}
	}
	if rotated, err := rotateNodeIfConfigured(store, updated); err != nil {
		store.appendEvent(guardEvent{Event: "node_rotation_failed", NodeID: updated.ID, NodeName: updated.Name, Reason: err.Error()})
	} else if rotated {
		// A newly rotated IP gets exactly one real-model confirmation before it
		// can leave quarantine. A healthy result restores the node; anomalies keep
		// it isolated for the normal recovery worker.
		_, _ = runNodeQuality(store, updated.ID, "", "")
	}
}

func runNodeConnectivity(store *stateStore, id string) (map[string]any, error) {
	n, ok := store.getNode(id)
	if !ok {
		return nil, fmt.Errorf("节点不存在")
	}
	ip, ms, err := probeConnectivity(n.ProxyURL)
	status := "ok"
	if err != nil {
		status = "error"
	}
	_, _ = store.updateNode(id, func(node *nodeRecord) error {
		node.ProbeStatus = status
		node.ProbeLatencyMs = ms
		node.LastProbeAt = float64(time.Now().Unix())
		if ip != "" {
			node.ExitIP = ip
		}
		if err != nil {
			node.LastReason = err.Error()
		}
		return nil
	})
	out := map[string]any{"id": id, "status": status, "exitIp": ip, "latencyMs": ms}
	if err != nil {
		out["error"] = err.Error()
	}
	return out, nil
}

func runNodeQuality(store *stateStore, id, profileID, preferAuth string) (map[string]any, error) {
	n, ok := store.getNode(id)
	if !ok {
		return nil, fmt.Errorf("节点不存在")
	}
	if n.DisabledByGuard && n.QuarantinedUntil > float64(time.Now().Unix()) {
		// still allow manual quality test for recovery
	}
	res := probeQuality(store, n, profileID, preferAuth)
	if res.Classification == "error" && res.ErrorKind != "transport_error" {
		res.Classification = "ignored"
	}
	applyObservation(store, id, "active", res)
	reason := res.Error
	if reason == "" && res.ErrorKind == reasonMarkerMissing {
		reason = "响应缺少预期标记"
	}
	store.appendEvent(guardEvent{
		Event:          "quality_probe_completed",
		NodeID:         id,
		NodeName:       n.Name,
		Classification: res.Classification,
		OutputTPS:      res.TPS,
		Reason:         reason,
	})
	return map[string]any{
		"id":              id,
		"classification":  res.Classification,
		"tps":             res.TPS,
		"outputTokens":    res.OutputTokens,
		"durationMs":      res.DurationMs,
		"firstTokenMs":    res.FirstTokenMs,
		"exitIp":          res.ExitIP,
		"error":           res.Error,
		"errorKind":       res.ErrorKind,
		"model":           res.Model,
		"profileId":       res.ProfileID,
		"profileName":     res.ProfileName,
		"expectedMatched": res.ExpectedMatched,
		"hasThinking":     res.HasThinking,
	}, nil
}

// handlePassiveUsage maps a CPA usage event onto a node by auth proxy_url.
func handlePassiveUsage(store *stateStore, record map[string]any) {
	pol := store.policy()
	if pol.Mode == "active" {
		return
	}
	provider := strings.ToLower(firstString(record, "Provider", "provider"))
	authType := strings.ToLower(firstString(record, "AuthType", "auth_type", "AuthType"))
	model := strings.ToLower(firstString(record, "Model", "model", "Alias", "alias"))
	if !looksLikeXAIUsage(provider, authType, model) {
		return
	}
	authID := firstString(record, "AuthID", "auth_id", "authId", "AuthIndex", "auth_index")
	authIndex := firstString(record, "AuthIndex", "auth_index")
	failed := false
	if v, ok := record["Failed"]; ok {
		failed, _ = v.(bool)
	}
	if v, ok := record["failed"]; ok {
		failed, _ = v.(bool)
	}

	var outTokens, durMs, ttftMs int64
	if detail, ok := record["Detail"].(map[string]any); ok {
		outTokens = outputTokensFromUsage(detail)
	}
	if detail, ok := record["detail"].(map[string]any); ok {
		if outTokens == 0 {
			outTokens = outputTokensFromUsage(detail)
		}
	}
	if outTokens == 0 {
		outTokens = maxInt64(firstInt(record, "output_tokens", "OutputTokens", "completion_tokens", "completionTokens"), firstInt(record, "reasoning_tokens", "ReasoningTokens"))
	}
	durMs = firstInt(record, "duration_ms", "DurationMs", "latency_ms")
	if durMs == 0 {
		if lat, ok := record["Latency"].(float64); ok {
			// encoding/json decodes time.Duration as nanoseconds
			if lat > 1e6 {
				durMs = int64(lat / 1e6)
			} else {
				durMs = int64(lat)
			}
		}
		if lat, ok := record["latency"].(float64); ok && durMs == 0 {
			if lat > 1e6 {
				durMs = int64(lat / 1e6)
			} else {
				durMs = int64(lat)
			}
		}
	}
	ttftMs = firstInt(record, "first_token_ms", "FirstTokenMs", "ttft_ms")
	if ttftMs == 0 {
		if t, ok := record["TTFT"].(float64); ok {
			if t > 1e6 {
				ttftMs = int64(t / 1e6)
			} else {
				ttftMs = int64(t)
			}
		}
	}

	class := "unknown"
	tps := 0.0
	errorKind := ""
	hasThinking := false
	if failed {
		failure, _ := record["Failure"].(map[string]any)
		if failure == nil {
			failure, _ = record["failure"].(map[string]any)
		}
		status := int(firstInt(failure, "StatusCode", "status_code", "status"))
		body := firstString(failure, "Body", "body", "message", "error")
		errorKind = classifyFailureKind(status, body)
		if errorKind == "transport_error" {
			class = "error"
		} else {
			class = "ignored"
		}
	} else {
		tps = computeTPS(outTokens, durMs, ttftMs, pol.MinGenerationMs)
		hasThinking = recordHasThinking(record)
		hint, hasHint := recentRequestHint(authID, authIndex)
		if !hasThinking && hasHint && hint.Multimodal {
			// Vision/image turns often omit reasoning_tokens and thinking_content
			// on the usage event. Do not treat that as missing-thinking 降智.
			hasThinking = true
		}
		thinkingOn := true
		if requested, known := usageThinkingRequested(record); known {
			thinkingOn = requested
		} else if hasHint && hint.ThinkingKnown {
			thinkingOn = hint.ThinkingRequested
		}
		if thinkingOn {
			if hasHint && hint.StreamSeen && !hint.Multimodal {
				hasThinking = hint.StreamHasThinking
			} else if !hasThinking {
				// Usage omitted reasoning_tokens. Without a stream sample
				// that is missing evidence, not missing thinking.
				hasThinking = true
			}
			class = classifyQuality(tps, outTokens, hasThinking, pol)
		} else {
			// Client disabled thinking: missing thinking is expected, use TPS only.
			class = classifyQuality(tps, outTokens, true, pol)
			hasThinking = true
		}
	}

	// On anomaly, force auth-proxy cache refresh so we don't miss mappings.
	if class == "hard" || class == "soft" {
		invalidateAuthProxyCache()
	}
	nodeID := resolveNodeIDForAuth(store, authID, authIndex,
		filepath.Base(authID), strings.TrimSuffix(filepath.Base(authID), ".json"))
	if nodeID == "" {
		nodeID = fallbackNodeIDForUnboundAuth(store)
	}
	authKey := firstNonEmpty(authID, authIndex)
	res := qualityResult{
		Classification: class,
		TPS:            tps,
		OutputTokens:   outTokens,
		DurationMs:     durMs,
		FirstTokenMs:   ttftMs,
		HasThinking:    hasThinking,
		AuthID:         authKey,
		AuthLabel:      authKey,
		ErrorKind:      errorKind,
	}
	if class == "hard" && !failed && pol.ThinkingGuard && !hasThinking {
		res.Error = "响应缺少 thinking_content（降智）"
	}
	if nodeID == "" {
		store.bumpStat("passive", class, outTokens)
		if class == "hard" || class == "soft" {
			store.appendEvent(guardEvent{
				Event:          "unmapped_" + class,
				AuthID:         authKey,
				Classification: class,
				OutputTPS:      tps,
				Reason:         fmt.Sprintf("usage 未映射到出口节点 auth=%s idx=%s tokens=%d dur=%dms ttft=%dms", authID, authIndex, outTokens, durMs, ttftMs),
			})
		}
		// Unmapped egress: still attribute account samples. Missing-thinking hard counts
		// as degrade immediately (no node to cross-verify against). Soft does not.
		if authKey != "" && class != "error" && class != "ignored" && class != "unknown" {
			degraded := false
			reason := res.Error
			if pol.ThinkingGuard && class == "hard" && !hasThinking && !failed {
				degraded = true
				if reason == "" {
					reason = "响应缺少 thinking_content（降智）"
				}
			} else if class == "hard" && !failed {
				degraded = true
				if reason == "" {
					reason = fmt.Sprintf("硬阈值 Token/s=%.1f", tps)
				}
			}
			store.recordAuthObservation(authKey, authKey, "passive", "", "", class, reason, tps, degraded)
		}
		return
	}
	// Always apply observation for mapped nodes (quarantine on hard/soft).
	applyObservation(store, nodeID, "passive", res)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// fallbackNodeIDForUnboundAuth maps account traffic that has no proxy_url onto
// the only enabled egress. Multi-node deployments stay unmapped.
func fallbackNodeIDForUnboundAuth(store *stateStore) string {
	if store == nil {
		return ""
	}
	var only *nodeRecord
	for _, n := range store.listNodes() {
		if n == nil || !n.Enabled || n.ProxyURL == "" {
			continue
		}
		if only != nil {
			return ""
		}
		only = n
	}
	if only == nil {
		return ""
	}
	return only.ID
}

func looksLikeXAIUsage(provider, authType, model string) bool {
	for _, v := range []string{provider, authType, model} {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if strings.Contains(v, "xai") || strings.Contains(v, "grok") {
			return true
		}
	}
	// Empty provider/model still reaches the plugin for xAI-only deployments.
	return provider == "" && authType == ""
}

func busiestEnabledNode(store *stateStore) string {
	bestID := ""
	bestN := -1
	for _, n := range store.listNodes() {
		if !n.Enabled || n.DisabledByGuard || n.ProxyURL == "" {
			continue
		}
		if n.AssignedAccountCount > bestN {
			bestN = n.AssignedAccountCount
			bestID = n.ID
		}
	}
	return bestID
}

// backgroundWorker periodically probes quarantined / active mode nodes.
func startGuardWorker(ctx context.Context, store *stateStore) {
	go func() {
		// First reconcile is deferred so plugin.register never blocks on
		// host.auth.list/get (store-install activation path).
		reconcile := time.NewTimer(3 * time.Second)
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		tick := 0
		for {
			select {
			case <-ctx.Done():
				if !reconcile.Stop() {
					select {
					case <-reconcile.C:
					default:
					}
				}
				_ = store.Flush()
				return
			case <-reconcile.C:
				refreshAssignedCounts(store)
			case <-t.C:
				tick++
				pol := store.policy()
				now := float64(time.Now().Unix())
				for _, n := range store.listNodes() {
					if n.DisabledByGuard && n.QuarantinedUntil > 0 && now >= n.QuarantinedUntil {
						_, _ = runNodeQuality(store, n.ID, "", "")
						continue
					}
					if pol.Mode == "active" || pol.Mode == "hybrid" {
						// light active cadence per node via last probe
						if n.Enabled && !n.DisabledByGuard && (n.LastProbeAt == 0 || now-n.LastProbeAt >= float64(pol.ActiveIntervalSec)) {
							// don't stampede — one per tick
							_, _ = runNodeQuality(store, n.ID, "", "")
							break
						}
					}
				}
				// Assigned counts are maintained on rebalance/migrate; a slow
				// background reconcile is enough for drift from external edits.
				if tick%10 == 0 {
					refreshAssignedCounts(store)
				}
			}
		}
	}()
}
