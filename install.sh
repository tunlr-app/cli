#!/bin/sh
# Installs tunlr (https://github.com/tunlr-app/cli) on a Linux server with systemd, then runs tunlr init.
#
#   curl -fsSL https://cdn.jsdelivr.net/gh/tunlr-app/cli@main/install.sh | sh
#   curl -fsSL https://cdn.jsdelivr.net/gh/tunlr-app/cli@main/install.sh | sh -s -- --sub home=https://...
#
# Environment:
#   TUNLR_VERSION   release to install, e.g. v0.2.0 (default: the latest release)
#   TUNLR_MIRROR    GitHub download mirror prefix (default: https://gh-proxy.com/; set it empty to download
#                   from github.com directly). It only carries the bytes: the sha256 always comes from
#                   api.github.com and the download is rejected when it does not match. The same mirror
#                   is passed to tunlr init (--mirror) unless the arguments give their own.
#   TUNLR_INIT=no   install the binary only; do not run tunlr init.
#
# Arguments are passed to tunlr init (see: tunlr init --help). Without a controlling terminal init
# runs with --yes and asks nothing.
set -eu

REPO=tunlr-app/cli
BIN=/usr/local/bin/tunlr
API="https://api.github.com/repos/$REPO/releases"
MIRROR=${TUNLR_MIRROR-https://gh-proxy.com/}

say() { printf '%s\n' "$*"; }
fail() { say "install.sh: $*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || fail "tunlr only runs on Linux"
[ "$(id -u)" -eq 0 ] || fail "run it as root: curl -fsSL ... | sudo sh"
command -v systemctl >/dev/null 2>&1 || fail "tunlr needs systemd"
for tool in curl tar sha256sum; do
	command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done
case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) fail "unsupported architecture $(uname -m): only amd64 and arm64 are supported" ;;
esac
case "$MIRROR" in
'' | */) ;;
*) fail "TUNLR_MIRROR must end with a slash, e.g. https://gh-proxy.com/" ;;
esac

# Which release.
if [ -n "${TUNLR_VERSION:-}" ]; then
	case "$TUNLR_VERSION" in
	v*) TAG=$TUNLR_VERSION ;;
	*) TAG=v$TUNLR_VERSION ;;
	esac
	RELEASE="$API/tags/$TAG"
else
	RELEASE="$API/latest"
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM

say "Looking up the tunlr release ($RELEASE)"
JSON=$(curl -fsSL --retry 3 -H 'Accept: application/vnd.github+json' "$RELEASE" | tr -d '\n ') ||
	fail "cannot query the release; check the network (or TUNLR_VERSION)"
TAG=$(printf '%s' "$JSON" | sed -n 's/.*"tag_name":"\([^"]*\)".*/\1/p')
[ -n "$TAG" ] || fail "no tag_name in the release data"
VERSION=${TAG#v}
ASSET="tunlr_${VERSION}_linux_${ARCH}.tar.gz"
# The sha256 of the asset, from the asset's own entry (cut at its browser_download_url).
REST=${JSON#*\"name\":\"$ASSET\"}
[ "$REST" != "$JSON" ] || fail "release $TAG has no asset $ASSET"
REST=${REST%%browser_download_url*}
SHA=$(printf '%s' "$REST" | sed -n 's/.*"digest":"sha256:\([0-9a-f]\{64\}\)".*/\1/p')
[ -n "$SHA" ] || fail "GitHub provides no sha256 digest for $ASSET; refusing to download unverified"

URL="https://github.com/$REPO/releases/download/$TAG/$ASSET"
say "Downloading $MIRROR$URL"
# A progress bar only when stderr is a terminal; piped output would be flooded with it.
if [ -t 2 ]; then PROGRESS=--progress-bar; else PROGRESS=-sS; fi
curl -fL --retry 3 $PROGRESS -o "$TMP/$ASSET" "$MIRROR$URL" || fail "download failed"
GOT=$(sha256sum "$TMP/$ASSET" | cut -d' ' -f1)
[ "$GOT" = "$SHA" ] || fail "sha256 mismatch for $ASSET: got $GOT, GitHub says $SHA (a mirror may have served a wrong file)"
tar -xzf "$TMP/$ASSET" -C "$TMP" tunlr
"$TMP/tunlr" --version >/dev/null || fail "the downloaded tunlr does not run"

# Atomic replace: write next to the target, then rename over it.
mkdir -p "$(dirname "$BIN")"
install -m 0755 "$TMP/tunlr" "$BIN.new"
mv -f "$BIN.new" "$BIN"
say "Installed $("$BIN" --version) at $BIN"

if [ "${TUNLR_INIT:-}" = no ]; then
	say "Next: $BIN init"
	exit 0
fi

# tunlr init: the mirror used here is its default unless the arguments set one.
case " $* " in
*" --mirror "* | *" --mirror="*) ;;
*) set -- --mirror "$MIRROR" "$@" ;;
esac
# /dev/tty exists on every Linux but only opens with a controlling terminal (not under ssh without a
# tty, cloud-init or cron), so test by opening it rather than with [ -r ].
if ( : </dev/tty ) 2>/dev/null; then
	exec "$BIN" init "$@" </dev/tty
fi
exec "$BIN" init --yes "$@"
