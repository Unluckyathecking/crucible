#!/usr/bin/env bash
# deploy.sh — pull latest code and restart Crucible services
#
# Usage:
#   bash deploy/deploy.sh              # deploy from current directory
#   bash deploy/deploy.sh --skip-pull  # skip git pull (useful in CI)
#
# Idempotent: safe to re-run.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

SKIP_PULL=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --skip-pull) SKIP_PULL=true; shift ;;
        -h|--help)
            echo "Usage: deploy.sh [OPTIONS]"
            echo ""
            echo "Pull latest code and restart Crucible Docker services."
            echo ""
            echo "Options:"
            echo "  --skip-pull    Skip git pull (useful in CI/CD pipelines)"
            echo "  -h, --help     Show this help message"
            exit 0
            ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

cd "$PROJECT_ROOT"

# ---------------------------------------------------------------------------
# Pre-flight checks
# ---------------------------------------------------------------------------

if [[ ! -f docker-compose.yml ]]; then
    echo "Error: docker-compose.yml not found in $PROJECT_ROOT"
    exit 1
fi

if [[ ! -f .env ]]; then
    echo "Error: .env not found. Run hetzner-bootstrap.sh first, or copy .env.example to .env"
    exit 1
fi

if ! command -v docker &>/dev/null; then
    echo "Error: docker is not installed"
    exit 1
fi

if ! docker compose version &>/dev/null; then
    echo "Error: docker compose plugin is not installed"
    exit 1
fi

echo "[deploy] Project root: $PROJECT_ROOT"
echo "[deploy] Docker: $(docker --version)"
echo "[deploy] Compose: $(docker compose version)"

# ---------------------------------------------------------------------------
# 1. Git pull
# ---------------------------------------------------------------------------

if [[ "$SKIP_PULL" == "true" ]]; then
    echo "[deploy] Skipping git pull (--skip-pull)"
else
    if ! command -v git &>/dev/null; then
        echo "Error: git is not installed, and --skip-pull was not set"
        exit 1
    fi

    echo "[deploy] Pulling latest code..."
    git pull origin main
    echo "[deploy] At commit: $(git log --oneline -1)"
fi

# ---------------------------------------------------------------------------
# 2. Pull Docker images
# ---------------------------------------------------------------------------

echo "[deploy] Pulling Docker images..."
docker compose pull

# ---------------------------------------------------------------------------
# 3. Start services
# ---------------------------------------------------------------------------

echo "[deploy] Starting services..."
docker compose up -d --remove-orphans

# ---------------------------------------------------------------------------
# 4. Wait for health checks
# ---------------------------------------------------------------------------

echo "[deploy] Waiting for services to be healthy..."

MAX_WAIT=120
ELAPSED=0
INTERVAL=5

while [[ $ELAPSED -lt $MAX_WAIT ]]; do
    unhealthy=$(docker compose ps --format '{{.Name}}: {{.Status}}' 2>/dev/null | grep -c "unhealthy" || true)
    starting=$(docker compose ps --format '{{.Name}}: {{.Status}}' 2>/dev/null | grep -c "starting" || true)

    if [[ "$unhealthy" -eq 0 && "$starting" -eq 0 ]]; then
        break
    fi

    echo "[deploy]   Still waiting... ($unhealthy unhealthy, $starting starting)"
    sleep "$INTERVAL"
    ELAPSED=$((ELAPSED + INTERVAL))
done

if [[ $ELAPSED -ge $MAX_WAIT ]]; then
    echo "[deploy] WARNING: Timeout reached after ${MAX_WAIT}s. Some services may not be healthy."
    echo "[deploy] Check with: docker compose ps"
fi

# ---------------------------------------------------------------------------
# 5. Show status
# ---------------------------------------------------------------------------

echo ""
echo "[deploy] Service status:"
docker compose ps

echo ""
echo "[deploy] Gateway health check:"
if curl -sf http://localhost:8080/healthz &>/dev/null; then
    echo "[deploy]   Gateway is responding"
else
    echo "[deploy]   Gateway health check failed (may need a moment to start)"
fi

echo ""
echo "[deploy] Done."
