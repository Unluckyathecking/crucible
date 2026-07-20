#!/usr/bin/env bash
# hetzner-bootstrap.sh — first-run server setup for Crucible on Hetzner Cloud
#
# Run this once on a fresh Ubuntu 24.04 server (CX22 or similar).
# Installs Docker, configures the firewall, and sets up the .env file.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/YOU/crucible/main/deploy/hetzner-bootstrap.sh | bash
#   # or, after cloning the repo:
#   bash deploy/hetzner-bootstrap.sh
#
# Idempotent: safe to re-run.

set -euo pipefail

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log()  { echo "[bootstrap] $*"; }
ok()   { echo "[bootstrap]  OK: $*"; }
warn() { echo "[bootstrap] WARN: $*" >&2; }

require_root() {
    if [[ $EUID -ne 0 ]]; then
        echo "This script must be run as root (or with sudo)."
        exit 1
    fi
}

# ---------------------------------------------------------------------------
# Usage / help
# ---------------------------------------------------------------------------

usage() {
    cat <<'EOF'
Usage: hetzner-bootstrap.sh [OPTIONS]

First-run setup for a fresh Hetzner Cloud Ubuntu 24.04 server.

Options:
  -h, --help        Show this help message
  --skip-firewall   Skip ufw configuration (useful if firewall is managed externally)

What it does:
  1. Installs Docker + docker-compose plugin
  2. Configures ufw firewall (allow 22, 80, 443)
  3. Creates a .env file from .env.example template
  4. Sets up a daily cron job for database backups at 3am
EOF
    exit 0
}

SKIP_FIREWALL=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help)       usage ;;
        --skip-firewall) SKIP_FIREWALL=true; shift ;;
        *)               warn "Unknown option: $1"; shift ;;
    esac
done

# ---------------------------------------------------------------------------
# Pre-flight
# ---------------------------------------------------------------------------

require_root

# Detect the project root — assume this script lives inside the repo at deploy/
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

if [[ ! -f "$PROJECT_ROOT/docker-compose.yml" ]]; then
    echo "Error: docker-compose.yml not found at $PROJECT_ROOT"
    echo "Make sure this script is inside the crucible repo at deploy/"
    exit 1
fi

log "Project root: $PROJECT_ROOT"

# ---------------------------------------------------------------------------
# 1. Docker + docker-compose plugin
# ---------------------------------------------------------------------------

install_docker() {
    if command -v docker &>/dev/null; then
        ok "Docker already installed: $(docker --version)"
        return
    fi

    log "Installing Docker..."

    # Remove any old packages that might conflict
    for pkg in docker.io docker-doc docker-compose docker-compose-v2 \
               podman-docker containerd runc; do
        apt-get remove -y "$pkg" 2>/dev/null || true
    done

    # Install prerequisites
    apt-get update
    apt-get install -y ca-certificates curl gnupg

    # Add Docker's official GPG key
    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
        -o /etc/apt/keyrings/docker.asc
    chmod a+r /etc/apt/keyrings/docker.asc

    # Add the repository
    echo \
        "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
        https://download.docker.com/linux/ubuntu \
        $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
        | tee /etc/apt/sources.list.d/docker.list > /dev/null

    # Install Docker Engine + compose plugin
    apt-get update
    apt-get install -y docker-ce docker-ce-cli containerd.io \
        docker-buildx-plugin docker-compose-plugin

    # Enable and start
    systemctl enable --now docker

    ok "Docker installed: $(docker --version)"
    ok "Compose plugin: $(docker compose version)"
}

# No host reverse proxy: the docker-compose caddy service owns 80/443 and is wired
# to the repo Caddyfile. A host Caddy would fight the container for those ports.

# ---------------------------------------------------------------------------
# 2. Firewall (ufw)
# ---------------------------------------------------------------------------

configure_firewall() {
    if [[ "$SKIP_FIREWALL" == "true" ]]; then
        log "Skipping firewall configuration (--skip-firewall)"
        return
    fi

    if command -v ufw &>/dev/null && ufw status verbose &>/dev/null; then
        local status
        status="$(ufw status | head -1)"
        if [[ "$status" == *"active"* ]]; then
            ok "ufw is already active"
            return
        fi
    fi

    log "Configuring ufw firewall..."

    apt-get install -y ufw

    # Default policies
    ufw default deny incoming
    ufw default allow outgoing

    # Allow essential ports
    ufw allow 22/tcp   comment "SSH"
    ufw allow 80/tcp   comment "HTTP (Caddy)"
    ufw allow 443/tcp  comment "HTTPS (Caddy)"

    # Enable (non-interactive)
    echo "y" | ufw enable

    ok "ufw enabled — ports 22, 80, 443 open"
}

# ---------------------------------------------------------------------------
# 3. Create .env from template
# ---------------------------------------------------------------------------

setup_env() {
    local env_file="$PROJECT_ROOT/.env"

    if [[ -f "$env_file" ]]; then
        ok ".env already exists at $env_file"
        log "Review it and make sure passwords are set before deploying."
        return
    fi

    if [[ ! -f "$PROJECT_ROOT/.env.example" ]]; then
        warn ".env.example not found — skipping .env creation"
        return
    fi

    log "Creating .env from .env.example..."
    cp "$PROJECT_ROOT/.env.example" "$env_file"

    # Generate a random postgres password if still at placeholder
    if grep -q "REPLACE_WITH_HEX_OUTPUT" "$env_file"; then
        local new_pass
        new_pass="$(openssl rand -hex 32)"
        sed -i "s|REPLACE_WITH_HEX_OUTPUT_FROM_openssl_rand_hex_32|${new_pass}|" "$env_file"
        log "Generated POSTGRES_PASSWORD"
    fi

    # Generate grafana admin password if still at placeholder
    if grep -q "REPLACE_WITH_STRONG_GRAFANA_PASSWORD" "$env_file"; then
        local gf_pass
        gf_pass="$(openssl rand -base64 24)"
        sed -i "s|REPLACE_WITH_STRONG_GRAFANA_PASSWORD|${gf_pass}|" "$env_file"
        log "Generated GF_ADMIN_PASSWORD"
    fi

    # Generate API key salt if still at placeholder
    if grep -q "REPLACE_WITH_32_PLUS_BYTES" "$env_file"; then
        local salt
        salt="$(openssl rand -base64 32)"
        sed -i "s|REPLACE_WITH_32_PLUS_BYTES_OF_RANDOM_DATA__REPLACE_ME|${salt}|" "$env_file"
        log "Generated API_KEY_HASH_SALT"
    fi

    chmod 600 "$env_file"
    ok ".env created at $env_file (mode 600)"
    log "IMPORTANT: Fill in STRIPE_SECRET_KEY and STRIPE_WEBHOOK_SECRET manually."
}

# ---------------------------------------------------------------------------
# 4. Daily backup cron
# ---------------------------------------------------------------------------

setup_backup_cron() {
    local cron_cmd="0 3 * * * $PROJECT_ROOT/deploy/backup.sh >> /var/log/crucible-backup.log 2>&1"

    # Check if already installed
    if crontab -l 2>/dev/null | grep -q "deploy/backup.sh"; then
        ok "Backup cron already configured"
        return
    fi

    log "Adding daily backup cron (3am)..."

    # Create log file
    touch /var/log/crucible-backup.log

    # Add to root's crontab
    (crontab -l 2>/dev/null; echo "$cron_cmd") | crontab -

    ok "Cron job added: $cron_cmd"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
    log "Starting Hetzner bootstrap..."
    log "Server: $(hostname) | $(cat /etc/os-release | grep PRETTY_NAME | cut -d= -f2)"

    install_docker
    configure_firewall
    setup_env
    setup_backup_cron

    log ""
    log "Bootstrap complete. Next steps:"
    log "  1. Edit $PROJECT_ROOT/.env and set Stripe keys"
    log "  2. If using a domain, set DOMAIN in $PROJECT_ROOT/.env (the compose Caddy reads $PROJECT_ROOT/Caddyfile)"
    log "  3. Run: bash $PROJECT_ROOT/deploy/deploy.sh"
    log ""
    ok "Done."
}

main "$@"
