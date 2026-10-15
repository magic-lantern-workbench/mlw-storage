# mlw-storage
Storage management for Magic Lantern Workbench

## Running SFTPGo

Requires Docker with Compose.

1. Create your environment file and set the admin password:

   ```bash
   cp .env.example .env
   # edit .env and set SFTPGO_ADMIN_PASSWORD
   ```

2. Start the service:

   ```bash
   docker compose up -d
   ```

3. Open the web admin at http://localhost:8380 (via the proxy gateway) and log in as `admin`
   (or the value of `SFTPGO_ADMIN_USER`) with your password.

SFTPGo's own ports are not published on the host; reach it through the proxy
gateway (see below): web admin on 8380, SFTP on 2222.

Create SFTP users in the web admin. Their files are stored in `./data`
(git-ignored); SFTPGo's database and host keys live in the `sftpgo-config`
Docker volume.

The admin account is only created on first start. After that, change the
password in the web admin; editing `.env` has no effect.

Stop the service with `docker compose down` (add `-v` to also delete the
config volume).

## Proxy gateway

The Go application in `src/` runs as the `proxy` service in
`docker-compose.yml` and sits in front of SFTPGo. It is built automatically by
`docker compose up -d --build`.

| Host port | Purpose                                                  |
|-----------|----------------------------------------------------------|
| 8380      | HTTP reverse proxy to SFTPGo (web admin, WebClient, REST API) |
| 2222      | SFTP proxy: remote servers connect here to transfer files |

Remote servers use any SFTP client against the gateway, for example:

```bash
sftp -P 2222 <user>@<gateway-host>
```

Users are created in the SFTPGo web admin; authentication is still handled by
SFTPGo. The gateway also serves `GET /healthz` on the HTTP port.

Configuration (environment variables on the `proxy` service):

| Variable            | Default               | Meaning                    |
|---------------------|-----------------------|----------------------------|
| `PROXY_HTTP_LISTEN` | `:8000`               | HTTP listen address        |
| `PROXY_SFTP_LISTEN` | `:2222`               | SFTP listen address        |
| `UPSTREAM_HTTP`     | `http://sftpgo:8080`  | SFTPGo web/API endpoint    |
| `UPSTREAM_SFTP`     | `sftpgo:2022`         | SFTPGo SFTP endpoint       |

### Optional: FTP and WebDAV

FTP and WebDAV are **disabled by default**. Enable them in `.env` and run
`docker compose up -d`:

```bash
FTP_PORT=2121                 # enables FTP via the gateway
FTP_PASSIVE_IP=203.0.113.10   # public IP/hostname clients use to reach the gateway
WEBDAV_PORT=10080             # enables WebDAV via the gateway
```

| Host port    | Purpose                              |
|--------------|--------------------------------------|
| 2121         | FTP control (when `FTP_PORT=2121`)   |
| 50000-50009  | FTP passive data (relayed 1:1)       |
| 10080        | WebDAV (when `WEBDAV_PORT=10080`)    |

Use exactly these port values; the host port mappings are fixed. The ports are
always published, but the gateway only listens on them when enabled, so
connections are refused while disabled. Plain FTP is unencrypted; prefer SFTP
unless a client requires FTP.

The gateway does not add authentication or rate limiting of its own. It is the
only entry point: SFTPGo's own ports are not published on the host.
