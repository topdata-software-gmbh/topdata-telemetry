---
filename: "_ai/backlog/reports/260824_1441__IMPLEMENTATION_REPORT__periodic-shop-discovery.md"
title: "Implementation Report: Periodic shop discovery"
status: done
filesCreated:
  - internal/monitor/supervisor.go
filesModified:
  - cmd/serve.go
  - internal/monitor/disk_scan.go
  - internal/monitor/log_monitor.go
  - internal/monitor/shops.go
  - README.md
  - CHANGELOG.md
  - AGENTS.md
filesDeleted: []
planFile: _ai/backlog/active/260824_1441__IMPLEMENTATION_PLAN__periodic-shop-discovery.md
id: 89424e14-edbf-44c6-a63c-1b488d792aa9
content_hash: ac324db7fbe36dcbe07091e4ef19db81
---

# Implementation Report: Periodic shop discovery

## Summary

Shop discovery previously ran exactly once at startup in `cmd/serve.go`, so shops
added to or removed from `shops.root` were invisible (or left stale metric
series) until a service restart. This change adds a periodic re-discovery loop
driven by a new `discovery.interval` config key (default 15m) and a
`ShopSupervisor` that owns the live set of shops: added shops get a disk-scan +
log-tail goroutine each bound to a per-shop `context.Context`; removed shops
have their context cancelled and their Prometheus series deleted. `/info`'s
`shops_total` is now live.

## Files changed

- **`internal/monitor/supervisor.go`** (new): `ShopSupervisor` with `New`,
  `Count`, `reconcile`, and `Run`. Reconciles desired vs. running shops on each
  tick, starting/stopping monitors and updating the `shops_total` gauge.
- **`cmd/serve.go`**: added `discovery.interval` default + read; replaced the
  one-shot discovery + goroutine fan-out with `supervisor.Run()` in a goroutine;
  `/info` now reads `supervisor.Count()` instead of a frozen startup value.
- **`internal/monitor/disk_scan.go`**: refactored `Start(shops)` into
  `StartShop(ctx, shop)` (context-aware, per-shop phase preserved) and added
  `RemoveShop(shop)`.
- **`internal/monitor/log_monitor.go`**: `TailLog` now takes a `context.Context`
  and returns on cancel (stops the active tail); added `RemoveShopLog` and the
  `sleepCtx` helper. Midnight-rotation watchdog and `Poll:true` retained.
- **`internal/monitor/shops.go`**: removed `SetShopsTotal`; the gauge is now
  written only by the supervisor.
- **Docs**: `README.md` (config table + architecture note), `CHANGELOG.md`
  (`[Unreleased]` → Added), `AGENTS.md` (config bullet + supervisor note).

## Key changes

- Per-shop lifecycle is now owned by a `context.Context` (dependency inversion);
  subsystems no longer own their own shutdown.
- Series removal on shop removal uses `diskUsage.DeleteLabelValues` and
  `criticalErrors.DeleteLabelValues` (plus dropping the per-shop growth state).

## Deviations

None from the plan. `Start` was deleted (not kept as a wrapper) as recommended;
`SetShopsTotal` was removed and the `shopsTotal` gauge is now exclusively written
by the supervisor.

## Technical decisions

- Per-shop context + supervisor provides clean add/remove semantics without a
  global restart.
- `DeleteLabelValues` is the idiomatic Prometheus way to retire a label series
  for a removed shop.

## Testing notes

`go build ./...` and `go vet ./...` pass. Manual smoke test with a temp
`shops-root` and `TOPDATA_AGENT_DISCOVERY_INTERVAL=5s`:
- `/info` reported `shops_total = 2` initially.
- Adding `c/` → log `shop added: c`, `/info` → 3, `disk_usage_bytes{shop="c"}`
  appeared in `/metrics`.
- Removing `b/` → log `shop removed: b`, `/info` → 2, and both
  `disk_usage_bytes{shop="b"}` and `critical_errors_total{shop="b"}` disappeared
  from `/metrics` (grep count `0`).

## Documentation updates

`README.md`, `CHANGELOG.md` (`[Unreleased]`), and `AGENTS.md` updated as
specified in Phase 5.
