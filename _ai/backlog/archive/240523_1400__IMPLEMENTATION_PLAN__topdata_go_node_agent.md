---
filename: "_ai/backlog/active/240523_1400__IMPLEMENTATION_PLAN__topdata_go_node_agent.md"
title: "Implementation of Topdata Go Node Agent for Shopware Log Monitoring and Host Metrics"
createdAt: 2024-05-23 14:00
updatedAt: 2024-05-23 14:00
status: completed
completedAt: 2026-08-20 20:19
priority: high
tags: [golang, prometheus, shopware, monitoring, cobra]
estimatedComplexity: moderate
documentRevision: 1
documentType: IMPLEMENTATION_PLAN
id: b0423b57-dd89-4fd2-9f60-cc0e4e90b453
content_hash: 149bdc3ec2c8ec33ef344cb14d80d34e
---

## 1. Problem Description
Monitoring multiple Shopware 6 instances across different servers is currently fragmented. The existing PHP-based `node-agent` is heavy, unused, and requires a full web-server stack. Furthermore, there is no real-time visibility into `[CRITICAL]` errors occurring in Shopware logs across the fleet, leading to delayed incident response.

## 2. Executive Summary
This plan introduces `topdata-node-agent`, a self-sufficient Go-based binary. It will:
1.  **Auto-discover** active Shopware 6 shops in `/srv/topdata-shops/prod-shops/`.
2.  **Monitor** Shopware logs in real-time using high-efficiency tailing to count critical errors.
3.  **Migrate** key features from the old PHP agent (Disk usage, Host statistics).
4.  **Expose** a Prometheus-compatible `/metrics` endpoint secured with Basic Auth.
5.  **Run** as a single systemd service with zero external runtime dependencies.

## 3. Project Environment
- **Project Name**: Topdata Node Agent (Go)
- **Primary Language**: Go 1.21+
- **CLI Framework**: Cobra
- **Metrics**: Prometheus Client Golang
- **Target OS**: Linux (Debian/Ubuntu/Bookworm)

---

## 4. Phase 1: Project Initialization & CLI Structure
Setup the Go module and the Cobra command structure.

### [NEW FILE] `go.mod`
```go
module github.com/topdata/node-agent

go 1.21

require (
	github.com/spf13/cobra v1.8.0
	github.com/spf13/viper v1.18.2
	github.com/prometheus/client_golang v1.19.0
	github.com/hpcloud/tail v1.0.0
)
```

### [NEW FILE] `main.go`
```go
package main

import "github.com/topdata/node-agent/cmd"

func main() {
	cmd.Execute()
}
```

### [NEW FILE] `cmd/root.go`
```go
package cmd

import (
	"fmt"
	"os"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "topdata-agent",
	Short: "Topdata Node Agent for Shopware Monitoring",
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
```

---

## 5. Phase 2: Discovery & Log Monitoring
Implement the logic to scan `/srv/topdata-shops/prod-shops/` and start tailing Shopware logs.

### [NEW FILE] `internal/discovery/discovery.go`
```go
package discovery

import (
	"os"
	"path/filepath"
)

type Shop struct {
	Name    string
	Path    string
	LogPath string
}

func FindShops(root string) ([]Shop, error) {
	var shops []Shop
	// Only scan prod-shops
	entries, err := os.ReadDir(filepath.Join(root, "prod-shops"))
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			shopPath := filepath.Join(root, "prod-shops", entry.Name())
			// Shopware 6 log path pattern
			logDir := filepath.Join(shopPath, "var/log")
			if _, err := os.Stat(logDir); err == nil {
				shops = append(shops, Shop{
					Name:    entry.Name(),
					Path:    shopPath,
					LogPath: logDir,
				})
			}
		}
	}
	return shops, nil
}
```

### [NEW FILE] `internal/monitor/log_monitor.go`
```go
package monitor

import (
	"strings"
	"time"
	"fmt"
	"github.com/hpcloud/tail"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	criticalErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "topdata_agent_shopware_critical_errors_total",
		Help: "Total number of critical errors in Shopware logs",
	}, []string{"shop"})
)

func TailLog(shopName, logDir string) {
	for {
		// Calculate current log filename: prod-YYYY-MM-DD.log
		currentLog := fmt.Sprintf("%s/prod-%s.log", logDir, time.Now().Format("2006-01-02"))
		
		t, err := tail.TailFile(currentLog, tail.Config{
			Follow:   true,
			ReOpen:   true, // Handles rotation at midnight
			MustExist: false,
			Poll:      true,
		})
		
		if err != nil {
			time.Sleep(10 * time.Second)
			continue
		}

		for line := range t.Lines {
			if strings.Contains(line.Text, ".CRITICAL:") || strings.Contains(line.Text, "[critical]") {
				criticalErrors.WithLabelValues(shopName).Inc()
			}
		}
	}
}
```

---

## 6. Phase 3: Porting PHP Agent Features (Metadata & Stats)
Migrate `Disk Usage` and `Host Stats` logic into Go.

### [NEW FILE] `internal/monitor/host_monitor.go`
```go
package monitor

import (
	"os/exec"
	"strconv"
	"strings"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	diskUsage = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "topdata_agent_shopware_shop_disk_usage_bytes",
		Help: "Disk usage of the shop directory in bytes",
	}, []string{"shop"})
)

func UpdateDiskUsage(shopName, shopPath string) {
	// Equivalent to the PHP du -sb logic
	out, err := exec.Command("du", "-sb", shopPath).Output()
	if err == nil {
		parts := strings.Fields(string(out))
		if len(parts) > 0 {
			bytes, _ := strconv.ParseFloat(parts[0], 64)
			diskUsage.WithLabelValues(shopName).Set(bytes)
		}
	}
}
```

---

## 7. Phase 4: Secure Metrics Endpoint
Implement the server with Basic Auth.

### [NEW FILE] `cmd/serve.go`
```go
package cmd

import (
	"net/http"
	"github.com/spf13/cobra"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/topdata/node-agent/internal/discovery"
	"github.com/topdata/node-agent/internal/monitor"
	"time"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the metrics exporter",
	Run: func(cmd *cobra.Command, args []string) {
		shops, _ := discovery.FindShops("/srv/topdata-shops")

		for _, shop := range shops {
			go monitor.TailLog(shop.Name, shop.LogPath)
			// Background ticker for expensive du command
			go func(s discovery.Shop) {
				for {
					monitor.UpdateDiskUsage(s.Name, s.Path)
					time.Sleep(1 * time.Hour)
				}
			}(shop)
		}

		http.Handle("/metrics", authMiddleware(promhttp.Handler()))
		http.ListenAndServe(":9144", nil)
	},
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		// Logic to be moved to config/env
		if !ok || user != "admin" || pass != "fete" {
			w.Header().Set("WWW-Authenticate", `Basic realm="metrics"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func init() {
	rootCmd.AddCommand(serveCmd)
}
```

---

## 8. Phase 5: Housekeeping & Documentation

### [MODIFY] `.gitignore`
- Add `/bin/`
- Add `*.env`
- Add `dist/`

### [MODIFY] `README.md`
- Documentation of the new Go-based architecture.
- Setup instructions for `systemd`.
- Description of Prometheus metrics available.

### [NEW FILE] `CHANGELOG.md`
- Initial release of Go Node Agent.
- Migration of log monitoring and disk usage tracking from PHP.

---

## 9. Final Phase: Implementation Report
A report will be generated in `_ai/backlog/reports/` detailing the performance gain and the final security implementation.

---

### Discussion Points before proceeding:
1.  **Docker Support**: The old PHP agent used the Docker SDK. Do you want the Go agent to also list Docker container statuses (Running/Exited) for each shop?
2.  **Config**: Should I implement a `config.yaml` or just use Environment Variables for the Basic Auth credentials and the Root Directory?
3.  **Retired Shops**: Confirming we ignore `/srv/topdata-shops/retired-shops/` entirely for metrics.

**What do you think? Shall we proceed with this plan?**
