#!/usr/bin/env bash
# Build dist/lib/linux_<goarch>/codex-reorder.so inside a manylinux2014 container
# (glibc 2.17), so the release library loads on old distributions as well as
# the CPA Docker image. The container must match the host architecture: run
# amd64 on an x86_64 host and arm64 on an aarch64 host (CI uses native runners).
#
# Usage: scripts/build-manylinux.sh [goarch]   (default: host goarch)
# Needs: docker and a Go toolchain whose `go` binary runs on glibc 2.17 (official
# golang.org builds do); its GOROOT is mounted read-only into the container.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
goarch="${1:-$(go env GOARCH)}"

case "${goarch}" in
  amd64)
    image="quay.io/pypa/manylinux2014_x86_64"
    machine="x86_64"
    portable=(--env GOAMD64=v1 --env CGO_CFLAGS=-march=x86-64)
    ;;
  arm64)
    image="quay.io/pypa/manylinux2014_aarch64"
    machine="aarch64"
    portable=(--env GOARM64=v8.0 --env CGO_CFLAGS=-march=armv8-a)
    ;;
  *)
    echo "unsupported goarch: ${goarch} (want amd64 or arm64)" >&2
    exit 1
    ;;
esac

if [[ "$(uname -m)" != "${machine}" ]]; then
  echo "host is $(uname -m); building ${goarch} needs a ${machine} host" >&2
  exit 1
fi

go_root="$(go env GOROOT)"

out_rel="dist/lib/linux_${goarch}/codex-reorder.so"
mkdir -p "${repo_root}/dist/lib/linux_${goarch}" "${repo_root}/dist/.cache"

# Run as the invoking user so nothing in the checkout ends up owned by root.
docker run --rm \
  --user "$(id -u):$(id -g)" \
  --volume "${repo_root}:/workspace" \
  --volume "${go_root}:/opt/go:ro" \
  --workdir /workspace \
  --env HOME=/tmp/home \
  --env GOCACHE=/workspace/dist/.cache/go-build \
  --env GOMODCACHE=/workspace/dist/.cache/gomod \
  --env GOPROXY="${GOPROXY:-https://proxy.golang.org,direct}" \
  --env GOTOOLCHAIN=local \
  --env CGO_ENABLED=1 \
  --env OUT="${out_rel}" \
  "${portable[@]}" \
  "${image}" \
  bash -euo pipefail -c '
    export PATH=/opt/go/bin:${PATH}
    mkdir -p "${HOME}"
    go version
    ldd --version | sed -n 1p   # not head: ldd would get SIGPIPE under pipefail
    go build -buildmode=c-shared -trimpath -buildvcs=false -ldflags="-s -w" -o "${OUT}" .
    rm -f "${OUT%.so}.h"
    chmod 0755 "${OUT}"

    # Prove the library loads on glibc 2.17 and exports the plugin ABI.
    cat > /tmp/dlcheck.c <<"EOF"
#include <dlfcn.h>
#include <stdio.h>
int main(int argc, char **argv) {
  const char *syms[] = {"cliproxy_plugin_init", "cliproxyPluginCall", "cliproxyPluginFree", "cliproxyPluginShutdown"};
  void *h = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
  if (!h) { fprintf(stderr, "dlopen: %s\n", dlerror()); return 1; }
  for (unsigned i = 0; i < sizeof syms / sizeof *syms; i++) {
    if (!dlsym(h, syms[i])) { fprintf(stderr, "missing symbol %s\n", syms[i]); return 1; }
  }
  printf("dlopen ok: %s\n", argv[1]);
  return 0;
}
EOF
    gcc -o /tmp/dlcheck /tmp/dlcheck.c -ldl
    /tmp/dlcheck "/workspace/${OUT}"
  '

echo "built ${out_rel}"
