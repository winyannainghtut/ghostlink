#!/usr/bin/env bash
# Upgrade test on a Linux host with Docker (CI runners), called by interop.sh before its own tests:
# the published 0.9.1 package, whose IPsec component exchanged its files with the server in the
# server account's own directory ($STATE/ipsec), is installed with IPsec on and a strongSwan client
# connected. Then the package under test is installed over it, as an upgrade is, and:
#   - the component and the server share /var/lib/ghostlink-ipsec (root's, in root's /var/lib, with
#     the server account's group), and nothing mounts or reads $STATE/ipsec any more;
#   - the old directory, with its copy of the clients' keys and hashes, is gone;
#   - the client that was connected connects again, and the server's DNS answers through the tunnel;
#   - a symlink the server account plants in the old place changes nothing, also across "ipsec
#     enable" and "setup", and what it points at is never touched.
# It ends with the package purged and the host clean, so that interop.sh starts from nothing.
# Usage: sudo tests/ipsec/upgrade.sh path/to/ghostlink-server_*.deb WORK-DIRECTORY
set -euo pipefail
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }
deb=$(realpath "$1")
work=$2
here=$(cd "$(dirname "$0")" && pwd)
state=/var/lib/ghostlink-server
exchange=/var/lib/ghostlink-ipsec
previous=0.9.1
step() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

cleanup() {
    status=$?
    set +e
    if [ $status -ne 0 ]; then
        echo "--- agent log"; docker logs --tail 120 ghostlink-ipsec 2>&1
        echo "--- client log"; docker logs --tail 120 glclient 2>&1
        echo "--- server log"; docker logs --tail 40 ghostlink-server 2>&1
        echo "--- status"; cat "$exchange/status.json" "$state/ipsec/status.json" 2>/dev/null
        echo "--- mounts"; docker inspect ghostlink-ipsec --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}' 2>&1
    fi
    docker rm -f glclient >/dev/null 2>&1
    exit $status
}
trap cleanup EXIT

step "Download the published $previous package (checked against its published checksum)"
arch=$(dpkg --print-architecture)
case "$arch" in
    amd64) sha256=e71c5d2b5930c11f09ce8a0e5db2fd301371c01a1c89e8deb8f48a747ee2970e ;;
    arm64) sha256=cdad63667190e224174cb1872e943c70fcaede762c0d325642a73da45f6b8502 ;;
    *) fail "there is no published $previous package for $arch" ;;
esac
name="ghostlink-server_${previous}-1_${arch}.deb"
mkdir "$work/previous"
old="$work/previous/$name"
# Release downloads sometimes answer 5xx for a while (503 for hours, once): curl's --retry treats
# those as transient, and a few tries more, spaced out, follow.
for attempt in 1 2 3 4 5; do
    curl -fLsS --retry 3 --retry-delay 15 -o "$old" "https://github.com/winyannainghtut/ghostlink/releases/download/v$previous/$name" && break
    [ "$attempt" = 5 ] && fail "could not download $name"
    sleep 15
done
printf '%s  %s\n' "$sha256" "$old" | sha256sum -c -

step "Install $previous, enable IPsec and connect a client (the old layout)"
GHOSTLINK_SERVER_HOSTNAME=10.99.0.1 apt-get install -y "$old" >/dev/null
test "$(ghostlink-server version)" = "$previous"
ghostlink-server ipsec enable >/dev/null
# The output goes to a file first: with "a | grep -q" under pipefail, grep may end first and the
# writer be killed by SIGPIPE, so that a pass would look like a failure.
ghostlink-server ipsec status >"$work/status.txt"
grep -q "running: true" "$work/status.txt"
# The starting point is really the old layout: the component mounts the state's ipsec directory.
test -d "$state/ipsec"
test ! -e "$exchange"
mounts() { docker inspect "$1" --format '{{range .Mounts}}{{.Source}}{{"\n"}}{{end}}' | grep . | sort | tr '\n' ' '; }
test "$(mounts ghostlink-ipsec)" = "$state/ipsec "
old_image=$(sed -n 's/^IPSEC_IMAGE=//p' /usr/lib/ghostlink-server/image.env)

# The management key names the server by the address the client reaches it at (10.99.0.1), so that
# address must exist before the first call to the API (the client's end is moved into its
# container below, once the container runs).
ip link add glh0 type veth peer name glc0
ip addr add 10.99.0.1/24 dev glh0 && ip link set glh0 up
key=$(ghostlink-server show-key | grep '^{')
url=$(echo "$key" | jq -r .apiUrl)
api() { # METHOD PATH [JSON]
    curl -sk --fail-with-body --connect-timeout 5 --max-time 90 -X "$1" ${3:+-d "$3"} "$url$2"
}
api PATCH /server '{"protocols":{"ikev2":{"enabled":true}}}' | jq -e '.protocols.ikev2.enabled and .ipsec_component.running' >/dev/null
api PUT /rules '{"blocked_domains":["blocked.example"]}' >/dev/null
user=$(api POST /users '{"name":"Legacy"}' | jq -r .id)
key_id=$(api POST "/users/$user/keys" '{"protocol":"ikev2","auth":"eap"}' | jq -r .id)
curl -sk --fail --max-time 60 -o "$work/legacy.mobileconfig" "$url/users/$user/keys/$key_id/profile"
python3 "$here/client_config.py" "$work/legacy-client" legacy="$work/legacy.mobileconfig"
chmod -R a+rX "$work/legacy-client"
# The old component keeps the old copy of the settings, in the state, and reports from there.
jq -e '.allowed | length == 1' "$state/ipsec/desired.json" >/dev/null

step "A client connects (network: the client at 10.99.0.2, the server at 10.99.0.1)"
# The client runs the old release's own image (the new one is not loaded before the upgrade).
docker run --detach --name glclient --network none --cap-drop ALL --cap-add NET_ADMIN --cap-add NET_RAW \
    --cap-add NET_BIND_SERVICE --read-only --tmpfs /run --volume "$work/legacy-client:/client:ro" \
    --env STRONGSWAN_CONF=/client/strongswan.conf --env SWANCTL_DIR=/client/swanctl \
    --entrypoint /usr/lib/strongswan/charon "$old_image" >/dev/null
pid=$(docker inspect --format '{{.State.Pid}}' glclient)
in_client() { nsenter --target "$pid" --net "$@"; }
ip link set glc0 netns "$pid"
in_client ip addr add 10.99.0.2/24 dev glc0
in_client ip link set glc0 up
in_client ip link set lo up
in_client ip route add default via 10.99.0.1
for i in $(seq 1 20); do docker exec glclient swanctl --stats >/dev/null 2>&1 && break; sleep 0.5; done
docker exec glclient swanctl --load-all --noprompt
swan() { docker exec glclient swanctl "$@"; }
connect() {
    swan --initiate --child legacy --timeout 40 >"$work/initiate.log" 2>&1 || { cat "$work/initiate.log"; fail "the client did not connect ($1)"; }
    swan --list-sas >"$work/sas.txt"
    grep -q ESTABLISHED "$work/sas.txt" || fail "no established session ($1)"
}
# The tunnel carries DNS to the server's DNS, which blocks the name (the engine takes a moment).
dns_through_tunnel() { # WHEN
    for i in $(seq 1 40); do
        [ "$(in_client python3 "$here/dnsquery.py" 10.79.0.1 www.blocked.example)" = NXDOMAIN ] && return 0
        sleep 1
    done
    fail "the server's DNS does not answer through the tunnel ($1)"
}
reconnect() { # WHEN: the old session may be gone (the component was recreated) or half there
    swan --terminate --ike legacy --timeout 10 >/dev/null 2>&1 || true
    connect "$1"
    dns_through_tunnel "$1"
}
connect "on $previous"
dns_through_tunnel "on $previous"

step "Upgrade to the package under test"
GHOSTLINK_SERVER_HOSTNAME=10.99.0.1 apt-get install -y "$deb" >/dev/null
test "$(ghostlink-server version)" != "$previous"
test "$(stat -c '%U:%G %a' "$exchange")" = 'root:ghostlink-server 2770'
test ! -L "$exchange"
test "$(mounts ghostlink-ipsec)" = "$exchange "
test "$(mounts ghostlink-server)" = "$exchange $state "
args=$(docker inspect ghostlink-server --format '{{join .Args " "}}')
[[ "$args" == *"--ipsec-dir $exchange"* ]] || fail "the server was not told the exchange directory: $args"
args=$(docker inspect ghostlink-ipsec --format '{{join .Args " "}}')
[[ "$args" == *"--dir $exchange "* ]] || fail "the component was not told the exchange directory: $args"
# The old directory (a copy of the clients' keys and hashes, also of removed ones) is taken away.
test ! -e "$state/ipsec"
ghostlink-server ipsec status >"$work/status.txt"
cat "$work/status.txt"
grep -q "running: true" "$work/status.txt"
# The new server wrote the settings in the new place (it did not copy the old file) and the
# component reports there: the user's identity is allowed, and the component applied that very file.
jq -e '.allowed | length == 1' "$exchange/desired.json" >/dev/null
for i in $(seq 1 30); do
    [ "$(jq .generation "$exchange/desired.json")" = "$(jq .applied_generation "$exchange/status.json" 2>/dev/null || echo 0)" ] && break
    sleep 1
done
test "$(jq .generation "$exchange/desired.json")" = "$(jq .applied_generation "$exchange/status.json")" || fail "the component did not apply the settings in the new directory"
# A change reaches the component through the new directory: a second user's key is allowed.
for i in $(seq 1 30); do curl -sk --fail --max-time 5 "$url/server" >/dev/null && break; sleep 1; done
before=$(jq .generation "$exchange/desired.json")
other=$(api POST /users '{"name":"Second"}' | jq -r .id)
api POST "/users/$other/keys" '{"protocol":"ikev2","auth":"eap"}' >/dev/null
for i in $(seq 1 30); do
    [ "$(jq -r '.allowed | length' "$exchange/desired.json")" = 2 ] && [ "$(jq .applied_generation "$exchange/status.json")" = "$(jq .generation "$exchange/desired.json")" ] && break
    sleep 1
done
[ "$(jq .generation "$exchange/desired.json")" != "$before" ] || fail "a new key did not reach the settings in the new directory"
api DELETE "/users/$other" >/dev/null
# The client that was connected connects again.
reconnect "after the upgrade"

step "A symlink in the old place changes nothing"
# The server account owns the state and can plant a symlink where 0.9.1 and earlier exchanged the
# files. It points at a directory of root's; nothing may enter it, mount it or change it.
decoy="$work/decoy"
mkdir -m 0755 "$decoy"
sudo -u ghostlink-server ln -sT "$decoy" "$state/ipsec"
sudo -u ghostlink-server test -L "$state/ipsec"
ghostlink-server ipsec enable >/dev/null
test "$(mounts ghostlink-ipsec)" = "$exchange "
reconnect "after ipsec enable with a symlink in the old place"
ghostlink-server ipsec status >"$work/status.txt"
grep -q "running: true" "$work/status.txt"
test "$(stat -c '%U %a' "$decoy")" = 'root 755'
test -z "$(ls -A "$decoy")"
# setup (as every upgrade runs it) unlinks the symlink without following it.
GHOSTLINK_SERVER_HOSTNAME=10.99.0.1 ghostlink-server setup >/dev/null
test ! -L "$state/ipsec"
test ! -e "$state/ipsec"
test "$(stat -c '%U %a' "$decoy")" = 'root 755'
test -z "$(ls -A "$decoy")"
test "$(mounts ghostlink-ipsec)" = "$exchange "
reconnect "after setup"
rmdir "$decoy"

step "The server account cannot replace the exchange directory"
# Its parent, /var/lib, is root's: the account can write into the directory (its settings), but
# cannot rename, remove or replace it, so the path Docker mounts stays that directory.
if sudo -u ghostlink-server mv "$exchange" "$exchange.moved" 2>"$work/denied.txt"; then fail "the server account renamed the exchange directory"; fi
grep -q 'Permission denied' "$work/denied.txt"
if sudo -u ghostlink-server rmdir "$exchange" 2>"$work/denied.txt"; then fail "the server account removed the exchange directory"; fi
if sudo -u ghostlink-server ln -sfT /etc "$exchange" 2>"$work/denied.txt"; then fail "the server account replaced the exchange directory"; fi
test -d "$exchange"
test ! -L "$exchange"
test "$(stat -c '%U:%G %a' "$exchange")" = 'root:ghostlink-server 2770'

step "Purge: nothing is left"
docker rm -f glclient >/dev/null
apt-get purge -y ghostlink-server >/dev/null
test ! -e "$state"
test ! -e "$exchange"
if docker ps -a --format '{{.Names}}' | grep -Eqx 'ghostlink-(server|ipsec)'; then fail "a container was left after the purge"; fi
if [ -n "$(docker image ls --quiet ghostlink-ipsec)" ]; then fail "an IPsec image was left after the purge"; fi
test ! -e /etc/sysctl.d/90-ghostlink-ipsec.conf
# The client's namespace is destroyed with its container, and its veth with it (a moment later);
# interop.sh makes the same names again.
for i in $(seq 1 20); do
    ip link show glh0 >/dev/null 2>&1 || break
    ip link del glh0 2>/dev/null || true
    sleep 0.5
done
if ip link show glh0 >/dev/null 2>&1; then fail "the test network is still there"; fi
step "Upgrade from $previous passed"
