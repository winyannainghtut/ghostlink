#!/bin/sh
# Ghostlink server pre-remove (deb "remove"/"upgrade", rpm %preun). On removal, stop and
# delete the container and image (a stopped Docker is started for it, because containers that
# restart unless stopped would otherwise start again with Docker, without their state; when it
# cannot be started, "ghostlink-server remove" says which containers are left); on upgrade, keep
# it running until the new version starts.
set -e
case "${1:-}" in
    remove | purge | 0) /usr/sbin/ghostlink-server remove || true ;;
esac
exit 0
