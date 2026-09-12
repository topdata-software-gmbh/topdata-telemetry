---
title: "Monitoring agent is strictly read-only (telemetry/control plane split)"
status: Accepted
date: 2026-09-12
deciders: Topdata Team
tags: [architecture, security, monitoring, read-only, control-plane]
adrId: 260912-1
---

# Monitoring agent is strictly read-only

## Context

The agent runs as root on every shop-hosting server and today only observes
(log tailing, disk scans, read-only HTTP endpoints over shared Basic Auth). A
separate deployment/orchestration capability is being considered for the fleet,
and the obvious shortcut is to add write/exec/deploy endpoints to this agent.

That shortcut is dangerous. The 2026-08-28 arm1 incident (see
`ADR__260829__disk-scan-io-throttling.md`) showed an agent fault can make a host
unreachable; if the same process also held deployment credentials, a crash or
I/O lockup during a release would blind monitoring exactly when it is needed
most. A shared Basic Auth credential on a root-running process that can write
is also a fleet-wide remote-code-execution risk if leaked.

## Decision

This project is the **telemetry plane** and stays **strictly read-only**:

- No `exec`, write, deploy, or mutation endpoints — ever.
- No mutation of shop directories; monitoring only.
- Control-plane work (deployment, orchestration, remediation) lives in a
  **separate control agent** with its own authentication (e.g. per-node
  identities/mTLS or signed declarative manifests) — never the monitoring
  Basic Auth.
- The monitoring agent needs root only to read shop files; it holds no
  write/exec capabilities.

Because the two are independent, control logic can iterate fast while the
monitoring agent remains boring and stable, and its credentials remain low-risk.

## Consequences

**Positive**
- Telemetry survives failed deployments and control-agent outages — the
  observability safety net stays intact.
- Compromising the monitoring credential cannot lead to remote code execution
  or writes.
- Independent lifecycles: monitoring changes rarely; deployment logic often.
- Least privilege: no write/exec capabilities in a fleet-wide root process.

**Negative / trade-offs**
- Two agents to deploy, monitor, and keep compatible.
- Operators cannot trigger actions from the monitoring API; they must use the
  control agent (or Ansible/SSH).
- Ongoing discipline required: "just one small write endpoint" requests must be
  rejected and routed to the control agent.

## Alternatives Considered

- **Add write/exec endpoints to this agent** — rejected: couples telemetry to
  deployment failures and turns the shared Basic Auth into a fleet-wide RCE
  vector.
- **Skip a node control agent; keep deployment purely Ansible/SSH** — viable and
  still available (decided separately); this ADR constrains scope, it does not
  mandate that a control agent be built.
- **Signed one-off commands on this agent instead of a separate process** —
  rejected: still puts write logic and credentials in the monitoring process,
  defeating isolation.

## Related Decisions

- `ADR__260820-1__go-node-agent-replacing-php-node-agent.md` — defines the
  agent's current scope.
- `ADR__260829__disk-scan-io-throttling.md` — incident showing agent faults can
  take a host down.
