# Deployment

HiwiKInsight is a single process with a single SQLite database. It needs:

- A public HTTPS URL, because the SDK and Apple's App Store Server Notifications must reach it.
- A persistent directory for the database (`hiwikinsight.db`, plus its `-wal` and `-shm` files) and the session secret (`session.key`).
- Outbound HTTPS to `api.appstoreconnect.apple.com` (if App Store Connect sync is configured), `open.feishu.cn` or `open.larksuite.com` (if `feishu` is configured), `open.er-api.com` (exchange rates) and your SMTP server.

Two supported setups follow: Docker Compose with Caddy, and a binary with systemd behind nginx.

## Option A: Docker Compose with Caddy

The image is published to GHCR for `linux/amd64` and `linux/arm64`:

```
ghcr.io/redhiwik/hiwikinsight:latest
ghcr.io/redhiwik/hiwikinsight:<version>     # e.g. v1.0.0
```

It is a distroless, non-root image containing `/usr/local/bin/hiwikinsight` and `/usr/local/bin/insight`, with `HIWIKINSIGHT_CONFIG=/etc/hiwikinsight/config.yaml` and `HIWIKINSIGHT_DATA_DIR=/data`.

[docker-compose.yml](../docker-compose.yml) runs two services:

- `hiwikinsight`: mounts `./config` read-only at `/etc/hiwikinsight` and the named volume `data` at `/data`; reads secrets from `.env`.
- `caddy`: listens on 80 and 443, obtains a certificate for `$DOMAIN`, and proxies to `hiwikinsight:8080` ([deploy/Caddyfile](../deploy/Caddyfile)).

Layout on the host:

```
HiwiKInsight/
  .env                      # DOMAIN and secrets (from deploy/.env.example)
  config/
    config.yaml             # from examples/config.example.yaml
    catalogs/pawprint.yaml  # referenced as catalogs/pawprint.yaml
    AuthKey_ABC123.p8       # App Store Connect key, referenced as key_path: AuthKey_ABC123.p8
  docker-compose.yml
  deploy/Caddyfile
```

Steps:

```bash
mkdir -p config && cp -r examples/catalogs config/
cp examples/config.example.yaml config/config.yaml
cp deploy/.env.example .env
openssl rand -hex 32                                                    # query token for .env
docker run --rm -it ghcr.io/redhiwik/hiwikinsight:latest hash-password  # dashboard password hash
docker compose run --rm hiwikinsight check-config
docker compose up -d
```

Notes:

- Leave `data_dir` unset in the container. A relative `data_dir` would resolve inside the read-only `/etc/hiwikinsight` mount.
- Bcrypt hashes contain `$`. In `.env`, single-quote them (`HIWIKINSIGHT_ADMIN_HASH='$2a$10$...'`) so Compose does not interpolate them. In `config.yaml` itself a bare hash is fine.
- The container runs as the distroless `nonroot` user (uid 65532). Files in `./config` (config, catalogs, `.p8` key) must be readable by that user: `sudo chown -R 65532 config` (then `chmod 600` the `.p8` key), or make non-secret files world-readable.
- To build from source instead of pulling, replace `image:` with `build: .` in `docker-compose.yml`.
- Logs are JSON on stdout: `docker compose logs -f hiwikinsight`.
- The CLI is in the image: `docker compose exec hiwikinsight insight --token <token> apps`.

## Option B: Binary with systemd and nginx

Release archives (`hiwikinsight_<version>_<os>_<arch>.tar.gz` for linux and darwin, amd64 and arm64) are on [GitHub Releases](https://github.com/RedHiwiK/HiwiKInsight/releases), with `checksums.txt`. Each contains `hiwikinsight`, `insight`, `LICENSE`, `README.md` and `config.example.yaml`. To build yourself: `make web build` (see [../CONTRIBUTING.md](../CONTRIBUTING.md)).

Install (from the comments in [deploy/systemd/hiwikinsight.service](../deploy/systemd/hiwikinsight.service)):

```bash
sudo useradd --system --home /var/lib/hiwikinsight --shell /usr/sbin/nologin hiwikinsight
sudo install -m 755 hiwikinsight insight /usr/local/bin/
sudo install -d -o hiwikinsight -m 750 /var/lib/hiwikinsight /etc/hiwikinsight
sudo cp config.yaml /etc/hiwikinsight/
sudo cp deploy/systemd/hiwikinsight.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now hiwikinsight
journalctl -u hiwikinsight -f
```

In `/etc/hiwikinsight/config.yaml` set:

```yaml
listen: 127.0.0.1:8080
data_dir: /var/lib/hiwikinsight
```

The unit runs with `ProtectSystem=strict` and only `/var/lib/hiwikinsight` writable, so `data_dir` must be there. Secrets referenced as `${NAME}` go in `/etc/hiwikinsight/env` (one `NAME=value` per line, `chmod 600`), which the unit loads as an optional `EnvironmentFile`. Make sure the `hiwikinsight` user can read the config, catalogs and `.p8` key.

Reverse proxy: [deploy/nginx/hiwikinsight.conf](../deploy/nginx/hiwikinsight.conf) terminates TLS (certificates from certbot or similar), redirects HTTP to HTTPS, and sets `client_max_body_size 2m`. Caddy works too:

```
insight.example.com {
	encode gzip
	reverse_proxy 127.0.0.1:8080
}
```

### Reverse proxy headers

| Header | Used for |
|---|---|
| `X-Forwarded-Proto: https` | Marks the session cookie `Secure` when TLS ends at the proxy |
| `X-Real-IP` / `X-Forwarded-For` | Throttling failed dashboard sign-ins per client IP (10 failures per 15 minutes). Trusted only when the direct peer is a loopback or private address, i.e. a proxy on the same host or Docker network |

Caddy sets `X-Forwarded-For` and `X-Forwarded-Proto` by default; the nginx example sets all of them. Ingestion never reads these headers. If you want no IP addresses on the server at all, disable the proxy's access log for `/v1/events` (see [privacy.md](privacy.md)).

Body size: SDK batches are at most 256 KB compressed and App Store notifications at most 256 KB, so a 2 MB proxy limit is ample.

## Health check

`GET /healthz` returns `200 ok`. It does not require authentication.

## Backups

All state is in `data_dir`:

- `hiwikinsight.db` (and, while the server runs, `hiwikinsight.db-wal` and `hiwikinsight.db-shm`)
- `session.key` (optional to back up; losing it only signs everyone out)

The database runs in WAL mode, so the main file alone is not a consistent copy while the server is running. Use one of:

1. **Online backup with the sqlite3 CLI** (safe while the server runs):

   ```bash
   sqlite3 /var/lib/hiwikinsight/hiwikinsight.db ".backup '/backups/hiwikinsight-$(date +%F).db'"
   ```

   With Docker, the image has no shell or sqlite3; back up the volume from the host or a helper container:

   ```bash
   docker run --rm -v hiwikinsight_data:/data -v "$PWD/backups:/backups" alpine \
     sh -c "apk add --no-cache sqlite >/dev/null && sqlite3 /data/hiwikinsight.db \".backup '/backups/hiwikinsight-$(date +%F).db'\""
   ```

   (The volume name is `<compose project>_data`; check with `docker volume ls`.)

2. **Copy while stopped**: stop the server (which checkpoints and closes the database), then copy the whole `data_dir`, including any `-wal` file.

Restore by stopping the server, replacing `hiwikinsight.db` (and removing stale `-wal`/`-shm` files), and starting it again.

The `sqlite3` CLI can open and back up the database, but queries on views that use HiwiKInsight's SQL functions (for example `v_user_summary`, which uses `local_date()`) fail there; use `insight sql` for queries.

## Upgrades and migrations

1. Back up the database.
2. Docker: `docker compose pull && docker compose up -d`. Binary: replace `/usr/local/bin/hiwikinsight` and `insight`, then `sudo systemctl restart hiwikinsight`.
3. Schema migrations run automatically at startup, in order, each in a transaction, tracked with `PRAGMA user_version`. They are append-only, so upgrading from any earlier version is supported. Downgrading to a binary that does not know the newer migrations is not supported; restore the backup instead.
4. Read [CHANGELOG.md](../CHANGELOG.md) for config changes. Unknown config keys are rejected, so run `check-config` with the new binary before restarting.

On shutdown (SIGINT/SIGTERM) the server stops accepting requests and flushes queued events before exiting.

## Security checklist

- [ ] HTTPS only; HTTP redirects to HTTPS (the Caddy and nginx examples do this).
- [ ] At least one dashboard user with a strong password (`hiwikinsight hash-password`); `insecure_no_auth` off.
- [ ] Query tokens generated with `openssl rand -hex 32`, one per client, stored in the environment, not in the config file.
- [ ] Firewall: only 80/443 open to the internet; the app port (8080) bound to `127.0.0.1` or reachable only by the proxy.
- [ ] `.env`, `/etc/hiwikinsight/env` and the `.p8` key readable only by the service user (`chmod 600`).
- [ ] App Store Connect API key with the least access that works (see [app-store-setup.md](app-store-setup.md)).
- [ ] Regular, tested backups of `data_dir`.
- [ ] Optional: disable proxy access logs for `/v1/events` if you do not want client IPs in logs.

What is exposed without authentication: `POST /v1/events` (accepts only configured app keys), `POST /v1/appstore/notifications` (accepts only payloads signed by Apple for configured bundle IDs), `/healthz`, the sign-in endpoints and the dashboard's static files (all data behind them requires a session or token).
