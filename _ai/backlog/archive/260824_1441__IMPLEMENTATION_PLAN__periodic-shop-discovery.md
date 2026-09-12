---
filename: "_ai/backlog/active/260824_1441__IMPLEMENTATION_PLAN__periodic-shop-discovery.md"
title: "Periodic shop discovery (discovery.interval) so added/removed shops are picked up without restart"
createdAt: 2026-08-24 14:41
updatedAt: 2026-08-24 14:41
status: completed
completedAt: 2026-08-24 15:47
priority: medium
tags: [discovery, lifecycle, goroutines, metrics, config]
estimatedComplexity: moderate
documentRevision: 1
documentType: IMPLEMENTATION_PLAN
id: 13537347-d70d-4103-a6ca-51af32f480de
content_hash: b0ebdcef9429b706ac6b6088f1de065d
---

# Implementation Plan: Periodic shop discovery

## Problem

Today shop discovery (`discovery.FindShops`) runs exactly **once** at process
startup in `cmd/serve.go`. Both monitoring subsystems are seeded with that fixed
slice and never observe the filesystem again:

- `monitor.DiskScanner.Start(shops)` (`internal/monitor/disk_scan.go:227`)
  launches one goroutine per shop over the captured slice.
- `go monitor.TailLog(shop.Name, shop.LogPath)` (`cmd/serve.go:77`) launches one
  never-ending tailer goroutine per shop.
- `monitor.SetShopsTotal(len(shops))` and the `agentInfo.ShopsTotal` field are
  set once and frozen.

Consequence: a shop added to or removed from `shops.root` is invisible (no log
tail, no disk gauge) until the systemd service is restarted. A removed shop also
leaves a stale `topdata_agent_shopware_shop_disk_usage_bytes{shop=...}` series
in `/metrics` forever.

## Executive summary

Add a periodic re-discovery loop driven by a new config key
`discovery.interval` (default **15 minutes**, env
`TOPDATA_AGENT_DISCOVERY_INTERVAL`). A single `ShopSupervisor`
(`internal/monitor/supervisor.go`) owns the live set of shops:

- On each tick it re-runs `discovery.FindShops` and diffs against the current
  set (keyed by shop name).
- **Added** shops: start a disk-scan goroutine and a log-tail goroutine, both
  bound to a per-shop `context.Context`.
- **Removed** shops: cancel that shop's context (stops both goroutines), delete
  its Prometheus series (`diskUsage.DeleteLabelValues`,
  `criticalErrors.DeleteLabelValues`), drop its growth state, and update
  `shops_total`.
- `shops_total` gauge and `/info`'s `shops_total` become live (read from the
  supervisor at request time).

Discovery itself is cheap (one `ReadDir` + one `Stat` per subdir) so a 15-minute
cadence adds negligible load; the expensive per-shop `WalkDir` stays throttled
exactly as today.

## Project environment

- Project Name: Go CLI App — `topdata-node-agent` (module `github.com/topdata/node-agent`)
- Go 1.21+, Cobra CLI (`cmd/root.go`, `cmd/serve.go`), Viper config (`TOPDATA_AGENT_` env prefix)
- Prometheus metrics via `promauto` on the global registry
- No test files; verification is `go build ./...` + `go vet ./...` and manual smoke test
- Subsystems: `internal/discovery` (`FindShops`), `internal/monitor` (`log_monitor.go`, `disk_scan.go`)

## Conventions followed

- Go conventions: exported types/functions documented, stdlib/third-party/local
  import grouping, explicit error handling, mutex for shared state.
- Cobra conventions: config via Viper defaults + env binding in `init()`.
- SOLID: the supervisor has a single responsibility (reconcile desired vs.
  running shops); `DiskScanner` and the tailer depend on a `context.Context`
  (dependency inversion) rather than owning their lifecycle.

---

## Phase 1 — Config: `discovery.interval`

Add the new key alongside the existing defaults in `cmd/serve.go` `init()`.
Document it in the `AGENTS.md`-style config list and README (Phase 4).

### `cmd/serve.go` — [MODIFY]

In `init()`, after the existing `disk.*` defaults:

```go
viper.SetDefault("discovery.interval", 15*time.Minute)
```

Read it in `Run` next to the other durations:

```go
discoveryInterval := viper.GetDuration("discovery.interval")
if discoveryInterval <= 0 {
	discoveryInterval = 15 * time.Minute
}
```

## Phase 2 — Lifecycle-aware subsystems

Both subsystems must accept a `context.Context` and exit cleanly when it is
cancelled. Changes are additive and keep the current behaviour when the context
is never cancelled.

### `internal/monitor/disk_scan.go` — [MODIFY]

1. Change the per-shop loop in `Start` to be driven per-shop with a context, and
   expose methods the supervisor can call. Refactor `Start(shops)` into
   `StartShop(ctx, shop)` so the supervisor controls each shop's goroutine.

```go
// StartShop runs the periodic disk scan for a single shop until ctx is
// cancelled. The first scan runs promptly (serialized through the semaphore),
// then the shop settles into its own fixed random phase so scans stay
// permanently desynchronized and never realign into a spike.
func (d *DiskScanner) StartShop(ctx context.Context, shop discovery.Shop) {
	go func() {
		select {
		case <-time.After(time.Duration(rand.Int63n(int64(30 * time.Second)))): // small boot spread
		case <-ctx.Done():
			return
		}

		d.sem <- struct{}{}
		d.scanShop(shop)
		<-d.sem

		offset := time.Duration(rand.Int63n(int64(d.interval))) // fixed phase for this shop
		timer := time.NewTimer(d.interval + offset)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				d.sem <- struct{}{}
				d.scanShop(shop)
				<-d.sem
				timer.Reset(d.interval + offset)
			}
		}
	}()
}
```

2. Add a removal method that deletes the Prometheus series and drops the shop's
   growth state.

```go
// RemoveShop stops tracking a shop: it deletes the disk-usage series and the
// shop's per-directory growth state so /metrics and /disk-eaters no longer
// report it.
func (d *DiskScanner) RemoveShop(shop string) {
	diskUsage.DeleteLabelValues(shop)
	d.mu.Lock()
	delete(d.state, shop)
	delete(d.growers, shop)
	d.mu.Unlock()
}
```

3. Keep `Start(shops)` as a thin convenience wrapper that never cancels
   (background context) so existing behaviour/tests are unchanged — or delete it
   and let the supervisor call `StartShop` directly. Prefer deleting it to avoid
   a dead path; the supervisor becomes the only caller.

### `internal/monitor/log_monitor.go` — [MODIFY]

Give `TailLog` a context. On cancel, stop the active tail and return. Preserve
the midnight-rotation watchdog and the `Poll: true` behaviour verbatim (see
`AGENTS.md`: do not refactor back to a single blocking `range t.Lines`).

```go
// TailLog tails the shop's daily Shopware log until ctx is cancelled. It counts
// lines matching .CRITICAL: or [critical] into the critical-errors counter and
// restarts the tail at midnight to follow the new daily file.
func TailLog(ctx context.Context, shopName, logDir string) {
	var current *tail.Tail
	var lastDay string

	stop := func() {
		if current != nil {
			current.Stop()
			current.Cleanup()
			current = nil
		}
	}
	defer stop()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		day := time.Now().Format("2006-01-02")
		if day != lastDay {
			stop()
			lastDay = day
			next, err := tail.TailFile(fmt.Sprintf("%s/prod-%s.log", logDir, day), tail.Config{
				Follow:    true,
				ReOpen:    true,
				MustExist: false,
				Poll:      true,
				Logger:    tail.DiscardingLogger,
				Location:  &tail.SeekInfo{Offset: 0, Whence: io.SeekEnd},
			})
			if err != nil {
				if !sleepCtx(ctx, 10*time.Second) {
					return
				}
				continue
			}
			current = next
			go func(t *tail.Tail) {
				for line := range t.Lines {
					if strings.Contains(line.Text, ".CRITICAL:") || strings.Contains(line.Text, "[critical]") {
						criticalErrors.WithLabelValues(shopName).Inc()
					}
				}
			}(current)
		}

		if !sleepCtx(ctx, 30*time.Second) {
			return
		}
	}
}

// sleepCtx sleeps for d, returning false early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
```

Add a removal helper for the counter series:

```go
// RemoveShopLog deletes the critical-errors series for a removed shop.
func RemoveShopLog(shop string) {
	criticalErrors.DeleteLabelValues(shop)
}
```

> Note: `tail.Tail.Stop()` causes the inner `range t.Lines` goroutine to drain
> and exit, so cancelling the context (which triggers `stop()` via `defer`)
> leaves no leaked tail goroutines.

## Phase 3 — `ShopSupervisor`

New file `internal/monitor/supervisor.go`. One supervisor owns reconciliation:
it holds the current shop set, a `cancel` per shop, and re-runs discovery on a
ticker.

### `internal/monitor/supervisor.go` — [NEW FILE]

```go
package monitor

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/topdata/node-agent/internal/discovery"
)

// shopsTotal (internal/monitor/shops.go) is the existing gauge; the supervisor
// updates it on every reconcile and SetShopsTotal is removed once the
// supervisor is the only writer.

// shopHandle bundles the per-shop lifecycle: cancel stops both the disk scanner
// and the log tailer for that shop.
type shopHandle struct {
	shop   discovery.Shop
	cancel context.CancelFunc
}

// ShopSupervisor reconciles the running monitors with the shops present on disk.
// It re-runs discovery on a fixed interval, starts monitors for newly added
// shops and stops monitors (and removes their metrics) for removed shops.
type ShopSupervisor struct {
	root     string
	interval time.Duration
	scanner  *DiskScanner

	mu    sync.Mutex
	shops map[string]shopHandle // keyed by shop name
}

// NewShopSupervisor creates a supervisor over the given shops root.
func NewShopSupervisor(root string, interval time.Duration, scanner *DiskScanner) *ShopSupervisor {
	return &ShopSupervisor{
		root:     root,
		interval: interval,
		scanner:  scanner,
		shops:    map[string]shopHandle{},
	}
}

// Count reports the number of shops currently monitored.
func (s *ShopSupervisor) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.shops)
}

// reconcile discovers shops on disk and diffs them against the running set.
func (s *ShopSupervisor) reconcile() {
	found, err := discovery.FindShops(s.root)
	if err != nil {
		log.Printf("discovery: %v (keeping current shops)", err)
		return
	}

	foundByName := make(map[string]discovery.Shop, len(found))
	for _, sh := range found {
		foundByName[sh.Name] = sh
	}

	s.mu.Lock()
	// Removed shops: stop monitors, delete series, drop state.
	for name, h := range s.shops {
		if _, ok := foundByName[name]; !ok {
			log.Printf("shop removed: %s", name)
			h.cancel()
			s.scanner.RemoveShop(name)
			RemoveShopLog(name)
			delete(s.shops, name)
		}
	}
	// Added shops: start monitors.
	for name, sh := range foundByName {
		if _, ok := s.shops[name]; ok {
			continue
		}
		log.Printf("shop added: %s (logs: %s)", sh.Name, sh.LogPath)
		ctx, cancel := context.WithCancel(context.Background())
		s.shops[name] = shopHandle{shop: sh, cancel: cancel}
		s.scanner.StartShop(ctx, sh)
		go TailLog(ctx, sh.Name, sh.LogPath)
	}
	n := len(s.shops)
	s.mu.Unlock()

	shopsTotal.Set(float64(n))
}

// Run performs an immediate reconciliation and then re-runs it every interval
// until the process exits. It blocks, so call it in a goroutine.
func (s *ShopSupervisor) Run() {
	s.reconcile()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for range ticker.C {
		s.reconcile()
	}
}
```

> The `shopsTotal` gauge already exists in `internal/monitor/shops.go` (with the
> one-shot `SetShopsTotal` setter). The supervisor reuses that gauge and updates
> it on every reconcile; remove `SetShopsTotal` (and the `shops.go` wrapper if
> nothing else references it) once the supervisor is the only writer.

## Phase 4 — Wire it into `serve`

### `cmd/serve.go` — [MODIFY]

Replace the one-shot discovery + per-shop goroutine fan-out with the supervisor.

```go
// before:
//   shops, err := discovery.FindShops(...)
//   monitor.SetShopsTotal(len(shops))
//   scanner.Start(shops)
//   for _, shop := range shops { go monitor.TailLog(...) }
// after:
	scanner := monitor.NewDiskScanner(scanInterval, scanConcurrency, excludeList, maxDepth, stateFile)
	supervisor := monitor.NewShopSupervisor(viper.GetString("shops.root"), discoveryInterval, scanner)
	go supervisor.Run()
```

Make `/info` report the live count instead of the frozen startup value. The
`agentInfo.ShopsTotal` static field is removed; `infoHandler` reads
`supervisor.Count()`:

```go
// in Run, keep a reference the handler can use (package-level or closure):
var shopCount func() int // set to supervisor.Count in Run

// in infoHandler:
	ShopsTotal: shopCount(),
```

(`healthzHandler`, the auth middleware, and the `?format` negotiation are
unchanged.)

### Update the `shops_total` source

Remove the old `monitor.SetShopsTotal` call and symbol; the supervisor's
`shopsTotal` gauge is the single source of truth and is now updated on every
reconcile.

## Phase 5 — Docs & housekeeping

- **`README.md`** [MODIFY]:
  - Config table: add `TOPDATA_AGENT_DISCOVERY_INTERVAL` (default `15m`) —
    how often the agent re-scans `shops.root` for added/removed shops.
  - Architecture: note that discovery is periodic, so new shops are picked up
    automatically within one interval and removed shops stop being monitored
    (their metric series are deleted) without a restart.
- **`CHANGELOG.md`** [MODIFY]: under `[Unreleased]` → `### Added`:
  - "`discovery.interval` (env `TOPDATA_AGENT_DISCOVERY_INTERVAL`, default
    `15m`): the agent now re-discovers shops periodically. Added shops are
    monitored automatically; removed shops are stopped and their Prometheus
    series removed — no service restart required. `/info`'s `shops_total` is now
    live."
- **`.gitignore`**: no new file types/artifacts introduced — no change.
- **`AGENTS.md`**: add the `discovery.interval` bullet to the Config section and
  note the supervisor in the Architecture section (per repo convention that
  AGENTS.md must stay in sync).

## SOLID notes

- **Single responsibility**: `ShopSupervisor` only reconciles desired vs.
  running shops; `DiskScanner` only scans; `TailLog` only tails.
- **Open/closed**: subsystems are extended with a context parameter, not
  rewritten.
- **Dependency inversion**: monitors depend on `context.Context` for lifecycle
  rather than owning a global shutdown.

## Validation

1. `go build ./...` and `go vet ./...` pass.
2. Smoke test: create a temp `shops-root` with two fake shops
   (`<root>/a/vol/www/var/log`, `<root>/b/vol/www/var/log`), run
   `TOPDATA_AGENT_AUTH_USERNAME=u TOPDATA_AGENT_AUTH_PASSWORD=p \
    TOPDATA_AGENT_LISTEN_ADDRESS=:19144 TOPDATA_AGENT_DISCOVERY_INTERVAL=15s \
    ./bin/topdata-agent serve --shops-root <root>`.
   - `curl -u u:p :19144/info` shows `shops_total = 2`.
   - Add `<root>/c/vol/www/var/log` → within ~15s log shows `shop added: c`,
     `/info` shows 3, `/metrics` has `...disk_usage_bytes{shop="c"}`.
   - Remove `<root>/b` → log shows `shop removed: b`, `/info` shows 2,
     `...disk_usage_bytes{shop="b"}` and `...critical_errors_total{shop="b"}`
     disappear from `/metrics`.
3. Confirm no goroutine leak: after several add/remove cycles the process
   goroutine count (e.g. via `pprof` or `SIGQUIT` stack dump) stays flat.

## Final phase — Report

Write `_ai/backlog/reports/260824_1441__IMPLEMENTATION_REPORT__periodic-shop-discovery.md`
with the frontmatter fields from the report spec (`status`, `filesCreated`,
`filesModified`, `filesDeleted`, `planFile` pointing at this file), covering:
summary, files changed, key changes, deviations, technical decisions
(per-shop context + supervisor; `DeleteLabelValues` on removal), testing notes
(the smoke test above), and documentation updates.
