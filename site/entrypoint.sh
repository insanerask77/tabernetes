#!/bin/sh
# Substitute data-registry into index.html at container start, so the image
# does not need a rebuild to point at a different Docker Hub mirror (a
# registry that sends CORS headers). See AGENTS.md, "Hosting and network
# constraints": Docker Hub sends no CORS, so the browser pulls through a
# pull-through mirror.
set -e

REGISTRY_URL="${REGISTRY:-http://localhost:5050}"
# NODE_IMAGE is the node image in the "docker run" that join prints. Empty
# means the image built into tabernetes.js: ghcr.io/insanerask77/tabernetes
# with the version of the release, so the page and the node always match.
NODE_IMAGE="${NODE_IMAGE-}"
# RELAY is the ntfy server for join --keep and join --kubectl. Empty means
# the default of the page (https://ntfy.sh); the messages are end to end
# encrypted.
RELAY="${RELAY-}"
sed -i \
  -e "s|__TABERNETES_REGISTRY__|${REGISTRY_URL}|g" \
  -e "s|__TABERNETES_NODE_IMAGE__|${NODE_IMAGE}|g" \
  -e "s|__TABERNETES_RELAY__|${RELAY}|g" \
  /usr/share/nginx/html/index.html

exec /docker-entrypoint.sh "$@"
