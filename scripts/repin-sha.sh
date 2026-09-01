#!/bin/sh
# Recompute a Dockerfile sha256 integrity pin after its version pin moved.
#
# No Renovate datasource publishes the sha256 of the artifact a version
# literal names (github-tags exposes a git commit, npm exposes SHA-512, most
# dist tarballs publish nothing), so this script runs via postUpgradeTasks to
# compute it and land it in the bump commit.
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
# Exits 0 and changes nothing when no marker names <depName>. Exits non-zero
# on a marker it cannot honour (bad shape, unreachable URL, unchanged file).
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

# The version is interpolated into the sed EXPRESSION below, and GNU sed's `e`
# flag/command executes the pattern space as a shell command, so a version
# carrying `|`/`;` is arbitrary code execution here (and reaches curl as part
# of a URL, where `{}`/`[]` trigger curl's own globbing). Constrain its shape
# before any use.
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
# staged lives beside the Dockerfile (a rename must be same-filesystem), so
# it is outside $tmp and the trap removes it separately on interrupt.
staged=
cleanup() {
  rm -rf "$tmp"
  [ -n "$staged" ] && rm -f "$staged"
  return 0
}
trap cleanup EXIT INT TERM HUP

# resolve_target prints the real path of $1, following symlinks.
#
# The rewrite below commits by RENAME, which must land beside the actual
# file, so a Dockerfile reached through a symlink must be updated at its
# TARGET rather than replaced by a regular file.
#
# realpath/readlink -f are not POSIX, so both are probed. With neither
# available a SYMLINKED Dockerfile fails closed (a rename would replace the
# symlink); an ordinary file uses the path as given.
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

  # Emit "<ARG name> <url template>" for every marker naming this dep. The
  # marker must sit on the line immediately above its ARG.
  awk -v dep="$dep" '
		/^#[[:space:]]*repin:/ {
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
			if ($0 !~ /^ARG [A-Za-z_][A-Za-z0-9_]*=/) { exit 3 }
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

    # A sed replacement is NOT a literal context ('&'/'\1'/'|' are special),
    # so this is safe only because version was constrained to
    # [A-Za-z0-9._+~-] above.
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

    # Anchored on the ARG name and the 64-hex value, so nothing else in the
    # file can match; the trailing group preserves an inline Renovate anchor
    # comment.
    sed -E "s|^(ARG ${name}=)[0-9a-f]{64}([[:space:]].*)?\$|\1${sha}\2|" \
      "$dockerfile_target" >"$tmp/rewritten"

    if ! grep -qE "^ARG ${name}=${sha}([[:space:]]|\$)" "$tmp/rewritten"; then
      printf 'repin: %s: no 64-hex pin to rewrite in %s\n' "$name" "$dockerfile" >&2
      exit 1
    fi

    # Replace atomically: '>' truncates before the first byte lands, so a
    # killed task or ENOSPC would leave a truncated Dockerfile. Stage beside
    # the TARGET (mktemp -d is a different filesystem) and rename over it;
    # `cp -p` carries the original's mode across the replace.
    #
    # mktemp, not a $$-derived name: it creates the file with O_EXCL under an
    # unpredictable name, closing the window a symlink planted at a guessable
    # path could exploit. shell.md, "Temp files and atomic writes".
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
