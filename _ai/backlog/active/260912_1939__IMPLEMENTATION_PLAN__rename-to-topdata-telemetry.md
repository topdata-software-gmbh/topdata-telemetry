---
title: "Implementation Plan: Rename project to topdata-telemetry (full rename incl. metrics)"
date: 2026-09-12
status: active
---

# Goal

Rename the project from `topdata-node-agent` / `topdata-agent` to
`topdata-telemetry` everywhere: GitHub repo, Go module, binary, systemd unit,
env prefix, metrics namespace, state path, and docs. No functional change — the
agent stays strictly read-only (see
`_ai/technical_decisions/ADR__260912-1__read-only-monitoring-agent.md`).

This is a breaking change (env prefix + metric names) and ships as **v2.0.0**.

# Identity matrix

| Surface | Old | New |
|---|---|---|
| GitHub repo | `topdata-node-agent-v2` | `topdata-telemetry` |
| Go module | `github.com/topdata/node-agent` | `github.com/topdata-software-gmbh/topdata-telemetry` |
| Binary | `topdata-agent` | `topdata-telemetry` |
| systemd unit | `topdata-agent.service` | `topdata-telemetry.service` |
| Env file | `/etc/topdata-agent.env` | `/etc/topdata-telemetry.env` |
| Env prefix | `TOPDATA_AGENT_*` | `TOPDATA_TELEMETRY_*` |
| Metrics | `topdata_agent_*` | `topdata_telemetry_*` |
| State file | `/var/lib/topdata-agent/disk-state.json` | `/var/lib/topdata-telemetry/disk-state.json` |
| Build artifacts | `deploy/bin/topdata-agent-{arm64,amd64}` | `deploy/bin/topdata-telemetry-{arm64,amd64}` |
| Prometheus job | `job_name: topdata-agent` | `job_name: topdata-telemetry` |
| Control plane | `.ctx.yaml project_id: topdata-node-agent-v2` | `topdata-telemetry` |

Module path rationale: match the real VCS location (`topdata-software-gmbh`)
instead of the current unresolved short path; cheaper to fix now than later.
Alternative (short prefix `github.com/topdata/telemetry`) rejected as it keeps
the repo/module mismatch.

# Changes

## 1. Go module + code

- `go.mod`: module path; update imports in `main.go`, `cmd/serve.go`,
  `internal/monitor/*.go`, `internal/discovery/*.go` (build catches stragglers).
- `cmd/root.go`: `Use: "topdata-telemetry"`, Short description
  (`"Read-only Shopware fleet telemetry agent"`).
- `cmd/serve.go`: startup log name, `viper.SetEnvPrefix("TOPDATA_TELEMETRY")`,
  required-credentials error message, state-file defaults (2 occurrences).
- Metric definitions `topdata_agent_*` → `topdata_telemetry_*` in
  `internal/monitor/{shops,log_monitor,disk_scan}.go` (+ help strings, tests).
- No endpoint/behavior changes.

## 2. Deploy + systemd

- Rename templates `topdata-agent.env.j2` → `topdata-telemetry.env.j2`,
  `topdata-agent.service.j2` → `topdata-telemetry.service.j2`; update env var
  names in the env template and unit `Description`/`EnvironmentFile`/
  `ExecStart`.
- `playbook-deploy.yaml`: vars (`agent_bin`, `agent_env_file`, `agent_unit`),
  artifact names, service name, metrics smoke-check prefix.
- **One-time migration tasks (order matters — old unit would hold :9144):**
  1. `stat` legacy unit; if present: stop + disable `topdata-agent`, remove
     legacy unit file, `daemon-reload`.
  2. Create `/var/lib/topdata-telemetry`; if legacy `disk-state.json` exists and
     new one does not, move it (preserves growth continuity).
  3. Install/enable/start `topdata-telemetry`.
- `deploy-to-prod.sh`: ldflags module path, artifact names, echo/help text,
  Prometheus snippet job name.
- `.gitignore`: drop the stale `node-agent` entry (replace with the new binary
  name if it is meant as a stray-binary guard — check intent).

## 3. Repo + project metadata + docs

- `gh repo rename topdata-telemetry`; update local remote (`set-url`); GitHub
  keeps redirects for the old slug. Update `.ctx.yaml` project_id and the
  control-plane registry entry if it is keyed by project_id.
- README: title, intro ("formerly `topdata-node-agent`"), env table, metrics
  table, build/run, systemd unit path, Prometheus job name.
- AGENTS.md: title of binary, env prefix, metric names, state path, plan/ADR
  references.
- CHANGELOG: one `### Changed` entry under `[Unreleased]` describing the
  breaking rename; historical entries stay untouched.
- `_ai/` history (old ADRs, lessons, archived plans/reports) is append-only —
  leave old names there.

## 4. External consumers (land with the rollout)

- Prometheus scrape file `config/scrapes/topdata-agent.yaml` → job/targets
  renamed.
- Grafana dashboards, alert rules, recording rules referencing
  `topdata_agent_*` → `topdata_telemetry_*`. This is the highest-risk item and
  must be prepared before the fleet rollout.

# Rollout

1. Land rename on `main`, run `scripts/deploy/deploy-next-version.sh` → Major →
   v2.0.0 (CHANGELOG rotation, tag, build, ansible deploy).
2. Test one host first: `./deploy/deploy-to-prod.sh --limit arm1`; verify new
   unit active, old unit gone, `/healthz` 200, `topdata_telemetry_` in
   `/metrics`, state file moved, credentials still required.
3. Fleet deploy; update Prometheus + Grafana/alerts in the same window.
4. Rollback (if needed): checkout previous tag and deploy with the old playbook
   (unit names/prefix revert; state file move is idempotent). Note: rollback
   path is manual — old tag's playbook only.

# Validation

- `go build ./...`, `go vet ./...`, `go test ./...`.
- `grep -rn 'TOPDATA_AGENT_\|topdata_agent_\|topdata-agent'` → only historical
  docs (`CHANGELOG` old sections, `_ai/` history) may match.
- Synthetic smoke test per AGENTS.md on `:19144` using `TOPDATA_TELEMETRY_*`.
- `topdata-telemetry --version` reports v2.0.0; binary size unchanged.
- Ansible smoke test (checks `topdata_telemetry_`) passes on arm1, then fleet.
- Prometheus target up and dashboard queries return data under new names.

# Out of scope

- Behavior/endpoint changes — read-only invariant per ADR__260912-1.
- Compatibility aliases for old env vars or dual-exporting old metric names
  (decision: clean break, v2.0.0).
- Renaming the ansible inventory group `agent` (generic and still correct).
