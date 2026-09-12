---
title: "Throttle disk-scan I/O via defer-first + systemd idle class"
status: Accepted
date: 2026-08-29
deciders: Topdata Team
tags: [golang, disk, io, systemd, incident, monitoring]
id: 6f1c2a9b-3d4e-4f5a-8b6c-1a2b3c4d5e6f
content_hash: d704ebfee48140fc402b1e2d576c01cf
---

# Throttle disk-scan I/O to prevent agent-induced host outages

## Context

On 2026-08-28 `arm1.srv.topinfra.de` went unresponsive for ~2.5h, coincident with
a disk-I/O storm and the agent being unreachable. Reading the source revealed the
root cause: `DiskScanner.scanShop` performs a full recursive `os.ReadDir` + stat
walk of every shop tree on each scan, and `StartShop` ran the first scan of every
shop within ~30s of process start. Any agent restart (systemd `Restart=on-failure`,
OOM, or manual) therefore triggered a synchronized full walk of all shops at once —
enough to saturate I/O on SD/HDD-class storage and make the host unreachable.

## Decision

Apply a two-layer fix:

1. **Application layer (Go):** add `ScanOptions` to `DiskScanner`:
   - `DeferFirstOnState` — skip the immediate startup scan when persisted state
     already exists for a shop; schedule the first scan into the shop's randomized
     phase so restarts no longer burst.
   - `YieldEvery`/`YieldSleep` — `runtime.Gosched()` + optional sleep every N
     directories during the walk, so a single scan cannot monopolize disk I/O.
   - `StateSaveInterval` — debounce the full `disk-state.json` rewrite to at most
     once per interval instead of on every per-shop scan.
   - Export `topdata_agent_disk_scan_last_duration_seconds` and
     `topdata_agent_disk_scan_total` for Prometheus-side diagnosis.
   - Rehydrate the `diskUsage` gauge and `/disk-eaters` `growers` from persisted
     state on load so a deferred restart does not blank metrics.
2. **OS layer (systemd):** set `IOSchedulingClass=idle` (+`IOWeight=10`) and keep a
   `RestartSec` throttle in the rendered unit, so the agent only consumes disk
   bandwidth that no other process wants.

Ship safe defaults fleet-wide via `topdata-agent.env.j2`
(`scan_defer_on_state=true`, `state_save_interval=30s`, `scan_yield_every=100`,
`scan_yield_sleep=2ms`).

## Consequences

**Positive**
- Restarting the agent no longer triggers a synchronized full-walk I/O burst.
- Even a worst-case walk cannot starve the host (`IOSchedulingClass=idle`).
- New metrics make the next incident diagnosable from Prometheus without SSH.
- Changes are config-gated and backward compatible (yield off by default in code;
  defer-first is safe because state is rehydrated).

**Negative / trade-offs**
- A deferred restart delays fresh growth deltas by up to one `scan_interval` (sizes
  are rehydrated immediately; growth-per-hour repopulates on the next scan).
- `IOWeight=10` is ignored when `IOSchedulingClass=idle` is set on some systemd
  versions; kept for portability. The effective cap is `idle`.

## Alternatives Considered

- Reintroduce a synchronized hourly `du -sb` per shop: rejected — that was the
  original design whose concurrent burst caused prod disk-pressure incidents.
- Make yielding the only guard and skip the systemd `idle` class: rejected — the OS
  cap is the primary backstop and costs nothing.
- Persist growth history (two snapshots) to fully restore growth on restart:
  deferred as YAGNI; one-interval staleness of growth is acceptable.

## Related

- Plan: `_ai/backlog/active/260828_2330__IMPLEMENTATION_PLAN__disk-scan-io-throttling.md`
- Analysis: `_ai/backlog/reports/260828_2300__ANALYSIS_REPORT__arm1-topdata-agent-disk-io-outage.md`
