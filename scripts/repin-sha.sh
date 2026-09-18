#!/bin/sh
# Recompute a Dockerfile sha256 integrity pin after Renovate moved its version
# literal, run from postUpgradeTasks because no datasource publishes a sha256.
# Marker convention and enrollment: ci-renovate.md, "Automated sha-pin recompute".
# CANONICAL COPY in cplieger/ci (configs/repin-sha.sh), synced to every repo with a
# root Dockerfile: edit it there, never here.
# A marker it cannot honour exits non-zero rather than skipping, because a silent
# miss reproduces the stale-pin build failure the pin exists to catch.
set -eu

usage() {
  printf 'usage: repin-sha.sh <depName> <newVersion> [dockerfile ...]\n' >&2
  exit 2
}

[ $# -ge 2 ] || usage
dep=$1
version=$2
shift 2
[ -n "$dep" ] && [ -n "$version" ] || usage

# The version reaches a sed EXPRESSION below (GNU sed's `e` executes the pattern space
# as a shell command) and a curl URL (`{}`/`[]` are curl globbing), and its value is
# whatever a third-party datasource reported. Constrain its shape before either.
case $version in
  *[!A-Za-z0-9._+~-]*)
    printf 'repin: refusing version with unexpected characters: %s\n' "$version" >&2
    exit 1
    ;;
esac

version_nov=${version#v}

if [ $# -eq 0 ]; then
  set -- Dockerfile
fi

tmp=$(mktemp -d)
# staged lives OUTSIDE $tmp by necessity (a rename must be same-filesystem), so the
# mktemp -d cleanup cannot reach it; tracked here so the trap removes it when an
# interrupt lands between the copy and the rename.
staged=
cleanup() {
  rm -rf "$tmp"
  [ -n "$staged" ] && rm -f "$staged"
  return 0
}
trap cleanup EXIT INT TERM HUP

# The rewrite commits by RENAME, so a Dockerfile reached through a symlink must be
# updated at its TARGET, not replaced by a regular file. Neither realpath nor readlink
# is in POSIX before POSIX.1-2024, so a SYMLINKED Dockerfile with neither fails closed.
resolve_target() {
  if command -v realpath >/dev/null 2>&1; then
    realpath "$1"
  elif command -v readlink >/dev/null 2>&1 && readlink -f "$1" >/dev/null 2>&1; then
    readlink -f "$1"
  else
    if [ -L "$1" ]; then
      printf 'repin: %s: cannot safely update symlink without realpath or readlink -f\n' "$1" >&2
      return 1
    fi
    printf '%s\n' "$1"
  fi
}

updated=0

for dockerfile in "$@"; do
  [ -f "$dockerfile" ] || continue

  dockerfile_target=$(resolve_target "$dockerfile") || {
    printf 'repin: %s: cannot resolve target path\n' "$dockerfile" >&2
    exit 1
  }

  # Emit "<ARG name> <url template>" per marker naming this dep. The marker sits on
  # the line immediately above its ARG, which is what makes the pairing unambiguous.
  awk -v dep="$dep" '
		/^#[[:space:]]*repin:/ {
			# END reports it (awk runs END on exit with pending_dep still set).
			if (pending_dep != "") { exit 3 }
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
			if ($0 !~ /^ARG [A-Za-z_][A-Za-z0-9_]*=/) { exit 3 }  # END reports it
			if (pending_dep == dep) {
				name = $0
				sub(/^ARG /, "", name)
				sub(/=.*/, "", name)
				printf "%s %s\n", name, pending_url
			}
			pending_dep = ""
			next
		}
		END {
			if (pending_dep != "") {
				printf "repin: marker at %s:%d is not followed by an ARG assignment\n", FILENAME, pending_line > "/dev/stderr"
				exit 3
			}
		}
	' "$dockerfile_target" >"$tmp/pins" || exit $?

  while read -r name url; do
    [ -n "$name" ] || continue

    # A sed replacement is not a literal context ('&' re-inserts the match, '|' closes
    # the command), so this is safe only because of the shape check on $version above.
    resolved=$(printf '%s\n' "$url" \
      | sed -e "s|{version}|$version|g" -e "s|{version_nov}|$version_nov|g")

    case $resolved in
      https://*) ;;
      *)
        printf 'repin: %s: refusing non-https URL: %s\n' "$name" "$resolved" >&2
        exit 1
        ;;
    esac

    printf 'repin: %s: %s <- %s\n' "$dockerfile" "$name" "$resolved"
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 \
      --connect-timeout 20 --max-time 300 --retry 3 --retry-delay 5 \
      -fsSL -o "$tmp/artifact" "$resolved"

    sha=$(sha256sum "$tmp/artifact" | cut -d' ' -f1)
    case $sha in
      [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;;
      *)
        printf 'repin: %s: sha256sum produced no digest\n' "$name" >&2
        exit 1
        ;;
    esac

    # The optional trailing group preserves an inline anchor comment (# tool v1.2.3).
    sed -E "s|^(ARG ${name}=)[0-9a-f]{64}([[:space:]].*)?\$|\1${sha}\2|" \
      "$dockerfile_target" >"$tmp/rewritten"

    if ! grep -qE "^ARG ${name}=${sha}([[:space:]]|\$)" "$tmp/rewritten"; then
      printf 'repin: %s: no 64-hex pin to rewrite in %s\n' "$name" "$dockerfile" >&2
      exit 1
    fi

    # Replace by rename: '>' truncates the target before the first byte lands, so a
    # killed postUpgradeTask or an ENOSPC would leave a truncated Dockerfile in the
    # commit Renovate makes. `cp -p` carries the original's mode across (mktemp's own
    # 0600 must not reach the committed file) where `chmod --reference` is GNU-only.
    staged=$(mktemp "$dockerfile_target.repin.XXXXXX")
    cp -p "$dockerfile_target" "$staged"
    cat "$tmp/rewritten" >"$staged"
    mv -f "$staged" "$dockerfile_target"
    staged= # renamed away: nothing left for the trap to remove
    updated=$((updated + 1))
  done <"$tmp/pins"
done

if [ "$updated" -eq 0 ]; then
  printf 'repin: no pin declares dep=%s; nothing to do\n' "$dep"
fi
