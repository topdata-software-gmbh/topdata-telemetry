package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/topdata-software-gmbh/topdata-telemetry/internal/discovery"
)

var diskUsage = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "topdata_telemetry_shopware_shop_disk_usage_bytes",
	Help: "Disk usage of the shop directory in bytes (excluding configured dirs)",
}, []string{"shop"})

var scanDuration = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "topdata_telemetry_disk_scan_last_duration_seconds",
	Help: "Duration of the most recent disk scan for a shop, in seconds.",
}, []string{"shop"})

var scanTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "topdata_telemetry_disk_scan_total",
	Help: "Total number of disk scans performed per shop.",
}, []string{"shop"})

type dirState struct {
	Size     int64 `json:"size"`
	ScanTime int64 `json:"scan_time"` // unix seconds of the scan that produced Size
}

// Grower is one directory ranked by disk-growth on the /disk-eaters endpoint.
type Grower struct {
	Shop               string  `json:"shop"`
	Path               string  `json:"path"`
	SizeBytes          int64   `json:"size_bytes"`
	GrowthBytesPerHour float64 `json:"growth_bytes_per_hour"`
	LastScanAgeSeconds float64 `json:"last_scan_age_seconds"`
}

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

func maxDepthOrOne(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func (d *DiskScanner) loadState() {
	data, err := os.ReadFile(d.stateFile)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &d.state); err != nil {
		return
	}
	// Rehydrate the disk-usage gauge and the /disk-eaters ranking from persisted
	// sizes so a deferred restart does not leave metrics blank for up to one
	// scan interval. Growth-per-hour needs two scans and therefore repopulates
	// on the next walk (left at 0 here).
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for shop, shopState := range d.state {
		var list []Grower
		for rel, ds := range shopState {
			if rel == "" {
				diskUsage.WithLabelValues(shop).Set(float64(ds.Size))
				continue
			}
			list = append(list, Grower{
				Shop:               shop,
				Path:               rel,
				SizeBytes:          ds.Size,
				GrowthBytesPerHour: 0,
				LastScanAgeSeconds: now.Sub(time.Unix(ds.ScanTime, 0)).Seconds(),
			})
		}
		d.growers[shop] = list
	}
}

func (d *DiskScanner) saveState() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(d.stateFile), 0o755); err != nil {
		log.Printf("disk scanner: cannot create state dir: %v", err)
		return
	}
	data, err := json.Marshal(d.state)
	if err != nil {
		return
	}
	if err := os.WriteFile(d.stateFile, data, 0o644); err != nil {
		log.Printf("disk scanner: cannot write state file: %v", err)
	}
}

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

func (d *DiskScanner) isExcluded(rel string) bool {
	for _, ex := range d.excludes {
		ex = strings.Trim(ex, "/")
		if ex == "" {
			continue
		}
		if rel == ex || strings.HasPrefix(rel, ex+"/") {
			return true
		}
	}
	return false
}

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

func (d *DiskScanner) recordGrowth(shop string, sizes map[string]int64, now time.Time) {
	d.mu.Lock()
	shopState, ok := d.state[shop]
	if !ok {
		shopState = map[string]dirState{}
		d.state[shop] = shopState
	}

	var list []Grower
	for rel, sz := range sizes {
		if rel == "" {
			// root size is the gauge; skip it from the ranking list
			shopState[rel] = dirState{Size: sz, ScanTime: now.Unix()}
			continue
		}
		prev, had := shopState[rel]
		var growth, age float64
		if had && prev.ScanTime > 0 {
			elapsed := now.Sub(time.Unix(prev.ScanTime, 0))
			if h := elapsed.Hours(); h > 0 {
				growth = float64(sz-prev.Size) / h
				age = elapsed.Seconds()
			}
		}
		shopState[rel] = dirState{Size: sz, ScanTime: now.Unix()}
		list = append(list, Grower{
			Shop:               shop,
			Path:               rel,
			SizeBytes:          sz,
			GrowthBytesPerHour: growth,
			LastScanAgeSeconds: age,
		})
	}
	d.growers[shop] = list
	d.mu.Unlock()

	d.scheduleSave()
}

// subtractChildren rewrites each directory's size and growth to exclude its
// single biggest direct child (by size). A parent therefore reports only the
// residual that is not already explained by a child entry, which keeps the
// ranking informative instead of echoing the same big directory at every
// ancestor level.
func subtractChildren(growers []Grower) []Grower {
	gross := make(map[string]Grower, len(growers))
	for _, g := range growers {
		gross[g.Path] = g
	}
	out := make([]Grower, len(growers))
	for i, g := range growers {
		ng := g
		prefix := g.Path
		if prefix != "" {
			prefix += "/"
		}
		var best Grower
		found := false
		for _, c := range gross {
			if c.Path == g.Path {
				continue
			}
			if !strings.HasPrefix(c.Path, prefix) {
				continue
			}
			rest := c.Path[len(prefix):]
			if strings.Contains(rest, "/") {
				continue // only immediate children
			}
			if !found || c.SizeBytes > best.SizeBytes {
				best = c
				found = true
			}
		}
		if found {
			ng.SizeBytes -= best.SizeBytes
			ng.GrowthBytesPerHour -= best.GrowthBytesPerHour
		}
		out[i] = ng
	}
	return out
}

// TopGrowers returns the ranked directories, optionally scoped to one shop.
func (d *DiskScanner) TopGrowers(shop string, top int, by string) []Grower {
	d.mu.Lock()
	var groups [][]Grower
	if shop == "" {
		for _, l := range d.growers {
			groups = append(groups, l)
		}
	} else {
		groups = append(groups, d.growers[shop])
	}
	d.mu.Unlock()

	var all []Grower
	for _, g := range groups {
		all = append(all, subtractChildren(g)...)
	}

	switch by {
	case "size":
		sort.SliceStable(all, func(i, j int) bool {
			if all[i].SizeBytes == all[j].SizeBytes {
				return all[i].GrowthBytesPerHour > all[j].GrowthBytesPerHour
			}
			return all[i].SizeBytes > all[j].SizeBytes
		})
	default: // "rate" / "delta" — growth per hour
		sort.SliceStable(all, func(i, j int) bool {
			if all[i].GrowthBytesPerHour == all[j].GrowthBytesPerHour {
				return all[i].SizeBytes > all[j].SizeBytes
			}
			return all[i].GrowthBytesPerHour > all[j].GrowthBytesPerHour
		})
	}

	if top > 0 && len(all) > top {
		all = all[:top]
	}
	return all
}

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

// LastScanTimes returns, for each tracked shop, the unix timestamp (seconds) of
// the most recent directory scan. A shop that has never been scanned is absent.
func (d *DiskScanner) LastScanTimes() map[string]int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]int64{}
	for shop, shopState := range d.state {
		var latest int64
		for _, ds := range shopState {
			if ds.ScanTime > latest {
				latest = ds.ScanTime
			}
		}
		if latest > 0 {
			out[shop] = latest
		}
	}
	return out
}

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

// humanBytes renders a byte count as a short human-readable string (e.g. 1.5G).
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(b)/float64(div), "KMGTPE"[exp])
}

// Handler serves the /disk-eaters ranking behind basic auth.
//
// Output format is chosen by `?format=json|text|markdown`; if absent it falls
// back to content negotiation via the Accept header (text/plain or
// text/markdown) and finally defaults to JSON. This lets the listing be viewed
// directly in a browser by appending `?format=text` or `?format=markdown`.
func (d *DiskScanner) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shop := r.URL.Query().Get("shop")
		top := 20
		if v := r.URL.Query().Get("top"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				top = n
			}
		}
		by := r.URL.Query().Get("by")
		if by == "" {
			by = "rate"
		}
		growers := d.TopGrowers(shop, top, by)

		format := r.URL.Query().Get("format")
		if format == "" {
			accept := r.Header.Get("Accept")
			switch {
			case strings.Contains(accept, "text/markdown"):
				format = "markdown"
			case strings.Contains(accept, "text/plain"):
				format = "text"
			default:
				format = "json"
			}
		}

		switch format {
		case "text":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "%-28s %-42s %12s %12s\n", "SHOP", "PATH", "SIZE", "GROWTH/h")
			for _, g := range growers {
				fmt.Fprintf(w, "%-28s %-42s %12s %12s\n", g.Shop, g.Path, humanBytes(g.SizeBytes), humanBytes(int64(g.GrowthBytesPerHour)))
			}
		case "markdown":
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			fmt.Fprintf(w, "| %s | %s | %s | %s |\n", "Shop", "Path", "Size", "Growth/h")
			fmt.Fprintf(w, "| --- | --- | ---: | ---: |\n")
			for _, g := range growers {
				fmt.Fprintf(w, "| %s | %s | %s | %s |\n", g.Shop, g.Path, humanBytes(g.SizeBytes), humanBytes(int64(g.GrowthBytesPerHour)))
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(growers)
		}
	})
}
