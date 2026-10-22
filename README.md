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

3. Open http://localhost:8380 (via the proxy gateway). It shows the mlw-storage
   sign-in page (see [Web UI](#web-ui)); sign in as `admin` (or the value of
   `SFTPGO_ADMIN_USER`) with your password. The SFTPGo web admin is at
   http://localhost:8380/web/admin/ and has its own sign-in.

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
| `PROXY_UI`          | `true`                | `false` disables the mlw-storage web UI |

### Web UI

The gateway serves a small mlw-storage web UI on the HTTP port, and `/`
redirects to it:

| Page | URL | Purpose |
|------|-----|---------|
| Sign in | `/mlw/login` | mlw-storage login; use the SFTPGo administrator credentials |
| Dashboard | `/mlw/dashboard` | the home page after sign-in: a table of the SFTPGo users (Username, Status, Last login, Description), 20 users per table with left and right arrows and a range label (`1..20`, `21..40`, ...) to move between tables, and the **SFTPGo administration** button, which opens SFTPGo's own admin console (`/web/admin/`, separate sign-in). |

The gateway checks the credentials against SFTPGo's REST API and keeps the
resulting API token on the server, behind an HttpOnly, SameSite=Strict session
cookie; the session ends when the SFTPGo token expires (20 minutes by default) or
on sign-out, and is lost when the gateway restarts. After 10 failed sign-ins
from one address within 5 minutes, further attempts are refused for a while.
The gateway serves plain HTTP; put TLS in front of it in production so the
password and cookie are encrypted. Set `PROXY_UI=false` on the `proxy` service
to turn the UI off, in which case `/` goes to SFTPGo as before.

### Optional: drive detection

While mlw-storage runs it can notice physical drives (USB sticks, external or
extra internal disks) that are mounted on the host, and offer them on a **Drives**
page of the web UI, next to the Dashboard. It is **disabled by default**; enable
it by adding the overlay file to `COMPOSE_FILE` in `.env`, then
`docker compose up -d --build`:

```bash
COMPOSE_FILE=docker-compose.yml:docker-compose.media.yml
# production:  docker-compose.prod.yml:docker-compose.media.yml
# with Keycloak login, add :docker-compose.keycloak.yml
# MLW_MEDIA_DIR=/run/media     # where your desktop mounts drives (default /media)
```

- **Detection:** the host's media directory (`/media`) is bind-mounted read-only
  into the gateway container with `rslave` propagation, so drives the desktop
  mounts there appear in the container's `/proc/self/mountinfo`. The gateway
  sleeps in `epoll_wait` on that file (the kernel signals `POLLPRI|POLLERR` when
  the mount table changes), so it uses no CPU until a drive is mounted or
  unmounted. `/run/udev` and `/sys` (read-only) supply the label, UUID, model and
  bus. The host's `/` must be a shared mount (`findmnt -no PROPAGATION /` prints
  `shared` on systemd systems).
- **Only physical volumes:** loop, RAM, zram and other virtual devices, network
  shares and mounts outside the media directory are ignored. An unlocked
  encrypted volume (device-mapper) counts when it sits on a physical disk.
- **Enabling and publishing:** a new drive appears on the Drives page as *New:
  not enabled* and as a red count on the **Drives** link. **Enable** opens the
  Access page, where you choose the **users** (and, with Keycloak login on, the
  **Keycloak realm roles**) who get the drive, and whether it is **read-only**
  (default: read-write). On save the gateway creates an SFTPGo virtual folder
  `mlw-drive-<name>` for the drive and attaches it to those users at
  `/drives/<name>`; role members get it when they sign in. The drive is then
  *Available*; **Access** changes the choice, **Disable** withdraws it (the folder
  is deleted from SFTPGo; the files on the drive are untouched) and **Ignore**
  hides a new drive. Read-write means the user's own SFTPGo permissions apply
  there, so a read-only user (or the Keycloak read-only role) stays read-only.
  The page updates by itself when a drive is mounted, unmounted or ejected: an
  enabled drive that is removed stays listed as *Not mounted* (use **Remove** to
  forget it) and becomes *Available* again when plugged back in.
- **How SFTPGo reaches it:** the overlay also bind-mounts the media directory
  read-write (`rslave`) into the `sftpgo` container at `/srv/drives`, so SFTPGo
  (uid 1000) must be allowed to write to the drive for read-write access: ext4
  ownership, or `uid=1000,gid=1000` mount options for FAT/exFAT. SFTPGo changes
  use the signed-in administrator's token; the gateway stores no SFTPGo
  credentials, so **unplugging a drive does not revoke it**: SFTPGo just reports
  errors for the missing directory. Users who sign in with a Keycloak token are
  rebuilt by the authentication hook at every login, which adds the drives they
  are entitled to. Do not delete a `mlw-drive-*` folder in the SFTPGo console
  while it is in use: Keycloak logins that reference it fail until the drive is
  re-saved on the Access page.
- **Detection only:** to keep drives out of SFTPGo, remove the `sftpgo` service and
  `MEDIA_SFTPGO_DIR` from `docker-compose.media.yml`; Enable then only records
  the decision.
- **Adding a drive by hand:** for a volume that is mounted below the media
  directory but not detected (a FUSE, exFAT-fuse or network mount, or a drive
  whose sysfs entry is missing), press **Add drive** on the Drives page. The dialog
  asks for a **name** and the **mount point** (absolute host path below the media
  directory, which must be mounted now and must not be a symlink leaving it);
  **device** and **file system** are optional and default to what the kernel
  reports; *Make available now* decides whether it is enabled at once (choose who may use it afterwards with **Access**). A manual
  drive is marked *manual*, can be disabled and re-enabled, stays listed while
  unmounted, and is deleted with **Remove**.
- **Remembered:** decisions are stored by filesystem UUID in the `proxy-state`
  volume (`media.json`). The gateway never writes to a drive; SFTPGo does, for
  users given read-write access.
- **No desktop pop-up:** the gateway runs in a container, so the question is asked
  on the Drives page, not as a desktop dialog.
- Used space is shown when the container may read the mount point; udisks mounts
  are often readable only by the logged-in user, in which case only the device size
  is shown.
- `MLW_MEDIA_INCLUDE_VIRTUAL=true` also reports loop devices, which is useful for
  trying the feature without a physical drive (for example `udisksctl loop-setup`).

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
| `GET /mlw/v1/storage/drives` | list the removable drives the user may use now (enabled for the user or one of the token's roles, and mounted): `{"drives": [{"name", "path": "/drives/<name>", "read_only"}]}`; empty when drive publishing is off |

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

## Production deployment

`docker-compose.prod.yml` is a standalone file for production releases. Use it
instead of `docker-compose.yml`, not together with it. It pins image versions,
hardens the containers (no new privileges, dropped capabilities, read-only
gateway file system), rotates logs and restarts the services `always`.

1. Create `.env` as above and set, in addition to `SFTPGO_ADMIN_PASSWORD`:

   ```bash
   SFTPGO_VERSION=v2.x.y   # required: a pinned SFTPGo tag from Docker Hub
   MLW_VERSION=1.0.0       # optional: tag for the locally built gateway image
   MLW_BIND_ADDR=0.0.0.0   # optional: host address the gateway's ports bind to
   ```

2. Create the data directory and start the stack:

   ```bash
   mkdir -p data
   docker compose -f docker-compose.prod.yml up -d --build
   ```

To add Keycloak login (see above), pass the override file as well, instead of
setting `COMPOSE_FILE` for the development file:

```bash
docker compose -f docker-compose.prod.yml -f docker-compose.keycloak.yml up -d --build
```

Ports, the optional FTP and WebDAV settings and the first-start admin account
behave as in development. Set `MLW_BIND_ADDR` to an internal interface address
to keep the gateway off public networks, and put a TLS-terminating reverse
proxy in front of port 8380 for the web admin, since the gateway itself serves
plain HTTP. To upgrade, change `SFTPGO_VERSION` (and `MLW_VERSION` for a new
gateway build) and run the same `up -d --build` command.
