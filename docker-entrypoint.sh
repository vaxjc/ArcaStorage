#!/bin/sh
set -e
if [ "$(id -u)" = "0" ]; then
  chown -R 65532:65532 /data
  exec su-exec 65532:65532 arca "$@"
fi
exec arca "$@"
