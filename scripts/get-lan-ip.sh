#!/usr/bin/env bash
# Get the LAN address(es) of a DroidBeam server via its public domain.
#
#   DROIDBEAM_URL=https://sms.example.com DROIDBEAM_API_KEY=xxxx ./scripts/get-lan-ip.sh
#
# Output: one address per line, e.g. "http://192.168.1.10:8080".
# Add --check to print only addresses that are reachable from this machine
# (and really are the same server, verified by server_id).
set -euo pipefail

: "${DROIDBEAM_URL:?set DROIDBEAM_URL to the public domain, e.g. https://sms.example.com}"
: "${DROIDBEAM_API_KEY:?set DROIDBEAM_API_KEY}"
check=false
[[ "${1:-}" == "--check" ]] && check=true

json=$(curl -fsS --max-time 10 -H "X-API-Key: ${DROIDBEAM_API_KEY}" "${DROIDBEAM_URL%/}/api/v1/server-info")

field() { # field <python-expr on d>
  python3 -c "import json,sys; d=json.load(sys.stdin); $1" <<<"$json"
}
server_id=$(field 'print(d["server_id"])')

field 'print("\n".join(d.get("lan_urls") or []))' | while read -r url; do
  [[ -z "$url" ]] && continue
  if $check; then
    got=$(curl -fsS --max-time 3 "$url/healthz" 2>/dev/null || true)
    [[ "$got" == *"\"server_id\":\"$server_id\""* ]] || continue
  fi
  echo "$url"
done
