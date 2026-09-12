---
title: "Ansible Fleet Deployment of the Go Node Agent"
date: 2026-08-20
status: completed
completedAt: 2026-08-20 21:55
project: topdata-node-agent-v2
id: 7a114d4f-a7de-470a-be81-3ff374fca076
content_hash: e17c9be0f5cae2c3af331694fbe80cc7
---

# Ansible Fleet Deployment of the Go Node Agent

## Goal

Deploy the `topdata-agent` binary to all 10 shop-hosting servers (mixed
arm64/amd64) via Ansible, mirroring the `package-service` deploy pattern:
`deploy/` folder with `hosts.ini`, `playbook-deploy.yaml`, and a convenience
`deploy-to-prod.sh` script.

## Steps

1. **`deploy/hosts.ini`** — inventory with the 10 hosts from `sites.toml`
   (arm1-6, rs1, vps1, amd1, trifun, xerox), `ansible_user=root`.

2. **`deploy/playbook-deploy.yaml`** — per host:
   - copy correct binary (`arm64`/`amd64` picked via `ansible_architecture`)
     to `/usr/local/bin/topdata-agent`, mode 0755
   - render `/etc/topdata-agent.env` from vault vars (username, password,
     shops root), mode 0600
   - write `/etc/systemd/system/topdata-agent.service`
   - `daemon-reload` + `enable --now` + restart
   - smoke test: `curl -u user:pass http://127.0.0.1:9144/metrics` retry loop

3. **`deploy/deploy-to-prod.sh`** — cross-compiles both arches into
   `deploy/bin/` (gitignored), then runs `ansible-playbook
   -i deploy/hosts.ini deploy/playbook-deploy.yaml --ask-vault-pass`.

4. **`deploy/vars/vault.yml`** — ansible-vault encrypted `auth_username`,
   `auth_password` (user creates locally with `ansible-vault create`).

5. **`.gitignore`** — add `deploy/bin/`.

6. **`CHANGELOG.md`** — entry under `[Unreleased]` (deployment via Ansible).

## Validation

- `ansible-playbook --syntax-check` passes.
- Dry-run `--check` against one host.
- Real run on one pilot host (e.g. `arm1`), verify `/metrics` returns 200
  with vault credentials.