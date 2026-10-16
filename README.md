# mlw-storage
Storage management for Magic Lantern Workbench

See the [architecture document](doc/mlw-storage_Architecture.docx) for how the
platform fits together, and the [testing document](doc/mlw-storage_Testing.docx)
for how to verify it. Administrators setting up users with Keycloak SSO should
see the [user guide](doc/mlw-storage_User_Guide.docx), which also lists what
the client application (app1) must provide.

## Running SFTPGo

Requires Docker with Compose.

1. Create the data directory (so it is owned by you, not root; SFTPGo runs as
   uid 1000) and your environment file, then set the admin password:

   ```bash
   mkdir -p data
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
always published, but the gateway only listens on them when enabled. While
disabled, Docker accepts a connection and immediately closes it; nothing is
served. Plain FTP is unencrypted; prefer SFTP
unless a client requires FTP.

### Optional: Keycloak login (SSO)

A Keycloak user can log in over SFTP using their Keycloak access token as the
password. This is meant for a web app (app1), possibly on another machine, that
authenticates users with Keycloak and then opens and saves their files through
this gateway. It is **disabled by default**.

How it works:

- app1 obtains an access token for the user from Keycloak (realm `mlw`).
- app1 opens an SFTP/SSH connection to the gateway (port 2222) with the SSH
  username set to the token's `sub` (the Keycloak user id) and the token as the
  password.
- SFTPGo asks the gateway to authenticate the login (an external auth hook on an
  internal-only port). The gateway checks the token's signature against the
  realm's keys, issuer, expiry and that it is an access token, and that the
  username equals `sub`. The user must hold the realm role `mlw-user`.
- SFTPGo creates the user on first login, with the Description field set to the
  token's name and e-mail (`Name <email>`, from the `name` or `given_name` and
  `family_name` claims and `email`), so an administrator can tell whose directory a
  user id is; it is refreshed at each login. The user's SFTP root is either their
  own private directory `data/users/<sub>` (empty at first) or an existing
  directory they chose (see below). Files are saved there, on the local file
  system. If `OIDC_READONLY_ROLE` is set, users with that role can only list and
  download.

#### Choosing which directory a user can open

To open files that already exist, put them under `data/shared/` on the storage
host (create it with `mkdir -p data/shared`; it must be writable by uid 1000 if
users are to save into it). The user then points their root at a directory
inside it, *before* connecting over SFTP, with a small HTTP API on the gateway's
web port. Every call needs `Authorization: Bearer <access token>` for a user
with the required role, and acts on the token's own `sub`, so app1 never needs
to know the user id in advance:

| Call | Purpose |
|------|---------|
| `GET /mlw/v1/storage/dirs?path=projects` | list the subdirectories that can be chosen |
| `PUT /mlw/v1/storage/root` with `{"path": "projects/a"}` | choose the SFTP root |
| `GET /mlw/v1/storage/root` | show the current choice and the resulting root |
| `DELETE /mlw/v1/storage/root` | go back to the private `data/users/<sub>` directory |

```bash
curl -H "Authorization: Bearer $TOKEN" -X PUT \
  -d '{"path": "projects/a"}' http://localhost:8380/mlw/v1/storage/root
```

The path is relative to `data/shared`; absolute paths, `..`, files, missing
directories and symlinks that lead outside `data/shared` are rejected (HTTP
400). The choice is stored by user id (volume `proxy-state`) and applied at
every login, so it survives restarts and applies to the next SFTP connection
(an open session keeps its old root). If the chosen directory is later deleted,
the login is refused rather than silently using another directory; reset it with
`DELETE`.

Access control: any user with the required role can choose **any** directory
under `data/shared`. Put only material there that all such users may see;
per-user grants are not implemented. Private directories under `data/users` can
never be chosen.

Enable it by adding to `.env`, then `docker compose up -d --build`:

```bash
COMPOSE_FILE=docker-compose.yml:docker-compose.keycloak.yml
```

This joins the gateway to the Keycloak stack's network (`mlw-platform_default`,
so that stack must be running) to fetch the realm keys from `keycloak:8080`.
Settings (`OIDC_ISSUER`, `OIDC_REQUIRED_ROLE`, `OIDC_READONLY_ROLE`,
`OIDC_ALLOWED_CLIENTS`) are in `.env.example`. `OIDC_ISSUER` must equal the
tokens' `iss` claim, for example `http://localhost:8180/realms/mlw`.

Notes:

- The storage API is only served while the Keycloak feature is enabled (404
  otherwise). It does not send CORS headers, so call it from app1's server side.
- Tokens are checked at login only. An open SSH session keeps working after the
  token expires. Use short-lived tokens and a fresh token per connection.
- While enabled, **all password logins** (SFTP, FTP, WebDAV, WebClient) must be
  Keycloak tokens. SFTPGo local users can still log in with public keys, and the
  admin UI is unaffected. There is no safe fallback to SFTPGo's own password
  check.
- Tokens are long, so SFTPGo's password hashing is switched to argon2id (bcrypt
  rejects passwords over 72 bytes).
- A client library must send the token as the SSH *password* (for example
  paramiko `auth_password`). The interactive `sftp` command cannot paste a
  token of this size conveniently.
- SFTPGo does not support SSH port forwarding. The "tunnel" is the SFTP
  subsystem over SSH.

The gateway does not add authentication or rate limiting of its own. It is the
only entry point: SFTPGo's own ports are not published on the host.
