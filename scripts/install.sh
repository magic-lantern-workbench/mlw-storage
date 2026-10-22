#!/usr/bin/env bash
# COPYRIGHT_BEGIN
# MIT License
#
# Copyright (c) 2026 Wizzer Works
#
# Permission is hereby granted, free of charge, to any person obtaining a copy
# of this software and associated documentation files (the "Software"), to deal
# in the Software without restriction, including without limitation the rights
# to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
# copies of the Software, and to permit persons to whom the Software is
# furnished to do so, subject to the following conditions:
#
# The above copyright notice and this permission notice shall be included in all
# copies or substantial portions of the Software.
#
# THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
# IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
# FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
# AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
# LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
# OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
# SOFTWARE.
# COPYRIGHT_END
#
# Install mlw-storage from an unpacked release package.
#
#   ./install.sh [--start] [--generate-password] [--update-versions] [--skip-verify]
#
# Run it from the unpacked <version>-release directory. It:
#   1. checks Docker and Docker Compose,
#   2. verifies the package checksums (SHA256SUMS),
#   3. loads the gateway and SFTPGo images into Docker,
#   4. creates .env from .env.example on a first install (asking for the SFTPGo
#      admin password) and selects the production Compose file,
#   5. creates the ./data directory,
#   6. with --start, starts the stack.
#
# Options:
#   --start              start the stack when the installation is done
#   --generate-password  create a random admin password instead of asking; it is
#                        stored in .env and shown once
#   --update-versions    on an upgrade, set SFTPGO_VERSION and MLW_VERSION in an
#                        existing .env to the versions in this package
#   --skip-verify        do not verify SHA256SUMS
#   -h, --help           show this text
#
# The admin password can also be given in the SFTPGO_ADMIN_PASSWORD environment
# variable, for unattended installs. Existing .env files are never overwritten
# except for the two version lines with --update-versions.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

die() { echo "install.sh: $*" >&2; exit 1; }
say() { echo "==> $*"; }
warn() { echo "install.sh: warning: $*" >&2; }

usage() {
	sed -n '/^# Install mlw-storage/,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'
}

START=0 GENERATE=0 UPDATE_VERSIONS=0 VERIFY=1
while [ $# -gt 0 ]; do
	case "$1" in
	--start) START=1 ;;
	--generate-password) GENERATE=1 ;;
	--update-versions) UPDATE_VERSIONS=1 ;;
	--skip-verify) VERIFY=0 ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option $1 (try --help)" ;;
	esac
	shift
done

[ -f manifest.env ] || die "manifest.env not found: run this script from the unpacked release directory"
# shellcheck disable=SC1091
. ./manifest.env

# ---- 1. prerequisites --------------------------------------------------------
command -v docker >/dev/null || die "Docker is not installed (see Installation.md)"
docker compose version >/dev/null 2>&1 || die "the Docker Compose plugin (docker compose) is not installed"
docker info >/dev/null 2>&1 || die "cannot talk to the Docker daemon: is it running, and are you in the docker group (or using sudo)?"
case "$(uname -m)" in
x86_64 | amd64) HOST_ARCH=amd64 ;;
aarch64 | arm64) HOST_ARCH=arm64 ;;
*) HOST_ARCH="$(uname -m)" ;;
esac
[ "$HOST_ARCH" = "$IMAGE_ARCH" ] || die "this package holds $IMAGE_ARCH images but this machine is $HOST_ARCH"

# ---- 2. integrity ------------------------------------------------------------
if [ "$VERIFY" = 1 ]; then
	say "Verifying package checksums"
	sha256sum --quiet -c SHA256SUMS || die "checksum verification failed: the package is damaged or was modified"
fi

# ---- 3. images ---------------------------------------------------------------
say "Loading images"
docker load -i "$PROXY_IMAGE_FILE"
docker load -i "$SFTPGO_IMAGE_FILE"

# ---- 4. .env -------------------------------------------------------------------
set_var() { # set_var KEY VALUE: replace or append a line in .env
	local tmp
	tmp="$(mktemp)"
	grep -v "^$1=" .env >"$tmp" || true
	printf '%s=%s\n' "$1" "$2" >>"$tmp"
	cat "$tmp" >.env
	rm -f "$tmp"
}
get_var() { sed -n "s/^$1=//p" .env | tail -n 1; }

NEW_PASSWORD=""
if [ ! -f .env ]; then
	say "Creating .env"
	pw="${SFTPGO_ADMIN_PASSWORD:-}"
	if [ "$GENERATE" = 1 ]; then
		pw="$(head -c 18 /dev/urandom | base64 | tr '+/' 'xy')"
		NEW_PASSWORD="$pw"
	elif [ -z "$pw" ]; then
		[ -t 0 ] || die "set SFTPGO_ADMIN_PASSWORD, or use --generate-password, for a non-interactive install"
		read -r -s -p "SFTPGo admin password (at least 8 characters): " pw
		echo
		read -r -s -p "Repeat the password: " pw2
		echo
		[ "$pw" = "$pw2" ] || die "the passwords differ"
	fi
	[ "${#pw}" -ge 8 ] || die "the admin password must have at least 8 characters"
	[[ "$pw" =~ ^[A-Za-z0-9._@%+=:,/-]+$ ]] ||
		die "the admin password may only contain letters, digits and . _ @ % + = : , / - (it is stored in .env)"
	(umask 077; cp .env.example .env)
	set_var SFTPGO_ADMIN_PASSWORD "$pw"
	set_var COMPOSE_FILE docker-compose.prod.yml
	set_var SFTPGO_VERSION "$SFTPGO_VERSION"
	set_var MLW_VERSION "$MLW_VERSION"
	chmod 600 .env
else
	say "Keeping the existing .env"
	if [ "$UPDATE_VERSIONS" = 1 ]; then
		set_var SFTPGO_VERSION "$SFTPGO_VERSION"
		set_var MLW_VERSION "$MLW_VERSION"
		say "Set SFTPGO_VERSION=$SFTPGO_VERSION and MLW_VERSION=$MLW_VERSION in .env"
	else
		[ "$(get_var SFTPGO_VERSION)" = "$SFTPGO_VERSION" ] ||
			warn ".env has SFTPGO_VERSION=$(get_var SFTPGO_VERSION), this package carries $SFTPGO_VERSION (use --update-versions)"
		[ "$(get_var MLW_VERSION)" = "$MLW_VERSION" ] ||
			warn ".env has MLW_VERSION=$(get_var MLW_VERSION), this package carries $MLW_VERSION (use --update-versions)"
	fi
fi

# ---- 5. data directory -----------------------------------------------------------
# SFTPGo runs as uid 1000 and must be able to write here.
mkdir -p data
if [ "$(id -u)" = 0 ]; then
	chown 1000:1000 data
elif [ "$(id -u)" != 1000 ]; then
	warn "./data is owned by uid $(id -u) but SFTPGo runs as uid 1000: run  sudo chown 1000:1000 $(pwd)/data"
fi

say "Checking the Compose configuration"
docker compose config -q || die "docker compose config failed (see the message above)"

# ---- 6. start ----------------------------------------------------------------------
if [ "$START" = 1 ]; then
	say "Starting the stack"
	docker compose up -d --no-build
	docker compose ps
fi

echo
echo "mlw-storage $MLW_VERSION is installed in $(pwd)"
if [ -n "$NEW_PASSWORD" ]; then
	echo "The generated SFTPGo admin password is:  $NEW_PASSWORD"
	echo "(it is also stored in .env; keep it safe)"
fi
if [ "$START" = 1 ]; then
	echo "Open http://localhost:8380 and sign in as admin."
else
	echo "Start it with:  docker compose up -d --no-build"
	echo "Then open http://localhost:8380 and sign in as admin. See Installation.md."
fi
