#!/bin/sh
# Run Torza locally with Docker. Usage, from anywhere:
#   deploy/local.sh up        build and start everything
#   deploy/local.sh down      stop everything (data is kept)
#   deploy/local.sh logs [service]
#   deploy/local.sh ps
#   deploy/local.sh user EMAIL [-admin]   create a user and print its API key
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
	dc up -d --no-build
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
*)
	sed -n '2,8p' "$0"
	exit 2
	;;
esac
