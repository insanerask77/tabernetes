# Tabernetes web app as a static site

This directory turns the `dist/` build output (the browser terminal with a
Kubernetes control plane) into a small Docker image served by nginx, ready to
host on a subdomain.

## Build

The assets are not in this repository; build them first with mise:

```bash
mise run build      # writes dist/ (the Wasm binary, the terminal, the workers, the VM)
```

Then build the site image (the build context is the repository root):

```bash
docker build -f site/Dockerfile -t tabernetes-site .
```

`dist/` is the only part of the build context that the image takes, plus this
`site/` directory. A `.dockerignore` keeps the Kubernetes checkout, the node
image sources and the git history out of the context.

## Run

```bash
docker run --rm -p 8080:80 tabernetes-site
```

Open http://localhost:8080 and click **Boot the cluster**. The Wasm binary is
~290 MB (28 MB zstd), so the first download takes a moment; the browser caches
it.

## Host on a subdomain

The terminal, the shared worker and the service worker all need a *secure
context* (HTTPS or localhost). `http://localhost` works for a local test; a
public subdomain must be behind TLS. See `deploy.yaml` for a minimal
Deployment + Service + Ingress.

## Things to know before you rely on it

- **Container images in the browser need a registry mirror.** Docker Hub sends
  no CORS headers, so `kubectl run nginx --image=nginx` on a *tab node* pulls
  through a pull-through mirror. `data-registry` defaults to the reference
  mirror; run your own (`registry:2` with `REGISTRY_PROXY_REMOTEURL` plus CORS
  headers, see `docker-compose.yml`) and set the environment variable
  `REGISTRY=https://your-mirror` when the container starts. The
  `join` command and the `llm` feature (Hugging Face sends CORS) work without it.
- **The binary is cached in the browser by URL.** The page stores it in Cache
  Storage keyed by the file URL and never revalidates. When you rebuild and
  redeploy, either keep the old URL (it stays stale) or change the file name
  and the `data-wasm` attribute so browsers fetch the new one.
- **No `Content-Encoding` for the `.zst`.** The page decompresses the zstd
  frames itself. nginx serves the file raw (see `nginx.conf`), so the download
  progress stays correct.
- **CSP**: nginx here sends none, so Wasm, workers and WebRTC all work. If you
  add one, it needs `script-src 'self' 'wasm-unsafe-eval'` and
  `worker-src 'self'`.
