---
filename: "_ai/backlog/active/260828_2330__IMPLEMENTATION_PLAN__disk-scan-io-throttling.md"
title: "Throttle disk-scan I/O to prevent agent-induced host outages"
createdAt: 2026-08-28 23:30
updatedAt: 2026-08-28 23:30
status: completed
completedAt: 2026-08-29 00:06
priority: high
tags: [go, disk, io, prometheus, incident, arm1, monitoring]
estimatedComplexity: moderate
documentRevision: 1
documentType: IMPLEMENTATION_PLAN
id: d3c3d6d7-1dd0-43f4-9f06-f8d3c325c05c
content_hash: 3c2e53827ea6d1926a2c2974a818eec0
---

# Throttle disk-scan I/O to prevent agent-induced host outages

## 1. Problem

On 2026-08-28 `arm1.srv.topinfra.de` went unresponsive for ~2.5h. Analysis
(`_ai/backlog/reports/260828_2300__ANALYSIS_REPORT__arm1-topdata-agent-disk-io-outage.md`)
found a disk-I/O storm coincident with the agent being unreachable. The agent
binary itself was not inspectable during analysis, but reading the source now
reveals the mechanism:

- `DiskScanner.scanShop` (`internal/monitor/disk_scan.go:117`) performs a **full
  recursive** `os.ReadDir` + `e.Info()` stat of **every** file in every shop tree
  on each scan. The persisted `disk-state.json` is used only for growth deltas —
  it does **not** let the walker skip directories, so there is **no incremental
  scan**.
- `StartShop` (`disk_scan.go:287`) runs the first scan of every shop within
  ~30s of process start, then settles into a 6h randomized phase. So **every
  agent restart triggers a full walk of all shops at once** — a burst.
- The process cannot crash-loop on its own (`serve` only `log.Fatal`s at startup;
  `reconcile` swallows errors), so the "crash-loop" seen in Prometheus was almost
  certainly I/O-starved scrape timeouts, amplified by whatever restarted the agent
  (systemd `Restart=on-failure`, OOM, or manual) → repeated full walks.
- On slow storage (arm1 is an ARM host; its `vda` is likely SD/HDD class) a full
  recursive metadata walk at 14.8 MB/s with 3–5% iowait is enough to saturate I/O
  and make the host unreachable. arm2 ran the same agent and recovered because it
  was not in the same restart/spin-up state.

The fix has two layers: (1) **make the agent's I/O gentle by design** so a restart
or a single walk can never monopolize the disk, and (2) **isolate the agent's I/O
at the OS level** via the systemd unit so even worst-case behavior is bounded.

## 2. Executive Summary of the Solution

1. **Defer the startup walk when state already exists** — on every restart after
   the first, skip the immediate full walk and let each shop's randomized phase
   schedule its next scan. This removes the restart→burst amplifier entirely.
2. **Yield / throttle during the walk** — optionally sleep every N directories so
   a single scan cannot saturate disk I/O even without an OS-level cap.
3. **Batch the state-file writes** — today `recordGrowth` rewrites the *entire*
   `disk-state.json` on *every* per-shop scan; debounce to once per
   `state_save_interval` (default 30s).
4. **Self-observability** — export `topdata_agent_disk_scan_last_duration_seconds`
   and `topdata_agent_disk_scan_total` so the next incident is diagnosable from
   Prometheus without SSH.
5. **OS-level isolation** — add `IOSchedulingClass=idle` + `IOWeight=10` to the
   systemd unit (and a `RestartSec` throttle) so the agent only consumes disk I/O
   that no other process wants.
6. **Config surface** — expose the new knobs via `TOPDATA_AGENT_DISK_*` env vars
   with safe defaults (backward compatible: all new behavior is opt-in / off by
   default except the defer-first and the debounce, which are safe).

## 3. Project Environment

- **Project Name**: topdata-node-agent-v2 (Go monitoring agent for Shopware 6)
- **Module path**: `github.com/topdata/node-agent`
- **Language / version**: Go 1.21+
- **Build**: `go build -o bin/topdata-agent .` ; `go build ./...`
- **Verify**: `go vet ./...` ; `go test ./...`
- **Metrics**: registered via `prometheus/client_golang/promauto` on the global
  default registry (package-level, init-time). New metrics follow the same pattern
  and will NOT cause duplicate-registration panics because they are declared at
  package scope, not inside `NewDiskScanner`.
- **Deploy**: `deploy/templates/topdata-agent.service.j2` is rendered by Ansible
  into `/etc/systemd/system/topdata-agent.service`. Changes there ship via
  `./deploy/deploy-to-prod.sh`.
- **Conventions**: package `monitor`; exported funcs/types documented; no comments
  added to source beyond doc comments (per repo AGENTS.md "DO NOT ADD COMMENTS").
  The code blocks below therefore keep only the existing doc-comment style and add
  none.

---

## Phase 1 — Deploy artifacts: fleet-wide safe defaults (systemd unit + env)

`deploy/templates/topdata-agent.service.j2` is the strongest single mitigation:
`IOSchedulingClass=idle` guarantees the agent only gets disk bandwidth when no
other process wants it, so even a full walk can never starve the host.

[MODIFY] `deploy/templates/topdata-agent.service.j2`

```ini
[Unit]
Description=Topdata Go Node Agent (Shopware 6 metrics exporter)
After=network.target

[Service]
Type=simple
EnvironmentFile=/etc/topdata-agent.env
ExecStart=/usr/local/bin/topdata-agent serve
Restart=on-failure
RestartSec=5
# Bound the agent's disk I/O so a full directory walk can never saturate the
# host's storage (root-cause mitigation for the 2026-08-28 arm1 outage). The
# agent only consumes disk bandwidth that no other process wants.
IOSchedulingClass=idle
IOWeight=10

[Install]
WantedBy=multi-user.target
```

Notes:
- `IOWeight=10` is ignored when `IOSchedulingClass=idle` is set on some systemd
  versions; both are harmless and the effective cap is `idle`. Keep both for
  portability across the mixed arm64/amd64 fleet.
- `RestartSec=5` already present; the throttle limits restart-storm I/O.

### 1.1 Fleet-wide env defaults

The systemd `idle` class is the primary guard, but the app-level throttle knobs
must also be shipped to every host. Bake the safe defaults directly into the
rendered env file so they apply fleet-wide on the next deploy (no vault edit
needed — these are identical on every server, and safe because the agent cannot
starve the host under `IOSchedulingClass=idle`).

[MODIFY] `deploy/templates/topdata-agent.env.j2`

```ini
# Rendered by the topdata-agent deploy playbook from ansible vault vars.
TOPDATA_AGENT_SHOPS_ROOT={{ shops_root }}
TOPDATA_AGENT_AUTH_USERNAME={{ auth_username }}
TOPDATA_AGENT_AUTH_PASSWORD={{ auth_password }}
TOPDATA_AGENT_LISTEN_ADDRESS={{ listen_address }}

# Safe defaults for slow storage (SD/HDD-class fleet). The agent's disk walk is
# throttled and deferred on restart so it can never saturate the host's disk.
# IOSchedulingClass=idle in the systemd unit is the backstop that makes these safe.
TOPDATA_AGENT_DISK_SCAN_DEFER_ON_STATE=true
TOPDATA_AGENT_DISK_STATE_SAVE_INTERVAL=30s
TOPDATA_AGENT_DISK_SCAN_YIELD_EVERY=100
TOPDATA_AGENT_DISK_SCAN_YIELD_SLEEP=2ms
```

---

## Phase 2 — DiskScanner: gentle startup, yield, debounced state writes, self-metrics

### 2.1 Struct, options, constructor

[MODIFY] `internal/monitor/disk_scan.go` — replace the `DiskScanner` struct and
`NewDiskScanner` with the versions below (adds `ScanOptions`, yield fields,
`saveInterval`, defer flag, and a `saveMu`/`saveTimer` for debounced writes).

```go
// ScanOptions configures the I/O footprint of the DiskScanner. All fields are
// safe to leave at zero value except where a default is applied by the caller.
type ScanOptions struct {
	// YieldEvery is the number of directories walked between scheduler yields.
	// 0 disables yielding.
	YieldEvery int
	// YieldSleep is the duration slept on each yield. 0 disables the sleep.
	YieldSleep time.Duration
	// StateSaveInterval is the minimum interval between state-file writes.
	// 0 writes on every scan (legacy behavior).
	StateSaveInterval time.Duration
	// DeferFirstOnState skips the immediate startup scan when persisted state
	// already exists for a shop, so restarts do not trigger a full walk burst.
	DeferFirstOnState bool
}

// DiskScanner periodically walks each shop, updates the disk-usage gauge and
// maintains a per-directory growth index served by /disk-eaters.
type DiskScanner struct {
	interval    time.Duration
	concurrency int
	excludes    []string
	maxDepth    int
	stateFile   string

	yieldEvery   int
	yieldSleep   time.Duration
	saveInterval time.Duration
	deferFirst   bool

	sem chan struct{}

	mu      sync.Mutex
	state   map[string]map[string]dirState
	growers map[string][]Grower

	saveMu    sync.Mutex
	saveTimer *time.Timer
}

func NewDiskScanner(interval time.Duration, concurrency int, excludes []string, maxDepth int, stateFile string, opts ScanOptions) *DiskScanner {
	d := &DiskScanner{
		interval:    interval,
		concurrency: concurrency,
		excludes:    excludes,
		maxDepth:    maxDepth,
		stateFile:   stateFile,
		sem:         make(chan struct{}, maxDepthOrOne(concurrency)),
		state:       map[string]map[string]dirState{},
		growers:     map[string][]Grower{},
		yieldEvery:  opts.YieldEvery,
		yieldSleep:  opts.YieldSleep,
		saveInterval: opts.StateSaveInterval,
		deferFirst:  opts.DeferFirstOnState,
	}
	d.loadState()
	return d
}
```

### 2.2 Package-level metrics

[MODIFY] `internal/monitor/disk_scan.go` — add these next to the existing
`diskUsage` promauto registration (package scope, init-time, no dup risk):

```go
var scanDuration = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "topdata_agent_disk_scan_last_duration_seconds",
	Help: "Duration of the most recent disk scan for a shop, in seconds.",
}, []string{"shop"})

var scanTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "topdata_agent_disk_scan_total",
	Help: "Total number of disk scans performed per shop.",
}, []string{"shop"})
```

### 2.3 hasState + debounced save

[MODIFY] `internal/monitor/disk_scan.go` — add `hasState` and replace the
`saveState` call site inside `recordGrowth` with `scheduleSave`; add `scheduleSave`.

```go
// hasState reports whether persisted scan state exists for a shop. It is used to
// decide whether the immediate startup scan can be skipped on a restart.
func (d *DiskScanner) hasState(shop string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.state[shop]
	return ok
}

func (d *DiskScanner) scheduleSave() {
	if d.saveInterval <= 0 {
		d.saveState()
		return
	}
	d.saveMu.Lock()
	defer d.saveMu.Unlock()
	if d.saveTimer != nil {
		return // a write is already scheduled
	}
	d.saveTimer = time.AfterFunc(d.saveInterval, func() {
		d.saveMu.Lock()
		d.saveTimer = nil
		d.saveMu.Unlock()
		d.saveState()
	})
}
```

In `recordGrowth`, change the final `d.saveState()` to `d.scheduleSave()`.

### 2.4 Yield during the walk

[MODIFY] `internal/monitor/disk_scan.go` — add `maybeYield` and thread a directory
counter through `scanShop`'s `walk` closure. Add `"runtime"` to the imports.

```go
// maybeYield yields to the scheduler and optionally sleeps after every
// yieldEvery directories walked, so a full recursive scan cannot monopolize
// disk I/O on slow storage.
func (d *DiskScanner) maybeYield(n int) {
	if d.yieldEvery <= 0 || n%d.yieldEvery != 0 {
		return
	}
	runtime.Gosched()
	if d.yieldSleep > 0 {
		time.Sleep(d.yieldSleep)
	}
}
```

Inside `scanShop`, update the `walk` definition to count directories and yield,
and wrap the walk with timing metrics:

```go
func (d *DiskScanner) scanShop(shop discovery.Shop) {
	now := time.Now()
	sizes := map[string]int64{}

	var n int
	var walk func(abs, rel string, depth int) int64
	walk = func(abs, rel string, depth int) int64 {
		n++
		d.maybeYield(n)
		var sz int64
		ents, err := os.ReadDir(abs)
		if err != nil {
			return 0
		}
		for _, e := range ents {
			name := e.Name()
			childAbs := filepath.Join(abs, name)
			if e.IsDir() {
				childRel := name
				if rel != "" {
					childRel = rel + "/" + name
				}
				if d.isExcluded(childRel) {
					continue
				}
				childSz := walk(childAbs, childRel, depth+1)
				sz += childSz
				if depth+1 <= d.maxDepth {
					sizes[childRel] = childSz
				}
			} else if e.Type().IsRegular() {
				if fi, err := e.Info(); err == nil {
					sz += fi.Size()
				}
			}
		}
		return sz
	}

	start := time.Now()
	total := walk(shop.Path, "", 0)
	scanDuration.WithLabelValues(shop.Name).Set(time.Since(start).Seconds())
	scanTotal.WithLabelValues(shop.Name).Inc()
	sizes[""] = total
	diskUsage.WithLabelValues(shop.Name).Set(float64(total))
	d.recordGrowth(shop.Name, sizes, now)
}
```

### 2.5 Defer-first startup

[MODIFY] `internal/monitor/disk_scan.go` — replace `StartShop` so that, when
`deferFirst` is set and state already exists for the shop, the first scan is
skipped in favor of the randomized phase. Extract the scanned work into `runScan`
so the semaphore + ctx handling is shared.

```go
// StartShop runs the periodic disk scan for a single shop until ctx is
// cancelled. On a first-ever run (no persisted state for the shop) the first
// scan runs promptly with a small boot spread so metrics populate quickly. On
// subsequent agent restarts the first scan is deferred to the shop's own
// randomized phase, so restarting the agent no longer triggers a synchronized
// full-tree walk of every shop at once (which previously caused disk-I/O
// storms on slow storage).
func (d *DiskScanner) StartShop(ctx context.Context, shop discovery.Shop) {
	go func() {
		if d.deferFirst && d.hasState(shop.Name) {
			offset := time.Duration(rand.Int63n(int64(d.interval)))
			timer := time.NewTimer(d.interval + offset)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
					d.runScan(ctx, shop)
					timer.Reset(d.interval + offset)
				}
			}
		}

		select {
		case <-time.After(time.Duration(rand.Int63n(int64(30 * time.Second)))):
		case <-ctx.Done():
			return
		}
		d.runScan(ctx, shop)

		offset := time.Duration(rand.Int63n(int64(d.interval)))
		timer := time.NewTimer(d.interval + offset)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				d.runScan(ctx, shop)
				timer.Reset(d.interval + offset)
			}
		}
	}()
}

// runScan performs one scan of the shop, respecting the concurrency semaphore
// and the context.
func (d *DiskScanner) runScan(ctx context.Context, shop discovery.Shop) {
	select {
	case <-ctx.Done():
		return
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
		d.scanShop(shop)
	}
}
```

---

## Phase 3 — serve.go: wire config + defaults

[MODIFY] `internal/monitor/...` call site is in `cmd/serve.go`. Add the new viper
keys, parse them, build `monitor.ScanOptions`, and pass to `NewDiskScanner`.

In `Run` (after the existing `maxDepth`/`stateFile` parsing, before
`NewDiskScanner`):

```go
		yieldEvery := viper.GetInt("disk.scan_yield_every")
		yieldSleep := viper.GetDuration("disk.scan_yield_sleep")
		stateSaveInterval := viper.GetDuration("disk.state_save_interval")
		if stateSaveInterval <= 0 {
			stateSaveInterval = 30 * time.Second
		}
		deferFirst := viper.GetBool("disk.scan_defer_on_state")

		opts := monitor.ScanOptions{
			YieldEvery:        yieldEvery,
			YieldSleep:        yieldSleep,
			StateSaveInterval: stateSaveInterval,
			DeferFirstOnState: deferFirst,
		}
		scanner := monitor.NewDiskScanner(scanInterval, scanConcurrency, excludeList, maxDepth, stateFile, opts)
```

In `init()` add defaults (next to the existing `viper.SetDefault("disk.*", ...)`):

```go
	viper.SetDefault("disk.scan_yield_every", 0)
	viper.SetDefault("disk.scan_yield_sleep", 0)
	viper.SetDefault("disk.state_save_interval", 30*time.Second)
	viper.SetDefault("disk.scan_defer_on_state", true)
```

Default behavior is backward compatible: `scan_yield_every=0` and
`scan_yield_sleep=0` mean no yielding (same throughput as today); `defer_first=true`
only changes restart behavior (skips the immediate scan when state exists — metrics
stay populated from persisted state); `state_save_interval=30s` only batches
writes (the file is still written, just less often).

> Operator guidance for slow storage (e.g. arm1): set
> `TOPDATA_AGENT_DISK_SCAN_YIELD_EVERY=200` and
> `TOPDATA_AGENT_DISK_SCAN_YIELD_SLEEP=1ms` to cap walk I/O; the systemd
> `IOSchedulingClass=idle` is the primary guard.

---

## Phase 4 — Unit tests

[NEW FILE] `internal/monitor/disk_scan_test.go` — covers the new behavior without
starting `serve` (keeps the global registry safe, per AGENTS.md). Tests are not
run in parallel (shared package state).

```go
package monitor

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestHasState(t *testing.T) {
	dir := t.TempDir()
	sf := filepath.Join(dir, "state.json")
	d := &DiskScanner{stateFile: sf, mu: sync.Mutex{}, state: map[string]map[string]dirState{}}
	if d.hasState("shop-a") {
		t.Fatal("expected no state for unknown shop")
	}
	d.state["shop-a"] = map[string]dirState{}
	if !d.hasState("shop-a") {
		t.Fatal("expected state after insert")
	}
}

func TestScanOptionsApplied(t *testing.T) {
	d := NewDiskScanner(time.Hour, 1, []string{"var/cache"}, 3, filepath.Join(t.TempDir(), "s.json"), ScanOptions{
		YieldEvery:        100,
		YieldSleep:        time.Millisecond,
		StateSaveInterval: 30 * time.Second,
		DeferFirstOnState: true,
	})
	if d.yieldEvery != 100 || d.yieldSleep != time.Millisecond || d.saveInterval != 30*time.Second || !d.deferFirst {
		t.Fatalf("scan options not applied: %+v", d)
	}
}

func TestScheduleSaveDebounced(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "state.json")
	_ = os.Remove(sf)
	d := &DiskScanner{stateFile: sf, saveInterval: 50 * time.Millisecond, mu: sync.Mutex{}, state: map[string]map[string]dirState{}}
	d.state["s"] = map[string]dirState{"": {Size: 1, ScanTime: time.Now().Unix()}}

	// Rapid calls must not write synchronously.
	for i := 0; i < 5; i++ {
		d.scheduleSave()
	}
	if _, err := os.Stat(sf); !os.IsNotExist(err) {
		t.Fatal("expected debounced write to be delayed, file exists immediately")
	}

	// After the interval elapses, exactly one write should have happened.
	time.Sleep(80 * time.Millisecond)
	if _, err := os.Stat(sf); err != nil {
		t.Fatalf("expected state file written after interval: %v", err)
	}
}

func TestSubtractChildrenResidual(t *testing.T) {
	in := []Grower{
		{Path: "", SizeBytes: 100, GrowthBytesPerHour: 100},
		{Path: "a", SizeBytes: 90, GrowthBytesPerHour: 90},
		{Path: "a/b", SizeBytes: 10, GrowthBytesPerHour: 10},
		{Path: "c", SizeBytes: 5, GrowthBytesPerHour: 5},
	}
	out := subtractChildren(in)
	byPath := map[string]Grower{}
	for _, g := range out {
		byPath[g.Path] = g
	}
	// "" has biggest child "a" (90) -> residual 10.
	if byPath[""].SizeBytes != 10 {
		t.Errorf("root residual size = %d, want 10", byPath[""].SizeBytes)
	}
	// "a" has child "a/b" (10) -> residual 80.
	if byPath["a"].SizeBytes != 80 {
		t.Errorf("a residual size = %d, want 80", byPath["a"].SizeBytes)
	}
	// "c" has no child -> unchanged.
	if byPath["c"].SizeBytes != 5 {
		t.Errorf("c size = %d, want 5", byPath["c"].SizeBytes)
	}
}
```

Run with `go test ./internal/monitor/`.

---

## Phase 5 — Housekeeping

### 5.1 CHANGELOG.md

[MODIFY] `CHANGELOG.md` — the file currently has four duplicated `## [Unreleased]`
headers (lines 33–39). Collapse them into a single `## [Unreleased]` and add the
entries below under it:

```markdown
## [Unreleased]

### Changed
- The disk scanner no longer triggers a full recursive walk of every shop at
  process startup on restarts: when persisted scan state already exists for a
  shop, its first scan after a restart is deferred to the shop's randomized phase.
  This removes the restart→I/O-burst amplifier that contributed to the 2026-08-28
  arm1 disk-I/O outage.
- The disk-state file (`disk.state_file`) is now rewritten at most once per
  `disk.state_save_interval` (default 30s) instead of on every per-shop scan,
  reducing redundant full-file writes on slow storage.
- The systemd unit now sets `IOSchedulingClass=idle` and `IOWeight=10` so the
  agent's disk I/O can never starve the host (see deploy template).

### Added
- Disk-scan I/O throttling knobs (env `TOPDATA_AGENT_DISK_*`): `scan_yield_every`
  (directories between scheduler yields, default 0 = off), `scan_yield_sleep`
  (sleep per yield, e.g. `1ms`, default 0), `state_save_interval` (default 30s),
  and `scan_defer_on_state` (default true).
- Self-observability metrics `topdata_agent_disk_scan_last_duration_seconds`
  (gauge, label `shop`) and `topdata_agent_disk_scan_total` (counter, label
  `shop`) for diagnosing scan cost from Prometheus.
- The deployed `topdata-agent.env.j2` now ships safe disk-scan defaults fleet-wide
  (`disk.scan_defer_on_state=true`, `disk.state_save_interval=30s`,
  `disk.scan_yield_every=100`, `disk.scan_yield_sleep=2ms`) for the slow-storage
  fleet, so the throttle applies to every host on the next deploy without a vault
  edit.
```

### 5.2 README.md

[MODIFY] `README.md` — fix the stale config table and disk description, and add
the new keys. Two concrete corrections required:

- The table currently says `TOPDATA_AGENT_SHOPS_ROOT` default `/srv/topdata-shops`
  — the real default is `/srv/topdata-shops/prod-shops` (changed in v1.0.1).
- The `topdata_agent_shopware_shop_disk_usage_bytes` row says "measured with `du -sb` and
  refreshed hourly" — it is actually a pure-Go recursive walk refreshed on the
  `disk.scan_interval` (default 6h).

Update the config table to include the disk keys and the new I/O-knobs, and add a
note that the systemd unit caps I/O via `IOSchedulingClass=idle`. Concretely,
append these rows to the env-var table:

```markdown
| `TOPDATA_AGENT_DISK_SCAN_INTERVAL` | `6h` | How often each shop's directory tree is walked to refresh disk usage / growth. |
| `TOPDATA_AGENT_DISK_SCAN_CONCURRENCY` | `1` | Max simultaneous shop walks (semaphore). Keep at 1 on slow storage. |
| `TOPDATA_AGENT_DISK_EXCLUDE` | `var/cache` | Comma-separated relative paths skipped from size + growth. |
| `TOPDATA_AGENT_DISK_GROWTH_MAX_DEPTH` | `3` | Depth at which per-directory growth is tracked. |
| `TOPDATA_AGENT_DISK_STATE_FILE` | `/var/lib/topdata-agent/disk-state.json` | Persists per-dir sizes + scan times for cross-restart growth. |
| `TOPDATA_AGENT_DISK_SCAN_YIELD_EVERY` | `0` | Directories walked between scheduler yields (0 = off). Set e.g. `200` on slow storage. |
| `TOPDATA_AGENT_DISK_SCAN_YIELD_SLEEP` | `0` | Sleep applied on each yield (e.g. `1ms`); caps walk I/O. Default off. |
| `TOPDATA_AGENT_DISK_STATE_SAVE_INTERVAL` | `30s` | Minimum interval between state-file rewrites. |
| `TOPDATA_AGENT_DISK_SCAN_DEFER_ON_STATE` | `true` | Skip the immediate startup scan when persisted state exists (prevents restart I/O bursts). |
```

And update the `topdata_agent_shopware_shop_disk_usage_bytes` description row to:
"Disk usage of the shop directory in bytes, measured by a pure-Go recursive walk
(refreshed every `disk.scan_interval`, default 6h; excludes `var/cache`)."

Also update the systemd example block to include the I/O guards so the docs match
the deployed unit:

```ini
[Service]
Type=simple
User=root
EnvironmentFile=/etc/topdata-agent.env
ExecStart=/usr/local/bin/topdata-agent serve
Restart=on-failure
RestartSec=5
IOSchedulingClass=idle
IOWeight=10
```

### 5.3 .gitignore

No repository artifacts are introduced (the state file lives under
`/var/lib/topdata-agent/`, a runtime path, already outside the repo). **No change
required.**

---

## Phase 6 — On-host verification & rollout

This plan is the code half of the fix. To confirm the original hypothesis and
validate the fix, run on arm1 (and arm2) **before and after** deploy:

1. `journalctl -u topdata-agent --since "2026-08-28 19:00"` — count restarts and
   check for `OOMKilled` / `code=killed`. Confirms the restart→burst trigger.
2. `cat /sys/block/vda/queue/rotational` + `lsblk` — confirm storage class (the
   "modest" 14.8 MB/s only downs an SD/HDD-class device).
3. `iotop -p $(pidof topdata-agent)` during a scan — confirm the walk is the I/O
   source and that, post-fix, `IOSchedulingClass=idle` keeps it below other load.
4. In Prometheus, after deploy: confirm `topdata_agent_disk_scan_total` and
   `topdata_agent_disk_scan_last_duration_seconds` populate, and that an agent
   *restart* no longer produces a simultaneous spike in
   `rate(node_disk_read_bytes_total[5m])` across all shops at once.
5. Add a proactive alert (suggested in the analysis report):
   `rate(node_cpu_seconds_total{mode="iowait"}[5m])*100 > 5` and
   `rate(node_disk_read_bytes_total[5m]) > 10e6` for the agent hosts.

Roll out via `./deploy/deploy-to-prod.sh` (or `--limit arm1` first), since the
systemd template change requires the unit to be redeployed and `daemon-reload`ed.

---

## Phase 7 — Implementation report

After implementing Phases 1–5 and verifying Phase 6, write the implementation
report to
`_ai/backlog/reports/260828_2330__IMPLEMENTATION_REPORT__disk-scan-io-throttling.md`
with the required frontmatter (see the instruction template) and the nine content
sections (Summary, Files Changed, Key Changes, Deviations, Technical Decisions,
Testing Notes, Usage Examples, Documentation Updates, Next Steps). Record
`filesCreated`, `filesModified`, `filesDeleted` counts and the final
`status` (`completed` / `partial` / `blocked`).
