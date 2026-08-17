package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

type authFile struct {
	ID       string
	Index    string
	Name     string
	Path     string
	Email    string
	Disabled bool
	ProxyURL string
	Raw      map[string]any
}

// auth list cache: avoids the host.auth.list + N×host.auth.get stampede on
// every scheduler / usage / worker tick. Mutations patch the cache in place;
// a 60s TTL still picks up external CPA auth changes.
const authListCacheTTL = 60 * time.Second

var (
	authListMu      sync.Mutex
	authListCache   []authFile
	authListAt      time.Time
	authListLoading bool
	authListWait    = sync.NewCond(&authListMu)
)

func invalidateAuthListCache() {
	authListMu.Lock()
	authListCache = nil
	authListAt = time.Time{}
	authListMu.Unlock()
}

func cloneAuthFiles(in []authFile) []authFile {
	if in == nil {
		return nil
	}
	out := make([]authFile, len(in))
	copy(out, in)
	return out
}

// listAuthFiles returns xAI auth files, preferring a warm cache.
func listAuthFiles() ([]authFile, error) {
	return listAuthFilesCached(false)
}

// listAuthFilesFresh bypasses TTL (management UI, rebalance entry).
func listAuthFilesFresh() ([]authFile, error) {
	return listAuthFilesCached(true)
}

func listAuthFilesCached(force bool) ([]authFile, error) {
	authListMu.Lock()
	for authListLoading {
		authListWait.Wait()
	}
	if !force && authListCache != nil && time.Since(authListAt) < authListCacheTTL {
		out := cloneAuthFiles(authListCache)
		authListMu.Unlock()
		return out, nil
	}
	authListLoading = true
	authListMu.Unlock()

	loaded, err := fetchAuthFilesFromHost()

	authListMu.Lock()
	authListLoading = false
	if err == nil {
		authListCache = loaded
		authListAt = time.Now()
	}
	out := cloneAuthFiles(authListCache)
	authListWait.Broadcast()
	authListMu.Unlock()
	if err != nil {
		if out != nil {
			// serve stale on transient host errors to keep hot path alive
			return out, nil
		}
		return nil, err
	}
	return out, nil
}

func fetchAuthFilesFromHost() ([]authFile, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthList, mustJSON(map[string]any{}))
	if err != nil {
		return nil, err
	}
	var resp hostAuthListResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		var files []pluginapi.HostAuthFileEntry
		if err2 := json.Unmarshal(raw, &files); err2 != nil {
			return nil, fmt.Errorf("decode auth list: %w", err)
		}
		resp.Files = files
	}
	// 10k+ hosts: never N+1 host.auth.get on first paint / reconcile.
	index := map[string]string{}
	if store != nil {
		index = store.authProxyIndexSnapshot()
	}
	out := make([]authFile, 0, len(resp.Files))
	indexOut := map[string]string{}
	for _, f := range resp.Files {
		a, ok := authFileFromListEntry(f, index)
		if !ok {
			continue
		}
		out = append(out, a)
		if a.ProxyURL == "" {
			continue
		}
		for _, key := range authIndexKeys(a) {
			indexOut[key] = a.ProxyURL
		}
	}
	if store != nil {
		store.replaceAuthProxyIndex(indexOut)
	}
	return out, nil
}

func isXAIAuthEntry(f pluginapi.HostAuthFileEntry) bool {
	prov := strings.ToLower(strings.TrimSpace(f.Provider + " " + f.Type + " " + f.Name))
	if prov == "" {
		return true
	}
	return strings.Contains(prov, "xai") || strings.Contains(prov, "grok")
}

func authFileFromListEntry(f pluginapi.HostAuthFileEntry, index map[string]string) (authFile, bool) {
	idx := strings.TrimSpace(f.AuthIndex)
	if idx == "" {
		idx = strings.TrimSpace(f.ID)
	}
	if idx == "" {
		idx = strings.TrimSpace(f.Name)
	}
	if idx == "" {
		return authFile{}, false
	}
	if !isXAIAuthEntry(f) {
		return authFile{}, false
	}
	name := strings.TrimSpace(f.Name)
	if name == "" {
		name = filepath.Base(strings.TrimSpace(f.Path))
	}
	email := strings.TrimSpace(f.Email)
	if name == "" && email != "" {
		name = "xai-" + email + ".json"
	}
	a := authFile{
		ID:       strings.TrimSpace(f.ID),
		Index:    idx,
		Name:     name,
		Path:     strings.TrimSpace(f.Path),
		Email:    email,
		Disabled: f.Disabled,
		Raw:      map[string]any{"type": "xai", "disabled": f.Disabled},
	}
	if email != "" {
		a.Raw["email"] = email
	}
	if disk, ok := readAuthJSONOnDisk(a.Path); ok {
		a.Raw = disk
		if proxy, _ := disk["proxy_url"].(string); strings.TrimSpace(proxy) != "" {
			a.ProxyURL = strings.TrimSpace(proxy)
		}
		if diskEmail, _ := disk["email"].(string); diskEmail != "" {
			a.Email = diskEmail
		}
		if disabled, exists := disk["disabled"].(bool); exists {
			a.Disabled = disabled
		}
	} else if proxy := lookupAuthProxy(index, a); proxy != "" {
		a.ProxyURL = proxy
	} else if strings.TrimSpace(a.Path) == "" {
		// Memory-only / test fixtures have no path; a single get is required.
		// File-backed production auths never take this branch.
		if got, err := getAuthFile(idx); err == nil {
			if got.ID == "" {
				got.ID = a.ID
			}
			if got.Index == "" {
				got.Index = a.Index
			}
			return got, true
		}
	}
	if a.ProxyURL != "" {
		a.Raw["proxy_url"] = a.ProxyURL
	}
	return a, true
}

func readAuthJSONOnDisk(path string) (map[string]any, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

func lookupAuthProxy(index map[string]string, a authFile) string {
	if len(index) == 0 {
		return ""
	}
	for _, key := range authIndexKeys(a) {
		if proxy := strings.TrimSpace(index[key]); proxy != "" {
			return proxy
		}
	}
	return ""
}

func authIndexKeys(a authFile) []string {
	out := make([]string, 0, 8)
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		out = append(out, v)
	}
	add(a.Index)
	add(a.ID)
	add(a.Name)
	if a.Name != "" {
		add(strings.TrimSuffix(a.Name, ".json"))
	}
	add(a.Email)
	if a.Email != "" {
		add("xai-" + a.Email + ".json")
	}
	add(a.Path)
	if a.Path != "" {
		add(filepath.Base(a.Path))
	}
	return out
}

// patchAuthListCacheAfterSave updates one entry after host.auth.save so migrate
// of hundreds of accounts does not re-trigger N+1 list/get.
func patchAuthListCacheAfterSave(name string, obj map[string]any) {
	if name == "" || obj == nil {
		return
	}
	proxy, _ := obj["proxy_url"].(string)
	proxy = strings.TrimSpace(proxy)
	disabled, _ := obj["disabled"].(bool)
	email, _ := obj["email"].(string)

	authListMu.Lock()
	var keys []string
	if authListCache != nil {
		for i := range authListCache {
			a := &authListCache[i]
			if a.Name != name && a.Index != name && a.ID != name {
				continue
			}
			a.ProxyURL = proxy
			a.Disabled = disabled
			a.Raw = obj
			if email != "" {
				a.Email = email
			}
			keys = authIndexKeys(*a)
			break
		}
	}
	authListMu.Unlock()
	if store != nil {
		if len(keys) == 0 {
			keys = []string{name}
		}
		store.patchAuthProxyKeys(keys, proxy)
	}
}

func getAuthFile(authIndex string) (authFile, error) {
	raw, err := hostCall(pluginabi.MethodHostAuthGet, mustJSON(map[string]any{"auth_index": authIndex}))
	if err != nil {
		raw, err = hostCall(pluginabi.MethodHostAuthGet, mustJSON(map[string]any{"name": authIndex}))
		if err != nil {
			return authFile{}, err
		}
	}
	var resp hostAuthGetResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return authFile{}, err
	}
	obj := map[string]any{}
	if len(resp.JSON) > 0 {
		_ = json.Unmarshal(resp.JSON, &obj)
	}
	email, _ := obj["email"].(string)
	proxy, _ := obj["proxy_url"].(string)
	disabled, _ := obj["disabled"].(bool)
	name := resp.Name
	if name == "" {
		name = filepath.Base(resp.Path)
	}
	if name == "" && email != "" {
		name = "xai-" + email + ".json"
	}
	idx := resp.AuthIndex
	if idx == "" {
		idx = authIndex
	}
	return authFile{
		Index:    idx,
		Name:     name,
		Path:     resp.Path,
		Email:    email,
		Disabled: disabled,
		ProxyURL: strings.TrimSpace(proxy),
		Raw:      obj,
	}, nil
}

func saveAuthFile(name string, obj map[string]any) error {
	if name == "" {
		return fmt.Errorf("auth name required")
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = hostCall(pluginabi.MethodHostAuthSave, mustJSON(map[string]any{
		"name": name,
		"json": json.RawMessage(raw),
	}))
	if err == nil {
		patchAuthListCacheAfterSave(name, obj)
		invalidateAuthProxyCache()
	}
	return err
}

func setAuthProxyAndFlags(a authFile, proxyURL string, disabled bool, reason string) error {
	full, err := hydrateAuthFile(a)
	if err != nil {
		return err
	}
	a = full
	if a.Raw == nil {
		a.Raw = map[string]any{}
	}
	if proxyURL == "" {
		delete(a.Raw, "proxy_url")
	} else {
		a.Raw["proxy_url"] = proxyURL
	}
	a.Raw["disabled"] = disabled
	if disabled && reason != "" {
		a.Raw["disabled_reason"] = reason
		a.Raw["disabled_at"] = nowRFC3339()
		if strings.Contains(reason, "账号降智") {
			a.Raw["note"] = "降智账号"
		}
	} else {
		delete(a.Raw, "disabled_reason")
		delete(a.Raw, "disabled_at")
	}
	if _, ok := a.Raw["type"]; !ok {
		a.Raw["type"] = "xai"
	}
	return saveAuthFile(a.Name, a.Raw)
}

func hydrateAuthFile(a authFile) (authFile, error) {
	if authRawHasSecret(a.Raw) {
		return a, nil
	}
	if disk, ok := readAuthJSONOnDisk(firstNonEmpty(a.Path, a.Name)); ok {
		a.Raw = disk
		if proxy, _ := disk["proxy_url"].(string); strings.TrimSpace(proxy) != "" {
			a.ProxyURL = strings.TrimSpace(proxy)
		}
		if email, _ := disk["email"].(string); email != "" {
			a.Email = email
		}
		if authRawHasSecret(a.Raw) {
			return a, nil
		}
	}
	key := firstNonEmpty(a.Index, a.Name, a.ID, filepath.Base(a.Path))
	if key == "" {
		return authFile{}, fmt.Errorf("账号缺少 auth index")
	}
	got, err := getAuthFile(key)
	if err != nil {
		return authFile{}, err
	}
	if got.ID == "" {
		got.ID = a.ID
	}
	return got, nil
}

func authRawHasSecret(raw map[string]any) bool {
	if raw == nil {
		return false
	}
	for _, key := range []string{"access_token", "refresh_token", "token", "sso"} {
		if v, _ := raw[key].(string); strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

func isGuardDisabledAuth(a authFile) bool {
	if !a.Disabled {
		return false
	}
	reason, _ := a.Raw["disabled_reason"].(string)
	return strings.Contains(reason, "egress-guard") || strings.Contains(reason, "降智")
}

func isAccountQualityDisabled(a authFile) bool {
	if !a.Disabled {
		return false
	}
	reason, _ := a.Raw["disabled_reason"].(string)
	return strings.Contains(reason, "账号降智")
}

func authMatchesKey(a authFile, key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	base := filepath.Base(key)
	for _, k := range authIndexKeys(a) {
		if k == key || k == base {
			return true
		}
	}
	return false
}

func preferAuths(auths []authFile, prefer string) []authFile {
	prefer = strings.TrimSpace(prefer)
	if prefer == "" || len(auths) < 2 {
		return auths
	}
	head := make([]authFile, 0, 1)
	rest := make([]authFile, 0, len(auths))
	for _, a := range auths {
		if authMatchesKey(a, prefer) {
			head = append(head, a)
			continue
		}
		rest = append(rest, a)
	}
	if len(head) == 0 {
		return auths
	}
	return append(head, rest...)
}

func disableAuthByID(authID, reason string) error {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return nil
	}
	a, ok := findAuthByID(authID)
	if !ok {
		return fmt.Errorf("账号不存在")
	}
	if a.Disabled {
		return nil
	}
	if strings.TrimSpace(reason) == "" {
		reason = "egress-guard 降智隔离"
	}
	return setAuthProxyAndFlags(a, a.ProxyURL, true, reason)
}

func findAuthByID(authID string) (authFile, bool) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return authFile{}, false
	}
	auths, err := listAuthFiles()
	if err != nil {
		return authFile{}, false
	}
	for _, a := range auths {
		if authMatchesKey(a, authID) {
			return a, true
		}
	}
	return authFile{}, false
}

func mergePreferredAuth(candidates []authFile, prefer string) []authFile {
	prefer = strings.TrimSpace(prefer)
	if prefer == "" {
		return candidates
	}
	if a, ok := findAuthByID(prefer); ok && !a.Disabled {
		if full, err := hydrateAuthFile(a); err == nil {
			tok, _ := full.Raw["access_token"].(string)
			if strings.TrimSpace(tok) != "" {
				return preferAuths(append([]authFile{full}, candidates...), prefer)
			}
		}
	}
	return preferAuths(candidates, prefer)
}

func cachedAuthDisabled(authID string) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	authListMu.Lock()
	warm := authListCache
	authListMu.Unlock()
	for _, a := range warm {
		if a.Disabled && authMatchesKey(a, authID) {
			return true
		}
	}
	return false
}

func verifyAuthBinding(a authFile, expectedProxy string, expectedDisabled bool) error {
	key := firstNonEmpty(a.Index, a.Name)
	if key == "" {
		return fmt.Errorf("账号缺少 auth index")
	}
	got, err := getAuthFile(key)
	if err == nil && got.ProxyURL == expectedProxy && got.Disabled == expectedDisabled {
		return nil
	}
	for _, candidate := range []string{a.Name, filepath.Base(a.Path), a.Index, a.ID} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || candidate == key {
			continue
		}
		got2, err2 := getAuthFile(candidate)
		if err2 == nil && got2.ProxyURL == expectedProxy && got2.Disabled == expectedDisabled {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("读取迁移结果失败: %w", err)
	}
	return fmt.Errorf("迁移结果未生效")
}

func listAuthFilesBestEffort() []authFile {
	items, err := listAuthFiles()
	if err != nil {
		return nil
	}
	return items
}

func rebalanceAuthsToNodes(store *stateStore) (map[string]int, error) {
	auths, err := listAuthFilesFresh()
	if err != nil {
		return nil, err
	}
	nodes := store.listNodes()
	eligible := make([]*nodeRecord, 0)
	for _, n := range nodes {
		if n.Enabled && !n.DisabledByGuard && n.ProxyURL != "" {
			eligible = append(eligible, n)
		}
	}
	counts := map[string]int{}
	if len(eligible) == 0 {
		store.setAssignedCounts(counts)
		return counts, fmt.Errorf("没有可调度出口节点")
	}
	active := make([]authFile, 0)
	for _, a := range auths {
		if a.Disabled {
			for _, n := range nodes {
				if a.ProxyURL != "" && a.ProxyURL == n.ProxyURL {
					counts[n.ID]++
				}
			}
			continue
		}
		active = append(active, a)
	}
	cursor := 0
	for _, a := range active {
		var chosen *nodeRecord
		for tried := 0; tried < len(eligible); tried++ {
			n := eligible[cursor%len(eligible)]
			cursor++
			cap := n.AccountCapacity
			if cap > 0 && counts[n.ID] >= cap {
				continue
			}
			chosen = n
			break
		}
		if chosen == nil {
			chosen = eligible[len(eligible)-1]
		}
		if a.ProxyURL == chosen.ProxyURL && !a.Disabled {
			counts[chosen.ID]++
			continue
		}
		if err := setAuthProxyAndFlags(a, chosen.ProxyURL, false, ""); err != nil {
			return counts, fmt.Errorf("绑定 %s 失败: %w", a.Name, err)
		}
		if err := verifyAuthBinding(a, chosen.ProxyURL, false); err != nil {
			return counts, fmt.Errorf("绑定 %s 校验失败: %w", a.Name, err)
		}
		counts[chosen.ID]++
	}
	store.setAssignedCounts(counts)
	return counts, nil
}

type assignResult struct {
	NodeID    string `json:"nodeId"`
	Requested int    `json:"requested"`
	Target    int    `json:"target"`
	Bound     int    `json:"bound"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
}

// assignAuthsToNode sets this node's enabled-account count to target.
// Extra bound accounts are unbound; missing slots take unbound first, then
// other nodes. Disabled accounts are never moved.
func assignAuthsToNode(store *stateStore, nodeID string, count int) (assignResult, error) {
	out := assignResult{NodeID: nodeID, Requested: count}
	if count < 0 {
		return out, fmt.Errorf("绑定数量不能为负数")
	}
	if count > 100000 {
		return out, fmt.Errorf("绑定数量不能超过 100000")
	}
	node, ok := store.getNode(nodeID)
	if !ok {
		return out, fmt.Errorf("节点不存在")
	}
	if strings.TrimSpace(node.ProxyURL) == "" {
		return out, fmt.Errorf("节点没有代理 URL")
	}
	if !node.Enabled {
		return out, fmt.Errorf("节点未启用")
	}
	if node.DisabledByGuard {
		return out, fmt.Errorf("节点已被隔离，不能绑定账号")
	}
	target := count
	if node.AccountCapacity > 0 && target > node.AccountCapacity {
		target = node.AccountCapacity
	}
	out.Target = target

	auths, err := listAuthFilesFresh()
	if err != nil {
		return out, err
	}
	onNode := make([]authFile, 0)
	unbound := make([]authFile, 0)
	other := make([]authFile, 0)
	for _, a := range auths {
		if a.Disabled {
			continue
		}
		if a.ProxyURL == node.ProxyURL {
			onNode = append(onNode, a)
			continue
		}
		if strings.TrimSpace(a.ProxyURL) == "" {
			unbound = append(unbound, a)
			continue
		}
		other = append(other, a)
	}

	for len(onNode) > target {
		a := onNode[len(onNode)-1]
		onNode = onNode[:len(onNode)-1]
		if err := setAuthProxyAndFlags(a, "", false, ""); err != nil {
			refreshAssignedCounts(store)
			return out, fmt.Errorf("解绑 %s 失败: %w", a.Name, err)
		}
		out.Removed++
	}
	take := func(src *[]authFile) error {
		for len(onNode) < target && len(*src) > 0 {
			a := (*src)[0]
			*src = (*src)[1:]
			if err := setAuthProxyAndFlags(a, node.ProxyURL, false, ""); err != nil {
				return fmt.Errorf("绑定 %s 失败: %w", a.Name, err)
			}
			if err := verifyAuthBinding(a, node.ProxyURL, false); err != nil {
				return fmt.Errorf("绑定 %s 校验失败: %w", a.Name, err)
			}
			onNode = append(onNode, a)
			out.Added++
		}
		return nil
	}
	if err := take(&unbound); err != nil {
		refreshAssignedCounts(store)
		out.Bound = len(onNode)
		return out, err
	}
	if err := take(&other); err != nil {
		refreshAssignedCounts(store)
		out.Bound = len(onNode)
		return out, err
	}
	refreshAssignedCounts(store)
	out.Bound = len(onNode)
	if out.Bound < target {
		return out, fmt.Errorf("可用账号不足：已绑定 %d / 目标 %d", out.Bound, target)
	}
	return out, nil
}

func refreshAssignedCounts(store *stateStore) {
	auths, err := listAuthFiles()
	if err != nil {
		return
	}
	nodes := store.listNodes()
	byProxy := map[string]string{}
	for _, n := range nodes {
		if n.ProxyURL != "" {
			byProxy[n.ProxyURL] = n.ID
		}
	}
	counts := map[string]int{}
	for _, a := range auths {
		if id, ok := byProxy[a.ProxyURL]; ok {
			counts[id]++
		}
	}
	store.setAssignedCounts(counts)
}

func disableAuthsOnNode(store *stateStore, node *nodeRecord, reason string) error {
	if node == nil || node.ProxyURL == "" {
		return nil
	}
	auths, err := listAuthFiles()
	if err != nil {
		return err
	}
	for _, a := range auths {
		if a.ProxyURL == node.ProxyURL && !a.Disabled {
			if err := setAuthProxyAndFlags(a, a.ProxyURL, true, reason); err != nil {
				return fmt.Errorf("停用 %s 失败: %w", a.Name, err)
			}
		}
	}
	return nil
}

func enableAuthsOnNode(node *nodeRecord) error {
	if node == nil || node.ProxyURL == "" {
		return nil
	}
	auths, err := listAuthFiles()
	if err != nil {
		return err
	}
	for _, a := range auths {
		if a.ProxyURL == node.ProxyURL && isGuardDisabledAuth(a) && !isAccountQualityDisabled(a) {
			_ = setAuthProxyAndFlags(a, a.ProxyURL, false, "")
		}
	}
	return nil
}

func pickAuthForNode(node *nodeRecord) (authFile, error) {
	list, err := listAuthsForNode(node, 1)
	if err != nil {
		return authFile{}, err
	}
	if len(list) == 0 {
		return authFile{}, fmt.Errorf("没有可用的 CPA xAI 账号")
	}
	return list[0], nil
}

// listAuthsForNode returns up to limit enabled xAI auths bound to the node proxy,
// preferring non-expired tokens. Falls back to any enabled xAI auth if none bound.
func listAuthsForNode(node *nodeRecord, limit int) ([]authFile, error) {
	if limit <= 0 {
		limit = 5
	}
	auths, err := listAuthFiles()
	if err != nil {
		return nil, err
	}
	var primary, fallback []authFile
	for _, a := range auths {
		if a.Disabled {
			continue
		}
		onNode := node != nil && node.ProxyURL != "" && a.ProxyURL == node.ProxyURL
		if onNode {
			primary = append(primary, a)
		} else {
			fallback = append(fallback, a)
		}
	}
	out := make([]authFile, 0, limit)
	appendHydrated := func(src []authFile) {
		for _, a := range src {
			if len(out) >= limit {
				return
			}
			full, err := hydrateAuthFile(a)
			if err != nil {
				continue
			}
			tok, _ := full.Raw["access_token"].(string)
			if strings.TrimSpace(tok) == "" {
				continue
			}
			if isAuthExpired(full) && len(out) > 0 {
				continue
			}
			out = append(out, full)
		}
	}
	appendHydrated(primary)
	if len(out) < limit {
		appendHydrated(fallback)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没有可用的 CPA xAI 账号")
	}
	return out, nil
}

// listBoundAuthSummaries returns lightweight account info for a node (no secrets).
func listBoundAuthSummaries(node *nodeRecord) ([]map[string]any, error) {
	auths, err := listAuthFilesFresh()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0)
	if node == nil || node.ProxyURL == "" {
		return out, nil
	}
	for _, a := range auths {
		if a.ProxyURL != node.ProxyURL {
			continue
		}
		out = append(out, map[string]any{
			"name":     a.Name,
			"email":    a.Email,
			"disabled": a.Disabled,
			"expired":  isAuthExpired(a),
		})
	}
	return out, nil
}

func verifiedMigrationTargets(store *stateStore, bad *nodeRecord) []*nodeRecord {
	if store == nil || bad == nil {
		return nil
	}
	pol := store.policy()
	freshness := time.Duration(pol.ActiveIntervalSec*2) * time.Second
	if freshness < time.Hour {
		freshness = time.Hour
	}
	cutoff := float64(time.Now().Add(-freshness).Unix())
	targets := make([]*nodeRecord, 0)
	for _, n := range store.listNodes() {
		if n.ID == bad.ID || !n.Enabled || n.DisabledByGuard || n.ProxyURL == "" {
			continue
		}
		if n.LastClassification != "healthy" || n.LastProbeAt <= cutoff || n.ExitIP == "" {
			continue
		}
		if bad.ExitIP != "" && n.ExitIP == bad.ExitIP {
			continue
		}
		targets = append(targets, n)
	}
	return targets
}

// migrateAuthsOffNode fails closed, then moves only guard-managed accounts to
// recently active-verified nodes with a different observed exit IP.
func migrateAuthsOffNode(store *stateStore, bad *nodeRecord) error {
	if bad == nil || bad.ProxyURL == "" {
		return nil
	}
	auths, err := listAuthFilesFresh()
	if err != nil {
		return err
	}
	affected := make([]authFile, 0)
	for _, a := range auths {
		if a.ProxyURL == bad.ProxyURL && !isAccountQualityDisabled(a) && (!a.Disabled || isGuardDisabledAuth(a)) {
			affected = append(affected, a)
		}
	}
	if len(affected) == 0 {
		return nil
	}
	for _, a := range affected {
		if a.Disabled {
			continue
		}
		if err := setAuthProxyAndFlags(a, a.ProxyURL, true, "egress-guard 隔离中"); err != nil {
			return fmt.Errorf("隔离账号 %s 失败: %w", a.Name, err)
		}
	}
	healthy := verifiedMigrationTargets(store, bad)
	if len(healthy) == 0 {
		return fmt.Errorf("没有通过主动检测且出口 IP 不同的健康通道")
	}
	cursor := 0
	moved := 0
	failed := 0
	for _, a := range affected {
		dest := healthy[cursor%len(healthy)]
		cursor++
		if err := setAuthProxyAndFlags(a, dest.ProxyURL, false, ""); err != nil || verifyAuthBinding(a, dest.ProxyURL, false) != nil {
			failed++
			continue
		}
		moved++
	}
	refreshAssignedCounts(store)
	if moved > 0 {
		store.appendEvent(guardEvent{
			Event:    "accounts_migrated",
			NodeID:   bad.ID,
			NodeName: bad.Name,
			Reason:   fmt.Sprintf("隔离后迁出 %d 个账号到健康通道，失败 %d 个", moved, failed),
		})
	}
	if failed > 0 {
		return fmt.Errorf("%d 个账号迁移或验证失败", failed)
	}
	return nil
}
