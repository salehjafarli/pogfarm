#!/bin/bash
# Health check of the pogfarm image: exits 0 when GET /healthz on the local
# job API answers 200, and 1 otherwise.
#
# It uses bash's /dev/tcp so the image needs no curl or wget. The port is
# taken from POGFARM_LISTEN_ADDR (":8080", "0.0.0.0:8080" and "[::]:8080" all
# give 8080), or from POGFARM_HEALTHCHECK_PORT when that is set. pogfarm must
# listen on all interfaces or on 127.0.0.1 for the check to reach it.
set -eu

addr="${POGFARM_LISTEN_ADDR:-:8080}"
port="${POGFARM_HEALTHCHECK_PORT:-${addr##*:}}"

if ! exec 3<>"/dev/tcp/127.0.0.1/${port}"; then
	echo "unhealthy: cannot connect to 127.0.0.1:${port}" >&2
	exit 1
fi
printf 'GET /healthz HTTP/1.0\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n' >&3

status=""
IFS= read -r -t 4 status <&3 || true
exec 3<&-

case "$status" in
"HTTP/1."?" 200 "*) exit 0 ;;
esac
echo "unhealthy: ${status:-no response}" >&2
exit 1
