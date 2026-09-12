---
title: "Go Node Agent Replacing PHP Node Agent for Shopware Monitoring"
status: Accepted
date: 2026-08-20
deciders: Topdata Team
tags: [golang, monitoring, prometheus, shopware, migration]
id: 5f462a38-1379-40d9-9662-a7723349fe43
content_hash: b09e6d688e4d9d9c025d17c5e57a0872
---

# Go Node Agent Replacing PHP Node Agent for Shopware Monitoring

## Context

Monitoring Shopware 6 instances across servers was fragmented. The PHP-based `node-agent` was heavy, unused, and required a full web-server stack. There was no real-time visibility into `[CRITICAL]` errors in Shopware logs across the fleet, delaying incident response.

## Decision

Replace the PHP agent with `topdata-node-agent`, a single self-contained Go binary (Go 1.21+, Cobra CLI) running as one systemd service with zero runtime dependencies. It auto-discovers active shops in `/srv/topdata-shops/prod-shops/`, tails daily Shopware logs (`vol/www/var/log/prod-YYYY-MM-DD.log`) in real time counting CRITICAL entries, reports per-shop disk usage (`du -sb`), and exposes a Prometheus `/metrics` endpoint on port 9144 secured with Basic Auth. All settings (shops root, credentials, listen address) are configurable via `TOPDATA_AGENT_*` environment variables with documented defaults.

## Consequences

**Positive**
- Single static binary, trivial deployment and updates; no PHP/web-server stack required.
- Real-time CRITICAL error counters enable faster incident response.
- Prometheus-native metrics integrate directly with the existing fleet monitoring.

**Negative**
- Daily log tailing uses the archived `hpcloud/tail` library, requiring a `replace` directive to repair its broken transitive `gopkg.in/fsnotify.v1` dependency (build-time only, no runtime impact).
- Default Basic Auth credentials (`admin`/`fete`) ship in the binary unless overridden via env — operations must set credentials in the systemd `EnvironmentFile`.

## Alternatives Considered

- **Keep/optimize the PHP agent** — rejected: heavy footprint, web-server dependency, and no fleet-wide CRITICAL visibility.
- **Use `github.com/nxadm/tail`** (maintained fork) — rejected for now to stay faithful to the plan's library choice; noted as a follow-up to drop the fsnotify `replace` directive.
- **Config file (`config.yaml`)** — rejected for this iteration; environment variables (via viper) are sufficient and simpler for systemd deployment.

## Related Decisions

None.