# Torza

A small, private debrid service. Check whether content is already cached,
add a magnet link or .torrent file, and get direct HTTP links to download or
stream its files. Each item is downloaded once and shared by every user who
adds it; adding something cached is instant.

**Status: early.** It runs locally on a Mac with Docker (below) and its
database tests run against a real Postgres, but it hasn't been deployed to a
server yet. Design: [docs/designs/torza.md](docs/designs/torza.md).

## Install

On any machine with Docker (a server, a home server or a Mac):

```sh
curl -fsSL https://raw.githubusercontent.com/FurkanCodes/torza/main/install/torza-setup.sh -o torza-setup.sh
bash torza-setup.sh
```

It asks whether people reach Torza through your own domain (HTTPS), through
Tailscale (private, no server needed) or from this machine only, and prints
your admin API key. Details, settings and updates: [docs/INSTALL.md](docs/INSTALL.md).

How it works: [ARCHITECTURE.md](ARCHITECTURE.md).

## Layout

```
cmd/            the four programs: api, stream, ingest, scheduler
internal/
  api/          REST API handlers and the test web page
  service/      rules shared by the API and the stream service
  stream/       file serving with range support
  ingest/       download loop: one per fetcher, crash recovery, policy
  source/       the interface every fetcher fits; qbitsource (qBittorrent)
  scheduler/    eviction, takedown purges, orphan-folder sweep
  db/           Postgres queries, migrations, LISTEN/NOTIFY wake-ups
  dbtest/       per-test Postgres schemas for database tests
  qbit/         qBittorrent Web API client
  links/        signed download links
  magnet/       magnet link and .torrent file parsing
  store/        cache folders on disk
  auth/         API keys
  config/       environment settings
  run/          shared startup code
deploy/         docker-compose, Caddy, example settings
```

## Development

```sh
go test ./...   # database tests skip without TEST_DATABASE_URL
go vet ./...
go build ./...
```

Database tests need a Postgres; a throwaway one:

```sh
docker run -d --rm --name torza-testpg -e POSTGRES_USER=torza -e POSTGRES_PASSWORD=torza \
  -e POSTGRES_DB=torza_test -p 127.0.0.1:55432:5432 postgres:17-alpine
TEST_DATABASE_URL='postgres://torza:torza@127.0.0.1:55432/torza_test?sslmode=disable' go test -race ./...
```

## API

All `/v1` requests need `Authorization: Bearer <api key>`.

| Method | Path | Does |
|--------|------|------|
| `GET` | `/v1/me` | Who you are, your limits, active downloads and library size |
| `POST` | `/v1/items/check` | Is it cached? `{"items": ["<magnet or hash>", ...]}` (up to 100) → `cached` (with files), `downloading`, `not_cached`, `blocked` or `invalid` per input. Starts nothing. |
| `POST` | `/v1/items` | Add: `{"magnet": "magnet:?xt=urn:btih:..."}` (a bare info hash works), or a multipart upload with the `.torrent` file in field `torrent`. `200` = already cached, ready now; `202` = download started |
| `GET` | `/v1/items` | Your library |
| `GET` | `/v1/items/{id}` | One item, with its files once ready |
| `DELETE` | `/v1/items/{id}` | Remove from your library (the cached copy stays for others) |
| `POST` | `/v1/items/{id}/links` | Signed links for every file of a ready item |
| `POST` | `/v1/files/{id}/link` | A signed link for one file: `{"url": ..., "expires_at": ...}` |
| `POST` | `/v1/admin/blocklist` | Owner: takedown. `{"torrent": "<magnet or hash>", "reason": "...", "reference": "..."}` |
| `POST` | `/v1/stremio` | Make a Stremio add-on link for one device: `{"label": "Living room TV"}` → `manifest_url` and a `stremio://` `install_url` (shown once) |
| `GET` | `/v1/stremio` | Your add-on links |
| `DELETE` | `/v1/stremio/{id}` | Remove one; Stremio on that device stops seeing your library |
| `PUT` | `/v1/admin/users/{id}/limits` | Owner: `{"max_active": 3, "max_item_bytes": 50000000000, "priority": 0}`; `null` = server default |

Example:

```sh
curl -H "Authorization: Bearer $KEY" -d '{"items":["<hash>"]}' http://localhost:8080/v1/items/check
curl -H "Authorization: Bearer $KEY" -d '{"magnet":"magnet:?xt=urn:btih:..."}' http://localhost:8080/v1/items
curl -H "Authorization: Bearer $KEY" -F torrent=@file.torrent http://localhost:8080/v1/items
curl -H "Authorization: Bearer $KEY" -X POST http://localhost:8080/v1/items/<id>/links
```

Status codes worth knowing: `202` from a link request means the item had
been evicted and is downloading again (ask again when it's ready); `429`
means you have as many downloads running as your limit allows; `451` means
the content was removed after a takedown; `410` from a download link means it
expired (make a new one).

## Stremio

Torza is a Stremio add-on for your own library: what you added shows up in
Stremio under Discover → Torza library, and plays from the cache. Items with
several video files show as a series, one episode per file. Something that
was evicted downloads again when you press play; try again a minute later.
It never searches for content.

Make a link on the web page ("Watch in Stremio") or with `POST /v1/stremio`,
then open the `stremio://` link or paste the manifest URL into Stremio's
add-on search. Stremio installs plain-HTTP add-ons only from `127.0.0.1` on
the same computer, so locally open the page at http://127.0.0.1:8080 to make
the link. Phones and TVs need HTTPS: use Tailscale (below) or a server.

### Phones and TVs through Tailscale (no server needed)

Tailscale gives the Mac a private HTTPS address that only your own devices
can reach, with a real certificate Stremio accepts.

1. Install Tailscale on the Mac and on the phone or TV, signed in to the same
   account.
2. In https://login.tailscale.com/admin/dns turn on MagicDNS and HTTPS
   Certificates.
3. `deploy/local.sh tailscale on`. It prints the address
   (`https://<mac>.<tailnet>.ts.net`), points download links there, and keeps
   the Mac awake while it serves (closing the lid still sleeps it).
4. On the phone or TV, open that address, make a Stremio link, and paste it
   into Stremio's add-on search.

`deploy/local.sh tailscale off` stops it. Running `lan on|off` afterwards
switches links back to the local addresses.

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

## Takedowns

When a rightsholder sends a notice, block the torrent:

```sh
curl -H "Authorization: Bearer $ADMIN_KEY" \
  -d '{"torrent":"<magnet or info hash>","reason":"copyright notice","reference":"<notice id>"}' \
  https://torza.example.com/v1/admin/blocklist
```

It can't be added again, existing links stop working, and its files are
deleted within one ingest check.

## License

[AGPL-3.0](LICENSE). You may run, change and share Torza. If you run a
changed version for other people, you must offer them its source code.
