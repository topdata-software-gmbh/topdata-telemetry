#!/usr/bin/env bash
# topdata-telemetry deploy script.
#
# Cross-compiles the agent for linux/arm64 + linux/amd64 and deploys the
# matching binary to all hosts in deploy/hosts.ini via ansible-playbook.
#
# Usage: ./deploy/deploy-to-prod.sh [options] [ansible-playbook args...]
#
# Options:
#   -h, --help     Show this help and exit.
#   --build-only   Only cross-compile both binaries into deploy/bin/.
#   --deploy-only  Only run ansible-playbook (binaries must already exist).
#
# Any other argument is passed through to ansible-playbook unchanged.
# Test a single server first, e.g.:
#   ./deploy/deploy-to-prod.sh --limit arm1

set -euo pipefail

usage() {
    sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
}

BUILD_ONLY=0
DEPLOY_ONLY=0
ANSIBLE_ARGS=()

for arg in "$@"; do
    case "$arg" in
        -h|--help)
            usage
            exit 0
            ;;
        --build-only)
            BUILD_ONLY=1
            ;;
        --deploy-only)
            DEPLOY_ONLY=1
            ;;
        *)
            ANSIBLE_ARGS+=("$arg")
            ;;
    esac
done

if [ "$BUILD_ONLY" -eq 1 ] && [ "$DEPLOY_ONLY" -eq 1 ]; then
    echo "Error: --build-only and --deploy-only are mutually exclusive." >&2
    usage >&2
    exit 1
fi

cd "$(dirname "$0")/.."

# Version baked into the binary via -ldflags. `git describe --tags --always`
# yields the exact tag (e.g. `v1.2.0`) when HEAD is on a tag, or
# `v1.2.0-3-gabc123` between tags — so every build reports a meaningful,
# traceable version regardless of whether a release tag exists.
GIT_VERSION="$(git describe --tags --always 2>/dev/null || echo dev)"
BUILD_LDFLAGS="-s -w -X github.com/topdata-software-gmbh/topdata-telemetry/cmd.version=${GIT_VERSION}"

if [ "$DEPLOY_ONLY" -eq 0 ]; then
    echo "==> Cross-compiling topdata-telemetry binaries"
    echo "    go:       $(go version)"
    echo "    version:  ${GIT_VERSION}"
    echo "    module:   $(head -1 go.mod)"
    echo "    building: deploy/bin/topdata-telemetry-arm64 (linux/arm64)"
    GOOS=linux GOARCH=arm64 go build -ldflags "${BUILD_LDFLAGS}" -o deploy/bin/topdata-telemetry-arm64 .
    echo "    building: deploy/bin/topdata-telemetry-amd64 (linux/amd64)"
    GOOS=linux GOARCH=amd64 go build -ldflags "${BUILD_LDFLAGS}" -o deploy/bin/topdata-telemetry-amd64 .
    echo "    done. built artifacts:"
    ls -l deploy/bin/topdata-telemetry-arm64 deploy/bin/topdata-telemetry-amd64
fi

if [ "$BUILD_ONLY" -eq 0 ]; then
    if [ ! -f deploy/vars/vault.yml ]; then
        echo "Error: deploy/vars/vault.yml not found." >&2
        echo "Create it once with: ansible-vault create deploy/vars/vault.yml" >&2
        echo "(keys: auth_username, auth_password, shops_root — see deploy/vars/vault.example.yml)" >&2
        exit 1
    fi

    ansible-playbook -i deploy/hosts.ini deploy/playbook-deploy.yaml --ask-vault-pass "${ANSIBLE_ARGS[@]}"

    echo "Deployed topdata-telemetry to all hosts in deploy/hosts.ini."
    echo
    echo "Scrape targets (credentials from deploy/vars/vault.yml):"
    awk '/^\[agent\]/{in_agent=1; next} /^\[/{in_agent=0} in_agent && /ansible_host=/{sub(/^.*ansible_host=/, ""); sub(/ .*$/, ""); print "  http://<user>:<pass>@" $0 ":9144/metrics"}' deploy/hosts.ini
    echo
    echo "Prometheus scraper config (drop into config/scrapes/topdata-telemetry.yaml"
    echo "next to config/prometheus.yaml, which already loads scrapes/*.yaml):"
    echo "scrape_configs:"
    echo "    -   job_name: 'topdata-telemetry'"
    echo "        scrape_interval: 30s"
    echo "        static_configs:"
    echo "            -   targets:"
    awk '/^\[agent\]/{in_agent=1; next} /^\[/{in_agent=0} in_agent && /ansible_host=/{sub(/^.*ansible_host=/, ""); sub(/ .*$/, ""); print "                    - \"" $0 ":9144\""}' deploy/hosts.ini
    echo "        basic_auth:"
    echo "            username: <auth_username>"
    echo "            password: <auth_password>"
fi
