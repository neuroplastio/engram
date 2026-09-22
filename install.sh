#!/bin/sh
# Install a binary from an engram channel, trusting nothing but a pinned key —
# and using nothing but tools already on the machine: curl, ssh-keygen,
# sha256sum (or shasum), tar. This is the point of the format: you do not need
# engram to check engram.
#
#   ENGRAM_SIGNERS='release@neuroplast.io namespaces="engram" ssh-ed25519 AAAA…' \
#     sh install.sh [name] [dir]
#
# name defaults to engram, dir to ./bin. Also read from the environment:
#   ENGRAM_URL      where channels are served   (https://pkg.neuroplast.io)
#   ENGRAM_PROJECT  the project                 (engram)
#   ENGRAM_CHANNEL  the channel                 (dev)
#   ENGRAM_COMMIT   a specific build            (the channel's head)
#
# ENGRAM_SIGNERS is required, and must come from your own source — a workflow
# file, a Dockerfile — not from the server this script talks to. A key fetched
# from the place it vouches for proves nothing.
set -eu

name="${1:-engram}"
dir="${2:-./bin}"
url="${ENGRAM_URL:-https://pkg.neuroplast.io}"
project="${ENGRAM_PROJECT:-engram}"
channel="${ENGRAM_CHANNEL:-dev}"
base="$url/$project/$channel"

[ -n "${ENGRAM_SIGNERS:-}" ] || { echo "install: ENGRAM_SIGNERS is not set (see the top of this script)" >&2; exit 2; }

case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) echo "install: unsupported OS $(uname -s)" >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64 | amd64) arch=amd64 ;; aarch64 | arm64) arch=arm64 ;; armv6l | armv7l | armv8l) arch=arm ;; *) echo "install: unsupported machine $(uname -m)" >&2; exit 1 ;; esac

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# field <key> — the value of key= on the line on stdin. The whole grammar is a
# split on spaces and a split on '='.
field() { tr ' ' '\n' | sed -n "s/^$1=//p" | head -n 1; }

commit="${ENGRAM_COMMIT:-}"
if [ -z "$commit" ]; then
	# The head is unsigned: it only says where to look.
	commit="$(curl -fsS "$base/head" | grep '^publish ' | field commit)"
	[ -n "$commit" ] || { echo "install: $project/$channel has no live build" >&2; exit 1; }
fi

curl -fsS -o "$tmp/manifest" "$base/builds/$commit/manifest"
curl -fsS -o "$tmp/manifest.sig" "$base/builds/$commit/manifest.sig"

printf '%s\n' "$ENGRAM_SIGNERS" >"$tmp/allowed_signers"
identity="${ENGRAM_SIGNERS%% *}"
ssh-keygen -Y verify -f "$tmp/allowed_signers" -I "$identity" -n engram \
	-s "$tmp/manifest.sig" <"$tmp/manifest" >/dev/null ||
	{ echo "install: the manifest of $commit is not signed by a pinned key" >&2; exit 1; }

# Signed for another project, channel or commit is as bad as not signed.
build="$(grep '^build ' "$tmp/manifest")"
[ "$(echo "$build" | field project)" = "$project" ] &&
	[ "$(echo "$build" | field channel)" = "$channel" ] &&
	[ "$(echo "$build" | field commit)" = "$commit" ] ||
	{ echo "install: signed manifest is not for $project/$channel $commit" >&2; exit 1; }

line="$(grep '^artifact ' "$tmp/manifest" | grep " name=$name " | grep " os=$os " | grep " arch=$arch " | head -n 1)"
[ -n "$line" ] || { echo "install: no $name for $os/$arch in $commit" >&2; exit 1; }
path="$(echo "$line" | field path)"
want="$(echo "$line" | field sha256)"

curl -fsS -o "$tmp/artifact" "$base/builds/$commit/$path"
if command -v sha256sum >/dev/null 2>&1; then
	got="$(sha256sum "$tmp/artifact" | cut -d' ' -f1)"
else
	got="$(shasum -a 256 "$tmp/artifact" | cut -d' ' -f1)"
fi
[ "$got" = "$want" ] || { echo "install: $path does not match the signed manifest" >&2; exit 1; }

mkdir -p "$dir"
tar -xzf "$tmp/artifact" -C "$dir" "$name"
echo "installed $dir/$name — $(echo "$build" | field version) ($commit), verified"
