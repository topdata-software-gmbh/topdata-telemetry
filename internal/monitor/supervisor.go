package monitor

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/topdata-software-gmbh/topdata-telemetry/internal/discovery"
)

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

	lastDiscoveryMu sync.Mutex
	lastDiscovery   time.Time
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

// LastDiscovery returns the time of the most recent discovery/reconcile run.
// It is the zero time if discovery has not run yet.
func (s *ShopSupervisor) LastDiscovery() time.Time {
	s.lastDiscoveryMu.Lock()
	defer s.lastDiscoveryMu.Unlock()
	return s.lastDiscovery
}

// reconcile discovers shops on disk and diffs them against the running set.
func (s *ShopSupervisor) reconcile() {
	s.lastDiscoveryMu.Lock()
	s.lastDiscovery = time.Now()
	s.lastDiscoveryMu.Unlock()

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
