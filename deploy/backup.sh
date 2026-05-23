#!/usr/bin/env bash
# backup.sh — dump the Crucible postgres database and keep the last 7 backups
#
# Usage:
#   bash deploy/backup.sh                     # run backup now
#   bash deploy/backup.sh --restore FILE      # restore from a backup file
#
# Stores backups in /var/backups/crucible/ with date stamps.
# Keeps the last 7 backups, deletes older ones.
#
# Idempotent: safe to re-run.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

BACKUP_DIR="/var/backups/crucible"
RETENTION=7
CONTAINER_NAME="crucible-postgres-1"

# ---------------------------------------------------------------------------
# Usage / help
# ---------------------------------------------------------------------------

usage() {
    cat <<EOF
Usage: backup.sh [OPTIONS]

Dump the Crucible postgres database and manage backup retention.

Options:
  -h, --help              Show this help message
  --restore FILE.gz       Restore from a specific backup file
  --retention N           Number of backups to keep (default: $RETENTION)
  --backup-dir PATH       Backup directory (default: $BACKUP_DIR)

Examples:
  backup.sh                          # run a backup now
  backup.sh --retention 14           # keep 14 backups instead of 7
  backup.sh --restore /var/backups/crucible/crucible-20250101-030000.sql.gz
EOF
    exit 0
}

MODE="backup"
RESTORE_FILE=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help)       usage ;;
        --restore)       MODE="restore"; RESTORE_FILE="$2"; shift 2 ;;
        --retention)     RETENTION="$2"; shift 2 ;;
        --backup-dir)    BACKUP_DIR="$2"; shift 2 ;;
        *)               echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

# ---------------------------------------------------------------------------
# Restore mode
# ---------------------------------------------------------------------------

if [[ "$MODE" == "restore" ]]; then
    if [[ -z "$RESTORE_FILE" ]]; then
        echo "Error: --restore requires a file path"
        exit 1
    fi

    if [[ ! -f "$RESTORE_FILE" ]]; then
        echo "Error: backup file not found: $RESTORE_FILE"
        exit 1
    fi

    echo "[backup] Restoring from: $RESTORE_FILE"

    if ! docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        echo "Error: postgres container '$CONTAINER_NAME' is not running"
        echo "Run 'docker compose up -d postgres' first"
        exit 1
    fi

    # Copy the backup file into the container, then restore
    docker cp "$RESTORE_FILE" "${CONTAINER_NAME}:/tmp/restore.sql.gz"
    docker exec "$CONTAINER_NAME" bash -c "gunzip -c /tmp/restore.sql.gz | psql -U crucible -d crucible"
    docker exec "$CONTAINER_NAME" rm -f /tmp/restore.sql.gz

    echo "[backup] Restore complete."
    exit 0
fi

# ---------------------------------------------------------------------------
# Backup mode
# ---------------------------------------------------------------------------

# Ensure backup directory exists
mkdir -p "$BACKUP_DIR"

# Find the postgres container — try the compose name first, fall back to any running postgres
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    PG_CONTAINER="$CONTAINER_NAME"
else
    PG_CONTAINER=$(docker ps --filter "name=postgres" --format '{{.Names}}' | head -1)
    if [[ -z "$PG_CONTAINER" ]]; then
        echo "Error: no running postgres container found"
        exit 1
    fi
    echo "[backup] Using postgres container: $PG_CONTAINER (not the expected $CONTAINER_NAME)"
fi

TIMESTAMP="$(date +%Y%m%d-%H%M%S)"
BACKUP_FILE="$BACKUP_DIR/crucible-${TIMESTAMP}.sql.gz"

echo "[backup] Starting backup to $BACKUP_FILE"
echo "[backup] Container: $PG_CONTAINER"

# Read password from .env if available
POSTGRES_PASSWORD=""
if [[ -f "$PROJECT_ROOT/.env" ]]; then
    POSTGRES_PASSWORD="$(grep '^POSTGRES_PASSWORD=' "$PROJECT_ROOT/.env" | cut -d= -f2- | tr -d '"' | tr -d "'")"
fi

# Build the pg_dump command
if [[ -n "$POSTGRES_PASSWORD" ]]; then
    docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" "$PG_CONTAINER" \
        pg_dump -U crucible -d crucible --no-owner --no-privileges | gzip > "$BACKUP_FILE"
else
    docker exec "$PG_CONTAINER" \
        pg_dump -U crucible -d crucible --no-owner --no-privileges | gzip > "$BACKUP_FILE"
fi

# Verify the backup is not empty
if [[ ! -s "$BACKUP_FILE" ]]; then
    echo "Error: backup file is empty — something went wrong"
    rm -f "$BACKUP_FILE"
    exit 1
fi

BACKUP_SIZE="$(du -h "$BACKUP_FILE" | cut -f1)"
echo "[backup] Backup complete: $BACKUP_FILE ($BACKUP_SIZE)"

# ---------------------------------------------------------------------------
# Retention — keep last N backups, delete the rest
# ---------------------------------------------------------------------------

echo "[backup] Cleaning old backups (keeping last $RETENTION)..."

BACKUP_COUNT=$(ls -1 "$BACKUP_DIR"/crucible-*.sql.gz 2>/dev/null | wc -l)

if [[ "$BACKUP_COUNT" -gt "$RETENTION" ]]; then
    DELETE_COUNT=$((BACKUP_COUNT - RETENTION))
    ls -1t "$BACKUP_DIR"/crucible-*.sql.gz | tail -n "$DELETE_COUNT" | while read -r old; do
        echo "[backup]   Deleting: $(basename "$old")"
        rm -f "$old"
    done
else
    echo "[backup]   $BACKUP_COUNT backups exist, nothing to clean"
fi

# ---------------------------------------------------------------------------
# Optional: remote backup (uncomment and configure for offsite copies)
# ---------------------------------------------------------------------------

# Example: copy to a backup server via scp
# scp "$BACKUP_FILE" backup-user@backup-server:/backups/crucible/

# Example: copy to an S3-compatible bucket via rclone
# rclone copy "$BACKUP_FILE" remote:crucible-backups/

echo "[backup] Done."
