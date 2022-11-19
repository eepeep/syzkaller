#!/usr/bin/env bash

# Copyright 2022 syzkaller project authors. All rights reserved.
# Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

set -o errexit
set -o errtrace
set -o nounset
set -o pipefail
shopt -s extdebug
IFS=$'\n\t'

# TODO: Make the workdir be a parameter.
# TODO: Scope locals, pass more things as parameters.
# TODO: This script is getting overgrown enough that it's probably time start
# using Go instead.

help="This script will set up, build, and run Syzkaller for Fuchsia. You will
need a Syzkaller checkout and a Fuchsia checkout, and you will need a working
installation of the Go programming language. See docs/fuchsia/README.md in the
Syzkaller repository for more information.

In the commands below, \`syzkaller-directory\` and \`fuchsia-directory\` must be
absolute pathnames.

Usage:

  setup.sh help

Prints this help message.

  setup.sh build syzkaller-directory fuchsia-directory [arch]

Builds Syzkaller and Fuchsia for \`arch\` (x64/arm64).

  setup.sh [-d] run syzkaller-directory fuchsia-directory

Runs Syzkaller on the Fuchsia emulator. (You must have built both first, using
\`setup.sh build ...\`.) If you pass the \`-d\` option, \`syz-manager\` will be
run with the \`--debug\` option.

  setup.sh update syzkaller-directory fuchsia-directory

Updates the Fuchsia system call definitions that Syzkaller will use."

die() {
  echo "$@" > /dev/stderr
  echo "For help, run \`setup.sh help\`."
  exit 1
}

usage() {
  echo "$help"
  exit 0
}

preflight() {
  if ! which go > /dev/null; then
    die "You need to install the Go language."
  fi

  syzkaller="$1"
  if [[ ! -d "$syzkaller" ]]; then
    die "$syzkaller is not a directory."
  fi
  fuchsia="$2"
  if [[ ! -d "$fuchsia" ]]; then
    die "$fuchsia is not a directory."
  fi
}

configure_build() {
  cd "$fuchsia"
  outdir="out/$arch"
  fx --dir $outdir set core.$arch \
    --with-base "//bundles/tools" \
    --with-base "//src/testing/fuzzing/syzkaller" \
    --args=syzkaller_dir="\"$syzkaller\"" \
    --variant=kasan
}

build() {
  preflight "$syzkaller" "$fuchsia"

  configure_build
  cd "$fuchsia"
  fx build

  cd "$syzkaller"
  make TARGETOS=fuchsia TARGETARCH=$syzkaller_arch SOURCEDIR="$fuchsia"
}

run() {
  preflight "$syzkaller" "$fuchsia"

  cd "$fuchsia"

  # Look up needed deps from build_api metadata
  fvm_path=$(jq -r '.[] | select(.name == "storage-full" and .type == "blk").path' $outdir/images.json)
  zbi_path=$(jq -r '.[] | select(.name == "zircon-a" and .type == "zbi").path' $outdir/images.json)
  multiboot_path=$(jq -r '.[] | select(.name == "qemu-kernel" and .type == "kernel").path' $outdir/images.json)

  # Make a separate directory for copies of files we need to modify
  syz_deps_path=$fuchsia/$outdir/syzdeps
  mkdir -p $syz_deps_path

  ./$outdir/host_x64/zbi -o $syz_deps_path/fuchsia-ssh.zbi $outdir/$zbi_path \
    --entry "data/ssh/authorized_keys=${fuchsia}/.ssh/authorized_keys"
  cp $outdir/$fvm_path \
    $syz_deps_path/fvm-extended.blk
  ./$outdir/host_x64/fvm \
    $syz_deps_path/fvm-extended.blk extend --length 3G

  echo "{
  \"name\": \"fuchsia\",
  \"target\": \"fuchsia/$syzkaller_arch\",
  \"http\": \":12345\",
  \"workdir\": \"$workdir\",
  \"kernel_obj\": \"$fuchsia/$outdir/kernel_$arch-kasan/obj/zircon/kernel\",
  \"syzkaller\": \"$syzkaller\",
  \"image\": \"$syz_deps_path/fvm-extended.blk\",
  \"sshkey\": \"$fuchsia/.ssh/pkey\",
  \"reproduce\": false,
  \"cover\": false,
  \"procs\": 8,
  \"type\": \"qemu\",
  \"vm\": {
    \"count\": 10,
    \"cpu\": 4,
    \"mem\": 2048,
    \"kernel\": \"$fuchsia/$outdir/$multiboot_path\",
    \"initrd\": \"$syz_deps_path/fuchsia-ssh.zbi\"
  }
}" > "$workdir/fx-syz-manager-config.json"

  cd "$syzkaller"
  # TODO: Find the real way to fix this: Syzkaller wants to invoke qemu
  # manually, but perhaps it should be calling `ffx emu ...` or the like. See
  # also //scripts/hermetic-env and //tools/devshell/lib/prebuilt.sh in
  # $fuchsia.
  PATH="$PATH:$fuchsia/prebuilt/third_party/qemu/linux-x64/bin:$fuchsia/prebuilt/third_party/qemu/mac-x64/bin"
  bin/syz-manager -config "$workdir/fx-syz-manager-config.json" "$debug"
}

build_sdk() {
  configure_build
  cd "$fuchsia"
  fx build sdk

  # The SDK is assembled with a bunch of symlinks to absolute paths that don't
  # resolve inside the Docker container.
  find $outdir/sdk/exported/core -type l -exec sh -c 'cp --remove-destination $(readlink -n $0) $0' {} \;

}

update_syscall_definitions() {
  preflight "$syzkaller" "$fuchsia"

  # syz-extract needs a sysroot available, but if the definitions are currently
  # out-of-date we may not be able to run a full build without running into an
  # error, so only build a subset here (the SDK seems reasonable).
  arch=x64
  build_sdk
  arch=arm64
  build_sdk

  cd "$syzkaller"
  tools/syz-env make extract TARGETOS=fuchsia SOURCEDIR="$fuchsia"
  tools/syz-env make generate
}

main() {
  debug=""
  while getopts "d" o; do
    case "$o" in
    d)
      debug="--debug"
    esac
  done
  shift $((OPTIND - 1))

  if [[ $# < 3 || $# > 4 ]]; then
    usage
  fi

  command="$1"
  syzkaller="$(realpath $2)"
  fuchsia="$(realpath $3)"

  arch="${4:-x64}"
  if [[ ! ("$arch" == "x64" || "$arch" == "arm64") ]]; then
    die "Invalid arch: must be arm64 or x64."
  fi

  # Syzkaller uses a different name for x64
  syzkaller_arch=$arch
  if [[ $syzkaller_arch == "x64" ]]; then
    syzkaller_arch="amd64"
  fi

  workdir="$syzkaller/workdir.fuchsia"
  mkdir -p "$workdir"

  case "$command" in
    build)
      build;;
    run)
      run;;
    update)
      update_syscall_definitions;;
    *)
      usage;;
  esac
}

main $@
