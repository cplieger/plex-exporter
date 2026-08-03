#!/bin/sh
# Recompute a Dockerfile sha256 integrity pin after its version pin moved.
#
# Renovate can bump a version literal but cannot compute the sha256 of the
# artifact that version names: no datasource publishes it (github-tags exposes a
# git commit, npm exposes SHA-512, most dist tarballs publish nothing). Every
# such pin therefore used to need a human to run curl | sha256sum and paste the
# result into the PR. This script is that step, run by Renovate itself via
# postUpgradeTasks so the recomputed pin lands in the bump commit.
#
# Each pin declares its own source URL in a marker comment on the line directly
# above the ARG it protects:
#
#   # repin: dep=ryanoasis/nerd-fonts url=https://github.com/ryanoasis/nerd-fonts/releases/download/{version}/Monaspace.tar.xz
#   ARG NERDFONT_SHA256=<64 lowercase hex>
#
#   {version}      the new version exactly as Renovate reports it (may lead v)
#   {version_nov}  the same with one leading v stripped
#
# A dep with several pins (per-arch artifacts) declares one marker per ARG.
#
# Usage: repin-sha.sh <depName> <newVersion> [dockerfile ...]
#
# Exits 0 and changes nothing when no marker names <depName>: the Renovate task
# is wired for a SET of deps and must be a silent no-op for every other one.
# Exits non-zero on a marker it cannot honour (bad shape, unreachable URL,
# unchanged file), because a silent miss reproduces exactly the stale-pin build
# failure this script exists to prevent.
set -eu

usage() {
  echo "usage: repin-sha.sh <depName> <newVersion> [dockerfile ...]" >&2
  exit 2
}

[ $# -ge 2 ] || usage
dep=$1
version=$2
shift 2
[ -n "$dep" ] && [ -n "$version" ] || usage

version_nov=${version#v}

if [ $# -eq 0 ]; then
  set -- Dockerfile
fi

tmp=$(mktemp -d)
# shellcheck disable=SC2064 # expand $tmp now: it must not depend on later state
trap "rm -rf '$tmp'" EXIT INT TERM

updated=0

for dockerfile in "$@"; do
  [ -f "$dockerfile" ] || continue

  # Emit "<ARG name> <url template>" for every marker naming this dep. The
  # marker must sit on the line immediately above its ARG so the pairing is
  # unambiguous in a file that carries several pins.
  awk -v dep="$dep" '
		/^#[[:space:]]*repin:/ {
			d = ""; u = ""
			for (i = 1; i <= NF; i++) {
				if ($i ~ /^dep=/) { d = substr($i, 5) }
				if ($i ~ /^url=/) { u = substr($i, 5) }
			}
			if (d == "" || u == "") {
				printf "repin: malformed marker at %s:%d (need dep= and url=)\n", FILENAME, FNR > "/dev/stderr"
				exit 3
			}
			pending_dep = d; pending_url = u; pending_line = FNR
			next
		}
		pending_dep != "" {
			if ($0 !~ /^ARG [A-Za-z_][A-Za-z0-9_]*=/) {
				printf "repin: marker at %s:%d is not followed by an ARG assignment\n", FILENAME, pending_line > "/dev/stderr"
				exit 3
			}
			if (pending_dep == dep) {
				name = $0
				sub(/^ARG /, "", name)
				sub(/=.*/, "", name)
				printf "%s %s\n", name, pending_url
			}
			pending_dep = ""
			next
		}
	' "$dockerfile" >"$tmp/pins" || exit $?

  while read -r name url; do
    [ -n "$name" ] || continue

    # Placeholder expansion is a literal substitution on the marker's own
    # text, so a URL is only ever built from the Dockerfile plus the version
    # Renovate reports.
    resolved=$(printf '%s\n' "$url" \
      | sed -e "s|{version}|$version|g" -e "s|{version_nov}|$version_nov|g")

    case $resolved in
      https://*) ;;
      *)
        echo "repin: $name: refusing non-https URL: $resolved" >&2
        exit 1
        ;;
    esac

    echo "repin: $dockerfile: $name <- $resolved"
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 \
      --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5 \
      -fsSL -o "$tmp/artifact" "$resolved"

    sha=$(sha256sum "$tmp/artifact" | cut -d' ' -f1)
    case $sha in
      [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;;
      *)
        echo "repin: $name: sha256sum produced no digest" >&2
        exit 1
        ;;
    esac

    # Anchored on the ARG name and the 64-hex value, so nothing else in the
    # file can match; the optional trailing group preserves an inline
    # Renovate anchor comment (ARG X=<sha>  # tool v1.2.3).
    sed -E "s|^(ARG ${name}=)[0-9a-f]{64}([[:space:]].*)?\$|\1${sha}\2|" \
      "$dockerfile" >"$tmp/rewritten"

    if ! grep -qE "^ARG ${name}=${sha}([[:space:]]|\$)" "$tmp/rewritten"; then
      echo "repin: $name: no 64-hex pin to rewrite in $dockerfile" >&2
      exit 1
    fi

    cat "$tmp/rewritten" >"$dockerfile"
    updated=$((updated + 1))
  done <"$tmp/pins"
done

if [ "$updated" -eq 0 ]; then
  echo "repin: no pin declares dep=$dep; nothing to do"
fi
