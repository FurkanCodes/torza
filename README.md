# Torza

A cloud torrent service. Add a magnet link and get fast HTTPS links to
download or stream the files. Each torrent is downloaded once and shared by
every user who adds it.

**Status: early.** The code builds and its unit tests pass, but it hasn't run
against real Postgres, NATS and qBittorrent yet. The database queries in
particular need testing against a real database before launch.

How it works: [ARCHITECTURE.md](ARCHITECTURE.md).

## Layout

```
cmd/            the four programs: api, stream, ingest, scheduler
internal/
  api/          REST API handlers
  stream/       file serving with range support
  ingest/       download pipeline (drives qBittorrent)
  scheduler/    cache eviction
  db/           Postgres queries and migrations
  qbit/         qBittorrent Web API client
  links/        signed download links
  magnet/       magnet link parsing
  store/        cache folders on disk
  auth/         API keys
  jobs/         NATS wake-up messages
  config/       environment settings
  run/          shared startup code
deploy/         docker-compose, Caddy, example settings
```

## Development

```sh
go test ./...   # unit tests; no database, network or torrents needed
go vet ./...
go build ./...
```

## API

All `/v1` requests need `Authorization: Bearer <api key>`.

| Method | Path | Does |
|--------|------|------|
| `GET` | `/v1/me` | Who you are |
| `POST` | `/v1/torrents` | Add a torrent: `{"magnet": "magnet:?xt=urn:btih:..."}` (a bare info hash also works) |
| `GET` | `/v1/torrents` | Your library |
| `GET` | `/v1/torrents/{infohash}` | One torrent, with its files once ready |
| `DELETE` | `/v1/torrents/{infohash}` | Remove from your library |
| `POST` | `/v1/files/{id}/link` | Get a signed download link: `{"url": ..., "expires_at": ...}` |
| `POST` | `/v1/admin/blocklist` | Admins: takedown. `{"torrent": "<magnet or hash>", "reason": "...", "reference": "..."}` |

Example:

```sh
curl -H "Authorization: Bearer $KEY" -d '{"magnet":"magnet:?xt=urn:btih:..."}' https://torza.example.com/v1/torrents
curl -H "Authorization: Bearer $KEY" https://torza.example.com/v1/torrents/<infohash>
curl -H "Authorization: Bearer $KEY" -X POST https://torza.example.com/v1/files/<file id>/link
```

Error codes worth knowing: `429` means you have too many downloads running,
`451` means the torrent was removed after a takedown, and `410` from a download
link means it expired (create a new one).

## Running locally (Mac)

Needs Docker Desktop. Everything runs in containers; nothing is installed on
the Mac itself.

```sh
deploy/local.sh up                      # build the image and start everything
deploy/local.sh user you@example.com -admin   # prints your API key once
deploy/local.sh logs ingest             # or api, stream, scheduler, qbittorrent
deploy/local.sh down                    # stop (downloads and the database are kept)
```

The first `up` creates `deploy/.env.local` with fresh secrets. qBittorrent
prints a temporary Web UI password on its first start
(`deploy/local.sh logs qbittorrent`); log in at http://localhost:8090, set a
permanent password, put it in `deploy/.env.local` as `QBIT_PASS`, and turn
off torrent queueing. Then `deploy/local.sh up` again.

The API is at http://localhost:8080 and download links point at
http://localhost:8081, both reachable from this Mac only. Open a link in
VLC, IINA or QuickTime to stream it. Downloads use your home connection, so
test with legal torrents such as Linux ISOs or Creative Commons films.

To use it from a phone or TV on the same Wi-Fi, run `deploy/local.sh lan on`.
It prints the address to open (for example http://192.168.1.166:8080), and
new download links point at that address too. The API still needs a key and
every link is still signed; qBittorrent's Web UI stays on this Mac only. Run
it again when the Mac's address changes, and `deploy/local.sh lan off` to
close it. Links made before switching keep the old address: make new ones.

## Deploying (first time)

These steps haven't been run yet. Check each one on the first deploy.

1. On the server, install Docker, clone this repo, then `cd deploy`.
2. `cp .env.example .env` and fill it in.
3. Create the data folder and give it to user 1000 (the user both Torza and
   qBittorrent run as): `mkdir -p data && sudo chown 1000:1000 data`.
4. Start qBittorrent alone first: `docker compose up -d qbittorrent`.
5. **First start: set qBittorrent's password.** The image prints a temporary
   password in its log: `docker compose logs qbittorrent`. From your own
   computer, open a tunnel with `ssh -L 8090:127.0.0.1:8090 you@server`, then
   go to http://localhost:8090 and log in as `admin`. In Tools → Options:
   - Web UI: set a permanent password, and put the same one in `.env` as `QBIT_PASS`
   - BitTorrent: turn off torrent queueing, or raise the limits, so torrents don't wait in qBittorrent's own queue
6. Start everything: `docker compose up -d --build`.
7. Create your admin user:
   `docker compose run --rm api api create-user -email you@example.com -admin`.
   Save the API key it prints.
8. Test with a legal torrent, such as an Ubuntu release or a public-domain film.

## Takedowns

When a rightsholder sends a notice, block the torrent:

```sh
curl -H "Authorization: Bearer $ADMIN_KEY" \
  -d '{"torrent":"<magnet or info hash>","reason":"copyright notice","reference":"<notice id>"}' \
  https://torza.example.com/v1/admin/blocklist
```

It can't be added again, existing links stop working, and its files are
deleted within one ingest check.
