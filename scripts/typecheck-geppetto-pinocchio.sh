#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

usage() {
  cat <<'EOF'
Type-check geppetto + pinocchio together (Go workspace mode).

This script assumes you are in a workspace that has sibling repos:
  ../geppetto and ../pinocchio relative to the oak-git-db repo.

Overrides:
  GEPPETTO_DIR=/abs/path/to/geppetto
  PINOCCHIO_DIR=/abs/path/to/pinocchio
  GOCACHE=/tmp/go-build-cache
  VET=off   (sets -vet=off)

Examples:
  ./scripts/typecheck-geppetto-pinocchio.sh
  VET=off ./scripts/typecheck-geppetto-pinocchio.sh
EOF
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
  usage
  exit 0
fi

GEPPETTO_DIR="${GEPPETTO_DIR:-$ROOT/../geppetto}"
PINOCCHIO_DIR="${PINOCCHIO_DIR:-$ROOT/../pinocchio}"
GOCACHE="${GOCACHE:-/tmp/go-build-cache}"

if [[ ! -f "$GEPPETTO_DIR/go.mod" ]]; then
  echo "ERROR: GEPPETTO_DIR does not look like a Go module: $GEPPETTO_DIR" >&2
  exit 2
fi
if [[ ! -f "$PINOCCHIO_DIR/go.mod" ]]; then
  echo "ERROR: PINOCCHIO_DIR does not look like a Go module: $PINOCCHIO_DIR" >&2
  exit 2
fi

GEPPETTO_REL="$(GEPPETTO_DIR="$GEPPETTO_DIR" PINOCCHIO_DIR="$PINOCCHIO_DIR" python3 - <<PY
import os,sys
print(os.path.relpath(os.environ["GEPPETTO_DIR"], os.environ["PINOCCHIO_DIR"]))
PY
)"

GOFLAGS=()
if [[ "${VET:-}" == "off" ]]; then
  GOFLAGS+=("-vet=off")
fi

echo "Type-checking in workspace mode"
echo "  pinocchio: $PINOCCHIO_DIR"
echo "  geppetto:  $GEPPETTO_DIR"
echo "  geppetto (relative): $GEPPETTO_REL"
echo "  GOCACHE:   $GOCACHE"
echo

(
  cd "$PINOCCHIO_DIR"
  export GEPPETTO_DIR PINOCCHIO_DIR GOCACHE
  echo "go env GOWORK=$(go env GOWORK)"
  echo
  set -x
  go test -run='^$' "${GOFLAGS[@]}" ./... "$GEPPETTO_REL/..."
)
