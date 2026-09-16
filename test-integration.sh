#!/usr/bin/env bash
# Disposable local Community test, without Telegram or production credentials.
# Usage: ./test-integration.sh /path/to/teleport /path/to/tctl [/path/to/go]
set -euo pipefail
bridge_teleport=$(realpath "${1:?Teleport server binary required}")
bridge_tctl=$(realpath "${2:?tctl binary required}")
bridge_go=$(command -v "${3:-go}")
cd "$(dirname "$0")"
bridge_fixture=$(mktemp -d /tmp/teleport-telegram-test.XXXXXXXX)
bridge_pid=''
cleanup() {
  if [[ -n "$bridge_pid" ]]; then
    kill "$bridge_pid" 2>/dev/null || true
    wait "$bridge_pid" 2>/dev/null || true
  fi
  rm -rf -- "$bridge_fixture"
}
trap cleanup EXIT
cat > "$bridge_fixture/teleport.yaml" <<EOF
version: v3
teleport:
  nodename: approval-test
  data_dir: $bridge_fixture/data
  log:
    output: stderr
    severity: WARN
auth_service:
  enabled: true
  listen_addr: 127.0.0.1:13025
  cluster_name: approval-test
  authentication:
    type: local
    second_factor: otp
proxy_service:
  enabled: false
ssh_service:
  enabled: false
EOF
"$bridge_teleport" start --config="$bridge_fixture/teleport.yaml" >"$bridge_fixture/server.log" 2>&1 &
bridge_pid=$!
bridge_ready=false
for ((bridge_attempt=0; bridge_attempt<30; bridge_attempt++)); do
  kill -0 "$bridge_pid" 2>/dev/null || { echo 'Test server failed to start (is port 13025 occupied?)' >&2; exit 1; }
  if "$bridge_tctl" --config="$bridge_fixture/teleport.yaml" status >/dev/null 2>&1; then
    bridge_ready=true
    break
  fi
  sleep 1
done
[[ "$bridge_ready" == true ]] || { echo 'Test server did not become ready' >&2; exit 1; }
"$bridge_tctl" --config="$bridge_fixture/teleport.yaml" create -f testdata/teleport-resources.yaml
"$bridge_tctl" --config="$bridge_fixture/teleport.yaml" auth sign --user=bridge --out="$bridge_fixture/bridge.pem" --ttl=1h
"$bridge_tctl" --config="$bridge_fixture/teleport.yaml" auth sign --user=requester --out="$bridge_fixture/requester.pem" --ttl=1h
bridge_request=$("$bridge_tctl" --config="$bridge_fixture/teleport.yaml" request create requester --roles=test-rw --reason='Disposable integration test')
TELEPORT_TEST_REQUEST_ID="$bridge_request" TELEPORT_TEST_DIR="$bridge_fixture" \
  "$bridge_go" test -count=1 -v -run TestLocalTeleportCommunity .
