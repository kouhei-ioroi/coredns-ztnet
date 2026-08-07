#!/usr/bin/env bash
# Build a CoreDNS binary with the ztnet plugin compiled in.
#
# Usage: hack/build-coredns.sh <coredns-version> [output-path]
#
#   <coredns-version>  upstream CoreDNS release WITHOUT the leading "v"
#                      (e.g. 1.14.6)
#   [output-path]      destination for the resulting binary (default: ./coredns)
#
# The upstream source is cloned at the requested tag, ztnet is registered in
# plugin.cfg, and the module is wired with a local replace directive before
# building. GOOS/GOARCH/CGO_ENABLED are honored from the environment so
# cross-compilation works by setting GOOS/GOARCH outside.
set -euo pipefail

VERSION="${1:?usage: hack/build-coredns.sh <coredns-version> [output-path]}"
OUT="${2:-coredns}"
PLUGIN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Resolve OUT to an absolute path before any cd, so the binary ends up in the
# caller's intended location even after the script cd's into the coredns clone.
case "${OUT}" in
  /*) ;; # already absolute
  *) OUT="$(pwd)/${OUT}" ;;
esac

OUT_DIR="$(dirname "${OUT}")"
mkdir -p "${OUT_DIR}"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

echo "==> cloning CoreDNS v${VERSION}"
git clone --quiet --depth 1 --branch "v${VERSION}" https://github.com/coredns/coredns.git "${WORK}/coredns"
cd "${WORK}/coredns"

echo "==> registering ztnet in plugin.cfg"
# Insert after "secondary" (before etcd/loop/forward) so zone-data lookups run
# before any forwarding plugin. Fall back to inserting before "forward" if the
# anchor moves in a future release.
sed -i '/^secondary:secondary/a ztnet:coredns-ztnet' plugin.cfg
if ! grep -q '^ztnet:coredns-ztnet' plugin.cfg; then
  sed -i '/^forward:forward/i ztnet:coredns-ztnet' plugin.cfg
fi
if ! grep -q '^ztnet:coredns-ztnet' plugin.cfg; then
  echo "error: could not register ztnet in plugin.cfg (anchors 'secondary' and 'forward' are missing in CoreDNS v${VERSION})" >&2
  exit 1
fi

echo "==> wiring module replacement"
go mod edit -replace "coredns-ztnet=${PLUGIN_DIR}"

echo "==> generating plugin registration"
# go generate runs a tool (directives_generate.go) that must execute on the
# build host. Temporarily override any cross-compile env vars so the tool is
# built for the runner's native architecture.
GOOS="$(go env GOHOSTOS)" GOARCH="$(go env GOHOSTARCH)" go generate coredns.go

# tidy must run after go generate: zplugin.go (which imports coredns-ztnet) is
# only regenerated above, and tidy would otherwise prune the require directive.
go mod tidy

echo "==> building ${OUT}"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "${OUT}" .

# Sanity check: confirm ztnet is registered. Skipped when cross-compiling
# (GOHOST* is the runner's native platform).
if [ "${GOOS:-$(go env GOHOSTOS)}" = "$(go env GOHOSTOS)" ] &&
   [ "${GOARCH:-$(go env GOHOSTARCH)}" = "$(go env GOHOSTARCH)" ]; then
  "${OUT}" -plugins | grep -w ztnet
fi

echo "==> done: ${OUT}"
