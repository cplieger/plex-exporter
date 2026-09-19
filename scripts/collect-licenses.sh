#!/bin/sh
# Copy every linked Go module's license files into the /usr/share/licenses tree of
# attribution.md section 4. usage: collect-licenses.sh --name IMAGE [--out DIR] [--src DIR] [PACKAGE ...]
# CANONICAL COPY in cplieger/ci (configs/collect-licenses.sh), synced to each
# root-Dockerfile repo's scripts/collect-licenses.sh: edit it there, never here.
# A module with no license file fails the build rather than being skipped, because a
# missing text is a section 4(a) breach and the fix is a human decision.
set -eu

OUT=/out/usr/share/licenses
NAME=""
SRC=.
while [ $# -gt 0 ]; do
  case "$1" in
    --out)
      OUT="${2:?--out needs a directory}"
      shift 2
      ;;
    --src)
      SRC="${2:?--src needs a module directory}"
      shift 2
      ;;
    --name)
      NAME="${2:?--name needs an image name}"
      shift 2
      ;;
    --)
      shift
      break
      ;;
    -*)
      printf 'collect-licenses: unknown option %s\n' "$1" >&2
      exit 2
      ;;
    *) break ;;
  esac
done
case "$NAME" in
  '')
    printf 'collect-licenses: --name IMAGE is required\n' >&2
    exit 2
    ;;
  *[!A-Za-z0-9._-]*)
    printf 'collect-licenses: --name must be a single path component, got "%s"\n' "$NAME" >&2
    exit 2
    ;;
esac
[ $# -gt 0 ] || set -- ./...
export GOWORK=off
case "$OUT" in /*) ;; *) OUT="$PWD/$OUT" ;; esac
cd "$SRC" || exit 2

modules=0
files=0
missing=""

# copy_files SRC_DIR DEST_DIR all|licenses: copy the regular files at SRC_DIR's root
# (all of them, or the license-family names only) into DEST_DIR; sets $copied.
copy_files() {
  copied=0
  for f in "$1"/*; do
    [ -f "$f" ] || continue
    if [ "$3" = licenses ]; then
      case "$(printf '%s' "${f##*/}" | tr '[:lower:]' '[:upper:]')" in
        *.GO) continue ;;
        LICENSE* | LICENCE* | COPYING* | NOTICE*) ;;
        *) continue ;;
      esac
    fi
    mkdir -p "$2"
    cp -f "$f" "$2/"
    copied=$((copied + 1))
  done
}

main_path=$(go list -m -f '{{.Path}}')
main_dir=$(go list -m -f '{{.Dir}}')

roots=$(go list -f '{{if eq .Name "main"}}{{.ImportPath}}{{end}}' "$@" | sed '/^$/d')
if [ -z "$roots" ]; then
  printf 'collect-licenses: no main package matches: %s\n' "$*" >&2
  exit 1
fi
# shellcheck disable=SC2086  # import paths carry no whitespace
deps=$(go list -deps -f '{{if not .Standard}}{{with .Module}}{{.Path}}|{{.Dir}}{{end}}{{end}}' $roots \
  | sed '/^$/d' | sort -u)

if [ ! -f "$main_dir/LICENSE" ]; then
  printf 'collect-licenses: %s has no LICENSE (the main module, at %s)\n' "$main_path" "$main_dir" >&2
  exit 1
fi
copy_files "$main_dir" "$OUT/$NAME" licenses
if [ -f "$main_dir/THIRD_PARTY_NOTICES.md" ]; then
  cp -f "$main_dir/THIRD_PARTY_NOTICES.md" "$OUT/$NAME/"
  copied=$((copied + 1))
fi
modules=$((modules + 1))
files=$((files + copied))

while IFS='|' read -r path dir; do
  [ "$path" = "$main_path" ] && continue
  if [ -z "$dir" ]; then
    printf 'collect-licenses: %s has no source directory (vendor mode is not supported)\n' "$path" >&2
    exit 1
  fi
  copy_files "$dir" "$OUT/$path" licenses
  if [ "$copied" -eq 0 ]; then
    copy_files "$main_dir/licenses/$path" "$OUT/$path" all
  fi
  if [ "$copied" -eq 0 ]; then
    missing="$missing
$path (at $dir; add licenses/$path/ to the repo or drop the module)"
    continue
  fi
  modules=$((modules + 1))
  files=$((files + copied))
done <<EOF
$deps
EOF

if [ -n "$missing" ]; then
  printf 'collect-licenses: no LICENSE*, LICENCE*, COPYING* or NOTICE* file at the module root of:\n' >&2
  printf '%s\n' "$missing" | sed '/^$/d; s/^/  /' >&2
  exit 1
fi
printf 'collect-licenses: %s modules, %s files under %s\n' "$modules" "$files" "$OUT"
