#!/bin/sh
# Removes tunlr completely (tunlr uninstall --purge): stops mihomo, removes the systemd units, the
# kernels, the nftables rules, the user tunlr, the settings, subscriptions, history, state and
# /usr/local/bin/tunlr.
#
#   curl -fsSL https://cdn.jsdelivr.net/gh/tunlr-app/cli@main/uninstall.sh | sh
#
# It asks for confirmation when a terminal is available; without one (no /dev/tty) it proceeds.
# If /usr/local/bin/tunlr is missing, the current release is downloaded first (TUNLR_MIRROR and
# TUNLR_VERSION as in install.sh) so the cleanup still runs.
set -eu

BIN=/usr/local/bin/tunlr
INSTALL_URL=${TUNLR_INSTALL_URL:-https://cdn.jsdelivr.net/gh/tunlr-app/cli@main/install.sh}

say() { printf '%s\n' "$*"; }
fail() { say "uninstall.sh: $*" >&2; exit 1; }

[ "$(uname -s)" = Linux ] || fail "tunlr only runs on Linux"
[ "$(id -u)" -eq 0 ] || fail "run it as root: curl -fsSL ... | sudo sh"

if [ ! -x "$BIN" ]; then
	say "$BIN is missing; downloading tunlr to run the cleanup"
	curl -fsSL "$INSTALL_URL" | TUNLR_INIT=no sh || fail "could not download tunlr"
fi

if [ -r /dev/tty ]; then
	exec "$BIN" uninstall --purge </dev/tty
fi
exec "$BIN" uninstall --purge --yes
