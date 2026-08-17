package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestComputeTPSUsesGenerationWindow(t *testing.T) {
	// 1050 tokens over 500ms generation window (1100-600) => 2100 TPS
	if got := computeTPS(1050, 1100, 600, 200); got < 2099 || got > 2101 {
		t.Fatalf("computeTPS()=%v, want ~2100", got)
	}
	// tiny generation window falls back to full duration (avoid false hard)
	// 100 tokens / 1000ms => 100 TPS
	if got := computeTPS(100, 1000, 950, 200); got < 99 || got > 101 {
		t.Fatalf("computeTPS()=%v, want ~100 with min window fallback", got)
	}
	if got := computeTPS(100, 0, 0, 200); got != 0 {
		t.Fatalf("computeTPS()=%v, want 0", got)
	}
}

func TestFailureClassificationDoesNotTreatAuthErrorsAsTransport(t *testing.T) {
	for _, test := range []struct {
		status int
		body   string
		kind   string
	}{
		{401, "invalid or expired token", "account_error"},
		{429, "rate limit", "account_error"},
		{0, "dial tcp 10.0.0.1: i/o timeout", "transport_error"},
		{503, "upstream unavailable", "upstream_error"},
	} {
		if got := classifyFailureKind(test.status, test.body); got != test.kind {
			t.Fatalf("classifyFailureKind(%d, %q)=%q, want %q", test.status, test.body, got, test.kind)
		}
	}
}

func TestXAITokenAccountingDoesNotDoubleCountReasoning(t *testing.T) {
	if got := maxInt64(180, 75); got != 180 {
		t.Fatalf("max token total=%d, want 180", got)
	}
	if got := outputTokensFromUsage(map[string]any{
		"completion_tokens": 180,
		"output_tokens":     180,
		"reasoning_tokens":  75,
	}); got != 180 {
		t.Fatalf("authoritative token total=%d, want 180", got)
	}
}

func TestThinkingPresenceIsPrimaryQualitySignal(t *testing.T) {
	pol := defaultPolicy()
	pol.ThinkingGuard = true
	// Small output without thinking is not enough evidence.
	if got := classifyQuality(5000, pol.MinOutputTokens-1, false, pol); got != "ignored" {
		t.Fatalf("small output without thinking=%q, want ignored", got)
	}
	// Enough output but no thinking → 降智 / hard.
	if got := classifyQuality(5000, pol.MinOutputTokens, false, pol); got != "hard" {
		t.Fatalf("no-thinking classification=%q, want hard", got)
	}
	// Thinking present falls back to original Token/s thresholds.
	if got := classifyQuality(5000, pol.MinOutputTokens, true, pol); got != "hard" {
		t.Fatalf("with-thinking high TPS classification=%q, want hard", got)
	}
	if got := classifyQuality(10, 200, true, pol); got != "healthy" {
		t.Fatalf("with-thinking low TPS classification=%q, want healthy", got)
	}
	if got := classifyQuality(750, 200, true, pol); got != "soft" {
		t.Fatalf("with-thinking mid TPS classification=%q, want soft", got)
	}
}

func TestThinkingGuardOffFallsBackToTPSOnly(t *testing.T) {
	pol := defaultPolicy()
	pol.ThinkingGuard = false
	if got := classifyQuality(5000, 200, false, pol); got != "hard" {
		t.Fatalf("guard off high TPS=%q, want hard", got)
	}
	if got := classifyQuality(10, 200, false, pol); got != "healthy" {
		t.Fatalf("guard off low TPS without thinking=%q, want healthy", got)
	}
}

func TestRecordHasThinkingFallsBackToReasoningTokens(t *testing.T) {
	if recordHasThinking(map[string]any{
		"Detail": map[string]any{"reasoning_tokens": float64(12)},
	}) != true {
		t.Fatal("reasoning_tokens in Detail should count as thinking")
	}
	if recordHasThinking(map[string]any{
		"delta": map[string]any{"thinking_content": "step 1"},
	}) != true {
		t.Fatal("thinking_content should count as thinking")
	}
	if recordHasThinking(map[string]any{
		"output_tokens": float64(64),
	}) != false {
		t.Fatal("plain output without reasoning must not count as thinking")
	}
}

func TestRecordHasThinkingAcceptsReasoningContentParts(t *testing.T) {
	if recordHasThinking(map[string]any{
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "thinking", "thinking": "look at the photo"},
				map[string]any{"type": "text", "text": "a cat"},
			},
		},
	}) != true {
		t.Fatal("typed thinking content parts should count as thinking")
	}
	if recordHasThinking(map[string]any{
		"delta": map[string]any{"encrypted_content": "opaque-reasoning-blob"},
	}) != true {
		t.Fatal("encrypted_content should count as thinking")
	}
	if recordHasThinking(map[string]any{
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "visible only"},
			},
		},
	}) != false {
		t.Fatal("plain text content parts must not count as thinking")
	}
}

func TestRequestLooksMultimodal(t *testing.T) {
	if !requestLooksMultimodal([]byte(`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aa"}}]}]}`)) {
		t.Fatal("openai image_url body should look multimodal")
	}
	if !requestLooksMultimodal([]byte(`{"input":[{"type":"input_image","image_url":"https://example.invalid/a.png"}]}`)) {
		t.Fatal("responses input_image should look multimodal")
	}
	if requestLooksMultimodal([]byte(`{"messages":[{"role":"user","content":"describe thinking"}]}`)) {
		t.Fatal("plain text must not look multimodal")
	}
}

func TestRequestThinkingRequested(t *testing.T) {
	if requested, known := requestThinkingRequested([]byte(`{"reasoning":{"effort":"none"}}`)); !known || requested {
		t.Fatalf("reasoning.effort=none want known+off, got known=%v requested=%v", known, requested)
	}
	if requested, known := requestThinkingRequested([]byte(`{"reasoning_effort":"high"}`)); !known || !requested {
		t.Fatalf("reasoning_effort=high want known+on, got known=%v requested=%v", known, requested)
	}
	if requested, known := requestThinkingRequested([]byte(`{"thinking":{"type":"disabled"}}`)); !known || requested {
		t.Fatalf("thinking.type=disabled want known+off, got known=%v requested=%v", known, requested)
	}
	if _, known := requestThinkingRequested([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)); known {
		t.Fatal("omitted thinking config must stay unknown")
	}
}

func TestPassiveUsageSkipsMissingThinkingForImageRequest(t *testing.T) {
	prevStore := store
	t.Cleanup(func() {
		store = prevStore
		resetMultimodalAuthMarks()
		invalidateAuthProxyCache()
	})
	resetMultimodalAuthMarks()
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	pol := store.policy()
	pol.ThinkingGuard = true
	pol.ThinkingCrossVerify = false
	pol.MinHealthyNodes = 0
	if err := store.updatePolicy(pol); err != nil {
		t.Fatal(err)
	}
	node, err := store.createNode("ipv6", "socks5h://u:p@127.0.0.1:1080", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	record := map[string]any{
		"Provider": "xai",
		"AuthID":   "xai-user.json",
		"Model":    "grok-4.6",
		"Detail":   map[string]any{"OutputTokens": int64(80)},
		"Latency":  float64(2500 * 1e6),
		"TTFT":     float64(400 * 1e6),
	}
	handlePassiveUsage(store, record)
	got, _ := store.getNode(node.ID)
	if got.LastClassification != "hard" || !strings.Contains(got.LastReason, "响应缺少 thinking_content（降智）") {
		t.Fatalf("text-only missing thinking want hard+missing thinking, got class=%q reason=%q", got.LastClassification, got.LastReason)
	}

	rawRequest, _ := json.Marshal(pluginapi.RequestInterceptRequest{
		Metadata: map[string]any{"selected_auth_id": "xai-user.json"},
		Body:     []byte(`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aa"}}]}]}`),
	})
	if _, err := handleRequestIntercept(rawRequest, true); err != nil {
		t.Fatal(err)
	}
	handlePassiveUsage(store, record)
	got, _ = store.getNode(node.ID)
	if got.LastClassification != "healthy" {
		t.Fatalf("image request missing reasoning_tokens want healthy/TPS, got class=%q reason=%q", got.LastClassification, got.LastReason)
	}
	if got.ThinkingStrikes != 0 {
		t.Fatalf("image request must not accumulate thinking strikes, got %d", got.ThinkingStrikes)
	}
}

func TestPassiveUsageSkipsMissingThinkingWhenThinkingOff(t *testing.T) {
	prevStore := store
	t.Cleanup(func() {
		store = prevStore
		resetRequestHints()
		invalidateAuthProxyCache()
	})
	resetRequestHints()
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	pol := store.policy()
	pol.ThinkingGuard = true
	pol.ThinkingCrossVerify = false
	pol.MinHealthyNodes = 0
	if err := store.updatePolicy(pol); err != nil {
		t.Fatal(err)
	}
	node, err := store.createNode("ipv6", "socks5h://u:p@127.0.0.1:1080", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	handlePassiveUsage(store, map[string]any{
		"Provider":        "xai",
		"AuthID":          "xai-user.json",
		"Model":           "grok-4.6",
		"ReasoningEffort": "none",
		"Detail":          map[string]any{"OutputTokens": int64(80)},
		"Latency":         float64(2500 * 1e6),
		"TTFT":            float64(400 * 1e6),
	})
	got, _ := store.getNode(node.ID)
	if got.LastClassification != "healthy" {
		t.Fatalf("thinking off want TPS-only healthy, got class=%q reason=%q", got.LastClassification, got.LastReason)
	}
	if got.ThinkingStrikes != 0 {
		t.Fatalf("thinking off must not accumulate thinking strikes, got %d", got.ThinkingStrikes)
	}

	rawRequest, _ := json.Marshal(pluginapi.RequestInterceptRequest{
		Metadata: map[string]any{"selected_auth_id": "xai-user.json"},
		Body:     []byte(`{"reasoning":{"effort":"none"},"messages":[{"role":"user","content":"hi"}]}`),
	})
	if _, err := handleRequestIntercept(rawRequest, true); err != nil {
		t.Fatal(err)
	}
	handlePassiveUsage(store, map[string]any{
		"Provider": "xai",
		"AuthID":   "xai-user.json",
		"Model":    "grok-4.6",
		"Detail":   map[string]any{"OutputTokens": int64(90)},
		"Latency":  float64(2600 * 1e6),
		"TTFT":     float64(400 * 1e6),
	})
	got, _ = store.getNode(node.ID)
	if got.LastClassification != "healthy" {
		t.Fatalf("intercept thinking off want healthy, got class=%q reason=%q", got.LastClassification, got.LastReason)
	}
}

func TestAccountQuotaExhaustedDetection(t *testing.T) {
	for _, test := range []struct {
		status int
		body   string
		want   bool
	}{
		{200, "free-usage-exhausted for plan", true},
		{403, "FREE_USAGE_EXHAUSTED", true},
		{400, "subscription:free-usage limit", true},
		{400, "Included Free Usage has ended", true},
		{429, "rate limit exceeded", true},
		{429, "quota remaining 0", true},
		{429, "daily usage cap", true},
		{200, "quota exhausted on account", true},
		{429, "please slow down", false},
		{500, "internal error", false},
	} {
		if got := isAccountQuotaExhausted(test.status, test.body); got != test.want {
			t.Fatalf("isAccountQuotaExhausted(%d, %q)=%v, want %v", test.status, test.body, got, test.want)
		}
	}
}

func TestShouldRetryProbeWithNextAuth(t *testing.T) {
	if !shouldRetryProbeWithNextAuth(true, 429, "free-usage-exhausted", "account_error") {
		t.Fatal("quota exhaustion must retry next auth")
	}
	if shouldRetryProbeWithNextAuth(false, 429, "free-usage-exhausted", "account_error") {
		t.Fatal("no next auth must not retry")
	}
	if !shouldRetryProbeWithNextAuth(true, 401, "invalid or expired token", "account_error") {
		t.Fatal("auth error must retry next auth")
	}
	if shouldRetryProbeWithNextAuth(true, 502, "bad gateway", "upstream_error") {
		t.Fatal("upstream error must not switch auth")
	}
}

func TestProbeUnstableErrDetection(t *testing.T) {
	for _, msg := range []string{
		"read: connection reset by peer",
		"unexpected EOF",
		"http2: stream closed",
		"tls: handshake failure",
		"i/o timeout",
	} {
		if !isProbeUnstableErr(errors.New(msg)) {
			t.Fatalf("expected unstable for %q", msg)
		}
	}
	if isProbeUnstableErr(errors.New("json: cannot unmarshal")) {
		t.Fatal("parse errors are not probe instability")
	}
	res := probeUnstableResult(qualityResult{Model: "grok-4.5"}, errors.New("connection reset by peer"), 1234)
	if res.Classification != "hard" || res.ErrorKind != "probe_unstable" {
		t.Fatalf("unstable result=%+v", res)
	}
	if !strings.Contains(res.Error, "断流不稳定") {
		t.Fatalf("error text=%q", res.Error)
	}
}

func TestDefaultPolicyThinkingFeaturesOn(t *testing.T) {
	pol := defaultPolicy()
	if !pol.ThinkingGuard {
		t.Fatal("default ThinkingGuard should be on")
	}
	if !pol.ThinkingCrossVerify {
		t.Fatal("default ThinkingCrossVerify should be on")
	}
	if !pol.SoftCrossVerify {
		t.Fatal("default SoftCrossVerify should be on")
	}
	if pol.ConsecutiveMissingThinking != 1 {
		t.Fatalf("default consecutive missing thinking=%d, want 1", pol.ConsecutiveMissingThinking)
	}
	if got := classifyQuality(100, 64, false, pol); got != "hard" {
		t.Fatalf("missing thinking with guard=%q, want hard", got)
	}
}

func TestNormalizePolicyFillsAbsentBoolDefaults(t *testing.T) {
	// Pure old state: no redesign keys, schema 0.
	p := policyConfig{HardTPS: 1000, SoftTPS: 500}
	normalizePolicy(&p, map[string]any{
		"hard_tps": 1000,
		"soft_tps": 500,
	})
	if !p.ThinkingGuard {
		t.Fatal("absent thinking_guard must default on")
	}
	if !p.ThinkingCrossVerify {
		t.Fatal("schema migration must turn thinking_cross_verify on")
	}
	if !p.SoftCrossVerify {
		t.Fatal("absent soft_cross_verify must default on")
	}
	if p.ConsecutiveMissingThinking != 1 {
		t.Fatalf("consecutive_missing_thinking=%d, want 1", p.ConsecutiveMissingThinking)
	}
	if p.PolicySchema != 3 {
		t.Fatalf("policy_schema=%d, want 3", p.PolicySchema)
	}

	// Intermediate build: explicit thinking_cross_verify=false but schema still 0.
	pMid := policyConfig{HardTPS: 1000, SoftTPS: 500, ThinkingGuard: true, ThinkingCrossVerify: false, ConsecutiveMissingThinking: 1}
	normalizePolicy(&pMid, map[string]any{
		"hard_tps":                     1000,
		"soft_tps":                     500,
		"thinking_guard":               true,
		"thinking_cross_verify":        false,
		"consecutive_missing_thinking": 1,
	})
	if !pMid.ThinkingCrossVerify {
		t.Fatal("schema<2 must migrate thinking_cross_verify to default on even if false was persisted")
	}
	if !pMid.SoftCrossVerify {
		t.Fatal("schema migration must turn soft_cross_verify on")
	}
	if pMid.PolicySchema != 3 {
		t.Fatalf("migrated policy_schema=%d, want 3", pMid.PolicySchema)
	}

	// After redesign (schema 2), explicit false must stick.
	p2 := policyConfig{HardTPS: 1000, SoftTPS: 500, ThinkingGuard: true, ThinkingCrossVerify: false, SoftCrossVerify: false, ConsecutiveMissingThinking: 2, PolicySchema: 3}
	normalizePolicy(&p2, map[string]any{
		"hard_tps":                     1000,
		"soft_tps":                     500,
		"thinking_guard":               true,
		"thinking_cross_verify":        false,
		"soft_cross_verify":            false,
		"consecutive_missing_thinking": 2,
		"policy_schema":                3,
	})
	if p2.ThinkingCrossVerify {
		t.Fatal("explicit false after schema 2 must stay false")
	}

	// Explicit thinking_guard=false stays false and forces cross-verify off.
	p3 := policyConfig{HardTPS: 1000, SoftTPS: 500, ThinkingGuard: false}
	normalizePolicy(&p3, map[string]any{
		"hard_tps":       1000,
		"soft_tps":       500,
		"thinking_guard": false,
	})
	if p3.ThinkingGuard || p3.ThinkingCrossVerify {
		t.Fatal("explicit thinking_guard=false must disable guard and cross-verify")
	}
}

func TestMissingThinkingRequiresConsecutiveStrikes(t *testing.T) {
	store := newStateStore(filepath.Join(t.TempDir(), "state.json"))
	node, err := store.createNode("n1", "http://127.0.0.1:7951", true, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	// Keep min_healthy satisfiable so quarantine is not suppressed.
	if _, err := store.createNode("n2", "http://127.0.0.1:7952", true, false, 10); err != nil {
		t.Fatal(err)
	}
	pol := store.policy()
	pol.ThinkingGuard = true
	pol.ThinkingCrossVerify = false
	pol.ConsecutiveMissingThinking = 2
	pol.MinHealthyNodes = 1
	if err := store.updatePolicy(pol); err != nil {
		t.Fatal(err)
	}
	res := qualityResult{Classification: "hard", HasThinking: false, OutputTokens: 64, TPS: 10}
	applyObservation(store, node.ID, "passive", res)
	got, _ := store.getNode(node.ID)
	if got.DisabledByGuard {
		t.Fatal("first missing-thinking must not quarantine when threshold=2")
	}
	if got.ThinkingStrikes != 1 {
		t.Fatalf("thinking strikes=%d, want 1", got.ThinkingStrikes)
	}
	applyObservation(store, node.ID, "passive", res)
	got, _ = store.getNode(node.ID)
	if !got.DisabledByGuard {
		t.Fatal("second missing-thinking should quarantine when threshold=2 and cross-verify off")
	}
}

func TestThinkingCrossVerifySchedulesInsteadOfQuarantine(t *testing.T) {
	store := newStateStore(filepath.Join(t.TempDir(), "state.json"))
	node, err := store.createNode("n1", "http://127.0.0.1:7951", true, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	// Add a second healthy node so quarantine is not suppressed by min_healthy.
	if _, err := store.createNode("n2", "http://127.0.0.1:7952", true, false, 10); err != nil {
		t.Fatal(err)
	}
	pol := store.policy()
	pol.ThinkingGuard = true
	pol.ThinkingCrossVerify = true
	pol.ConsecutiveMissingThinking = 1
	pol.MinHealthyNodes = 1
	if err := store.updatePolicy(pol); err != nil {
		t.Fatal(err)
	}
	res := qualityResult{Classification: "hard", HasThinking: false, OutputTokens: 64, TPS: 10}
	applyObservation(store, node.ID, "passive", res)
	got, _ := store.getNode(node.ID)
	if got.DisabledByGuard {
		t.Fatal("passive missing-thinking with cross-verify must not quarantine immediately")
	}
	if got.ThinkingStrikes < 1 {
		t.Fatal("thinking strikes should accumulate")
	}
	// Active confirmation quarantines without re-scheduling.
	applyObservation(store, node.ID, "active", res)
	got, _ = store.getNode(node.ID)
	if !got.DisabledByGuard {
		t.Fatal("active missing-thinking confirmation should quarantine")
	}
	endCrossVerify(node.ID)
}

func TestSoftCrossVerifySchedulesInsteadOfQuarantine(t *testing.T) {
	store := newStateStore(filepath.Join(t.TempDir(), "state.json"))
	node, err := store.createNode("n1", "http://127.0.0.1:7951", true, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.createNode("n2", "http://127.0.0.1:7952", true, false, 10); err != nil {
		t.Fatal(err)
	}
	pol := store.policy()
	pol.SoftCrossVerify = true
	pol.ConsecutiveSoft = 1
	pol.MinHealthyNodes = 1
	if err := store.updatePolicy(pol); err != nil {
		t.Fatal(err)
	}
	res := qualityResult{Classification: "soft", HasThinking: true, OutputTokens: 64, TPS: 600}
	applyObservation(store, node.ID, "passive", res)
	got, _ := store.getNode(node.ID)
	if got.DisabledByGuard {
		t.Fatal("soft cross-verify must defer quarantine")
	}
	applyObservation(store, node.ID, "active", res)
	got, _ = store.getNode(node.ID)
	if !got.DisabledByGuard {
		t.Fatal("active soft confirmation should quarantine")
	}
	endCrossVerify(node.ID)
}

func TestAuthDegradeCountsPassiveEvenWhenCrossVerifyScheduled(t *testing.T) {
	store := newStateStore(filepath.Join(t.TempDir(), "state.json"))
	node, err := store.createNode("n1", "http://127.0.0.1:7951", true, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.createNode("n2", "http://127.0.0.1:7952", true, false, 10); err != nil {
		t.Fatal(err)
	}
	pol := store.policy()
	pol.ThinkingGuard = true
	pol.ThinkingCrossVerify = true
	pol.ConsecutiveMissingThinking = 1
	pol.MinHealthyNodes = 1
	if err := store.updatePolicy(pol); err != nil {
		t.Fatal(err)
	}
	passive := qualityResult{
		Classification: "hard",
		HasThinking:    false,
		OutputTokens:   64,
		TPS:            10,
		AuthID:         "passive@x",
		AuthLabel:      "passive@x",
		Error:          "响应缺少 thinking_content（降智）",
	}
	applyObservation(store, node.ID, "passive", passive)
	items := store.listAuthDegradeStats()
	if len(items) != 1 || items[0].AuthID != "passive@x" || items[0].SampleCount != 1 || items[0].DegradedCount != 1 {
		t.Fatalf("passive missing-thinking must count degrade immediately: %+v", items)
	}
	// Cross-verify uses a different account and must not merge into the passive one.
	// Healthy retest → only the probe account gets a normal sample.
	probeOK := qualityResult{
		Classification: "healthy",
		HasThinking:    true,
		OutputTokens:   80,
		TPS:            40,
		AuthID:         "probe@x",
		AuthLabel:      "probe@x",
	}
	applyObservation(store, node.ID, "active", probeOK)
	items = store.listAuthDegradeStats()
	byID := map[string]*authDegradeRecord{}
	for _, it := range items {
		byID[it.AuthID] = it
	}
	if p := byID["passive@x"]; p == nil || p.DegradedCount != 1 || p.SampleCount != 1 {
		t.Fatalf("passive stats must stay degrade=1 sample=1, got %+v", p)
	}
	if p := byID["probe@x"]; p == nil || p.DegradedCount != 0 || p.SampleCount != 1 {
		t.Fatalf("healthy cross-verify account must be sample=1 degrade=0, got %+v", p)
	}
	// Same probe account missing thinking on a later retest counts only for itself.
	probeBad := qualityResult{
		Classification: "hard",
		HasThinking:    false,
		OutputTokens:   64,
		TPS:            10,
		AuthID:         "probe@x",
		AuthLabel:      "probe@x",
		Error:          "响应缺少 thinking_content（降智）",
	}
	applyObservation(store, node.ID, "active", probeBad)
	items = store.listAuthDegradeStats()
	byID = map[string]*authDegradeRecord{}
	for _, it := range items {
		byID[it.AuthID] = it
	}
	if p := byID["passive@x"]; p == nil || p.DegradedCount != 1 || p.SampleCount != 1 {
		t.Fatalf("passive must remain untouched after other-account retest: %+v", p)
	}
	if p := byID["probe@x"]; p == nil || p.DegradedCount != 1 || p.SampleCount != 2 {
		t.Fatalf("probe missing-thinking want degrade=1 sample=2, got %+v", p)
	}
	endCrossVerify(node.ID)
}

func TestRecordAuthDegradeStats(t *testing.T) {
	store := newStateStore(filepath.Join(t.TempDir(), "state.json"))
	store.recordAuthObservation("a1@x", "a1@x", "passive", "1", "n1", "hard", "响应缺少 thinking_content（降智）", 10, true)
	store.recordAuthObservation("a1@x", "a1@x", "passive", "1", "n1", "healthy", "", 8, false)
	store.recordAuthObservation("b2@x", "b2@x", "passive", "2", "n2", "hard", "响应缺少 thinking_content（降智）", 12, true)
	items := store.listAuthDegradeStats()
	if len(items) != 2 {
		t.Fatalf("auth stats len=%d, want 2", len(items))
	}
	if items[0].AuthID != "a1@x" && items[0].DegradedCount < items[1].DegradedCount {
		t.Fatalf("expected higher degrade count first: %+v", items)
	}
	var a1 *authDegradeRecord
	for _, it := range items {
		if it.AuthID == "a1@x" {
			a1 = it
		}
	}
	if a1 == nil || a1.DegradedCount != 1 || a1.SampleCount != 2 {
		t.Fatalf("a1 stats=%+v", a1)
	}
}

func TestManualDisabledAuthIsNotRestored(t *testing.T) {
	if isGuardDisabledAuth(authFile{Disabled: true, Raw: map[string]any{"disabled_reason": "operator: maintenance"}}) {
		t.Fatal("operator-disabled auth must not be treated as guard-managed")
	}
}

func TestSchedulerSkipsCoolingStatuses(t *testing.T) {
	for _, status := range []string{"disabled", "unavailable", "error", "cooling", "pending", "refreshing", "future-state"} {
		if schedulerCandidateAvailable(pluginapi.SchedulerAuthCandidate{Status: status}) {
			t.Fatalf("status %q should not be selected", status)
		}
	}
	for _, status := range []string{"", "active", "ready"} {
		if !schedulerCandidateAvailable(pluginapi.SchedulerAuthCandidate{Status: status}) {
			t.Fatalf("status %q should be selectable", status)
		}
	}
}

func TestMigrationFailsClosedAndVerifiesHostAuthSave(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "state.json"))
	bad, err := store.createNode("bad", "http://127.0.0.1:7951", true, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	good, err := store.createNode("good", "http://127.0.0.1:7952", true, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.updateNode(good.ID, func(node *nodeRecord) error {
		node.LastClassification = "healthy"
		node.LastProbeAt = float64(time.Now().Unix())
		node.ExitIP = "198.51.100.2"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.updateNode(bad.ID, func(node *nodeRecord) error {
		node.ExitIP = "198.51.100.1"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	auths := map[string]map[string]any{
		"bad.json": {
			"type": "xai", "email": "bad@example.test", "access_token": "bad-token", "proxy_url": bad.ProxyURL, "disabled": false,
		},
		"good.json": {
			"type": "xai", "email": "good@example.test", "access_token": "good-token", "proxy_url": good.ProxyURL, "disabled": false,
		},
		"manual.json": {
			"type": "xai", "email": "manual@example.test", "access_token": "manual-token", "proxy_url": bad.ProxyURL, "disabled": true, "disabled_reason": "operator maintenance",
		},
	}
	originalHostCall := hostCall
	hostCall = func(method string, payload []byte) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			entries := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
			for name, raw := range auths {
				disabled, _ := raw["disabled"].(bool)
				entries = append(entries, pluginapi.HostAuthFileEntry{ID: name, AuthIndex: name, Name: name, Provider: "xai", Type: "xai", Disabled: disabled})
			}
			return json.Marshal(hostAuthListResponse{Files: entries})
		case pluginabi.MethodHostAuthGet:
			var request map[string]string
			_ = json.Unmarshal(payload, &request)
			name := request["auth_index"]
			if name == "" {
				name = request["name"]
			}
			raw, ok := auths[name]
			if !ok {
				return nil, fmt.Errorf("auth not found: %s", name)
			}
			body, _ := json.Marshal(raw)
			return json.Marshal(hostAuthGetResponse{AuthIndex: name, Name: name, Path: "/auths/" + name, JSON: body})
		case pluginabi.MethodHostAuthSave:
			var request struct {
				Name string          `json:"name"`
				JSON json.RawMessage `json:"json"`
			}
			if err := json.Unmarshal(payload, &request); err != nil {
				return nil, err
			}
			updated := map[string]any{}
			if err := json.Unmarshal(request.JSON, &updated); err != nil {
				return nil, err
			}
			auths[request.Name] = updated
			return json.Marshal(pluginapi.HostAuthSaveResponse{Name: request.Name, Path: "/auths/" + request.Name})
		default:
			return nil, fmt.Errorf("unexpected host callback %s", method)
		}
	}
	defer func() {
		hostCall = originalHostCall
		authProxyMu.Lock()
		authProxyCache = nil
		authProxyAt = time.Time{}
		authProxyMu.Unlock()
		invalidateAuthListCache()
	}()

	if err := migrateAuthsOffNode(store, bad); err != nil {
		t.Fatalf("migrateAuthsOffNode() error = %v", err)
	}
	if got := auths["bad.json"]["proxy_url"]; got != good.ProxyURL {
		t.Fatalf("bad auth proxy=%q, want healthy proxy", got)
	}
	if disabled, _ := auths["bad.json"]["disabled"].(bool); disabled {
		t.Fatal("migrated auth remains disabled")
	}
	if got := auths["manual.json"]["proxy_url"]; got != bad.ProxyURL {
		t.Fatalf("manual auth proxy=%q, want unchanged bad proxy", got)
	}
	if disabled, _ := auths["manual.json"]["disabled"].(bool); !disabled {
		t.Fatal("manual disabled auth was re-enabled")
	}
}

func TestSchedulerSkipsQuarantinedNode(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "state.json"))
	bad, err := store.createNode("bad", "http://127.0.0.1:7951", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	good, err := store.createNode("good", "http://127.0.0.1:7952", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.updateNode(bad.ID, func(node *nodeRecord) error { node.DisabledByGuard = true; return nil }); err != nil {
		t.Fatal(err)
	}
	authProxyMu.Lock()
	authProxyCache = map[string]string{"auth-bad": bad.ProxyURL, "auth-good": good.ProxyURL}
	authProxyAt = time.Now()
	authProxyMu.Unlock()
	rawRequest, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "xai",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "auth-bad", Provider: "xai"},
			{ID: "auth-good", Provider: "xai"},
		},
	})
	raw, err := handleSchedulerPick(rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("scheduler envelope=%s err=%v", raw, err)
	}
	var response pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	if !response.Handled || response.AuthID != "auth-good" {
		t.Fatalf("scheduler response=%+v", response)
	}
}

func TestSchedulerDefersHealthyManagedAuthsToCPA(t *testing.T) {
	prevStore := store
	t.Cleanup(func() {
		store = prevStore
		authProxyMu.Lock()
		authProxyCache = nil
		authProxyAt = time.Time{}
		authProxyMu.Unlock()
	})
	store = newStateStore(filepath.Join(t.TempDir(), "state.json"))
	first, err := store.createNode("first", "http://127.0.0.1:7951", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.createNode("second", "http://127.0.0.1:7952", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	authProxyMu.Lock()
	authProxyCache = map[string]string{"auth-a": first.ProxyURL, "auth-b": second.ProxyURL}
	authProxyAt = time.Now()
	authProxyMu.Unlock()
	rawRequest, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "xai",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "auth-a", Provider: "xai"},
			{ID: "auth-b", Provider: "xai"},
		},
	})
	raw, err := handleSchedulerPick(rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("scheduler envelope=%s err=%v", raw, err)
	}
	var response pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	if response.Handled || response.AuthID != "" {
		t.Fatalf("healthy managed auths must leave scheduling to CPA: %+v", response)
	}
}

func TestSchedulerKeepsHostOrderWhenFilteringQuarantine(t *testing.T) {
	prevStore := store
	t.Cleanup(func() {
		store = prevStore
		authProxyMu.Lock()
		authProxyCache = nil
		authProxyAt = time.Time{}
		authProxyMu.Unlock()
	})
	store = newStateStore(filepath.Join(t.TempDir(), "state.json"))
	first, err := store.createNode("first", "http://127.0.0.1:7951", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.createNode("second", "http://127.0.0.1:7952", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	third, err := store.createNode("third", "http://127.0.0.1:7953", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.updateNode(first.ID, func(node *nodeRecord) error { node.DisabledByGuard = true; return nil }); err != nil {
		t.Fatal(err)
	}
	authProxyMu.Lock()
	authProxyCache = map[string]string{
		"auth-a": first.ProxyURL,
		"auth-b": second.ProxyURL,
		"auth-c": third.ProxyURL,
	}
	authProxyAt = time.Now()
	authProxyMu.Unlock()
	rawRequest, _ := json.Marshal(pluginapi.SchedulerPickRequest{
		Provider: "xai",
		Candidates: []pluginapi.SchedulerAuthCandidate{
			{ID: "auth-a", Provider: "xai"},
			{ID: "auth-b", Provider: "xai"},
			{ID: "auth-c", Provider: "xai"},
		},
	})
	raw, err := handleSchedulerPick(rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("scheduler envelope=%s err=%v", raw, err)
	}
	var response pluginapi.SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		t.Fatal(err)
	}
	if !response.Handled || response.AuthID != "auth-b" {
		t.Fatalf("filtered pick must keep host order, got %+v", response)
	}
}

func TestRequestInterceptorRejectsQuarantinedAuth(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "state.json"))
	node, err := store.createNode("bad", "http://127.0.0.1:7951", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.updateNode(node.ID, func(value *nodeRecord) error { value.DisabledByGuard = true; return nil }); err != nil {
		t.Fatal(err)
	}
	authProxyMu.Lock()
	authProxyCache = map[string]string{"auth-bad": node.ProxyURL}
	authProxyAt = time.Now()
	authProxyMu.Unlock()
	rawRequest, _ := json.Marshal(pluginapi.RequestInterceptRequest{Metadata: map[string]any{"selected_auth_id": "auth-bad"}})
	raw, err := handleRequestIntercept(rawRequest, true)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	var response pluginapi.RequestInterceptResponse
	_ = json.Unmarshal(env.Result, &response)
	if !response.Terminate || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("interceptor response=%+v", response)
	}
}

func TestClassifyTPS(t *testing.T) {
	if classifyTPS(1200, 500, 1000) != "hard" {
		t.Fatal("expected hard")
	}
	if classifyTPS(600, 500, 1000) != "soft" {
		t.Fatal("expected soft")
	}
	if classifyTPS(100, 500, 1000) != "healthy" {
		t.Fatal("expected healthy")
	}
}

func TestStoreNodeCRUD(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := newStateStore(path)
	n, err := s.createNode("ch-1", "http://127.0.0.1:7951", true, false, 200)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID == "" || n.ProxyURL == "" {
		t.Fatalf("bad node %#v", n)
	}
	pub := publicNode(n)
	if _, ok := pub["proxy_url"]; ok {
		t.Fatal("public node must not expose proxy_url")
	}
	if pub["hasProxy"] != true {
		t.Fatal("hasProxy")
	}
	list := s.listNodes()
	if len(list) != 1 {
		t.Fatalf("len=%d", len(list))
	}
	// reload
	s2 := newStateStore(path)
	n2, ok := s2.getNode(n.ID)
	if !ok || n2.ProxyURL != "http://127.0.0.1:7951" {
		t.Fatalf("reload failed %#v", n2)
	}
	_ = s2.deleteNodes([]string{n.ID})
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureAcceptsStoreInstallYAMLWithoutHostAuth(t *testing.T) {
	// Mirrors the YAML CPA passes on plugin.register after a store install:
	// enabled + store manifest + optional plugin fields. Must succeed without
	// any host.auth.* callbacks so the plugin becomes 已注册/生效中 immediately.
	prevStore := store
	prevCancel := workerCancel
	prevHost := hostCall
	t.Cleanup(func() {
		if workerCancel != nil {
			workerCancel()
		}
		workerCancel = prevCancel
		store = prevStore
		hostCall = prevHost
	})
	hostCall = func(method string, payload []byte) (json.RawMessage, error) {
		t.Fatalf("configure must not call host during register, got %s", method)
		return nil, fmt.Errorf("unexpected host call %s", method)
	}

	statePath := filepath.Join(t.TempDir(), "egress-guard", "state.json")
	configYAML := []byte(fmt.Sprintf(`
enabled: true
priority: 0
state_file: %s
store:
  schema-version: 1
  id: grok2api-egress
  version: 1.0.9
  release-tag: v1.0.9
  repository: https://github.com/lij768423-svg/grok2api-egress-enhancements
  install:
    type: github-release
`, statePath))
	lifecycle, err := json.Marshal(lifecycleRequest{ConfigYAML: configYAML})
	if err != nil {
		t.Fatal(err)
	}
	if err := configure(lifecycle); err != nil {
		t.Fatalf("configure(store install yaml): %v", err)
	}
	raw, err := handleMethod(pluginabi.MethodPluginRegister, lifecycle)
	if err != nil {
		t.Fatalf("plugin.register: %v", err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("register envelope ok=%v err=%v raw=%s", env.OK, err, raw)
	}
	var reg registration
	if err := json.Unmarshal(env.Result, &reg); err != nil {
		t.Fatal(err)
	}
	if reg.Metadata.Name != pluginName || reg.Metadata.Version != pluginVersion {
		t.Fatalf("metadata=%+v", reg.Metadata)
	}
	if !reg.Capabilities.ManagementAPI || !reg.Capabilities.Scheduler {
		t.Fatalf("capabilities=%+v", reg.Capabilities)
	}
	cfg, _ := currentConfig.Load().(pluginConfig)
	if cfg.StateFile != statePath {
		t.Fatalf("state_file=%q want %q", cfg.StateFile, statePath)
	}
}

func TestConfigureFallsBackWhenYAMLIsGarbage(t *testing.T) {
	prevStore := store
	prevCancel := workerCancel
	t.Cleanup(func() {
		if workerCancel != nil {
			workerCancel()
		}
		workerCancel = prevCancel
		store = prevStore
	})
	lifecycle, _ := json.Marshal(lifecycleRequest{ConfigYAML: []byte(":\n  - not: valid")})
	if err := configure(lifecycle); err != nil {
		t.Fatalf("configure must tolerate bad yaml: %v", err)
	}
	if store == nil {
		t.Fatal("store must still be initialized")
	}
}

func TestResolveDefaultStateFileIsWritable(t *testing.T) {
	path := resolveDefaultStateFile()
	if path == "" {
		t.Fatal("empty state path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "probe-write")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatalf("resolved path not writable: %v", err)
	}
	_ = os.Remove(probe)
}

func TestStoreCreateNodesIsAllOrNothing(t *testing.T) {
	s := newStateStore(filepath.Join(t.TempDir(), "state.json"))
	created, err := s.createNodes([]nodeCreateInput{
		{Name: "a", ProxyURL: "http://127.0.0.1:7951", Enabled: true, AccountCapacity: 100},
		{Name: "b", ProxyURL: "http://127.0.0.1:7952", Enabled: true, ProxyPool: true, AccountCapacity: 120},
	})
	if err != nil || len(created) != 2 || len(s.listNodes()) != 2 {
		t.Fatalf("created=%d nodes=%d err=%v", len(created), len(s.listNodes()), err)
	}
	if _, err := s.createNodes([]nodeCreateInput{
		{Name: "valid", ProxyURL: "http://127.0.0.1:7953", Enabled: true},
		{Name: "invalid", ProxyURL: "", Enabled: true},
	}); err == nil {
		t.Fatal("expected invalid import to fail")
	}
	if len(s.listNodes()) != 2 {
		t.Fatal("invalid batch must not create partial nodes")
	}
}

func TestRenderStatusPage(t *testing.T) {
	page := strings.Replace(pageTemplate, "/*__HALLMARK_TOKENS__*/", tokenCSS, 1)
	for _, want := range []string{"出口守护", "纯 CPA", "data-batch=\"enable\"", "重平衡账号", "批量添加", "/nodes/import", "页面每 15 秒刷新", "最短生成窗口", "X-Grok2API-Egress-UI"} {
		if !strings.Contains(page, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(page, "/*__HALLMARK_TOKENS__*/") {
		t.Fatal("tokens not replaced in test helper path only")
	}
}

func TestUIProxyRejectsMissingHeader(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	body, _ := json.Marshal(uiProxyRequest{Method: http.MethodGet, Path: "/nodes"})
	raw, err := handleUIProxy(managementRequest{Method: http.MethodPost, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"StatusCode":403`) {
		t.Fatalf("got %s", raw)
	}
}

func TestDispatchNodesList(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	_, _ = store.createNode("a", "http://127.0.0.1:1", true, false, 0)
	headers := make(http.Header)
	headers.Set("X-Grok2API-Egress-UI", "1")
	body, _ := json.Marshal(uiProxyRequest{Method: http.MethodGet, Path: "/nodes"})
	raw, err := handleUIProxy(managementRequest{Method: http.MethodPost, Headers: headers, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	var resp managementResponse
	_ = json.Unmarshal(env.Result, &resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Body)
	}
	if !strings.Contains(string(resp.Body), `"name":"a"`) {
		t.Fatalf("body %s", resp.Body)
	}
}

func TestDispatchNodesImportRedactsProxyURLs(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	headers := make(http.Header)
	headers.Set("X-Grok2API-Egress-UI", "1")
	requestBody, _ := json.Marshal(map[string]any{
		"items": []map[string]any{
			{"name": "fixed-a", "proxyURL": "http://user:pass@127.0.0.1:7951", "accountCapacity": 100},
			{"proxy_url": "http://user:pass@127.0.0.1:7952", "proxy_pool": true},
		},
	})
	body, _ := json.Marshal(uiProxyRequest{Method: http.MethodPost, Path: "/nodes/import", Body: requestBody})
	raw, err := handleUIProxy(managementRequest{Method: http.MethodPost, Headers: headers, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	var resp managementResponse
	_ = json.Unmarshal(env.Result, &resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), `"created":2`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, resp.Body)
	}
	if strings.Contains(string(resp.Body), "user:pass") || strings.Contains(string(resp.Body), "proxy_url") {
		t.Fatalf("response leaked proxy URL: %s", resp.Body)
	}
	if len(store.listNodes()) != 2 {
		t.Fatalf("node count=%d", len(store.listNodes()))
	}
}

func TestAuthListCacheAvoidsRepeatedHostGets(t *testing.T) {
	invalidateAuthListCache()
	calls := map[string]int{}
	auths := map[string]map[string]any{
		"a.json": {"type": "xai", "email": "a@example.test", "access_token": "t", "proxy_url": "http://127.0.0.1:1", "disabled": false},
		"b.json": {"type": "xai", "email": "b@example.test", "access_token": "t", "proxy_url": "http://127.0.0.1:2", "disabled": false},
	}
	original := hostCall
	hostCall = func(method string, payload []byte) (json.RawMessage, error) {
		calls[method]++
		switch method {
		case pluginabi.MethodHostAuthList:
			entries := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
			for name, raw := range auths {
				disabled, _ := raw["disabled"].(bool)
				entries = append(entries, pluginapi.HostAuthFileEntry{ID: name, AuthIndex: name, Name: name, Provider: "xai", Type: "xai", Disabled: disabled})
			}
			return json.Marshal(hostAuthListResponse{Files: entries})
		case pluginabi.MethodHostAuthGet:
			var request map[string]string
			_ = json.Unmarshal(payload, &request)
			name := request["auth_index"]
			if name == "" {
				name = request["name"]
			}
			raw, ok := auths[name]
			if !ok {
				return nil, fmt.Errorf("missing %s", name)
			}
			body, _ := json.Marshal(raw)
			return json.Marshal(hostAuthGetResponse{AuthIndex: name, Name: name, Path: "/auths/" + name, JSON: body})
		case pluginabi.MethodHostAuthSave:
			var request struct {
				Name string          `json:"name"`
				JSON json.RawMessage `json:"json"`
			}
			if err := json.Unmarshal(payload, &request); err != nil {
				return nil, err
			}
			updated := map[string]any{}
			if err := json.Unmarshal(request.JSON, &updated); err != nil {
				return nil, err
			}
			auths[request.Name] = updated
			return json.Marshal(pluginapi.HostAuthSaveResponse{Name: request.Name, Path: "/auths/" + request.Name})
		default:
			return nil, fmt.Errorf("unexpected %s", method)
		}
	}
	defer func() {
		hostCall = original
		invalidateAuthListCache()
		authProxyMu.Lock()
		authProxyCache = nil
		authProxyAt = time.Time{}
		authProxyMu.Unlock()
	}()

	first, err := listAuthFiles()
	if err != nil || len(first) != 2 {
		t.Fatalf("first list: n=%d err=%v", len(first), err)
	}
	if calls[pluginabi.MethodHostAuthList] != 1 || calls[pluginabi.MethodHostAuthGet] != 2 {
		t.Fatalf("cold list host calls list=%d get=%d, want 1/2", calls[pluginabi.MethodHostAuthList], calls[pluginabi.MethodHostAuthGet])
	}
	for i := 0; i < 5; i++ {
		if _, err := listAuthFiles(); err != nil {
			t.Fatal(err)
		}
	}
	if calls[pluginabi.MethodHostAuthList] != 1 || calls[pluginabi.MethodHostAuthGet] != 2 {
		t.Fatalf("warm path re-hit host: list=%d get=%d", calls[pluginabi.MethodHostAuthList], calls[pluginabi.MethodHostAuthGet])
	}
	if err := saveAuthFile("a.json", map[string]any{
		"type": "xai", "email": "a@example.test", "access_token": "t", "proxy_url": "http://127.0.0.1:9", "disabled": false,
	}); err != nil {
		t.Fatal(err)
	}
	// patched cache must reflect new proxy without another full list/get sweep
	got, err := listAuthFiles()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range got {
		if a.Name == "a.json" && a.ProxyURL == "http://127.0.0.1:9" {
			found = true
		}
	}
	if !found {
		t.Fatal("cache was not patched after save")
	}
	if calls[pluginabi.MethodHostAuthList] != 1 || calls[pluginabi.MethodHostAuthGet] != 2 {
		t.Fatalf("save+list triggered refetch list=%d get=%d", calls[pluginabi.MethodHostAuthList], calls[pluginabi.MethodHostAuthGet])
	}
}

func TestDebouncedPersistCoalescesStats(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := newStateStore(path)
	s.flushDelay = 50 * time.Millisecond
	for i := 0; i < 20; i++ {
		s.bumpStat("passive", "healthy", 10)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st guardState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Stats.Passive.Total != 20 {
		t.Fatalf("passive total=%d want 20", st.Stats.Passive.Total)
	}
}

func TestMatchExpectedModes(t *testing.T) {
	text := "天空是蓝的，因为瑞利散射。\nQUALITY_OK\n"
	if !matchExpected(text, "QUALITY_OK", matchLastLine) {
		t.Fatal("last line QUALITY_OK should match")
	}
	if matchExpected("hello\nNOT_OK", "QUALITY_OK", matchLastLine) {
		t.Fatal("wrong last line must not match")
	}
	if !matchExpected("prefix QUALITY_OK suffix", "QUALITY_OK", matchContains) {
		t.Fatal("contains should match")
	}
	if !matchExpected("done\nstatus=QUALITY_OK", "QUALITY_OK", matchLastLine) {
		t.Fatal("last line containing the marker should match")
	}
	if !matchExpected("alpha\nbeta QUALITY_OK", `QUALITY_OK$`, matchRegex) {
		t.Fatal("regex should match")
	}
	if matchExpected("nope", "[", matchRegex) {
		t.Fatal("invalid regex must not match")
	}
	if !matchExpected("anything", "", matchContains) {
		t.Fatal("empty expected is always a match")
	}
}

func TestClassifyWithProfileMarkerMissIsHard(t *testing.T) {
	pol := defaultPolicy()
	profile := ProbeProfile{ID: profileQualityMarker, ExpectedText: "QUALITY_OK", MatchMode: matchLastLine}
	res := classifyWithProfile(qualityResult{TPS: 12, OutputTokens: 8, ExpectedMatched: false}, profile, pol)
	if res.Classification != "hard" || res.ErrorKind != reasonMarkerMissing {
		t.Fatalf("marker miss=%+v", res)
	}
	shortOK := classifyWithProfile(qualityResult{TPS: 8000, OutputTokens: 4, ExpectedMatched: true}, profile, pol)
	if shortOK.Classification != "healthy" {
		t.Fatalf("short marker hit should be healthy, got %q", shortOK.Classification)
	}
	longHard := classifyWithProfile(qualityResult{TPS: 2000, OutputTokens: pol.MinOutputTokens, ExpectedMatched: true}, profile, pol)
	if longHard.Classification != "hard" {
		t.Fatalf("long marker hit with hard TPS=%q", longHard.Classification)
	}
}

func TestBuiltinProfilesSeededAndCustomCRUD(t *testing.T) {
	store := newStateStore(filepath.Join(t.TempDir(), "state.json"))
	items := store.listProfiles()
	if len(items) < 2 {
		t.Fatalf("builtins=%d, want >= 2", len(items))
	}
	got := store.resolveProfile("")
	if got.ID != profileThroughput {
		t.Fatalf("default profile=%s", got.ID)
	}
	created, err := store.createProfile(ProbeProfile{
		Name: "自定义标记", Prompt: "只输出 FLAG_OK", ExpectedText: "FLAG_OK", MatchMode: matchLastLine,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.BuiltIn {
		t.Fatalf("created=%+v", created)
	}
	pol := store.policy()
	pol.ActiveProfileID = created.ID
	if err := store.updatePolicy(pol); err != nil {
		t.Fatal(err)
	}
	if store.resolveProfile("").ID != created.ID {
		t.Fatal("active profile not resolved")
	}
	if _, err := store.updateProfile(profileQualityMarker, ProbeProfile{Name: "x", Prompt: "y"}); err == nil {
		t.Fatal("built-in update must fail")
	}
	if err := store.deleteProfile(profileThroughput); err == nil {
		t.Fatal("built-in delete must fail")
	}
	if err := store.deleteProfile(created.ID); err != nil {
		t.Fatal(err)
	}
	if store.policy().ActiveProfileID != profileThroughput {
		t.Fatalf("delete active should fall back, got %s", store.policy().ActiveProfileID)
	}
}

func TestFallbackNodeIDForUnboundAuth(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	only, err := store.createNode("only", "socks5h://u:p@127.0.0.1:1080", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := fallbackNodeIDForUnboundAuth(store); got != only.ID {
		t.Fatalf("single node fallback=%q want %q", got, only.ID)
	}
	if _, err := store.createNode("two", "socks5h://u:p@127.0.0.1:1081", true, false, 0); err != nil {
		t.Fatal(err)
	}
	if got := fallbackNodeIDForUnboundAuth(store); got != "" {
		t.Fatalf("two enabled nodes must stay unmapped, got %q", got)
	}
}

func TestUnboundPassiveUsageMapsToSingleNode(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	node, err := store.createNode("ipv6", "socks5h://u:p@146.56.100.15:1080", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	invalidateAuthListCache()
	invalidateAuthProxyCache()
	handlePassiveUsage(store, map[string]any{
		"Provider": "xai",
		"AuthID":   "xai-user.json",
		"Detail":   map[string]any{"OutputTokens": int64(80), "ReasoningTokens": int64(20)},
		"Latency":  float64(2500 * 1e6),
		"TTFT":     float64(400 * 1e6),
	})
	got, ok := store.getNode(node.ID)
	if !ok || got.LastClassification != "healthy" || got.LastSource != "passive" {
		t.Fatalf("unbound usage node=%+v", got)
	}
}

func TestIgnoredObservationDoesNotClearHealthy(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	node, err := store.createNode("ipv6", "socks5h://u:p@146.56.100.15:1080", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.updateNode(node.ID, func(n *nodeRecord) error {
		n.LastClassification = "healthy"
		n.LastOutputTPS = 34
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	applyObservation(store, node.ID, "active", qualityResult{
		Classification: "ignored",
		ErrorKind:      "no_account",
		Error:          "没有可用的 CPA xAI 账号",
	})
	got, _ := store.getNode(node.ID)
	if got.LastClassification != "healthy" {
		t.Fatalf("ignored overwrote class=%q", got.LastClassification)
	}
	if got.LastReason != "没有可用的 CPA xAI 账号" {
		t.Fatalf("reason=%q", got.LastReason)
	}
}

func TestRenderStatusPageShowsConnectivityBadge(t *testing.T) {
	html := renderPageHTML()
	if !strings.Contains(html, "连通正常") {
		t.Fatal("missing connectivity badge")
	}
	if !strings.Contains(html, "一键绑定账号") || !strings.Contains(html, "/assign") {
		t.Fatal("missing bind-count UI")
	}
	scriptStart := strings.Index(html, "(() => {")
	scriptEnd := strings.LastIndex(html, "})();")
	if scriptStart < 0 || scriptEnd <= scriptStart {
		t.Fatal("page script wrapper missing")
	}
	script := html[scriptStart : scriptEnd+len("})();")]
	if strings.Count(script, "{") != strings.Count(script, "}") {
		t.Fatalf("page script brace mismatch {=%d }=%d", strings.Count(script, "{"), strings.Count(script, "}"))
	}
}

func TestAssignAuthsToNodeBindsRequestedCount(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	node, err := store.createNode("ipv6", "socks5h://u:p@146.56.100.15:1080", true, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	auths := map[string]map[string]any{
		"a.json": {"type": "xai", "email": "a@example.test", "access_token": "t", "disabled": false},
		"b.json": {"type": "xai", "email": "b@example.test", "access_token": "t", "disabled": false},
		"c.json": {"type": "xai", "email": "c@example.test", "access_token": "t", "disabled": false},
		"d.json": {"type": "xai", "email": "d@example.test", "access_token": "t", "disabled": true},
	}
	original := hostCall
	hostCall = func(method string, payload []byte) (json.RawMessage, error) {
		switch method {
		case pluginabi.MethodHostAuthList:
			entries := make([]pluginapi.HostAuthFileEntry, 0, len(auths))
			for name, raw := range auths {
				disabled, _ := raw["disabled"].(bool)
				entries = append(entries, pluginapi.HostAuthFileEntry{ID: name, AuthIndex: name, Name: name, Provider: "xai", Type: "xai", Disabled: disabled})
			}
			return json.Marshal(hostAuthListResponse{Files: entries})
		case pluginabi.MethodHostAuthGet:
			var request map[string]string
			_ = json.Unmarshal(payload, &request)
			name := request["auth_index"]
			if name == "" {
				name = request["name"]
			}
			raw, ok := auths[name]
			if !ok {
				return nil, fmt.Errorf("missing %s", name)
			}
			body, _ := json.Marshal(raw)
			return json.Marshal(hostAuthGetResponse{AuthIndex: name, Name: name, Path: "/auths/" + name, JSON: body})
		case pluginabi.MethodHostAuthSave:
			var request struct {
				Name string          `json:"name"`
				JSON json.RawMessage `json:"json"`
			}
			if err := json.Unmarshal(payload, &request); err != nil {
				return nil, err
			}
			updated := map[string]any{}
			if err := json.Unmarshal(request.JSON, &updated); err != nil {
				return nil, err
			}
			auths[request.Name] = updated
			return json.Marshal(pluginapi.HostAuthSaveResponse{Name: request.Name, Path: "/auths/" + request.Name})
		default:
			return nil, fmt.Errorf("unexpected %s", method)
		}
	}
	t.Cleanup(func() {
		hostCall = original
		invalidateAuthListCache()
		invalidateAuthProxyCache()
	})
	invalidateAuthListCache()

	got, err := assignAuthsToNode(store, node.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bound != 2 || got.Added != 2 {
		t.Fatalf("assign=%+v", got)
	}
	bound := 0
	for _, raw := range auths {
		if disabled, _ := raw["disabled"].(bool); disabled {
			if raw["proxy_url"] != nil {
				t.Fatal("disabled auth must stay unbound")
			}
			continue
		}
		if raw["proxy_url"] == node.ProxyURL {
			bound++
		}
	}
	if bound != 2 {
		t.Fatalf("bound files=%d", bound)
	}

	got, err = assignAuthsToNode(store, node.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bound != 0 || got.Removed != 2 {
		t.Fatalf("unbind all=%+v", got)
	}
}
func TestNormalizeBareIPv6ProxyURL(t *testing.T) {
	got, err := normalizeProxyURL("socks5h://user:pass@2001:470:ef3f::1:1080")
	if err != nil {
		t.Fatal(err)
	}
	want := "socks5h://user:pass@[2001:470:ef3f::1]:1080"
	if got != want {
		t.Fatalf("normalized=%q want %q", got, want)
	}
	if err := validateProxyURL(got); err != nil {
		t.Fatal(err)
	}
	if err := validateProxyURL("socks5h://emptysuns:emptysuns@111.119.237.235:1080"); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPClientAcceptsSOCKS5H(t *testing.T) {
	for _, raw := range []string{
		"socks5h://user:pass@111.119.237.235:1080",
		"socks5h://user:pass@2001:470:ef3f::1:1080",
		"socks5h://user:pass@[2001:470:ef3f::1]:1080",
	} {
		client, err := httpClientThroughProxy(raw, time.Second)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if client == nil || client.Transport == nil {
			t.Fatalf("%s: empty client", raw)
		}
	}
}

func TestProbeConnectivityFallsBackToIPv6Endpoint(t *testing.T) {
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no ipv4", http.StatusBadGateway)
	}))
	defer fail.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("2001:470:ef3f::1"))
	}))
	defer ok.Close()

	prev := connectivityProbeURLs
	connectivityProbeURLs = []string{fail.URL, ok.URL}
	t.Cleanup(func() { connectivityProbeURLs = prev })

	ip, _, err := probeConnectivity("")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "2001:470:ef3f::1" {
		t.Fatalf("exit ip=%q", ip)
	}
}

func TestAuthListFilePathAvoidsHostGet(t *testing.T) {
	invalidateAuthListCache()
	dir := t.TempDir()
	path := filepath.Join(dir, "xai-a.json")
	body := []byte(`{"type":"xai","email":"a@example.test","access_token":"t","proxy_url":"socks5h://user:pass@111.119.237.235:1080","disabled":false}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	original := hostCall
	hostCall = func(method string, payload []byte) (json.RawMessage, error) {
		calls[method]++
		if method != pluginabi.MethodHostAuthList {
			return nil, fmt.Errorf("unexpected %s", method)
		}
		return json.Marshal(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{
			ID: "a.json", AuthIndex: "a.json", Name: "a.json", Path: path, Provider: "xai", Type: "xai",
		}}})
	}
	t.Cleanup(func() {
		hostCall = original
		invalidateAuthListCache()
	})
	got, err := listAuthFiles()
	if err != nil || len(got) != 1 {
		t.Fatalf("list n=%d err=%v", len(got), err)
	}
	if got[0].ProxyURL != "socks5h://user:pass@111.119.237.235:1080" {
		t.Fatalf("proxy=%q", got[0].ProxyURL)
	}
	if calls[pluginabi.MethodHostAuthGet] != 0 {
		t.Fatalf("file-backed list must not host.auth.get, got %d", calls[pluginabi.MethodHostAuthGet])
	}
}

func TestDispatchNodesListDoesNotSweepAuth(t *testing.T) {
	store = newStateStore(filepath.Join(t.TempDir(), "s.json"))
	_, _ = store.createNode("a", "socks5h://user:pass@111.119.237.235:1080", true, false, 0)
	original := hostCall
	hostCall = func(method string, payload []byte) (json.RawMessage, error) {
		t.Fatalf("GET /nodes must not call host, got %s", method)
		return nil, fmt.Errorf("unexpected %s", method)
	}
	t.Cleanup(func() { hostCall = original })
	headers := make(http.Header)
	headers.Set("X-Grok2API-Egress-UI", "1")
	body, _ := json.Marshal(uiProxyRequest{Method: http.MethodGet, Path: "/nodes"})
	raw, err := handleUIProxy(managementRequest{Method: http.MethodPost, Headers: headers, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	_ = json.Unmarshal(raw, &env)
	var resp managementResponse
	_ = json.Unmarshal(env.Result, &resp)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, resp.Body)
	}
}
