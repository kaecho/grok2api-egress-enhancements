# Grok2API & CPA egress quality guard

This is an unofficial enhancement distribution for [chenyme/grok2api](https://github.com/chenyme/grok2api): it provides immediate fixed-proxy recovery and egress quality-guard patches, plus a pure CPA-native plugin with no Grok2API runtime dependency. The repository does not copy the complete upstream source.

Current baseline:

- Upstream release: `v3.1.2` (quality guard and probe-wait recovery are already upstream)
- Upstream commit: `6e9eef7619b83899c82e24353177c8a819f15914`
- Today's delta: probe profiles + dual-probe recovery + thinking guard (isolate when output ≥ 32 and reasoning=0)
- Patch file: `patches/0005-fix-missing-thinking-32-token-floor.patch` (stack on 0004)
- Previous delta: `patches/0004-fix-dual-probe-recovery-and-thinking-guard.patch`
- Upstream PR: [chenyme/grok2api#930](https://github.com/chenyme/grok2api/pull/930)
- Runnable fork: [lij768423-svg/grok2api](https://github.com/lij768423-svg/grok2api) `main`

If you are still on `v3.0.11`, keep using the legacy patch `patches/0001-feat-add-egress-recovery-and-quality-guard.patch` (closed [#837](https://github.com/chenyme/grok2api/pull/837)).

## Features

### Immediate fixed-proxy recovery

- A pre-submission transport failure persists cooldown state and starts an immediate background probe.
- Concurrent failures for one node share a single probe.
- A later bound request waits for at most five seconds, reloads persisted state after healthy recovery, and continues early.
- Request cancellation stops the wait without canceling the shared probe.
- Submitted generation requests, authentication failures, quota exhaustion, and rate limits are never replayed by this mechanism.
- Upstream's existing proxy-pool mode keeps fresh-tunnel isolation, so one rotating exit failure does not cool the whole pool.

### Egress quality guard

- Passive audits use the grok2api panel formula `output tokens / (duration - first token)`; output tokens include reasoning tokens.
- **Passive hard-threshold hits quarantine immediately**. Soft hits still trigger a fixed-prompt active confirmation and require consecutive strikes.
- Active soft and hard thresholds, consecutive probe-error handling, minimum healthy-node protection, quarantine, and recovery.
- Fail-closed quarantine before confirmation, with same-IP confirmation for short buffered bursts to avoid false rotation.
- A trusted per-node rotation webhook and a 1024Proxy `sid-...-t-...` sticky-session rotator.
- One real-model check per new IP: healthy results restore immediately; anomalous or indeterminate results remain isolated.
- Account-selection failures are deferred without counting a proxy error or rotating the IP.
- If a target node's bound accounts are unavailable, administrator probes borrow any healthy Build account while still forcing the physical request through the node under test. Ordinary traffic is unchanged.
- If the entire account pool is unavailable, the guard uses a separate long backoff and suppresses duplicate no-account logs while keeping the node isolated.
- Admin UI, manual diagnostics, hot-reloadable policy, and persistent statistics.
- One replaceable toast per node action, with manual tests disabled while a node is quarantined or rotating.
- Create, edit, delete, enable, disable, and refresh Build proxy nodes directly from the node-quality table.
- Select individual or all nodes and batch enable, disable, or delete them with destructive-action confirmation.
- Automatically discover proxied Build nodes when `QUALITY_GUARD_NODE_IDS` is empty while publishing resolved IDs for compatibility with older admin pages.
- Python sidecar, Docker Compose and systemd examples, security notes, and bilingual documentation.

### Degraded-account monitor (v3.1.2 delta)

- Adds a Quality Guard tab that classifies user streaming requests (excluding quality-test probes) as `buffered_burst` / `soft_tps` / `hard_tps`.
- Same panel formula: `outputTokens * 1000 / (durationMs - firstTokenMs)`, defaults soft 500 / hard 1000; windows shorter than 1s that reach soft are `buffered_burst`.
- Windows: 1h / 6h / 24h / 7d. Filter by email/ID, schedule status, class, and hit count.
- Timeline bars grow from the bottom. Any row is selectable; bulk mute or unmute uses the existing account batch API with string `ids`.
- Endpoint: `GET /api/admin/v1/request-audits/degrade-accounts`.

### CPA-native egress guard plugin

`cpa-plugin/` is now the **v1.0.10 pure-CPA plugin**. It has no runtime dependency on Grok2API: it uses CPA Host APIs for auth files and usage events, binds `proxy_url` stickily to egress nodes, and provides node CRUD, line-based bulk import, batch operations, connectivity/real-model tests, configurable probe profiles (throughput / expected-marker / custom prompt), quarantine migration, hot-reloadable policy, statistics, events, and light/dark themes. v1.0.10 stops N+1 `host.auth.get` on the management UI, probes dual-stack exit IPs (`api64`/`api6` then `api.ipify`), and accepts `socks5h://` plus unbracketed IPv6 proxy URLs. In v1.0.9, active probes can verify a last-line or regex marker. In v1.0.8, store-install registration no longer blocks on a full auth scan (fixes plugins stuck as inactive/unregistered with many accounts). In v1.0.7, CPA scheduling skips quarantined or cooling egresses; credential, quota, and permission failures are recorded as ignored instead of quarantining a node; migrations are read-back verified; and an optional allowlisted internal IP-rotation webhook is available. See [cpa-plugin/README.md](./cpa-plugin/README.md) for build instructions and the Chinese [AI deployment and operations guide](./cpa-plugin/AI_USAGE_GUIDE.md) for proxy topology, capacity planning, quarantine recovery, and forced residential-IP rotation.

For the recommended end-to-end topology (residential/Resin -> Mihomo sharding and listeners -> Grok2API/CPA egress nodes -> Quality Guard detection, drain, rotation, and re-probing), see [Recommended egress deployment](./docs/RECOMMENDED_DEPLOYMENT.md).

CPA itself does not degrade model quality. This optional plugin acts as an egress circuit breaker for multi-account, multi-egress deployments; single-account or stable static-proxy installations may not need it.

The quality guard is a heuristic circuit breaker, not proof that upstream model capability changed. Immediate hard quarantine is intentionally aggressive; raise `hard_tps` when false positives are more costly. Soft anomalies still require confirmation probes.


## Apply directly

From a clean grok2api checkout:

```sh
git fetch --tags origin
git checkout -b egress-enhancements v3.1.2
git am --3way /path/to/grok2api-egress-enhancements/patches/0002-feat-add-degraded-account-monitor.patch
git am --3way /path/to/grok2api-egress-enhancements/patches/0003-feat-add-quality-guard-probe-profiles.patch
git am --3way /path/to/grok2api-egress-enhancements/patches/0004-fix-dual-probe-recovery-and-thinking-guard.patch
git am --3way /path/to/grok2api-egress-enhancements/patches/0005-fix-missing-thinking-32-token-floor.patch
```

On `v3.0.11`, apply `patches/0001-feat-add-egress-recovery-and-quality-guard.patch` instead. For newer upstream versions, follow [AI_MERGE_GUIDE.md](./docs/AI_MERGE_GUIDE.md) and resolve conflicts according to the documented invariants instead of replacing newer files wholesale.

## Validate

```sh
go test ./...
python3 -m unittest -v \
  tools/egress-quality-guard/quality_guard_test.py \
  tools/egress-quality-guard/session_rotator_test.py  # 26 tests
cd frontend
pnpm lint
pnpm build
```

## Security and privacy

Never provide real environment files, application config, databases, state volumes, proxy URLs, account credentials, or production logs to an AI merge tool. The upstream source, this patch, and sanitized test failures are sufficient.

## Related projects

- [Grok Register + Live Panel](https://github.com/lij768423-svg/grok-register-panel): a separate Camoufox-based Grok registration workflow and web control panel with multiple email backends, an external proxy pool, egress checks, an ASN blacklist, runtime statistics, and account recovery. It is not bundled with this patch.

## Friends

- [LINUX DO](https://linux.do) — A new kind of community

## License and attribution

The patch is distributed under the upstream MIT license. Preserve the upstream LICENSE, copyright notices, and Git history. This repository is not an official grok2api release and does not imply upstream endorsement.
