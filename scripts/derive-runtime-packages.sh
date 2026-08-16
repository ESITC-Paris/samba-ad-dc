#!/bin/sh
# Derive the runtime Debian package set for a Samba DESTDIR tree.
#
# SPEC §5.1 requires every installed package to be justifiable. This
# script supplies the evidence half of that: it derives, rather than
# guesses, which Debian packages the compiled artefacts actually link
# against.
#
# Method: walk every ELF object in the DESTDIR, ask the dynamic loader
# which shared libraries each one resolves, drop the ones the DESTDIR
# provides itself, and map every remaining library file to its owning
# dpkg package.
#
# Run it inside the *builder* image, which holds both the DESTDIR and the
# dpkg database of the packages the build linked against:
#
#   docker run --rm -v "$PWD/scripts:/scripts:ro" \
#       samba-ad-dc:builder-smoke sh /scripts/derive-runtime-packages.sh
#
# Output: one package name per line, sorted and unique. It is an *input*
# to runtime-packages.txt, not a substitute for it. Two limits are
# deliberate and are covered by hand-justified entries in that file:
#   * dlopen()ed libraries are invisible to ldd;
#   * interpreted dependencies (python modules, chrony, tini, ...) are
#     not linkage and never show up here.
set -eu

DEST=${1:-/dest}
[ -d "$DEST" ] || { echo "no such DESTDIR: $DEST" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# 1. Candidates: executables and shared objects, including versioned ones.
find "$DEST" -type f \( -perm -u+x -o -name '*.so' -o -name '*.so.*' \) \
    -print > "$work/candidates"

# 2. Keep the genuine ELF objects. Shell and python scripts are
#    executable too, and `file` is not part of the builder toolchain, so
#    test the four ELF magic bytes directly rather than adding a package
#    (and busting the builder cache) for a four-byte comparison.
: > "$work/elves"
while IFS= read -r obj; do
    magic=$(dd if="$obj" bs=4 count=1 2>/dev/null | od -A n -t x1 | tr -d ' \n')
    [ "$magic" = "7f454c46" ] && printf '%s\n' "$obj" >> "$work/elves"
done < "$work/candidates"

# 3. Resolve shared-library dependencies. ldd exits non-zero on static
#    objects, object files and foreign architectures; none of those is an
#    error here, so failures are tolerated per object.
while IFS= read -r obj; do
    ldd "$obj" 2>/dev/null || true
done < "$work/elves" \
    | awk '$2 == "=>" && $3 ~ /^\// { print $3 }' \
    | sort -u > "$work/libs"

# 4. Whatever the DESTDIR ships itself is not a package requirement.
grep -v "^$DEST/" "$work/libs" > "$work/external" || true

# 5. Map library files to owning packages. Resolve symlinks first: dpkg
#    owns libfoo.so.1.2.3 while the loader reports libfoo.so.1. A file
#    may be listed by several packages (dpkg prints "a, b: /path"), hence
#    the comma split.
while IFS= read -r lib; do
    real=$(readlink -f "$lib" 2>/dev/null) || continue
    [ -n "$real" ] || continue
    dpkg -S "$real" 2>/dev/null | cut -d: -f1 || true
done < "$work/external" \
    | tr ',' '\n' \
    | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' \
    | grep -v '^$' \
    | sort -u
