#!/usr/bin/env bash
set -euo pipefail

status=""
for _ in {1..30}; do
  status="$(curl --silent --output /dev/null --write-out '%{http_code}' http://localhost:8080/old/item || true)"
  if [[ "${status}" == "302" ]]; then
    break
  fi
  sleep 1
done

if [[ "${status}" != "302" ]]; then
  echo "expected status 302, got ${status}" >&2
  exit 1
fi

location="$(curl --silent --dump-header - --output /dev/null http://localhost:8080/old/item \
  | awk 'BEGIN { IGNORECASE=1 } /^Location:/ { sub(/\r$/, "", $2); print $2 }')"

if [[ "${location}" != "https://example.com/new/item" ]]; then
  echo "unexpected Location header: ${location}" >&2
  exit 1
fi
