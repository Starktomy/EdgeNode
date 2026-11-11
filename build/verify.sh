#!/usr/bin/env bash
set -euo pipefail

module_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
common_checks="$module_dir/../EdgeCommon/build/verify-lib.sh"
if [[ ! -f "$common_checks" ]]; then
  printf '%s\n' 'Check out the matching EdgeCommon revision beside EdgeNode before verification.' >&2
  exit 1
fi
cd "$module_dir"
source "$common_checks"
verify_module EdgeNode "$@"
