#!/bin/sh
# Generates a Corefile from environment variables and starts CoreDNS.
#
# Required:
#   ZTNET_NETWORKS   space-separated zone:networkID pairs, e.g.
#                    "example.com:8056c2e21c000001 example.org:8056c2e21c000002"
#   ZTNET_API_TOKEN  ZTNET API token (same as zt2hosts.sh's ZTNET_API_TOKEN)
#
# Optional:
#   ZTNET_API          ZTNET base URL (default: http://localhost:3000)
#   ZTNET_REFRESH      refresh interval (default: 30s)
#   ZTNET_TTL          record TTL (default: 60s)
#   ZTNET_FALLTHROUGH  set to any value to enable fallthrough
#
# Alternatively, leave ZTNET_NETWORKS unset and mount your own Corefile at
# /etc/coredns/Corefile.
set -eu

mkdir -p /etc/coredns

if [ -n "${ZTNET_NETWORKS:-}" ]; then
  : "${ZTNET_API_TOKEN:?ZTNET_API_TOKEN is required}"
  cat > /etc/coredns/Corefile <<EOF
. {
    ztnet ${ZTNET_NETWORKS} {
        api ${ZTNET_API:-http://localhost:3000}
        token ${ZTNET_API_TOKEN}
        refresh ${ZTNET_REFRESH:-30s}
        ttl ${ZTNET_TTL:-60s}
        ${ZTNET_FALLTHROUGH:+fallthrough}
    }
    log
    errors
}
EOF
elif [ ! -f /etc/coredns/Corefile ]; then
  echo "error: set ZTNET_NETWORKS or mount a Corefile at /etc/coredns/Corefile" >&2
  exit 1
fi

exec /coredns "$@"
