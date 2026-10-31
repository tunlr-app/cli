#!/bin/sh
# Installs tunlr (https://github.com/tunlr-app/cli) on a Linux server with systemd, then runs tunlr init.
#
#   curl -fsSL https://cdn.jsdelivr.net/gh/tunlr-app/cli@main/install.sh | sh
#   curl -fsSL https://cdn.jsdelivr.net/gh/tunlr-app/cli@main/install.sh | sh -s -- --sub home=https://...
#
# Environment:
#   TUNLR_VERSION   release to install, e.g. v0.2.0 (default: the latest release)
#   TUNLR_MIRROR    GitHub download mirror prefix (default: https://axisnow.gh-proxy.org/; set it empty to download
#                   from github.com directly). It only carries the bytes: the sha256 always comes from
#                   api.github.com and the download is rejected when it does not match. With a
#                   terminal tunlr init asks which mirror to keep (this one is the default); without one,
#                   or when TUNLR_MIRROR is set, it is passed to init as --mirror unless the arguments
#                   give their own.
#   TUNLR_INIT=no   install the binary only; do not run tunlr init.
#
# Arguments are passed to tunlr init (see: tunlr init --help). Without a controlling terminal init
# runs with --yes and asks nothing.
set -eu

REPO=tunlr-app/cli
BIN=/usr/local/bin/tunlr
API="https://api.github.com/repos/$REPO/releases"
MIRROR=${TUNLR_MIRROR-https://axisnow.gh-proxy.org/}

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
*) fail "TUNLR_MIRROR must end with a slash, e.g. https://axisnow.gh-proxy.org/" ;;
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
# The API is never queried through the mirror: GitHub counts the mirror's requests against its own
# IP, shared by everyone, so that quota is as good as always spent.
JSON=$(curl -fsSL --retry 3 -H 'Accept: application/vnd.github+json' "$RELEASE" | tr -d '\n ') ||
	fail "cannot query the release; check the network (or TUNLR_VERSION; unauthenticated API calls are limited to 60 per hour per IP)"
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
# Resume from what is there after a cut connection (-C -); curl exits 33 when the server ignores
# ranges, then start over. Five rounds, each with curl's own retries.
n=0
while :; do
	curl -fL -C - --retry 3 --retry-all-errors $PROGRESS -o "$TMP/$ASSET" "$MIRROR$URL" && break
	rc=$?
	[ "$rc" -eq 33 ] && rm -f "$TMP/$ASSET"
	n=$((n + 1))
	[ "$n" -lt 5 ] || fail "download failed (curl exit $rc)"
	say "install.sh: download interrupted (curl exit $rc); resuming" >&2
	sleep 2
done
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

# /dev/tty exists on every Linux but only opens with a controlling terminal (not under ssh without a
# tty, cloud-init or cron), so test by opening it rather than with [ -r ].
if ( : </dev/tty ) 2>/dev/null; then
	# Interactive: init asks for the mirror itself (its field starts with the same default; clearing it
	# means direct), unless TUNLR_MIRROR or --mirror already chose one.
	case " $* " in
	*" --mirror "* | *" --mirror="*) ;;
	*) [ -z "${TUNLR_MIRROR+x}" ] || set -- --mirror "$MIRROR" "$@" ;;
	esac
	exec "$BIN" init "$@" </dev/tty
fi
# Without a terminal nothing is asked: the mirror used here is init's too unless the arguments set one.
case " $* " in
*" --mirror "* | *" --mirror="*) ;;
*) set -- --mirror "$MIRROR" "$@" ;;
esac
exec "$BIN" init --yes "$@"
