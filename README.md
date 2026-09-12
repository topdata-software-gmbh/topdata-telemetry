# topdata-node-agent

A self-sufficient Go-based monitoring agent for Shopware 6 instances. It replaces
the previous PHP-based `node-agent` and runs as a single binary with zero external
runtime dependencies.

## Design invariant: strictly read-only

This agent is the **telemetry plane** and is **strictly read-only by design**.
It observes and reports; it never writes to shop directories, never executes
remote commands, and exposes no `exec`, deploy, or write endpoints. Deployment
and orchestration belong to a separate **control agent** with its own, stronger
authentication — never to this process.

This split is deliberate: telemetry must survive a failed deployment. If the
agent that deploys is also the agent that monitors, a crash or I/O lockup during
a release blinds you exactly when you need visibility most. Keeping monitoring
read-only also keeps its credential low-risk — Basic Auth on this agent can
never be turned into fleet-wide remote code execution. Any change that adds a
write, exec, or deployment capability here is an architectural bug: it belongs
in the control agent instead.

## Architecture

- **Discovery**: periodically scans `shops.root` for active Shopware 6 shops
  (identified by a `vol/www/var/log` directory), every `discovery.interval`
  (default 15m). Each matching subdirectory is treated as one shop. Shops added
  after startup are picked up automatically and removed shops are stopped (their
  metric series deleted) without restarting the agent.
- **Log monitoring**: tails each shop's `vol/www/var/log/prod-YYYY-MM-DD.log` in real-time
  and counts `[CRITICAL]` entries, keeping the last 100 full error lines per shop in
  memory for the `/critical-errors` endpoint. The tail automatically restarts when the
  date changes to follow the new daily log file.
- **Host statistics**: periodically reports the disk usage of each shop directory.
- **Metrics endpoint**: exposes Prometheus-compatible metrics on port `9144`,
  protected with HTTP Basic Auth.

## Configuration

All settings are configurable via environment variables (prefix `TOPDATA_AGENT`),
with the following defaults:

| Env var | Default | Description |
|---|---|---|
| `TOPDATA_AGENT_SHOPS_ROOT` | `/srv/topdata-shops/prod-shops` | Root directory containing the shop folders |
| `TOPDATA_AGENT_AUTH_USERNAME` | *(required)* | Basic Auth username for `/metrics` |
| `TOPDATA_AGENT_AUTH_PASSWORD` | *(required)* | Basic Auth password for `/metrics` |
| `TOPDATA_AGENT_LISTEN_ADDRESS` | `:9144` | Listen address of the metrics endpoint |
| `TOPDATA_AGENT_DISCOVERY_INTERVAL` | `15m` | How often the agent re-scans `shops.root` for added/removed shops. Shops added later are monitored automatically; removed shops are stopped and their metric series deleted — no service restart required. |
| `TOPDATA_AGENT_DISK_SCAN_INTERVAL` | `6h` | How often each shop's directory tree is walked to refresh disk usage / growth. |
| `TOPDATA_AGENT_DISK_SCAN_CONCURRENCY` | `1` | Max simultaneous shop walks (semaphore). Keep at 1 on slow storage. |
| `TOPDATA_AGENT_DISK_EXCLUDE` | `var/cache` | Comma-separated relative paths skipped from size + growth. |
| `TOPDATA_AGENT_DISK_GROWTH_MAX_DEPTH` | `3` | Depth at which per-directory growth is tracked. |
| `TOPDATA_AGENT_DISK_STATE_FILE` | `/var/lib/topdata-agent/disk-state.json` | Persists per-dir sizes + scan times for cross-restart growth. |
| `TOPDATA_AGENT_DISK_SCAN_YIELD_EVERY` | `0` | Directories walked between scheduler yields (0 = off). Set e.g. `200` on slow storage. |
| `TOPDATA_AGENT_DISK_SCAN_YIELD_SLEEP` | `0` | Sleep applied on each yield (e.g. `1ms`); caps walk I/O. Default off. |
| `TOPDATA_AGENT_DISK_STATE_SAVE_INTERVAL` | `30s` | Minimum interval between state-file rewrites. |
| `TOPDATA_AGENT_DISK_SCAN_DEFER_ON_STATE` | `true` | Skip the immediate startup scan when persisted state exists (prevents restart I/O bursts). |

> `TOPDATA_AGENT_AUTH_USERNAME` and `TOPDATA_AGENT_AUTH_PASSWORD` are required —
> the agent refuses to start without them. Set them via the systemd
> `EnvironmentFile` (deployed by the Ansible playbook from the vault).

## Metrics

The agent registers the following metrics (via `prometheus/client_golang`, on the
global default registry). All metrics except `topdata_agent_shops_total` carry a
`shop` label holding the shop's directory name under `shops.root`.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `topdata_agent_shopware_critical_errors_total` | Counter | `shop` | Total number of critical errors found in the shop's `vol/www/var/log/prod-YYYY-MM-DD.log`. A line counts when it matches `.CRITICAL:` or `[critical]` (case-insensitive). Monotonic — it only increases for the lifetime of the process. |
| `topdata_agent_shopware_shop_disk_usage_bytes` | Gauge | `shop` | Disk usage of the shop directory in bytes, measured by a pure-Go recursive walk (refreshed every `disk.scan_interval`, default 6h; excludes `var/cache`). |
| `topdata_agent_shops_total` | Gauge | — | Total number of Shopware shops currently monitored by the agent. |
| `topdata_agent_disk_scan_last_duration_seconds` | Gauge | `shop` | Duration of the most recent disk scan for a shop, in seconds. |
| `topdata_agent_disk_scan_total` | Counter | `shop` | Total number of disk scans performed per shop. |

> The `shop` label value is the directory name discovered under `shops.root`
> (e.g. `muster-shop`), not the full path.

## Recent critical errors

```sh
curl -su USER:PASSWORD 'http://host:9144/critical-errors'                 # all shops, JSON
curl -su USER:PASSWORD 'http://host:9144/critical-errors?shop=muster-shop'
curl -su USER:PASSWORD 'http://host:9144/critical-errors?limit=5&format=markdown'
```

Returns the last critical log lines per shop (up to 100 kept in memory, full
untruncated messages, newest first). Useful for quickly answering "what just
broke?" without SSH. History starts empty after an agent restart;
`agent_started_at` in the response tells you since when.

## Prometheus

Scrape target: `http://USERNAME:PASSWORD@host:9144/metrics`

Example `prometheus.yml` scrape config:

```yaml
scrape_configs:
  - job_name: topdata-agent
    metrics_path: /metrics
    static_configs:
      - targets:
          - arm1.srv.topinfra.de:9144
          - arm2.srv.topinfra.de:9144
          # ... all hosts from deploy/hosts.ini
    basic_auth:
      username: USERNAME
      password: PASSWORD
```

The `/metrics`, `/disk-eaters`, `/info` and `/critical-errors` endpoints
require Basic Auth. `/info` and `/disk-eaters` support
`?format=json|text|markdown` (plus `Accept` negotiation). The `/healthz`
endpoint is **unauthenticated** and only returns `200 OK` while the process
is listening — use it for liveness checks and the Ansible deploy smoke test.

## Deployment (Ansible)

The agent is deployed to all shop-hosting servers via the playbook in `deploy/`.
Secrets live in an ansible-vault file at `deploy/vars/vault.yml` (gitignored).
Create it once, then edit it whenever the credentials or the shops root change.

### Creating the vault

```sh
ansible-vault create deploy/vars/vault.yml
```

Use the keys documented in `deploy/vars/vault.example.yml`:

```yaml
auth_username: topdata
auth_password: CHANGE_ME
shops_root: /srv/topdata-shops/prod-shops
```

### Editing the vault

Re-open the encrypted file with:

```sh
ansible-vault edit deploy/vars/vault.yml
```

### Other useful commands

```sh
ansible-vault view deploy/vars/vault.yml          # print decrypted contents
ansible-vault rekey deploy/vars/vault.yml         # change the vault password
```

### Deploying

`./deploy/deploy-to-prod.sh` cross-compiles both architectures into
`deploy/bin/` and runs the playbook with `--ask-vault-pass`, which prompts for
the vault password used to decrypt `deploy/vars/vault.yml` in memory.

```sh
./deploy/deploy-to-prod.sh
# or, against a single host:
ansible-playbook -i deploy/hosts.ini deploy/playbook-deploy.yaml \
  --ask-vault-pass --limit arm6
```

## Build & Run

```sh
go build -o bin/topdata-agent .
./bin/topdata-agent serve
```

## systemd Service

`/etc/systemd/system/topdata-agent.service`:

```ini
[Unit]
Description=Topdata Node Agent for Shopware Monitoring
After=network.target

[Service]
Type=simple
User=root
EnvironmentFile=/etc/topdata-agent.env
ExecStart=/usr/local/bin/topdata-agent serve
Restart=on-failure
RestartSec=5
# Bound the agent's disk I/O so a full directory walk can never saturate the
# host's storage. The agent only consumes disk bandwidth that no other process
# wants. Rendered by the deploy template; keep in sync with it.
IOSchedulingClass=idle
IOWeight=10

[Install]
WantedBy=multi-user.target
```

```sh
sudo cp bin/topdata-agent /usr/local/bin/topdata-agent
sudo systemctl daemon-reload
sudo systemctl enable --now topdata-agent
```