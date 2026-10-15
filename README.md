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

3. Open the web admin at http://localhost:8280 and log in as `admin`
   (or the value of `SFTPGO_ADMIN_USER`) with your password.

| Port | Purpose           |
|------|-------------------|
| 8280 | Web admin / WebClient |
| 2022 | SFTP              |

Create SFTP users in the web admin. Their files are stored in `./data`
(git-ignored); SFTPGo's database and host keys live in the `sftpgo-config`
Docker volume.

The admin account is only created on first start. After that, change the
password in the web admin; editing `.env` has no effect.

Stop the service with `docker compose down` (add `-v` to also delete the
config volume).
