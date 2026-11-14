# Installing Torza

Torza runs on one machine with Docker: a Linux server, a home server or a
Mac. The installer sets up everything (settings with fresh secrets,
qBittorrent with its password already set, the services, and your admin
account) in one folder.

## Before you start

You need:

- **Docker** with Compose v2 (`docker compose version` works).
- **Disk:** the cache lives in `torza/data`. Plan for the largest things
  your users add, plus 20 GB free (`CACHE_MIN_FREE_BYTES`).
- **A way for people to reach it**, one of:
  - **Your own domain** pointing at a server with ports 80 and 443 open.
    Caddy gets the HTTPS certificate.
  - **Tailscale**, for a private Torza only your devices (and people you
    share the machine with) can reach. Turn on MagicDNS and HTTPS
    Certificates at https://login.tailscale.com/admin/dns first. No server
    or open ports needed; a Mac or home server works.
  - **This machine only**, for trying it out.

Stremio on phones and TVs needs HTTPS, so pick the domain or Tailscale if
you want those.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/FurkanCodes/torza/main/install/torza-setup.sh -o torza-setup.sh
bash torza-setup.sh
```

It asks how people reach Torza and your email, then prints the web address
and your admin API key. The key is also saved in `torza/admin-api-key`.

Read a script before running it. This one only writes inside `./torza` (and,
on Linux, gives `data/` to user 1000, which Torza and qBittorrent run as).

## After installing

- **Open the web address**, connect with your API key, add a magnet link.
- **Stremio:** in "Watch in Stremio", make a link for each device and paste
  it into Stremio's add-on search.
- **Add people:**
  `cd torza && docker compose run --rm api api create-user -email friend@example.com`
  prints their API key once. Limits per user:
  `PUT /v1/admin/users/{id}/limits` (see the README's API table).
- **Update:** run the installer again from the same place. Settings and data
  are kept. A specific version: `TORZA_VERSION=1.2.3 bash torza-setup.sh`.
- **Logs:** `cd torza && docker compose logs -f`
- **Stop:** `cd torza && docker compose down` (data is kept).
- **Back up:** `torza/.env` (secrets), and the database:
  `docker compose exec -T postgres pg_dump -U torza torza > backup.sql`.
  The cache itself can be downloaded again.

## Settings

Everything is in `torza/.env`. After changing it, run `docker compose up -d`.

| Setting | Meaning |
|---|---|
| `MAX_ACTIVE_PER_USER` | Downloads one user can have running at once |
| `MAX_TORRENT_BYTES` | Largest item a user may download (owner can raise it per user) |
| `CACHE_MAX_BYTES` | Evict least recently used items above this size; `0` = only the free-space rule |
| `CACHE_MIN_FREE_BYTES` | Evict when free disk space falls below this |
| `EVICT_IDLE_FOR` | Never evict anything used more recently than this |
| `LINK_TTL` | How long a download link works |

## You are the operator

Whoever runs a Torza is responsible for it. Content your users add is
downloaded over this machine's connection and served by you. Many hosting
providers don't allow torrent traffic on their servers; check yours.
When you receive a takedown request, block the content:

```sh
curl -H "Authorization: Bearer $ADMIN_KEY" \
  -d '{"torrent": "<magnet or info hash>", "reason": "takedown notice", "reference": "<notice id>"}' \
  https://your-torza/v1/admin/blocklist
```

It is removed from the cache and from every library, and can't be added
again.

## Uninstall

`cd torza && docker compose down -v`, then delete the `torza` folder. This
deletes the database and the cache.
