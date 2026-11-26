#!/bin/bash
# Runs INSIDE the container of one RPM distribution (tests/packages/rpm-install.sh starts it), as
# root, with the built packages mounted read-only at /pkgs. The container has no systemd and no
# Docker, like a minimal image or a build host, and that is what is tested:
#
#   - the client, the mesh agent and the server install with dnf (their dependencies resolve from
#     the distribution's own repositories) and remove again, alone and all three in one transaction;
#   - no scriptlet fails (rpm and dnf only warn about one: the package still goes in, dnf exits 0,
#     so the output is read for it), also on a reinstall, which runs the scriptlets as an upgrade;
#   - the installed programs run and report the package's version (this is also what finds a
#     program built against a newer glibc or OpenSSL than the distribution has);
#   - without Docker the server's post-install gives its soft failure: the install goes through
#     and the script says what to do (the same script is replayed to read what it prints);
#   - removing leaves no program or unit behind.
#
# It also prints what a plain "docker" would install here (information for the decision about a
# Docker dependency in the server package; nothing is asserted about it).
set -euo pipefail

. /etc/os-release
fail() { echo "FAIL on $PRETTY_NAME: $*" >&2; exit 1; }
step() { printf '\n--- %s\n' "$*"; }
log=/tmp/dnf.log

dnf --version > /tmp/dnf-version.txt 2>&1 || true
echo "=== $PRETTY_NAME, $(uname -m), $(rpm --version), $(head -n 1 /tmp/dnf-version.txt), $(ldd --version 2>&1 | head -n 1)"

[ ! -d /run/systemd/system ] || fail "systemd runs in this container; the test is about installs without it"
for program in docker podman systemctl; do
    ! command -v "$program" > /dev/null || fail "$program is in the image; the test is about hosts without it"
done

# The one package file of a kind.
package() {
    local files=($1)
    [ "${#files[@]}" = 1 ] && [ -f "${files[0]}" ] || fail "expected exactly one file for $1, found: ${files[*]}"
    echo "${files[0]}"
}
client=$(package '/pkgs/ghostlink-[0-9]*.rpm')
mesh=$(package '/pkgs/ghostlink-mesh-[0-9]*.rpm')
server=$(package '/pkgs/ghostlink-server-[0-9]*.rpm')
# The version the programs report: an rpm spells a pre-release 0.9.2~rc.1, the programs 0.9.2-rc.1.
version=$(rpm -qp --qf '%{VERSION}' "$client" | tr '~' '-')
echo "packages: $client $mesh $server (version $version)"
for file in "$client" "$mesh" "$server"; do
    echo "requires of $(basename "$file"): $(rpm -qp --requires "$file" | tr '\n' ';')"
done

# scriptlet_trouble LOG: lines that tell of a scriptlet that failed (rpm: "scriptlet failed, exit
# status 1"; dnf 4: "Non-fatal POSTIN scriptlet failure"; dnf 5 says it alike).
scriptlet_trouble() { grep -Ei 'scriptlet.*(fail|error)|(fail|error).*scriptlet' "$1"; }

# transaction COMMAND...: runs it with its output shown and kept; stops on a failure, or on a
# scriptlet that failed although the command succeeded.
transaction() {
    local status=0
    "$@" > "$log" 2>&1 || status=$?
    cat "$log"
    [ "$status" = 0 ] || fail "$* (exit $status)"
    if scriptlet_trouble "$log" > /dev/null; then fail "a scriptlet failed in: $*"; fi
}

present() { [ -e "$1" ] || fail "$1 is missing"; }
absent() { [ ! -e "$1" ] || fail "$1 is still there"; }
installed() { rpm -q "$1" > /dev/null || fail "$1 is not installed"; }
not_installed() { ! rpm -q "$1" > /dev/null 2>&1 || fail "$1 is still installed"; }

# runs COMMAND...: it runs, and its output names the package's version.
runs() {
    local output status=0
    output=$("$@" 2>&1) || status=$?
    echo "$* -> $output"
    [ "$status" = 0 ] || fail "$* does not run (exit $status)"
    case "$output" in *"$version"*) ;; *) fail "$* does not report version $version" ;; esac
}

step "repository metadata"
for attempt in 1 2 3; do
    dnf makecache && break
    [ "$attempt" != 3 ] || fail "dnf cannot reach the distribution's repositories"
    sleep 20
done

step "client: install, run, reinstall (an upgrade for its scriptlets), remove"
transaction dnf install -y "$client"
installed ghostlink
present /usr/bin/ghostlink
present /usr/bin/ghostlinkd
present /usr/bin/ghostlink-mcp
present /usr/lib/ghostlink/ghostlink-tunnel
present /usr/lib/systemd/system/ghostlinkd.service
present /etc/ghostlink/ghostlinkd.env
runs ghostlink --version
runs ghostlinkd --version
runs ghostlink-mcp --version
runs /usr/lib/ghostlink/ghostlink-tunnel version
# --nosignature: the packages are not signed, and rpm 6 (Fedora 45) refuses an unsigned package
# unless told (dnf installs one from a file, as the docs say to, after a warning).
transaction rpm -U --replacepkgs --nosignature "$client"
installed ghostlink
runs ghostlink --version
transaction dnf remove -y ghostlink
not_installed ghostlink
absent /usr/bin/ghostlink
absent /usr/bin/ghostlinkd
absent /usr/lib/ghostlink/ghostlink-tunnel
absent /usr/lib/systemd/system/ghostlinkd.service

step "mesh agent: install (iptables comes from the distribution), run, reinstall, remove"
transaction dnf install -y "$mesh"
installed ghostlink-mesh
present /usr/bin/ghostlink-mesh
present /usr/lib/systemd/system/ghostlink-mesh.service
command -v iptables > /dev/null || fail "the mesh agent's iptables dependency did not bring an iptables command"
runs ghostlink-mesh version
transaction rpm -U --replacepkgs --nosignature "$mesh"
runs ghostlink-mesh version
transaction dnf remove -y ghostlink-mesh
not_installed ghostlink-mesh
absent /usr/bin/ghostlink-mesh
absent /usr/lib/systemd/system/ghostlink-mesh.service

step "server: install without Docker (the post-install ends with guidance, not an error), run, reinstall, remove"
transaction dnf install -y "$server"
installed ghostlink-server
present /usr/sbin/ghostlink-server
present /usr/lib/ghostlink-server/image.tar.gz
present /usr/lib/ghostlink-server/ipsec-image.tar.gz
present /usr/lib/ghostlink-server/image.env
! command -v docker > /dev/null || fail "installing the server package brought Docker in"
if grep -q 'setup did not finish' "$log"; then
    echo "(dnf showed the post-install's guidance above)"
else
    echo "(this dnf does not show the post-install's output; it is read from the replay below)"
fi
runs ghostlink-server version
# The post-install script as installed, run as rpm does for a first install ($1 is 1): it must
# end with status 0 and say what to do. The wording is the script's; Docker's part is the server's.
rpm -q --qf '%{POSTIN}\n' ghostlink-server > /tmp/server-postin.sh
status=0
sh /tmp/server-postin.sh 1 > /tmp/server-postin.txt 2>&1 || status=$?
cat /tmp/server-postin.txt
[ "$status" = 0 ] || fail "the server's post-install ends with status $status without Docker"
grep -q 'setup did not finish' /tmp/server-postin.txt || fail "the post-install does not say that setup did not finish"
grep -q 'ghostlink-server setup' /tmp/server-postin.txt || fail "the post-install does not say how to run setup again"
grep -q 'Docker is not installed' /tmp/server-postin.txt || fail "the post-install does not say that Docker is missing"
usage=$(ghostlink-server help) || fail "ghostlink-server help does not run"
case "$usage" in *'usage: sudo ghostlink-server'*) ;; *) fail "ghostlink-server help does not print its usage" ;; esac
transaction rpm -U --replacepkgs --nosignature "$server"
runs ghostlink-server version
transaction dnf remove -y ghostlink-server
not_installed ghostlink-server
absent /usr/sbin/ghostlink-server
absent /usr/lib/ghostlink-server

step "all three in one transaction, and removed in one"
transaction dnf install -y "$client" "$mesh" "$server"
installed ghostlink
installed ghostlink-mesh
installed ghostlink-server
runs ghostlink --version
runs ghostlink-mesh version
runs ghostlink-server version
transaction dnf remove -y ghostlink ghostlink-mesh ghostlink-server
if rpm -qa 'ghostlink*' | grep .; then fail "a package is left after removing all three"; fi

step "information: what a plain 'docker' would install here (a dependency of the server package is decided on this)"
dnf install --assumeno docker 2>&1 | tail -n 30 || true

echo
echo "OK on $PRETTY_NAME"
