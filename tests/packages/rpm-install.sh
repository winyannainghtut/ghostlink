#!/usr/bin/env bash
# Installs the built client, mesh agent and server .rpm with dnf in a container of each RPM
# distribution that scripts/install-server.sh and scripts/install-mesh.sh support (Fedora, a
# RHEL-family one, Amazon Linux), through tests/packages/rpm-container.sh. Needs Docker and the
# packages in artifacts/linux-ARCH (scripts/build-linux*.sh). All distributions are tried; the
# status is 1 when any failed.
# Usage: tests/packages/rpm-install.sh x64|arm64
#
# The images are pinned by the digest of their multi-architecture index (the same for x64 and
# arm64), from each distribution's own registry (no Docker Hub: its pull limit hits shared CI
# runners). To move one: look up the tag's digest, check that the index lists linux/amd64 and
# linux/arm64, and change it here, for example
#   curl -fsSI -H 'Accept: application/vnd.oci.image.index.v1+json' https://registry.fedoraproject.org/v2/fedora/manifests/45
# (Docker-Content-Digest header; quay.io and public.ecr.aws want an anonymous token first).
# A pinned digest that a registry has dropped fails the pull: update it to the tag's new digest.
set -euo pipefail
cd "$(dirname "$0")/../.."

arch=${1:?usage: tests/packages/rpm-install.sh x64|arm64}
case "$arch" in x64 | arm64) ;; *) echo "unknown architecture: $arch" >&2; exit 2 ;; esac
packages="$PWD/artifacts/linux-$arch"
[ -d "$packages" ] || { echo "no $packages: build the packages first" >&2; exit 2; }

images=(
    # Fedora 45 (dnf 5), the release "latest" was on 2026-11-26
    "registry.fedoraproject.org/fedora:45@sha256:806e6bc4b2f302baca3b0f53382416888f6ad46476063a2b47e72fc29968ae25"
    # Rocky Linux 9 (RHEL 9: dnf 4, glibc 2.34, OpenSSL 3.0)
    "quay.io/rockylinux/rockylinux:9@sha256:bb5755f96bec05ff26165e779ec843d7167544be9e50770508f307019a6d05c4"
    # Amazon Linux 2023 (dnf 4, glibc 2.34)
    "public.ecr.aws/amazonlinux/amazonlinux:2023@sha256:5c38fd85b00ce8c094315d9b71f671f880052314a22bd25f1a027821bf61d9d7"
)

failed=()
for image in "${images[@]}"; do
    echo "::group::$image"
    name="rpm-install-$$"
    status=0
    # A hung dnf must not hold the job for hours: the container is removed after 25 minutes.
    timeout 1500 docker run --rm --name "$name" \
        --volume "$packages:/pkgs:ro" \
        --volume "$PWD/tests/packages/rpm-container.sh:/rpm-container.sh:ro" \
        "$image" bash /rpm-container.sh || status=$?
    echo "::endgroup::"
    if [ "$status" != 0 ]; then
        docker rm --force "$name" > /dev/null 2>&1 || true
        echo "::error::the .rpm packages do not install on ${image%%@*} (exit $status)"
        failed+=("${image%%@*}")
    fi
done

if [ "${#failed[@]}" != 0 ]; then
    echo "FAILED on: ${failed[*]}" >&2
    exit 1
fi
echo "The .rpm packages install, run and remove on all ${#images[@]} distributions."
