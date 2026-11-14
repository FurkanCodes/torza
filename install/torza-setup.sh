#!/usr/bin/env bash
# Torza installer. Sets up Torza in a folder on this machine with Docker:
# settings with fresh secrets, qBittorrent with its password already set,
# the services, and your admin account.
#
#   curl -fsSL https://raw.githubusercontent.com/FurkanCodes/torza/main/install/torza-setup.sh -o torza-setup.sh
#   bash torza-setup.sh
#
# Run it again in the same folder to update Torza; your settings and data stay.
#
# Settings (environment variables, all optional):
#   TORZA_DIR      install folder (default: ./torza)
#   TORZA_VERSION  image tag to run (default: latest)
#   TORZA_MODE     domain | tailscale | local (default: ask)
#   TORZA_DOMAIN   your domain, for domain mode
#   TORZA_EMAIL    the admin account's email
#   TORZA_SOURCE   a Torza checkout to copy files from instead of downloading
#                  (for testing the installer)
set -euo pipefail

REPO="FurkanCodes/torza"
REF="${TORZA_REF:-main}"
VERSION="${TORZA_VERSION:-latest}"
IMAGE="${TORZA_IMAGE:-ghcr.io/furkancodes/torza:$VERSION}"
DIR="${TORZA_DIR:-$PWD/torza}"

say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
die() { printf 'Error: %s\n' "$*" >&2; exit 1; }
ask() { # ask VAR "question" [default]
	local answer
	read -r -p "$2${3:+ [$3]}: " answer </dev/tty || true
	printf -v "$1" '%s' "${answer:-${3:-}}"
}

# ── Checks ──────────────────────────────────────────────────────────────────

command -v docker >/dev/null || die "Docker isn't installed. Install it first: https://docs.docker.com/engine/install/"
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 isn't available ('docker compose'). Update Docker."
docker info >/dev/null 2>&1 || die "Docker isn't running, or this user can't use it (try: sudo usermod -aG docker \$USER, then log in again)."
command -v openssl >/dev/null || die "openssl is needed to generate secrets."
command -v curl >/dev/null || die "curl is needed."

mkdir -p "$DIR"
cd "$DIR"
say "Installing Torza in $DIR"

fetch() { # fetch <path in repo> <destination>
	if [ -n "${TORZA_SOURCE:-}" ]; then
		cp "$TORZA_SOURCE/$1" "$2"
	else
		curl -fsSL "https://raw.githubusercontent.com/$REPO/$REF/$1" -o "$2" || die "couldn't download $1"
	fi
}

dc() { docker compose "$@"; }

# ── Update an existing install ──────────────────────────────────────────────

if [ -f .env ]; then
	step "Found an existing install: updating it"
	fetch deploy/docker-compose.yml docker-compose.yml
	fetch deploy/Caddyfile Caddyfile
	sed -i.bak -e "s#^TORZA_IMAGE=.*#TORZA_IMAGE=$IMAGE#" .env && rm -f .env.bak
	dc pull --quiet --ignore-pull-failures 2>/dev/null || true
	docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" || die "couldn't get the Torza image $IMAGE"
	dc up -d --remove-orphans
	say "Updated. Your settings and data were kept."
	exit 0
fi

# ── Questions ───────────────────────────────────────────────────────────────

MODE="${TORZA_MODE:-}"
if [ -z "$MODE" ]; then
	say ""
	say "How will people reach this Torza?"
	say "  1) My own domain, with HTTPS (a server with ports 80 and 443 open,"
	say "     and the domain's DNS pointing at it)"
	say "  2) Tailscale: only devices signed in to my Tailscale can reach it"
	say "  3) This machine only (for trying it out)"
	ask choice "Choose 1, 2 or 3" 2
	case "$choice" in 1) MODE=domain ;; 2) MODE=tailscale ;; 3) MODE=local ;; *) die "please choose 1, 2 or 3" ;; esac
fi

DOMAIN="${TORZA_DOMAIN:-}"
case "$MODE" in
domain)
	[ -n "$DOMAIN" ] || ask DOMAIN "Domain (for example torza.example.com)"
	[ -n "$DOMAIN" ] || die "a domain is needed"
	PUBLIC_URL="https://$DOMAIN"
	API_URL="https://$DOMAIN"
	PROFILES=caddy
	;;
tailscale)
	TS=$(command -v tailscale || echo /Applications/Tailscale.app/Contents/MacOS/Tailscale)
	[ -x "$TS" ] || die "Tailscale isn't installed: https://tailscale.com/download"
	info=$("$TS" status --json 2>/dev/null) || die "Tailscale isn't running or signed in. Sign in, then run this again."
	DOMAIN=$(printf '%s' "$info" | sed -n 's/.*"DNSName": *"\([^"]*\)\.".*/\1/p' | head -1)
	printf '%s' "$info" | grep -q '"CertDomains": *\[' ||
		die "turn on HTTPS certificates at https://login.tailscale.com/admin/dns (HTTPS Certificates → Enable), then run this again"
	[ -n "$DOMAIN" ] || die "couldn't read this machine's Tailscale name"
	PUBLIC_URL="https://$DOMAIN:8443"
	API_URL="https://$DOMAIN"
	PROFILES=""
	;;
local)
	DOMAIN=localhost
	PUBLIC_URL="http://localhost:${STREAM_PORT:-8081}"
	API_URL=""
	PROFILES=""
	;;
*) die "TORZA_MODE must be domain, tailscale or local" ;;
esac

EMAIL="${TORZA_EMAIL:-}"
[ -n "$EMAIL" ] || ask EMAIL "Your email, for the admin account"
[ -n "$EMAIL" ] || die "an email is needed"

# ── Files ───────────────────────────────────────────────────────────────────

step "Writing settings"
fetch deploy/docker-compose.yml docker-compose.yml
fetch deploy/Caddyfile Caddyfile
QBIT_PASS=$(openssl rand -hex 16)
umask 077
cat >.env <<EOF
# Torza settings, written by torza-setup.sh. Keep this file private.
TORZA_IMAGE=$IMAGE
COMPOSE_PROJECT_NAME=${COMPOSE_PROJECT_NAME:-torza}
COMPOSE_PROFILES=$PROFILES

DOMAIN=$DOMAIN
PUBLIC_URL=$PUBLIC_URL
API_PUBLIC_URL=$API_URL

SIGNING_KEY=$(openssl rand -hex 32)
POSTGRES_PASSWORD=$(openssl rand -hex 24)
QBIT_USER=admin
QBIT_PASS=$QBIT_PASS
QBIT_CONFIG=./qbittorrent
DATA_PATH=./data

# Ports on this machine (the API and stream ports are on 127.0.0.1 only).
API_PORT=${API_PORT:-8080}
STREAM_PORT=${STREAM_PORT:-8081}
TORRENT_PORT=${TORRENT_PORT:-6881}
QBIT_UI_PORT=${QBIT_UI_PORT:-8090}

# Limits (see ARCHITECTURE.md). 0 means no limit.
MAX_ACTIVE_PER_USER=3
MAX_TORRENT_BYTES=100GB
CACHE_MAX_BYTES=0
CACHE_MIN_FREE_BYTES=20GB
EVICT_IDLE_FOR=6h
LINK_TTL=6h
EOF
umask 022

mkdir -p data qbittorrent/qBittorrent
step "Getting Torza ($IMAGE)"
dc pull --quiet --ignore-pull-failures 2>/dev/null || true
docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull "$IMAGE" || die "couldn't get the Torza image $IMAGE"
# qBittorrent starts with the password Torza uses, and without its own
# download queue (Torza decides what runs).
docker run --rm "$IMAGE" api qbit-config -password "$QBIT_PASS" >qbittorrent/qBittorrent/qBittorrent.conf

# Torza and qBittorrent both run as user 1000 inside their containers.
if [ "$(uname)" = Linux ] && [ "$(stat -c %u data)" != 1000 ]; then
	if [ "$(id -u)" = 0 ]; then chown -R 1000:1000 data qbittorrent
	else sudo chown -R 1000:1000 data qbittorrent; fi
fi

step "Starting Torza"
dc up -d
for _ in $(seq 60); do
	curl -fsS "http://127.0.0.1:${API_PORT:-8080}/healthz" >/dev/null 2>&1 && break
	sleep 2
done
curl -fsS "http://127.0.0.1:${API_PORT:-8080}/healthz" >/dev/null 2>&1 || die "Torza didn't start; see: docker compose logs api"

if [ "$MODE" = tailscale ]; then
	step "Serving over Tailscale HTTPS"
	"$TS" serve --bg --https=443 "http://127.0.0.1:${API_PORT:-8080}" >/dev/null
	"$TS" serve --bg --https=8443 "http://127.0.0.1:${STREAM_PORT:-8081}" >/dev/null
fi

step "Creating your admin account"
out=$(dc run --rm --no-deps api api create-user -email "$EMAIL" -admin)
key=$(printf '%s\n' "$out" | sed -n 's/^API key: //p')
[ -n "$key" ] || die "couldn't create the admin account: $out"
umask 077
printf '%s\n' "$key" >admin-api-key
umask 022

case "$MODE" in
domain) web="https://$DOMAIN" ;;
tailscale) web="https://$DOMAIN" ;;
local) web="http://127.0.0.1:${API_PORT:-8080}" ;;
esac

cat <<EOF

Torza is running.

  Open:        $web
  API key:     $key
               (also saved in $DIR/admin-api-key; it can't be shown again)
  Add people:  cd $DIR && docker compose run --rm api api create-user -email friend@example.com
  Logs:        cd $DIR && docker compose logs -f
  Update:      run this installer again in $(dirname "$DIR")
  Stop:        cd $DIR && docker compose down   (data is kept)

You are the operator of this Torza: the content it downloads comes from
this machine's connection, and you handle any takedown requests for it
(Torza's blocklist: POST /v1/admin/blocklist). Read docs/INSTALL.md.
EOF
