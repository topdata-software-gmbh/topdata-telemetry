package monitor

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hpcloud/tail"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	criticalErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "topdata_telemetry_shopware_critical_errors_total",
		Help: "Total number of critical errors in Shopware logs",
	}, []string{"shop"})
)

// TailLog tails the shop's daily Shopware log until ctx is cancelled. It counts
// lines matching .CRITICAL: or [critical] into the critical-errors counter and
// restarts the tail at midnight to follow the new daily file. The midnight
// rotation watchdog and Poll:true behaviour are preserved verbatim.
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
				// Silence the library's internal debug logging ("Seeked ...",
				// "Re-opening ...", etc.) which otherwise floods the agent log.
				Logger: tail.DiscardingLogger,
				// Start at the end of the file so a (re)start does not replay
				// the day's already-logged CRITICAL lines; the counter begins
				// from 0 (new lines only).
				Location: &tail.SeekInfo{Offset: 0, Whence: io.SeekEnd},
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
						recordCritical(shopName, line.Text)
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

// RemoveShopLog deletes the critical-errors series and buffered error lines
// for a removed shop.
func RemoveShopLog(shop string) {
	criticalErrors.DeleteLabelValues(shop)
	removeShopCritical(shop)
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
