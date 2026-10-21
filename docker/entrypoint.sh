#!/bin/sh
set -eu

if [ "${1:-}" = serve ] && [ -z "${KNOTRA_SANDBOX_HELPER:-}" ]; then
  case "$KNOTRA_DATA_DIR" in
    /*) ;;
    *)
      echo 'KNOTRA_DATA_DIR must be an absolute host path mounted at the same container path' >&2
      exit 1
      ;;
  esac

  source=/usr/local/libexec/knotra/sandbox-helper
  digest=$(sha256sum "$source")
  export KNOTRA_SANDBOX_HELPER="$KNOTRA_DATA_DIR/bin/sandbox-helper-${digest%% *}"
  mkdir -p "$KNOTRA_DATA_DIR/bin"

  if [ ! -e "$KNOTRA_SANDBOX_HELPER" ]; then
    temporary=$(mktemp "$KNOTRA_DATA_DIR/bin/.helper-XXXXXX")
    trap 'rm -f "$temporary"' EXIT HUP INT TERM
    cp "$source" "$temporary"
    chmod 755 "$temporary"
    # Keep helpers referenced by admitted plans unchanged across upgrades and concurrent starts.
    ln "$temporary" "$KNOTRA_SANDBOX_HELPER" 2>/dev/null || test -e "$KNOTRA_SANDBOX_HELPER"
    rm -f "$temporary"
    trap - EXIT HUP INT TERM
  fi

  if ! cmp -s "$source" "$KNOTRA_SANDBOX_HELPER" || [ ! -x "$KNOTRA_SANDBOX_HELPER" ]; then
    echo "sandbox helper is altered or not executable: $KNOTRA_SANDBOX_HELPER" >&2
    exit 1
  fi
fi

exec /usr/local/bin/knotra "$@"
