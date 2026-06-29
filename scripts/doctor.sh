#!/usr/bin/env bash
# doctor.sh — preflight adapt-drift guard for Crucible clones.
#
# Asserts three invariants that silent adapt-misconfig can break before first boot:
#   1. Env prefix / salt parity  — API_KEY_PREFIX and API_KEY_HASH_SALT match
#      between root and dashboard/.env.example (v0 P2.2 drift class).
#   2. Symlink integrity         — workers/active resolves to a real directory.
#   3. Route table sanity        — V1Routes in routes_table.go is non-empty,
#      has no duplicate paths, and contains no framework-reserved paths.
#
# Usage: scripts/doctor.sh [<repo-root>]
#   repo-root defaults to the parent of the directory containing this script.
#
# On any mismatch: prints DOCTOR_FAIL: <cause> to stderr and exits non-zero.
# On success: prints "doctor: all checks passed" and exits 0.

set -euo pipefail

REPO="${1:-$(cd -- "$(dirname -- "$0")/.." && pwd)}"

_fails=0
_fail() {
  printf 'DOCTOR_FAIL: %s\n' "$1" >&2
  _fails=$((_fails + 1))
}

##############################################################################
# Check 1: Env prefix / salt parity
# API_KEY_PREFIX and API_KEY_HASH_SALT must be identical in both env files.
# Drift here means the dashboard issues keys the gateway will reject.
##############################################################################

root_env="$REPO/.env.example"
dash_env="$REPO/dashboard/.env.example"

[[ -f "$root_env" ]] || _fail "env_missing: .env.example not found at repo root"
[[ -f "$dash_env" ]] || _fail "env_missing: dashboard/.env.example not found"

if [[ -f "$root_env" && -f "$dash_env" ]]; then
  root_prefix=$(grep '^API_KEY_PREFIX=' "$root_env" | cut -d= -f2-)
  dash_prefix=$(grep '^API_KEY_PREFIX=' "$dash_env" | cut -d= -f2-)
  if [[ -z "$root_prefix" || -z "$dash_prefix" ]]; then
    _fail "env_prefix_missing: API_KEY_PREFIX absent in one or both env files"
  elif [[ "$root_prefix" != "$dash_prefix" ]]; then
    _fail "env_prefix_mismatch: API_KEY_PREFIX — root='$root_prefix' dashboard='$dash_prefix'"
  fi

  root_salt=$(grep '^API_KEY_HASH_SALT=' "$root_env" | cut -d= -f2-)
  dash_salt=$(grep '^API_KEY_HASH_SALT=' "$dash_env" | cut -d= -f2-)
  if [[ -z "$root_salt" || -z "$dash_salt" ]]; then
    _fail "env_salt_missing: API_KEY_HASH_SALT absent in one or both env files"
  elif [[ "$root_salt" == "REPLACE_WITH"* ]]; then
    _fail "env_salt_placeholder: API_KEY_HASH_SALT placeholder not replaced — run new-tool.sh or set a real secret"
  elif [[ "$root_salt" != "$dash_salt" ]]; then
    _fail "env_salt_mismatch: API_KEY_HASH_SALT differs between root and dashboard/.env.example"
  fi
fi

##############################################################################
# Check 2: Symlink integrity
# workers/active must be a symlink that resolves to a real directory.
# A missing or dangling symlink means no worker is wired to the gateway.
##############################################################################

worker_link="$REPO/workers/active"
if [[ ! -L "$worker_link" ]]; then
  _fail "symlink_missing: workers/active is not a symlink — create it pointing to the active worker directory"
else
  link_val=$(readlink "$worker_link" 2>/dev/null || true)
  case "$link_val" in
    /*) abs_target="$link_val" ;;
    *)  abs_target="$(dirname "$worker_link")/$link_val" ;;
  esac
  if [[ -z "$link_val" || ! -d "$abs_target" ]]; then
    _fail "symlink_dangling: workers/active -> '${link_val:-<empty>}' does not resolve to a directory"
  fi
fi

##############################################################################
# Check 3: Route table sanity (route ↔ OpenAPI sync)
# V1Routes in routes_table.go is the single source of truth for both the chi
# router and the openapi.Build() call — they cannot drift from each other at
# compile time. This check catches pre-compile adapt omissions: an empty or
# malformed table means no /v1 endpoints are registered or documented.
##############################################################################

routes_table="$REPO/gateway/internal/server/routes_table.go"
if [[ ! -f "$routes_table" ]]; then
  _fail "routes_missing: gateway/internal/server/routes_table.go not found"
else
  # Extract Path values from V1Routes: lines matching 'Path: "..."'.
  paths=$(grep 'Path:' "$routes_table" \
    | sed 's/.*Path:[[:space:]]*"\([^"]*\)".*/\1/' \
    | grep '^/' || true)

  if [[ -z "$paths" ]]; then
    _fail "routes_empty: V1Routes in routes_table.go has no valid entries — add at least one endpoint (adapt edit required)"
  else
    dupes=$(printf '%s\n' "$paths" | sort | uniq -d)
    if [[ -n "$dupes" ]]; then
      _fail "routes_duplicate: duplicate paths in V1Routes: $(printf '%s ' $dupes)"
    fi

    while IFS= read -r p; do
      case "$p" in
        /billing/*|/webhooks/*)
          _fail "routes_reserved_path: '$p' is a framework-reserved path; remove from V1Routes (framework routes are registered separately in routes.go)"
          ;;
      esac
    done <<< "$paths"
  fi
fi

##############################################################################

if [[ $_fails -gt 0 ]]; then
  printf '\ndoctor: %d check(s) failed\n' "$_fails" >&2
  exit 1
fi

echo "doctor: all checks passed"
