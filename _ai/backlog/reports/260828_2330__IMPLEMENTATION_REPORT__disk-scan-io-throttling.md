---
filename: "_ai/backlog/reports/260828_2330__IMPLEMENTATION_REPORT__disk-scan-io-throttling.md"
title: "Implementation Report: Disk-scan I/O throttling to prevent agent-induced host outages"
createdAt: 2026-08-29 00:10
updatedAt: 2026-08-29 00:10
project: "topdata-node-agent-v2"
status: completed
filesCreated: 1
filesModified: 5
filesDeleted: 0
tags: [go, disk, io, prometheus, systemd, incident, monitoring]
documentType: IMPLEMENTATION_REPORT
id: 70e684cb-b0ab-4d46-b9cc-27a2ad559de9
content_hash: abcab1e14f296795a79974024fd137ba
---

# Implementation Report: Disk-scan I/O throttling

## Summary

Implemented the two-layer fix for the 2026-08-28 arm1 disk-I/O outage. At the
application level the `DiskScanner` now (1) defers its first scan on restart when
persisted state already exists, eliminating the synchronized full-walk burst that
followed every agent restart; (2) yields/sleeps every N directories during the
walk so a single scan cannot monopolize disk I/O; (3) debounces the state-file
rewrite to at most once per `state_save_interval`; and (4) exports two
self-observability metrics. At the OS level the systemd unit now sets
`IOSchedulingClass=idle` (+`IOWeight=10`) plus a `RestartSec` throttle, so even a
worst-case walk can never starve the host. The deployed env template also ships
safe disk-scan defaults fleet-wide.

A startup configuration table (auth password redacted) was added as a small
operator-ergonomics bonus so effective settings are visible at a glance.

## Files Changed

- `internal/monitor/disk_scan.go` (modified) — `ScanOptions`, new struct fields,
  `hasState`, `scheduleSave`, `maybeYield`, rewritten `scanShop` with timing
  metrics, defer-first `StartShop` + `runScan`, and state rehydration in
  `loadState`.
- `internal/monitor/disk_scan_test.go` (created) — unit tests for the new
  behavior (`hasState`, `ScanOptions` application, debounced `scheduleSave`,
  `subtractChildren` residual math).
- `cmd/serve.go` (modified) — parse the new viper keys, build `monitor.ScanOptions`,
  pass it to `NewDiskScanner`, add the viper defaults, and print a startup config
  table with redaction.
- `deploy/templates/topdata-agent.service.j2` (modified) — added
  `IOSchedulingClass=idle`, `IOWeight=10`.
- `deploy/templates/topdata-agent.env.j2` (modified) — appended the four
  `TOPDATA_AGENT_DISK_*` safe-default lines.
- `CHANGELOG.md` (modified) — collapsed the four duplicate `## [Unreleased]`
  headers into one and added the Changed/Added entries.
- `README.md` (modified) — corrected the `SHOPS_ROOT` default and the
  `topdata_agent_shopware_shop_disk_usage_bytes` description, added the disk config rows, and
  updated the systemd example with the I/O guards.

## Key Changes

1. **Defer-first on restart** — `StartShop` checks `hasState(shop)`; when
   `deferFirst` is set and state exists, the first scan is scheduled into the
   shop's own randomized phase instead of running within ~30s of startup.
2. **Walk yielding** — `maybeYield(n)` calls `runtime.Gosched()` and optionally
   `time.Sleep(yieldSleep)` every `yieldEvery` directories.
3. **Debounced state writes** — `scheduleSave` uses a `time.AfterFunc` timer so
   repeated `recordGrowth` calls collapse into a single write per interval.
4. **Self-metrics** — `topdata_agent_disk_scan_last_duration_seconds` (gauge) and
   `topdata_agent_disk_scan_total` (counter), both labeled `shop`.
5. **State rehydration** — `loadState` now also repopulates the `diskUsage` gauge
   and the `/disk-eaters` `growers` from persisted sizes, so a deferred restart no
   longer blanks metrics for up to one scan interval (size residual only; growth
   repopulates on the next scan).
6. **Systemd isolation** — `IOSchedulingClass=idle` guarantees the agent only
   consumes disk bandwidth that no other process wants.

## Deviations

- **Added state rehydration** beyond the plan: the plan claimed "metrics stay
  populated from persisted state" but `loadState` previously restored only the raw
  `state` map. I rehydrated the gauge and `growers` so the claim actually holds
  (growth delta still requires the next scan).
- **Added a startup config table** (not in the plan): operators get a redacted
  view of effective settings on every start.
- **`env.j2` ships yield ON** (`YIELD_EVERY=100`, `YIELD_SLEEP=2ms`) fleet-wide;
  the plan's prose said new behavior is "off by default except defer-first and
  debounce", but the env override intentionally enables yielding for the
  slow-storage fleet. This is the desired incident mitigation, not a regression.

## Technical Decisions

- Kept `deferFirst` default `true` because it is purely a restart-burst reduction
  and metrics are preserved via rehydration; backward-incompatible only in the
  sense that a restarted agent no longer walks everything immediately.
- `scheduleSave` reuses the existing `saveState` (which owns `d.mu`); the debounce
  timer is guarded by a separate `saveMu` to avoid racing with synchronous saves.
- `scanDuration`/`scanTotal` are package-scope `promauto` vars, matching the
  existing `diskUsage` pattern, so no duplicate-registration risk.

## Testing Notes

- `go build ./...`, `go vet ./...`, `go test ./...` all pass.
- New `internal/monitor/disk_scan_test.go` exercises `hasState`,
  `ScanOptions` application, debounced `scheduleSave` (asserts no synchronous
  write, then a single delayed write), and `subtractChildren` residual math.
- Smoke test: ran `serve` with `TOPDATA_AGENT_AUTH_PASSWORD=secret` and confirmed
  the startup table prints `auth.password = *** (redacted)`.

## Usage Examples

```sh
# Throttle the walk further on very slow storage (per-host env override)
TOPDATA_AGENT_DISK_SCAN_YIELD_EVERY=200 TOPDATA_AGENT_DISK_SCAN_YIELD_SLEEP=1ms \
  /usr/local/bin/topdata-agent serve
```

```sh
# Confirm self-metrics after deploy
promtool query instant :9144 \
  'topdata_agent_disk_scan_last_duration_seconds{shop="muster-shop"}'
```

## Documentation Updates

- `CHANGELOG.md`: collapsed duplicate Unreleased headers; documented the Changed
  and Added entries including the config table.
- `README.md`: corrected `SHOPS_ROOT` default to `/srv/topdata-shops/prod-shops`,
  fixed the `topdata_agent_shopware_shop_disk_usage_bytes` description (pure-Go walk, 6h,
  excludes `var/cache`), added all `TOPDATA_AGENT_DISK_*` rows, and added the
  `IOSchedulingClass=idle` / `IOWeight=10` guards to the systemd example.

## Next Steps

- Run Phase 6 on-host verification on arm1/arm2 (`journalctl` restart count,
  `iotop -p $(pidof topdata-agent)`, Prometheus spike check after restart) once
  the unit is redeployed via `./deploy/deploy-to-prod.sh`.
- Add the suggested Prometheus alerts (`rate(node_cpu_seconds_total{mode="iowait"}[5m])*100 > 5`
  and `rate(node_disk_read_bytes_total[5m]) > 10e6`) for the agent hosts.
