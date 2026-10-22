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
# Build a release package of mlw-storage.
#
#   scripts/package.sh <version> [--sftpgo-version <tag>]
#
# Creates releases/<version>-release/ (the staging directory) and from it
# releases/mlw-storage-<version>.tar.gz. The staging directory holds:
#
#   images/             the gateway image built from src/ and the pinned SFTPGo
#                       image, as `docker save` archives
#   docker-compose*.yml the Compose files (the production file is the one to use)
#   .env.example        the settings template
#   src/                the gateway source and Dockerfile, so images can be rebuilt
#   Release Notes       doc/mlw-storage_v<version>_Release-Notes.docx
#   Installation.md     how to install, build and manage the stack
#   install.sh          loads the images and prepares the machine it is run on
#   manifest.env        versions and image names, read by install.sh
#   SHA256SUMS          checksums of every file in the package
#
# Needs Docker (with network access the first time, to pull base images and
# SFTPGo) and GNU tar. Run it from anywhere inside a checkout.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RELEASES="$ROOT/releases"
DEFAULT_SFTPGO_VERSION="v2.7.6"

die() { echo "package.sh: $*" >&2; exit 1; }
say() { echo "==> $*"; }

usage() {
	sed -n '/^# Build a release/,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'
}

VERSION=""
SFTPGO_VERSION="${SFTPGO_VERSION:-$DEFAULT_SFTPGO_VERSION}"
while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help) usage; exit 0 ;;
	--sftpgo-version) [ $# -ge 2 ] || die "--sftpgo-version needs a value"; SFTPGO_VERSION="$2"; shift 2 ;;
	-*) die "unknown option $1 (try --help)" ;;
	*) [ -z "$VERSION" ] || die "more than one version given"; VERSION="$1"; shift ;;
	esac
done
[ -n "$VERSION" ] || { usage >&2; exit 1; }
VERSION="${VERSION#v}"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$ ]] || die "version '$VERSION' is not like 1.2.3"
[[ "$SFTPGO_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "SFTPGo version '$SFTPGO_VERSION' must be a pinned tag like v2.7.6"

command -v docker >/dev/null || die "docker is not installed"
docker info >/dev/null 2>&1 || die "cannot talk to the Docker daemon (is it running, and are you allowed to use it?)"
tar --version 2>/dev/null | grep -q 'GNU tar' || die "GNU tar is required"

NOTES="$ROOT/doc/mlw-storage_v${VERSION}_Release-Notes.docx"
for f in "$NOTES" "$ROOT/doc/Installation.md" "$ROOT/scripts/install.sh" "$ROOT/.env.example" "$ROOT/LICENSE" \
	"$ROOT/doc/licenses.txt" "$ROOT/docker-compose.yml" "$ROOT/docker-compose.prod.yml" \
	"$ROOT/docker-compose.keycloak.yml" "$ROOT/docker-compose.media.yml" "$ROOT/src/Dockerfile"; do
	[ -f "$f" ] || die "missing ${f#"$ROOT"/}$([ "$f" = "$NOTES" ] && echo " (write the release notes for $VERSION first)")"
done

PROXY_IMAGE="mlw-storage-proxy:${VERSION}"
SFTPGO_IMAGE="drakkan/sftpgo:${SFTPGO_VERSION}"
STAGE="$RELEASES/${VERSION}-release"
TARBALL="$RELEASES/mlw-storage-${VERSION}.tar.gz"
case "$STAGE" in "$ROOT"/releases/?*-release) ;; *) die "refusing to use staging directory $STAGE" ;; esac

COMMIT="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
DIRTY=0
if [ -n "$(git -C "$ROOT" status --porcelain --untracked-files=no 2>/dev/null)" ]; then
	DIRTY=1
	echo "package.sh: warning: the working tree has uncommitted changes; they are included in the package" >&2
fi

say "Staging directory $STAGE"
rm -rf -- "$STAGE" "$TARBALL" "$TARBALL.sha256"
mkdir -p "$STAGE/images"

say "Building $PROXY_IMAGE"
docker build -q -t "$PROXY_IMAGE" \
	--label "org.opencontainers.image.title=mlw-storage-proxy" \
	--label "org.opencontainers.image.version=$VERSION" \
	--label "org.opencontainers.image.revision=$COMMIT" \
	"$ROOT/src" >/dev/null

if ! docker image inspect "$SFTPGO_IMAGE" >/dev/null 2>&1; then
	say "Pulling $SFTPGO_IMAGE"
	docker pull -q "$SFTPGO_IMAGE" >/dev/null
fi
ARCH="$(docker image inspect --format '{{.Architecture}}' "$PROXY_IMAGE")"
[ "$ARCH" = "$(docker image inspect --format '{{.Architecture}}' "$SFTPGO_IMAGE")" ] ||
	die "the two images have different architectures"

PROXY_FILE="images/mlw-storage-proxy-${VERSION}.tar.gz"
SFTPGO_FILE="images/sftpgo-${SFTPGO_VERSION}.tar.gz"
say "Saving images ($ARCH)"
docker save "$PROXY_IMAGE" | gzip -9 >"$STAGE/$PROXY_FILE"
docker save "$SFTPGO_IMAGE" | gzip -9 >"$STAGE/$SFTPGO_FILE"

say "Copying files"
cp "$ROOT"/docker-compose.yml "$ROOT"/docker-compose.prod.yml "$ROOT"/docker-compose.keycloak.yml \
	"$ROOT"/docker-compose.media.yml "$ROOT"/.env.example "$ROOT"/LICENSE "$STAGE/"
cp "$ROOT/doc/licenses.txt" "$STAGE/licenses.txt"
cp "$ROOT/doc/Installation.md" "$STAGE/Installation.md"
cp "$NOTES" "$STAGE/"
install -m 755 "$ROOT/scripts/install.sh" "$STAGE/install.sh"
mkdir -p "$STAGE/src"
cp "$ROOT"/src/Dockerfile "$ROOT"/src/go.mod "$ROOT"/src/go.sum "$ROOT"/src/*.go "$STAGE/src/"

cat >"$STAGE/manifest.env" <<MANIFEST
MLW_VERSION=$VERSION
SFTPGO_VERSION=$SFTPGO_VERSION
PROXY_IMAGE=$PROXY_IMAGE
SFTPGO_IMAGE=$SFTPGO_IMAGE
PROXY_IMAGE_FILE=$PROXY_FILE
SFTPGO_IMAGE_FILE=$SFTPGO_FILE
IMAGE_ARCH=$ARCH
GIT_COMMIT=$COMMIT
GIT_DIRTY=$DIRTY
BUILD_DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
MANIFEST

say "Writing checksums"
(cd "$STAGE" && find . -type f ! -name SHA256SUMS -printf '%P\n' | LC_ALL=C sort | xargs -d '\n' sha256sum >SHA256SUMS)

say "Creating $TARBALL"
tar --sort=name --owner=0 --group=0 --numeric-owner -C "$RELEASES" -czf "$TARBALL" "${VERSION}-release"
(cd "$RELEASES" && sha256sum "mlw-storage-${VERSION}.tar.gz" >"mlw-storage-${VERSION}.tar.gz.sha256")

say "Done"
du -h "$TARBALL" | awk '{print "  " $2 "  (" $1 ")"}'
echo "  ${TARBALL}.sha256"
echo "  staging directory kept at $STAGE"
