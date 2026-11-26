#!/bin/sh
# Ghostlink server post-remove (deb "remove"/"purge", rpm %postun). Only a Debian purge deletes
# the server's users, keys, rules and management key, its system account, and the directory it
# shared with the IPsec component (root's, outside the state: only settings derived from the state).
set -e
if [ "${1:-}" = "purge" ]; then
    rm -rf /var/lib/ghostlink-server /var/lib/ghostlink-ipsec
    if id -u ghostlink-server >/dev/null 2>&1; then
        userdel ghostlink-server >/dev/null 2>&1 || true
    fi
fi
exit 0
