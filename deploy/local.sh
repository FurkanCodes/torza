#!/bin/sh
# Run Torza locally with Docker. Usage, from anywhere:
#   deploy/local.sh up        build and start everything
#   deploy/local.sh down      stop everything (data is kept)
#   deploy/local.sh logs [service]
#   deploy/local.sh ps
#   deploy/local.sh user EMAIL [-admin]   create a user and print its API key
#   deploy/local.sh lan on|off   open Torza to other devices on your Wi-Fi, or close it
set -eu
cd "$(dirname "$0")"

if [ ! -f .env.local ]; then
	cp .env.local.example .env.local
	sed -i '' \
		-e "s/^SIGNING_KEY=.*/SIGNING_KEY=$(openssl rand -hex 32)/" \
		-e "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$(openssl rand -hex 24)/" \
		.env.local
	echo "created deploy/.env.local"
fi
mkdir -p data-local

dc() {
	docker compose -f docker-compose.yml -f docker-compose.local.yml --env-file .env.local "$@"
}

cmd=${1:-}
[ $# -gt 0 ] && shift
case "$cmd" in
up)
	# Build the shared image once; four services building it at the same
	# time collide on the tag.
	dc build api
	dc up -d --no-build --remove-orphans
	;;
down) dc down ;;
logs) dc logs --tail=100 "$@" ;;
ps) dc ps ;;
user)
	[ $# -ge 1 ] || { echo "usage: $0 user EMAIL [-admin]" >&2; exit 2; }
	email=$1
	shift
	dc run --rm api api create-user -email "$email" "$@"
	;;
lan)
	case "${1:-}" in
	on)
		ip=$(ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || true)
		[ -n "$ip" ] || { echo "no Wi-Fi or Ethernet address found" >&2; exit 1; }
		bind=0.0.0.0
		url=http://$ip:8081
		;;
	off)
		ip=localhost
		bind=127.0.0.1
		url=http://localhost:8081
		;;
	*) echo "usage: $0 lan on|off" >&2; exit 2 ;;
	esac
	# Older .env.local files have no BIND_ADDR line yet.
	grep -q '^BIND_ADDR=' .env.local || echo "BIND_ADDR=127.0.0.1" >>.env.local
	sed -i '' -e "s#^PUBLIC_URL=.*#PUBLIC_URL=$url#" -e "s#^BIND_ADDR=.*#BIND_ADDR=$bind#" .env.local
	dc up -d --no-build api stream
	echo "web page: http://$ip:8080   stream links: $url"
	[ "$1" = on ] && echo "If macOS asks whether Docker may accept incoming connections, allow it. Rerun this when the Mac's address changes."
	;;
*)
	sed -n '2,9p' "$0"
	exit 2
	;;
esac
