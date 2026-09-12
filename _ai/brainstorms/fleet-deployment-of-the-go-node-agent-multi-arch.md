---
alternatives: []
billingGroupId: ''
decisions:
- decided_at: '2026-08-20T19:32:27+00:00'
  decision: 'Deploy the Go node agent fleet-wide with Ansible now: small playbook
    + static inventory, copying per-arch static binaries (arm64/amd64) over SSH and
    installing the systemd service. Defer the server-registry microservice (TradeGuard
    SiteHostingServer entities stay unused for now); it may later become Ansible''s
    inventory source.'
  factors: []
  question: ''
- decided_at: '2026-08-20T19:46:48+00:00'
  decision: 'Deployment layout mirrors package-service: deploy/ folder in the agent
    repo with hosts.ini (sites.toml hosts only: arm1-6, rs1, vps1, amd1, trifun, xerox),
    playbook-deploy.yaml, deploy-to-prod.sh convenience script that cross-compiles
    both GOARCH (arm64/amd64) into deploy/bin/ (gitignored), and the playbook picks
    the binary per host via ansible_architecture. Auth credentials for /metrics live
    in ansible-vault (deploy/vars/vault.yml). Playbook installs /usr/local/bin/topdata-agent
    + /etc/topdata-agent.env (0600) + systemd unit, enables service, smoke-tests /metrics
    with retry loop.'
  factors: []
  question: ''
documentType: BRAINSTORM
id: 3f8be10e-5a2c-4ad6-a3e8-32672bc3b394
kind: brainstorm
open_questions:
- asked_at: '2026-08-20T19:23:42+00:00'
  question: Which servers need the agent deployed, and is the target set defined anywhere
    (sites.toml hosts, ip-mapping fleet, or a separate list)?
projectId: topdata-node-agent-v2
protocol_version: '1'
status: decided
tags:
- brainstorm
title: Fleet deployment of the Go node agent (multi-arch)
topic: Fleet deployment of the Go node agent (multi-arch)
updatedAt: '2026-08-20T19:46:48+00:00'
workspaceId: ''
content_hash: 7c4bc2478b050324790551a03c632be6
---

# Fleet deployment of the Go node agent (multi-arch) — Brainstorm

## Protocol Checklist

- [ ] Review the current project state first (files, docs, recent commits)
- [ ] Ask questions one at a time; prefer multiple choice when possible
- [ ] Focus on understanding: purpose, constraints, success criteria
- [ ] Propose 2-3 approaches with trade-offs; lead with the recommended option
- [ ] Present the design in 200-300 word sections, validating after each
- [ ] Apply YAGNI ruthlessly
- [ ] Record decisions with `ctx brainstorm decide`

## Open Questions


### Q-1: Which servers need the agent deployed, and is the target set defined anywhere (sites.toml hosts, ip-mapping fleet, or a separate list)?

_Asked: 2026-08-20T19:23:42+00:00_

## Decisions


### D-1: Deploy the Go node agent fleet-wide with Ansible now: small playbook + static inventory, copying per-arch static binaries (arm64/amd64) over SSH and installing the systemd service. Defer the server-registry microservice (TradeGuard SiteHostingServer entities stay unused for now); it may later become Ansible's inventory source.
- Decided: 2026-08-20T19:32:27+00:00

### D-2: Deployment layout mirrors package-service: deploy/ folder in the agent repo with hosts.ini (sites.toml hosts only: arm1-6, rs1, vps1, amd1, trifun, xerox), playbook-deploy.yaml, deploy-to-prod.sh convenience script that cross-compiles both GOARCH (arm64/amd64) into deploy/bin/ (gitignored), and the playbook picks the binary per host via ansible_architecture. Auth credentials for /metrics live in ansible-vault (deploy/vars/vault.yml). Playbook installs /usr/local/bin/topdata-agent + /etc/topdata-agent.env (0600) + systemd unit, enables service, smoke-tests /metrics with retry loop.
- Decided: 2026-08-20T19:46:48+00:00

## Alternatives