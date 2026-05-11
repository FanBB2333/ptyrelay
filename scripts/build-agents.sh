#!/usr/bin/env bash
# Build the ptyrelay-agent for the matrix of platforms an end-user is
# most likely to bootstrap against, and emit them in the layout that
# bootstrap.FileProvider / bootstrap.EmbedProvider expect:
#
#   dist/agents/
#     linux-amd64
#     linux-arm64
#     linux-386
#     linux-arm
#     darwin-amd64
#     darwin-arm64
#     freebsd-amd64
#     freebsd-arm64
#
# Each binary is statically linked (CGO_ENABLED=0) and stripped
# (-ldflags="-s -w"), which gets a typical agent under 3 MB. Pass
# --gzip to additionally emit .gz copies for upload-friendly release
# assets — bootstrap's --from-url handles .gz automatically.
#
# Usage:
#   scripts/build-agents.sh              # default matrix, no gzip
#   scripts/build-agents.sh --gzip       # same, also emit .gz alongside
#   OUT_DIR=foo scripts/build-agents.sh  # custom output dir (default: dist/agents)

set -euo pipefail

OUT_DIR="${OUT_DIR:-dist/agents}"
DO_GZIP=0
for arg in "$@"; do
  case "$arg" in
    --gzip) DO_GZIP=1 ;;
    -h|--help)
      sed -n '2,/^set -euo/p' "$0" | sed 's/^# \{0,1\}//; /^set -euo/d'
      exit 0
      ;;
    *) echo "unknown flag: $arg" 1>&2; exit 2 ;;
  esac
done

# (os, arch) pairs. Keep this list short and curated: every entry here
# becomes a binary in `dist/`. Add a platform only when you know someone
# wants to bootstrap to it.
TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "linux/386"
  "linux/arm"
  "darwin/amd64"
  "darwin/arm64"
  "freebsd/amd64"
  "freebsd/arm64"
)

mkdir -p "$OUT_DIR"

for t in "${TARGETS[@]}"; do
  OS="${t%/*}"
  ARCH="${t#*/}"
  OUT="$OUT_DIR/$OS-$ARCH"
  echo "  build  $OS/$ARCH -> $OUT"
  # `-trimpath` keeps the binary reproducible across machines.
  CGO_ENABLED=0 GOOS="$OS" GOARCH="$ARCH" \
    go build -trimpath -ldflags="-s -w" -o "$OUT" ./cmd/ptyrelay-agent

  if [ "$DO_GZIP" -eq 1 ]; then
    gzip -9 -c "$OUT" > "$OUT.gz"
    echo "  gzip   $OUT.gz ($(wc -c < "$OUT.gz") bytes)"
  fi
done

echo
echo "done. binaries in $OUT_DIR"
if [ "$DO_GZIP" -eq 1 ]; then
  echo "compute sha256 with:  shasum -a 256 $OUT_DIR/*.gz"
fi
