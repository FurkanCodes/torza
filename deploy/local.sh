#!/bin/sh
# Run Torza locally with Docker. Usage, from anywhere:
#   deploy/local.sh up        build and start everything
#   deploy/local.sh down      stop everything (data is kept)
#   deploy/local.sh logs [service]
#   deploy/local.sh ps
#   deploy/local.sh user EMAIL [-admin]   create a user and print its API key
#   deploy/local.sh lan on|off   open Torza to other devices on your Wi-Fi, or close it
#   deploy/local.sh tailscale on|off   serve Torza over HTTPS to your Tailscale devices (phone, TV)
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

# set_env KEY VALUE writes one setting in .env.local.
set_env() {
	grep -q "^$1=" .env.local || echo "$1=" >>.env.local
	sed -i '' -e "s#^$1=.*#$1=$2#" .env.local
}

dc() {
	docker compose -f docker-compose.yml -f docker-compose.build.yml --env-file .env.local "$@"
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
	set_env PUBLIC_URL "$url"
	set_env BIND_ADDR "$bind"
	set_env API_PUBLIC_URL ""
	dc up -d --no-build api stream
	echo "web page: http://$ip:8080   stream links: $url"
	[ "$1" = on ] && echo "If macOS asks whether Docker may accept incoming connections, allow it. Rerun this when the Mac's address changes."
	;;
tailscale)
	ts=$(command -v tailscale || echo /Applications/Tailscale.app/Contents/MacOS/Tailscale)
	[ -x "$ts" ] || { echo "Tailscale isn't installed: brew install --cask tailscale" >&2; exit 1; }
	case "${1:-}" in
	on)
		info=$("$ts" status --json) || { echo "Tailscale isn't running or signed in. Open the Tailscale app and sign in." >&2; exit 1; }
		name=$(echo "$info" | python3 -c "import sys,json;print(json.load(sys.stdin)['Self']['DNSName'].rstrip('.'))")
		certs=$(echo "$info" | python3 -c "import sys,json;print(1 if json.load(sys.stdin).get('CertDomains') else 0)")
		if [ "$certs" != 1 ]; then
			echo "Turn on HTTPS certificates first: https://login.tailscale.com/admin/dns" >&2
			echo "(scroll to HTTPS Certificates and click Enable), then run this again." >&2
			exit 1
		fi
		set_env PUBLIC_URL "https://$name:8443"
		set_env API_PUBLIC_URL "https://$name"
		dc up -d --no-build api stream
		# Tailscale terminates HTTPS with a real certificate for the .ts.net
		# name and forwards to Torza on this Mac. Only your Tailscale devices
		# can reach it.
		"$ts" serve --bg --https=443 http://127.0.0.1:8080 >/dev/null
		"$ts" serve --bg --https=8443 http://127.0.0.1:8081 >/dev/null
		# Keep the Mac from sleeping while it serves (closing the lid still
		# sleeps it, unless an external display is attached).
		if [ -f .caffeinate.pid ] && kill -0 "$(cat .caffeinate.pid)" 2>/dev/null; then :; else
			nohup caffeinate -i >/dev/null 2>&1 &
			echo $! >.caffeinate.pid
		fi
		echo "Torza on Tailscale: https://$name"
		echo "Open that on your phone or TV (with the Tailscale app signed in), make a"
		echo "Stremio add-on link there, and paste it into Stremio's add-on search."
		echo "The first visit can take a few seconds while Tailscale gets the certificate."
		;;
	off)
		"$ts" serve --https=443 off >/dev/null 2>&1 || true
		"$ts" serve --https=8443 off >/dev/null 2>&1 || true
		set_env PUBLIC_URL "http://localhost:8081"
		set_env API_PUBLIC_URL ""
		dc up -d --no-build api stream
		if [ -f .caffeinate.pid ]; then
			kill "$(cat .caffeinate.pid)" 2>/dev/null || true
			rm -f .caffeinate.pid
		fi
		echo "Tailscale serving is off; the Mac may sleep again."
		;;
	*) echo "usage: $0 tailscale on|off" >&2; exit 2 ;;
	esac
	;;
*)
	sed -n '2,10p' "$0"
	exit 2
	;;
esac
