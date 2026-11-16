# Docker in the tab (containerd + v86)

A plan and a feasibility spike for running Docker in the Tabernetes tab, next to
Kubernetes. The goal is a real `docker` command in the terminal, with containers
running in v86. The spike proved containerd compiles to WebAssembly. The code
lives in `.build/containerd` (gitignored) and is not integrated yet.

## The idea

Do not reimplement Docker. Compile the real pieces to WebAssembly:

- **containerd** manages images, snapshots and tasks. It compiles to wasm
  (proven below).
- **A v86 runtime shim** replaces runc. Each containerd task runs in a v86 VM,
  not in a Linux process.
- **A thin Docker Engine API** over containerd gives `docker run`, `docker ps`,
  `docker logs`, `docker exec`. It is a small Go server that speaks the REST API
  of the `docker` CLI, over containerd's gRPC.

## Why not dockerd

dockerd is a thin layer over containerd, but its daemon is Linux networking
(iptables, netlink, conntrack), storage (overlayfs, btrfs) and the container
runtime (cgroups, seccomp, namespaces). All of that is `//go:build linux`, so on
wasm every such package is "build constraints exclude all Go files". Stubbing
them gives a dockerd whose core does nothing, which cannot map ports or isolate
processes. It is a much bigger port than containerd for a useless result. The
thin API over containerd gets the same user experience without that work.

## What the spike proved

`cmd/containerd` (the daemon, CRI plugin included) compiles to
`GOOS=js GOARCH=wasm`. The binary is about 68 MB. The patch surface is about
34 `_js.go` stubs and 31 build-tag edits, all of the same class that
`k8s/patch.go` already handles for Kubernetes. The scary parts (netlink, mount,
cgroups, seccomp) are already behind build tags.

The stubs cover:

- bbolt: mmap (read-only, so it is a file read), flock, mlock.
- device nodes, FIFOs, unix sockets and SCM_RIGHTS.
- signals, xattrs, user/group lookup, `exec`, wait4.
- mount, and the `defaults.*` constants (address, fifo dir, snapshotter, ...).

## Running the daemon on wasm

containerd's control sockets (grpc, ttrpc, debug) are unix sockets, which do not
exist in wasm. They map onto fixed ports of the in-process fake TCP network
(`defaults.MapAddress`): grpc `127.0.0.1:10000`, ttrpc `127.0.0.1:10001`, debug
`127.0.0.1:10002`. The daemon (`pkg/sys`) listens and the client (`pkg/dialer`)
dials the same ports, so containerd's client and daemon talk over `127.0.0.1`.

## The v86 shim (`core/runtime/v2/v86`)

This is the "runc replacement". It implements containerd's `TTRPCTaskService`
(17 methods) over a small `Runtime`/`Handle` interface:

- `Runtime.Create(ctx, id, spec)` boots a VM for an OCI spec, but does not run
  its entrypoint yet.
- `Handle.Start` runs the entrypoint, `Wait` returns the exit code, `Exec`
  starts another process, `Kill` stops the VM, `Delete` releases it.

The `Runtime`/`Handle` interface is the seam to the tab node's v86 VM
(`k8s/tabernetes/v86.go`). The cgroup-only operations (Pause, Resume, Checkpoint,
Stats, Update, Pids) are no-ops or `ErrNotImplemented`, because a VM has no
cgroups.

`jsruntime` is the js/wasm implementation of that seam:

- `Translate` turns an OCI spec into a `VMRequest` (rootfs, resolved argv, env,
  cwd, tty, memory limit, and the image ref from the annotation
  `io.containerd.image.name`).
- `PrepareRootfs` serializes a snapshot directory into a flat file list.
- `js.go` drives the VM worker over `syscall/js`, using the same `vm.start`
  protocol as `k8s/tabernetes/v86.go`.

Both packages have unit tests and compile for wasm.

## The thin Docker API (`core/runtime/v2/v86/dockerapi`)

A minimal Docker Engine API over containerd's client. It maps `docker run`,
`docker ps` and `docker rm` to containerd's gRPC, and it does not pull images
itself: it writes the image ref into the OCI spec annotations, where the v86
shim reads it and boots the VM. The streaming endpoints (attach, logs, exec)
and networking are not wired yet.

## The shim in-process (`core/runtime/v2/v86/inprocess`)

containerd's shim manager forks shim binaries; wasm has no fork. This package
starts the v86 task service over a ttrpc server on the in-process fake TCP
network and returns a `ShimInstance` that talks to it, the same way the fork
path talks to a forked shim over a unix socket. `core/runtime/v2` grew an
`InProcessShimLoader` hook: when set, `ShimManager.Start` and `loadShim` use
the in-process shim instead of forking. `v86/daemon` sets it to
`inprocess.New` with the js/wasm `Runtime`. The package has a test that runs Create/Start/State/Delete
through ttrpc against a fake runtime.

## Two paths for the rootfs

A task boots from one of two things:

1. **An image ref.** The shim reads it from `io.containerd.image.name` and the
   VM worker pulls it (download, unpack to OPFS, serve over 9p). This reuses all
   the existing image pull code in `web/src/layers.js` and `web/src/vmworker.js`.
   It is the simple path, and it sidesteps containerd's content store.
2. **A prepared snapshot.** The shim serializes containerd's snapshot with
   `PrepareRootfs`. This is fine for a small rootfs, but it loads every file into
   memory. A full image needs the snapshot served from OPFS layers instead.

`web/src/vmworker.js` grew a `startRootfs` that boots from the serialized rootfs
without an image pull.

## Status

`docker run` works in the browser since 2026-11-13. The state, what it took
and the remaining gaps are in [docker-in-wasm-status.md](docker-in-wasm-status.md).

## The build integration

The mechanical parts are now in the repository:

- `k8s/patch.go` grew `vendorContainerd`: it clones containerd at the pinned
  commit, applies `k8s/containerd-js.patch`, runs `go mod vendor`, and copies
  containerd plus its dependencies into the checkout's vendor tree.
- `mise.toml` gained `CONTAINERD_COMMIT`.
- `k8s/tabernetes/docker.go` is the glue: it sets `InProcessShimLoader`, creates
  the client, and serves the Docker Engine API. It also registers a `docker`
  builtin (in `coreutils.go`).

The first `docker` command starts containerd (`ensureDocker`); the daemon
itself is `core/runtime/v2/v86/daemon` in the patch.

## Migration plan (do not lose the work)

The containerd work is captured in `k8s/containerd-js.patch`, a unified diff
against containerd commit `d83dc3d5c93d6dbfe53115b1206bb1fbf90c4e4d`
(2026-11-12). It holds the `_js.go` stubs, the build-tag edits and the
`core/runtime/v2/v86/` shim, all in one file. To keep it:

1. `k8s/patch.go` clones containerd at that commit (like it vendors
   `mvdan.cc/sh`), then applies `k8s/containerd-js.patch` (for example with
   `git apply`).
2. `mise.toml` gains the containerd version, next to `KUBERNETES_VERSION`.
3. `k8s/tabernetes` imports the shim and provides the real `Runtime` over the
   existing `v86VM`.

The only other change already in this repository is `web/src/vmworker.js`
(`startRootfs`).

## Reproduce the spike

```bash
# Go 1.27.4 (or mise), then:
git clone https://github.com/containerd/containerd.git
cd containerd
git checkout d83dc3d5c93d6dbfe53115b1206bb1fbf90c4e4d
git apply /path/to/tabernetes/k8s/containerd-js.patch
GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -mod=vendor ./cmd/containerd
go test ./core/runtime/v2/v86/...
```
