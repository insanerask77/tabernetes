# Serves the Tabernetes web app (the Kubernetes control plane that runs in a
# browser tab) as a static site.
#
# Build dist/ first (see README.md, "Build"), then:
#   docker build -f site/Dockerfile -t tabernetes-site .
#   docker run --rm -p 8080:80 -e REGISTRY=http://localhost:5000 tabernetes-site
#
# REGISTRY is the pull-through mirror of Docker Hub that the tab node uses to
# run container images in the browser. It is set at RUNTIME (env var), not at
# build time: the entrypoint substitutes it into index.html on start. Docker
# Hub sends no CORS headers, so the mirror is a registry:2 pull-through
# under the same origin (site/nginx.conf sends /v2/ to it, docker-compose.yml).
FROM nginx:1.27-alpine

# The static assets of the terminal (mise run build).
COPY dist/ /usr/share/nginx/html/

# The page that embeds the terminal. data-size is baked (it is the byte size
# of the .zst binary); data-registry stays a placeholder for the entrypoint.
COPY site/index.html /tmp/index.html

# proxy-sw.js must be served at /tabernetes-proxy/sw.js, the scope of the
# "proxy <port>" service worker.
RUN mkdir -p /usr/share/nginx/html/tabernetes-proxy \
 && mv /usr/share/nginx/html/proxy-sw.js /usr/share/nginx/html/tabernetes-proxy/sw.js \
 && size=$(wc -c < /usr/share/nginx/html/tabernetes.wasm.zst | tr -d ' ') \
 && sed -i \
      -e "s/__TABERNETES_WASM_SIZE__/${size}/g" \
      -e "s/__TABERNETES_WASM_MB__/$(( (size + 524288) / 1048576 ))/g" \
      /tmp/index.html \
 && mv /tmp/index.html /usr/share/nginx/html/index.html \
 # nginx runs as the user nginx: everything must be readable by it (dist/vm
 # can come out of "docker build --output" as 0700, and then /vm/* is a 404).
 && chmod -R a+rX /usr/share/nginx/html

COPY site/nginx.conf /etc/nginx/conf.d/default.conf
COPY site/entrypoint.sh /usr/local/bin/tabernetes-entrypoint.sh
RUN chmod +x /usr/local/bin/tabernetes-entrypoint.sh

EXPOSE 80
ENTRYPOINT ["/usr/local/bin/tabernetes-entrypoint.sh"]
CMD ["nginx", "-g", "daemon off;"]
