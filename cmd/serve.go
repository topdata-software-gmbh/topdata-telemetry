package cmd

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/topdata-software-gmbh/topdata-telemetry/internal/monitor"
)

var startTime = time.Now()

var agentInfo = struct {
	ListenAddress string
	ShopsRoot     string
}{}

// shopCount reports the number of currently monitored shops. It is set by the
// supervisor in Run so the live count is available to the /info handler.
var shopCount func() int = func() int { return 0 }

// lastScanTimes reports the most recent scan timestamp (unix seconds) per shop.
// It is set by the serve command once the disk scanner exists.
var lastScanTimes func() map[string]int64 = func() map[string]int64 { return map[string]int64{} }

// lastDiscovery reports the time of the most recent discovery run. It is set by
// the serve command once the shop supervisor exists.
var lastDiscovery func() time.Time = func() time.Time { return time.Time{} }

// route describes an HTTP endpoint the agent serves. It is the single source of
// truth for both registration (http.Handle) and the startup exports dump, so the
// two can never drift apart.
type route struct {
	path    string
	auth    bool
	handler http.Handler
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the metrics exporter",
	Run: func(cmd *cobra.Command, args []string) {
		if !viper.IsSet("auth.username") || !viper.IsSet("auth.password") {
			log.Fatal("basic auth credentials not configured: set TOPDATA_TELEMETRY_AUTH_USERNAME and TOPDATA_TELEMETRY_AUTH_PASSWORD")
		}

		log.Printf("topdata-telemetry %s starting", version)
		log.Printf("shops root: %s", viper.GetString("shops.root"))

		discoveryInterval := viper.GetDuration("discovery.interval")
		if discoveryInterval <= 0 {
			discoveryInterval = 15 * time.Minute
		}

		agentInfo.ListenAddress = viper.GetString("listen.address")
		agentInfo.ShopsRoot = viper.GetString("shops.root")

		scanInterval := viper.GetDuration("disk.scan_interval")
		if scanInterval <= 0 {
			scanInterval = 6 * time.Hour
		}
		scanConcurrency := viper.GetInt("disk.scan_concurrency")
		if scanConcurrency <= 0 {
			scanConcurrency = 1
		}
		excludeList := viper.GetStringSlice("disk.exclude")
		if len(excludeList) == 0 {
			excludeList = []string{"var/cache"}
		}
		maxDepth := viper.GetInt("disk.growth_max_depth")
		if maxDepth <= 0 {
			maxDepth = 3
		}
		stateFile := viper.GetString("disk.state_file")
		if stateFile == "" {
			stateFile = "/var/lib/topdata-telemetry/disk-state.json"
		}

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
		supervisor := monitor.NewShopSupervisor(viper.GetString("shops.root"), discoveryInterval, scanner)
		shopCount = supervisor.Count
		lastScanTimes = scanner.LastScanTimes
		lastDiscovery = supervisor.LastDiscovery
		go supervisor.Run()

		printConfig()

		routes := []route{
			{path: "/healthz", auth: false, handler: http.HandlerFunc(healthzHandler)},
			{path: "/metrics", auth: true, handler: promhttp.Handler()},
			{path: "/disk-eaters", auth: true, handler: scanner.Handler()},
			{path: "/info", auth: true, handler: http.HandlerFunc(infoHandler)},
			{path: "/critical-errors", auth: true, handler: monitor.CriticalErrorsHandler(startTime)},
		}
		for _, r := range routes {
			if r.auth {
				http.Handle(r.path, authMiddleware(r.handler))
			} else {
				http.Handle(r.path, r.handler)
			}
		}

		printExports(routes)

		log.Printf("listening on %s", viper.GetString("listen.address"))
		log.Fatal(http.ListenAndServe(viper.GetString("listen.address"), nil))
	},
}

// healthzHandler is an unauthenticated liveness probe. It only confirms the
// process is up and listening; it does not depend on any subsystem being ready.
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK\n"))
}

// infoHandler is an authenticated endpoint that reports basic agent info
// (version, uptime, config). Output format follows the same negotiation as
// /disk-eaters: ?format=json|text|markdown, falling back to the Accept header
// and finally JSON.
func infoHandler(w http.ResponseWriter, r *http.Request) {
	data := struct {
		Version       string           `json:"version"`
		UptimeSeconds float64          `json:"uptime_seconds"`
		Uptime        string           `json:"uptime"`
		StartedAt     string           `json:"started_at"`
		ListenAddress string           `json:"listen_address"`
		ShopsRoot     string           `json:"shops_root"`
		ShopsTotal    int              `json:"shops_total"`
		LastScan      map[string]int64 `json:"last_scan"`
		LastDiscovery string           `json:"last_discovery"`
	}{
		Version:       version,
		UptimeSeconds: time.Since(startTime).Seconds(),
		Uptime:        humanDuration(time.Since(startTime)),
		StartedAt:     startTime.UTC().Format(time.RFC3339),
		ListenAddress: agentInfo.ListenAddress,
		ShopsRoot:     agentInfo.ShopsRoot,
		ShopsTotal:    shopCount(),
		LastScan:      lastScanTimes(),
		LastDiscovery: lastDiscovery().UTC().Format(time.RFC3339),
	}

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

	// Sort last_scan entries by scan time (oldest first) for stable output.
	type scanEntry struct {
		shop string
		ts   int64
	}
	var scans []scanEntry
	for shop, ts := range data.LastScan {
		scans = append(scans, scanEntry{shop: shop, ts: ts})
	}
	sort.Slice(scans, func(i, j int) bool { return scans[i].ts < scans[j].ts })

	switch format {
	case "text":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "%-16s %s\n", "version", data.Version)
		fmt.Fprintf(w, "%-16s %s\n", "uptime", data.Uptime)
		fmt.Fprintf(w, "%-16s %s\n", "started_at", utcStamp(startTime))
		fmt.Fprintf(w, "%-16s %s\n", "listen_address", data.ListenAddress)
		fmt.Fprintf(w, "%-16s %s\n", "shops_root", data.ShopsRoot)
		fmt.Fprintf(w, "%-16s %d\n", "shops_total", data.ShopsTotal)
		fmt.Fprintf(w, "%-16s %s\n", "last_discovery", utcStamp(lastDiscovery()))
		if len(scans) > 0 {
			shopW := 0
			for _, e := range scans {
				if len(e.shop) > shopW {
					shopW = len(e.shop)
				}
			}
			fmt.Fprintf(w, "%-16s %-*s %s\n", "last_scan", shopW, "shop", "scanned_at (UTC)")
			for _, e := range scans {
				fmt.Fprintf(w, "%-16s %-*s %s\n", "last_scan", shopW, e.shop, utcStamp(time.Unix(e.ts, 0)))
			}
		}
	case "markdown":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		fmt.Fprintf(w, "| %s | %s |\n", "Field", "Value")
		fmt.Fprintf(w, "| --- | --- |\n")
		fmt.Fprintf(w, "| %s | %s |\n", "version", data.Version)
		fmt.Fprintf(w, "| %s | %s |\n", "uptime", data.Uptime)
		fmt.Fprintf(w, "| %s | %s |\n", "started_at", utcStamp(startTime))
		fmt.Fprintf(w, "| %s | %s |\n", "listen_address", data.ListenAddress)
		fmt.Fprintf(w, "| %s | %s |\n", "shops_root", data.ShopsRoot)
		fmt.Fprintf(w, "| %s | %d |\n", "shops_total", data.ShopsTotal)
		fmt.Fprintf(w, "| %s | %s |\n", "last_discovery", utcStamp(lastDiscovery()))
		for _, e := range scans {
			fmt.Fprintf(w, "| %s | %s %s |\n", "last_scan", e.shop, utcStamp(time.Unix(e.ts, 0)))
		}
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(data)
	}
}

// utcStamp renders a timestamp as UTC in "2006-01-02 15:04:05" form: space as
// the date-time separator and no zone suffix, for predictable cross-host output.
func utcStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

// printConfig logs the resolved agent configuration as a table. Sensitive
// values (auth password) are redacted so they never appear in logs.
func printConfig() {
	type row struct {
		key   string
		value string
	}
	rows := []row{
		{"shops.root", viper.GetString("shops.root")},
		{"listen.address", viper.GetString("listen.address")},
		{"discovery.interval", viper.GetDuration("discovery.interval").String()},
		{"disk.scan_interval", viper.GetDuration("disk.scan_interval").String()},
		{"disk.scan_concurrency", fmt.Sprintf("%d", viper.GetInt("disk.scan_concurrency"))},
		{"disk.exclude", strings.Join(viper.GetStringSlice("disk.exclude"), ",")},
		{"disk.growth_max_depth", fmt.Sprintf("%d", viper.GetInt("disk.growth_max_depth"))},
		{"disk.state_file", viper.GetString("disk.state_file")},
		{"disk.scan_yield_every", fmt.Sprintf("%d", viper.GetInt("disk.scan_yield_every"))},
		{"disk.scan_yield_sleep", viper.GetDuration("disk.scan_yield_sleep").String()},
		{"disk.state_save_interval", viper.GetDuration("disk.state_save_interval").String()},
		{"disk.scan_defer_on_state", fmt.Sprintf("%t", viper.GetBool("disk.scan_defer_on_state"))},
		{"auth.username", viper.GetString("auth.username")},
	}
	pw := viper.GetString("auth.password")
	if pw != "" {
		pw = "*** (redacted)"
	} else {
		pw = "(unset)"
	}
	rows = append(rows, row{"auth.password", pw})

	keyW := 0
	for _, r := range rows {
		if len(r.key) > keyW {
			keyW = len(r.key)
		}
	}
	log.Printf("agent configuration:")
	for _, r := range rows {
		log.Printf("  %-*s = %s", keyW, r.key, r.value)
	}
}

// printExports logs everything the agent exposes at startup: the application
// metrics registered on the global registry (the Go/process/promhttp runtime
// collectors are omitted) and the HTTP endpoints. Enumerating the registry at
// runtime means the dump always reflects the actual built binary, regardless of
// metric-name prefix or later renames.
func printExports(routes []route) {
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		log.Printf("exported metrics: could not gather registry: %v", err)
	} else {
		log.Printf("exported metrics:")
		count := 0
		for _, mf := range mfs {
			if isRuntimeMetric(mf.GetName()) {
				continue
			}
			count++
			log.Printf("  %s [%s] — %s", mf.GetName(), strings.ToLower(mf.GetType().String()), mf.GetHelp())
		}
		if count == 0 {
			log.Printf("  (no application metrics registered)")
		}
	}

	log.Printf("http endpoints:")
	for _, r := range routes {
		auth := "no"
		if r.auth {
			auth = "yes"
		}
		log.Printf("  %-16s auth=%s", r.path, auth)
	}
}

// isRuntimeMetric reports whether a metric name belongs to the collectors
// prometheus/client_golang registers automatically (Go runtime, process, and the
// promhttp handler), which are not part of the agent's own exports.
func isRuntimeMetric(name string) bool {
	return strings.HasPrefix(name, "go_") ||
		strings.HasPrefix(name, "process_") ||
		strings.HasPrefix(name, "promhttp_")
}

func humanDuration(d time.Duration) string {
	if d < time.Second {
		return "0s"
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	h := int(d / time.Hour)
	d -= time.Duration(h) * time.Hour
	m := int(d / time.Minute)
	d -= time.Duration(m) * time.Minute
	s := int(d / time.Second)
	parts := []string{}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if h > 0 || days > 0 {
		parts = append(parts, fmt.Sprintf("%dh", h))
	}
	if m > 0 || h > 0 || days > 0 {
		parts = append(parts, fmt.Sprintf("%dm", m))
	}
	parts = append(parts, fmt.Sprintf("%ds", s))
	return strings.Join(parts, " ")
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != viper.GetString("auth.username") || pass != viper.GetString("auth.password") {
			w.Header().Set("WWW-Authenticate", `Basic realm="metrics"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func init() {
	viper.SetDefault("shops.root", "/srv/topdata-shops/prod-shops")
	viper.SetDefault("listen.address", ":9144")
	viper.SetDefault("disk.scan_interval", 6*time.Hour)
	viper.SetDefault("disk.scan_concurrency", 1)
	viper.SetDefault("disk.exclude", []string{"var/cache"})
	viper.SetDefault("disk.growth_max_depth", 3)
	viper.SetDefault("disk.state_file", "/var/lib/topdata-telemetry/disk-state.json")
	viper.SetDefault("disk.scan_yield_every", 0)
	viper.SetDefault("disk.scan_yield_sleep", 0)
	viper.SetDefault("disk.state_save_interval", 30*time.Second)
	viper.SetDefault("disk.scan_defer_on_state", true)
	viper.SetDefault("discovery.interval", 15*time.Minute)
	viper.SetEnvPrefix("TOPDATA_TELEMETRY")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	serveCmd.Flags().String("shops-root", viper.GetString("shops.root"), "Directory containing shop folders")
	viper.BindPFlag("shops.root", serveCmd.Flags().Lookup("shops-root"))
	serveCmd.Flags().String("listen-address", viper.GetString("listen.address"), "Address to listen on")
	viper.BindPFlag("listen.address", serveCmd.Flags().Lookup("listen-address"))

	rootCmd.AddCommand(serveCmd)
}
