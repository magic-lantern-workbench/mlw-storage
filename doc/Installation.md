# mlw-storage: Installation

This document describes how to install, build and manage mlw-storage from a
release package (`mlw-storage-<version>.tar.gz`). mlw-storage is two containers
run by Docker Compose:

- **sftpgo**: the SFTPGo file transfer server (users, authentication, storage).
- **proxy** (`mlw-proxy`): the Go gateway that is the only entry point. It serves
  the mlw-storage web UI and reverse-proxies SFTPGo's web console and API
  (port 8380), and relays SFTP (port 2222), and optionally FTP and WebDAV.

## 1. What is in the package

Unpacking creates a directory named `<version>-release`:

| Item | Purpose |
|------|---------|
| `install.sh` | Loads the images and prepares this machine (section 3) |
| `images/` | The gateway image and the pinned SFTPGo image, as `docker save` archives |
| `docker-compose.prod.yml` | **The Compose file to use**: pinned versions, hardened containers, log rotation |
| `docker-compose.yml` | Development file (builds from source, uses the floating `drakkan/sftpgo:v2` tag, needs internet to pull) |
| `docker-compose.keycloak.yml` | Optional overlay: Keycloak token login |
| `docker-compose.media.yml` | Optional overlay: detection of physical drives |
| `.env.example` | Settings template |
| `src/` | Gateway source and Dockerfile, to rebuild the image |
| `mlw-storage_v<version>_Release-Notes.docx` | Release notes |
| `Installation.md` | This document |
| `LICENSE`, `licenses.txt` | Licenses of mlw-storage and the third-party components |
| `manifest.env` | Versions and image names read by `install.sh` |
| `SHA256SUMS` | Checksums of every file in the package |

## 2. Requirements

- A Linux machine with the **same CPU architecture as the images** (`manifest.env`
  shows `IMAGE_ARCH`, normally `amd64`).
- **Docker Engine** with the **Docker Compose v2 plugin** (`docker compose version`
  must work), and permission to use Docker (root, or membership of the `docker`
  group).
- Free host ports **8380** (web UI and API) and **2222** (SFTP). With the optional
  features also 2121 and 50000-50009 (FTP) and 10080 (WebDAV).
- About 1 GB of free disk space for the images, plus room for the files users
  store.
- No internet access is needed to install: the images are in the package.

## 3. Install

```bash
sha256sum -c mlw-storage-<version>.tar.gz.sha256     # next to the downloaded archive
tar xzf mlw-storage-<version>.tar.gz
cd <version>-release
./install.sh --start
```

`install.sh` does the following (and can be re-run safely):

1. checks that Docker and Docker Compose work and that the architecture matches;
2. verifies every file against `SHA256SUMS`;
3. loads the two images with `docker load`;
4. on a first install creates `.env` from `.env.example`: it asks for the SFTPGo
   **admin password**, selects `docker-compose.prod.yml` and pins `SFTPGO_VERSION`
   and `MLW_VERSION` to the versions in the package. An existing `.env` is never
   overwritten;
5. creates the `data/` directory (owned by uid 1000, which SFTPGo runs as);
6. validates the Compose configuration, and with `--start` starts the stack.

Options:

| Option | Meaning |
|--------|---------|
| `--start` | Start the stack when the installation is done |
| `--generate-password` | Create a random admin password, store it in `.env` and show it once |
| `--update-versions` | On an upgrade, set `SFTPGO_VERSION` and `MLW_VERSION` in the existing `.env` to this package's versions |
| `--skip-verify` | Skip the checksum verification |

For an unattended install give the password in the environment:
`SFTPGO_ADMIN_PASSWORD='...' ./install.sh --start`. The password may contain
letters, digits and `. _ @ % + = : , / -`.

If `install.sh` warns that `./data` is not owned by uid 1000 (you are not root),
run `sudo chown 1000:1000 data`.

### Installing by hand

The script only automates these steps:

```bash
sha256sum -c SHA256SUMS
docker load -i images/mlw-storage-proxy-<version>.tar.gz
docker load -i images/sftpgo-<tag>.tar.gz
cp .env.example .env            # then edit it, see section 4
mkdir -p data && sudo chown 1000:1000 data
docker compose up -d --no-build
```

### First login

Open `http://<host>:8380`. You are taken to the mlw-storage sign-in page; sign in
as `admin` (or `SFTPGO_ADMIN_USER`) with the admin password from `.env`. The
**SFTPGo administration** button on the Dashboard opens SFTPGo's own console
(`/web/admin/`, separate sign-in), where users are created. Remote servers use
SFTP: `sftp -P 2222 <user>@<host>`.

The admin account is created only on the first start, while the SFTPGo database
volume is empty. Changing `SFTPGO_ADMIN_PASSWORD` in `.env` afterwards has no
effect; change the password in the SFTPGo console.

## 4. Configure

Settings are in `.env` in the package directory, next to the Compose files. The
installer sets the first four; the rest are optional.

| Variable | Meaning |
|----------|---------|
| `SFTPGO_ADMIN_PASSWORD` | Password of the first SFTPGo admin (required) |
| `COMPOSE_FILE` | Compose files to use, separated by `:`. The installer sets `docker-compose.prod.yml` |
| `SFTPGO_VERSION` | Pinned SFTPGo image tag (required by the production file) |
| `MLW_VERSION` | Tag of the gateway image, `mlw-storage-proxy:<MLW_VERSION>` |
| `SFTPGO_ADMIN_USER` | Admin user name (default `admin`) |
| `MLW_BIND_ADDR` | Host address the gateway's ports bind to (default all interfaces) |
| `FTP_PORT`, `FTP_PASSIVE_IP` | Set `FTP_PORT=2121` and the address clients use to enable FTP (off by default) |
| `WEBDAV_PORT` | Set `10080` to enable WebDAV (off by default) |

**Optional features** are switched on by adding an overlay file to `COMPOSE_FILE`
(all entries separated by colons), then running `docker compose up -d --no-build`:

```bash
# Keycloak token login over SFTP; needs the Keycloak stack (network mlw-platform_default).
# Also set OIDC_ISSUER and friends, see .env.example.
COMPOSE_FILE=docker-compose.prod.yml:docker-compose.keycloak.yml

# Detection of USB and other physical drives, offered on the Drives page.
# Set MLW_MEDIA_DIR if your desktop mounts drives outside /media (e.g. /run/media).
COMPOSE_FILE=docker-compose.prod.yml:docker-compose.media.yml
```

Drive detection bind-mounts the host's media directory read-only into the gateway
container (needs the host's `/` to have shared mount propagation, the systemd
default: `findmnt -no PROPAGATION /` prints `shared`). It also bind-mounts the
directory read-write into the SFTPGo container at `/srv/drives`: when you enable
a drive on the Drives page you choose the users (and Keycloak roles) who get it,
and the gateway publishes it to them in SFTPGo as `/drives/<name>`, read-write
unless you tick *Read-only*. SFTPGo runs as uid 1000, so the drive must be
writable by that user (for FAT/exFAT, mount with `uid=1000,gid=1000`). Unplugging
a drive does not revoke it; use **Disable** first.

**TLS.** The gateway serves plain HTTP, so the admin password and session cookie
are not encrypted. Put a TLS-terminating reverse proxy in front of port 8380 for
anything beyond a trusted network, and bind the gateway to an internal address
with `MLW_BIND_ADDR`. Plain FTP is unencrypted too.

## 5. Build

The package carries ready-made images, so no build is needed. To rebuild the
gateway image from the included `src/` (for example after changing the source):

```bash
docker compose build proxy              # builds mlw-storage-proxy:<MLW_VERSION>
docker compose up -d                    # without --no-build, Compose rebuilds only if the image is missing
```

Building needs internet access the first time, to pull the base images
(`golang:1.22-alpine`, `gcr.io/distroless/static-debian12`). The development file
`docker-compose.yml` always builds from source (`docker compose -f docker-compose.yml up -d --build`).

To produce a release package from a source checkout, run `scripts/package.sh <version>`
there. It writes `releases/<version>-release/` and `releases/mlw-storage-<version>.tar.gz`.

## 6. Manage

Run these in the package directory (they read `COMPOSE_FILE` from `.env`):

| Task | Command |
|------|---------|
| Start | `docker compose up -d --no-build` |
| Status | `docker compose ps` |
| Logs | `docker compose logs -f proxy` (or `sftpgo`) |
| Restart | `docker compose restart proxy` |
| Stop (keeps data) | `docker compose stop` |
| Stop and remove the containers and network (keeps volumes and `data/`) | `docker compose down` |
| Gateway health | `curl http://localhost:8380/healthz` prints `ok` |
| Validate the configuration | `docker compose config -q` |

After changing `.env`, run `docker compose up -d --no-build` so the containers are
recreated with the new settings.

### Where the data lives

| What | Where |
|------|-------|
| Users' files | `./data` on the host |
| SFTPGo database and SSH host keys | Docker volume `<directory>_sftpgo-config` |
| Gateway state (Keycloak directory choices, drive decisions) | Docker volume `<directory>_proxy-state` |

`<directory>` is the Compose project name, the name of the package directory.
**Do not** use `docker compose down -v` unless you intend to delete users and
settings; removing `sftpgo-config` also regenerates the SSH host keys, so clients
will report a changed host key.

### Upgrade

1. Unpack the new package next to the old one and copy the old `.env` into it
   (and move or copy `data/` if you keep it inside the package directory).
2. In the new directory run `./install.sh --update-versions --start`. It loads the
   new images, keeps your `.env` apart from the two version lines, and recreates
   the containers. The Compose project name is the directory name, so to keep
   using the existing volumes either keep the same directory name or set
   `COMPOSE_PROJECT_NAME=<old directory name>` in `.env`.
3. Check `docker compose ps` and the web UI.

To roll back, start the old package directory again (`docker compose up -d --no-build`).
Old images stay in Docker until you remove them (`docker image ls`).

### Back up and restore

Stop the stack for a consistent copy, then archive the files and both volumes:

```bash
docker compose stop
tar czf mlw-data.tar.gz data
for v in sftpgo-config proxy-state; do
  docker run --rm -v "$(basename "$PWD")_$v:/v" -v "$PWD:/b" alpine tar czf "/b/$v.tar.gz" -C /v .
done
docker compose start
```

Restore by creating the volumes (`docker volume create`), unpacking the archives
into them with the same `docker run ... tar xzf` pattern, and extracting `data/`.
Keep `.env` in the backup.

### Uninstall

```bash
docker compose down -v                # removes containers, network and the two volumes
docker rmi mlw-storage-proxy:<version> drakkan/sftpgo:<tag>
rm -rf ../<version>-release           # includes data/ and .env: back up first
```

## 7. Troubleshooting

| Symptom | Likely cause and fix |
|---------|----------------------|
| `required variable SFTPGO_VERSION is missing a value` | `.env` is missing, or lacks `SFTPGO_VERSION`; run `./install.sh --update-versions` |
| `required variable SFTPGO_ADMIN_PASSWORD` | Set it in `.env` |
| `Cannot connect to the Docker daemon` / `permission denied` | Start Docker; use `sudo` or join the `docker` group |
| `port is already allocated` | Another service uses 8380, 2222 or an optional port; stop it. Fixed container names (`sftpgo`, `mlw-proxy`) also clash with another copy of the stack |
| Uploads fail, SFTPGo cannot create home directories | `data/` is not owned by uid 1000: `sudo chown 1000:1000 data` |
| `image ... not found` or Compose tries to build | The images were not loaded; run `./install.sh` again. Use `up -d --no-build` |
| `exec format error` | The images are for another CPU architecture (`IMAGE_ARCH` in `manifest.env`) |
| Admin login fails on a fresh install | The admin is created only while the database volume is empty; check the password in `.env`, or `docker compose down -v` to start over (this deletes users) |
| `network mlw-platform_default declared as external, but could not be found` | The Keycloak overlay needs the Keycloak stack running; remove the overlay from `COMPOSE_FILE` or start Keycloak |
| Drives page stays empty | Check `docker compose logs proxy` for `media: watching ...`; `findmnt -no PROPAGATION /` must print `shared`; set `MLW_MEDIA_DIR` if drives mount under `/run/media` |

## 8. Licenses

mlw-storage is released under the MIT License (`LICENSE`). The package also
contains the unmodified **SFTPGo** image, which is licensed under the
**AGPL-3.0**; its source is available at https://github.com/drakkan/sftpgo. See
`licenses.txt` for all components.
