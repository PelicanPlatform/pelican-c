#!/bin/sh
# package-release.sh <os> <arch> - build libpelicanclient and stage a
# release tarball under dist/.
#
# Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
# SPDX-License-Identifier: Apache-2.0

set -eu

os="${1:?usage: package-release.sh <os> <arch>}"
arch="${2:?usage: package-release.sh <os> <arch>}"

abi_version="$(make -s print-abi-version)"
case "$os" in
darwin)
    soext="dylib"
    libname="libpelicanclient.${abi_version}.dylib"
    ;;
*)
    soext="so"
    libname="libpelicanclient.so.${abi_version}"
    ;;
esac
linkname="libpelicanclient.${soext}"

# GITHUB_REF_NAME is the tag on release builds; fall back to a dev
# version for workflow_dispatch and local runs.
version="${GITHUB_REF_NAME:-}"
case "$version" in
v*) ;;
*) version="dev-$(git rev-parse --short HEAD)" ;;
esac

make

name="libpelicanclient_${version}_${os}_${arch}"
stage="dist/${name}"
rm -rf "$stage"
mkdir -p "$stage/include/pelican" "$stage/lib/pkgconfig"
# The versioned library plus its development symlink, matching what
# `make install` lays down.
cp "build/${libname}" "$stage/lib/"
ln -sf "${libname}" "$stage/lib/${linkname}"
cp build/libpelicanclient.pc "$stage/lib/pkgconfig/"
cp include/pelican/client.h "$stage/include/pelican/"
cp README.md LICENSE "$stage/"
cp docs/design.md "$stage/DESIGN.md"

tar -czf "dist/${name}.tar.gz" -C dist "$name"
rm -rf "$stage"
echo "packaged dist/${name}.tar.gz"
ls -l dist
