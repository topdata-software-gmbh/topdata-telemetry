---
title: "Implementation Plan: Compress topdata-agent binary for faster fleet transfer"
date: 2026-08-21
status: active
id: e092989c-7752-4f96-b9ea-57d91fc46916
content_hash: 06d1da8b654328ce12be75d07d0f974b
---

# Goal
Reduce fleet-deploy transfer time by stripping the Go binary at build time and
transferring it over zstd-compressed rsync instead of uncompressed copy.

# Changes

## 1. Build strip — `deploy/deploy-to-prod.sh`
- Change `BUILD_LDFLAGS="-X github.com/topdata/node-agent/cmd.version=${BUILD_VERSION}"`
  to `BUILD_LDFLAGS="-s -w -X github.com/topdata/node-agent/cmd.version=${BUILD_VERSION}"`.
- `-s -w` strips symbol table + DWARF (~33% smaller, measured 18.4 MB -> 12.4 MB).
- `-X version=...` is unchanged, so `--version` / smoke test still work.

## 2. Transfer — `deploy/playbook-deploy.yaml`
- Replace the `ansible.builtin.copy` task (binary) with:
  - A `command: rsync --version` check + `fail` when target rsync is not
    `version 3.[2-9]|version [4-9]` (i.e. < 3.2.0, no zstd).
  - `ansible.posix.synchronize` with `compress: yes`, `compress_choice: zstd`,
    `compress_level: 3`, `perms: yes`, `checksum: yes`, `delegate_to: localhost`.
  - A follow-up `ansible.builtin.file: mode: 0755` to guarantee executability.

## 3. Prerequisites
- Controller: ensure `ansible.posix` collection is installed
  (`ansible-galaxy collection install ansible.posix`). Available locally (2.0.0).
- Document the rsync >= 3.2.0 target requirement (already enforced by the fail task).

# Validation
- `./deploy/deploy-to-prod.sh --build-only` then `ls -l deploy/bin/*` -> binary ~12.4 MB.
- Confirm `/tmp`-built binary still prints version: `./deploy/bin/topdata-agent-amd64 --version`.
- Real deploy with `--limit <one host>`: observe faster transfer; smoke test passes.
- On a host with rsync < 3.2.0 the playbook fails fast with a clear message.

# Out of scope
- UPX packing (rejected: no transfer benefit, adds runtime cost).
- TinyGo rewrite.
