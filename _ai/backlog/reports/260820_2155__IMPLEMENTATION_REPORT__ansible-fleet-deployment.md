---
title: "Ansible Fleet Deployment of the Go Node Agent"
date: 2026-08-20
status: done
project: topdata-node-agent-v2
id: f3ca8f2b-cce2-48c8-af2e-a77c0ae20521
content_hash: 2b0af0a0f9b43956e8f7d6df9bc97365
---

# Implementation Report: Ansible Fleet Deployment

## Summary

Implemented the Ansible fleet deployment for `topdata-agent`, mirroring the
`package-service` deploy pattern (`deploy/` with `hosts.ini`, playbook, and
convenience script). All plan steps completed.

## What was built

- `deploy/hosts.ini` — `[agent]` group with the 10 shop-hosting servers
  (arm1-6, rs1, vps1, amd1, trifun), `ansible_user=root`. **xerox excluded**
  (not our server, per user instruction).
- `deploy/playbook-deploy.yaml` — per host:
  - fails with a helpful message if `deploy/vars/vault.yml` is missing
  - loads vault secrets (`auth_username`, `auth_password`, `shops_root`)
  - maps `ansible_facts.architecture` → Go binary name (`aarch64`/`arm64` →
    `arm64`, everything else → `amd64`)
  - fails if the local binary for that arch is missing (hints at
    `deploy-to-prod.sh`)
  - copies `deploy/bin/topdata-agent-<arch>` → `/usr/local/bin/topdata-agent`
    (0755)
  - renders `/etc/topdata-agent.env` (0600) from `deploy/templates/`
  - installs systemd unit `topdata-agent.service` (daemon-reload, enable,
    restart)
  - smoke test: `ansible.builtin.uri` retry loop against
    `http://127.0.0.1:9144/metrics` with Basic Auth from vault (runs also in
    `--check` mode via `check_mode: false`)
- `deploy/deploy-to-prod.sh` — cross-compiles both arches (`GOOS=linux
  GOARCH=arm64|amd64`) into `deploy/bin/` (gitignored), then runs
  `ansible-playbook -i deploy/hosts.ini deploy/playbook-deploy.yaml
  --ask-vault-pass`. Pre-checks vault existence with a clear error.
- `deploy/vars/vault.example.yml` — documented keys for the vault file.
- `.gitignore` — added `deploy/bin/` and `deploy/vars/vault.yml`.
- `CHANGELOG.md` — entry under `[Unreleased]`.
- `AGENTS.md` — new "Deployment (Ansible)" section.

## Deviations from plan

- xerox removed from inventory (user: "not our server"). 10 hosts remain.
- No pre-encrypted vault file shipped; the playbook fails with
  setup instructions when `deploy/vars/vault.yml` is missing (plan step 4
  already said the user creates it locally).
- Hostnames sourced from `topdata-ansible-playbooks/inventory.ini`
  (no `sites.toml` exists in the workspace).

## Validation performed

- `ansible-playbook --syntax-check` passes.
- `bash -n deploy/deploy-to-prod.sh` passes.
- Full playbook dry-run (`--check`) against localhost with a throwaway
  encrypted vault: vault loading, arch mapping (`aarch64` → arm64), binary
  existence check, and both templates render correctly.
- End-to-end local test: built both binaries; ran the amd64 binary with the
  rendered env file against a synthetic shop tree — `/metrics` returned 200
  with Basic Auth and exposed `topdata_agent_shopware_shop_disk_usage_bytes{shop="demo1"}`.
- `go vet ./...` clean.

## Not done (requires production access)

- Real run on pilot host (arm6) and full-fleet rollout — commands below.

## Test command (single pilot host)

Create the vault once (interactive password prompt):

```sh
ansible-vault create deploy/vars/vault.yml
# content:
# auth_username: <user>
# auth_password: <pass>
# shops_root: /srv/topdata-shops
```

Deploy to arm6 only:

```sh
./deploy/deploy-to-prod.sh --limit arm6
```

or without rebuilding the binaries:

```sh
ansible-playbook -i deploy/hosts.ini deploy/playbook-deploy.yaml \
  --limit arm6 --ask-vault-pass
```

Note: the smoke test fails on a server where `/srv/topdata-shops/prod-shops`
does not exist yet — the agent `log.Fatal`s at startup (AGENTS.md documents
this). Create the directory on the pilot host first if needed.