# Docker-in-wasm — resume status

Where the work stopped and how to pick it back up. The design and the full
picture are in [docker-in-wasm.md](docker-in-wasm.md); this file is only the
current state and the next step.

## Environment (already set up)

- `mise` and `go 1.27.4` are installed.
- Kubernetes v1.37.2 is cloned at `.build/kubernetes` (gitignored).
- containerd is cloned at `.build/containerd` (gitignored), with the js/wasm
  port applied and committed as a diff in `k8s/containerd-js.patch`.

## What is done and verified

- **containerd → wasm**: `GOOS=js GOARCH=wasm go build ./cmd/containerd` works
  (in `.build/containerd`).
- **containerd trimmed** (the "option 3" approach) so it compiles against
  Kubernetes' older dependency versions. The newer-API uses were removed or
  inlined (all in `k8s/containerd-js.patch`):
  - `core/unpack/unpacker.go`: `identity.ChainIDs` → local `computeChainIDs`.
  - `client/client.go`: removed the `features.Features` typeurl registration.
  - `core/runtime/v2/shim.go` + `plugins/server/ttrpc/server_linux.go`:
    removed the `otelttrpc` interceptors (no `otel/semconv` dependency).
  - `core/runtime/v2/logging/logging_js.go`: stub for the logging binary.
  - `core/runtime/v2/v86/`: the v86 shim (`TTRPCTaskService`), `jsruntime`
    (`Translate` + `PrepareRootfs`), `inprocess` (in-process `ShimInstance`),
    `dockerapi` (thin Docker Engine API). Tests pass.
- **`k8s/tabernetes/docker.go` + `dockercli.go` + `coreutils.go`**: the
  `docker` shell builtin (pure-stdlib HTTP client to the `dockerapi`).
- **`web/src/vmworker.js`**: `startRootfs` (boot a VM from a prepared rootfs).

## The vendoring works (2026-11-13)

`mise run patch` vendors containerd into the Kubernetes checkout, and both
`GOOS=js GOARCH=wasm go build ./cmd/tabernetes` and the full `k8s/build.sh`
(heap snapshot included: init 215 ms, Go holds only the 8 known values) pass.
(The size at this step, 28.5 MiB zstd, was misleading: nothing called the
daemon yet, so the linker dropped most of containerd. See below for the size
with the daemon wired.)

What was wrong, and the rules `k8s/patch.go` now follows (tests in
`k8s/patch_test.go`):

- **modules.txt has no blank lines between blocks.** `removeModuleBlock`
  removed everything from the target block to the end of the file. A block
  now ends at the next `# ` header (`moduleBlock`), and `setModule` inserts
  new blocks in sort order.
- **The `go X` annotation must stay.** Without `## explicit; go X` the
  compiler builds a vendored module as go1.16 (no generics). `parseModules`
  keeps it, and the containerd/v2 block takes the go version of containerd's
  go.mod.
- **Shared modules miss packages.** Kubernetes vendors only the packages it
  uses. `addPackages` adds the ones containerd lists, from the module cache at
  the checkout's version. Packages that version does not have are left out
  and logged (all Windows, test or otel semconv packages, unused on js/wasm).
- **The patch edits shared modules too.** The bbolt files of
  `containerd-js.patch` are copied over the checkout's bbolt (same v1.5.0;
  the patcher fails if the versions ever differ).
- **logrus goes to v1.10.2.** containerd/log v0.2.0 needs `hooks/slog`, and
  containerd v2 needs log v0.2.0 (`SetLevel(Level)`). v0.1.0 does not compile.
- **Workspace requires.** In workspace mode `go build` checks the requires of
  every go.mod in go.work, so an upgraded module is also set in the staging
  go.mod files (`setWorkspaceRequires`; apiserver and
  apiextensions-apiserver require logrus).
- An overwritten module first loses the .go files of its old version.

## docker run works in the browser (2026-11-13)

The first `docker` command in the terminal starts containerd in the shared
worker (`ensureDocker` in `k8s/tabernetes/docker.go`), and every command
makes its tab the one that runs new containers. Tested in headless Brave
against a local bundle: `docker run nginx` (about 25 s with the pull and the
boot), `docker ps`, `docker logs` (the real nginx output), `docker stop`,
`docker rm` (refused while running, like Docker), ids by unique prefix,
`docker run alpine echo hello` (Cmd replaces the CMD of the image), and a
Kubernetes pod running next to a Docker container in the same tab.

Binary: 29.3 MiB zstd, against 27.8 MiB for v1.9.0 (+1.5 MiB). Package init
in the heap snapshot: ~218 ms, against 215 ms.

What it took, in `k8s/containerd-js.patch` unless noted:

- **`core/runtime/v2/v86/daemon`**: starts containerd in-process with a
  subset of the plugins of `cmd/containerd/builtins` (no CRI, NRI, erofs,
  blockfile, restart, debug or metrics). `daemon_test.go` runs the whole
  path on js/wasm in Node.js: daemon, grpc, tasks service, in-process shim,
  runtime, and the Docker API.
- **New shims go in-process too.** `InProcessShimLoader` only covered the
  reload of old shims; `ShimManager.Start` still forked. Both now use
  `startInProcessShim`, which also keeps the delete result and deletes the
  bundle, like containerd's own shim type.
- **bbolt on js**: the emulated mmap is a copy of the file, so every write
  now goes to the copy too (`bolt_js.go`). Before, the second transaction
  read stale pages ("misplaced bucket header").
- **The in-process shim outlived its request** only on paper: it served ttrpc
  with the context of the create request, so Wait got "context canceled".
- **`Connect`** answers PID 1 (containerd asks for the task pid after Create).
- **Unmount on js is a no-op**, else a bundle could never be deleted.
- **No `TempDir` in the config**: containerd would set `TMPDIR` for the whole
  process, kubectl included.
- **Docker semantics in the API**: the spec holds only what the request sets
  (Cmd, Env, WorkingDir, Tty), so the VM worker uses the ENTRYPOINT, CMD, env
  and working dir of the image. Before, an empty Cmd became `/bin/sh`.
- **Network**: each VM gets 172.17.0.x, gateway 172.17.0.1, hostname = short
  id, like Docker's default bridge (each VM has its own virtual network).
- **Logs**: the runtime writes the console to `/var/log/v86/<id>.log`
  (`v86.LogPath`); `GET /containers/{id}/logs` sends it as a multiplexed
  stream, and `docker logs` in `dockercli.go` reads it.
- **Exit codes**: an exit is recorded once (State no longer eats it), and a
  kill gives 128 + signal (143 for `docker stop`).
- `k8s/patch.go` also vendors `cmd/containerd/server`.
- `.git/info/exclude` of `.build/containerd` had `containerd`, which hid
  every new file under any `containerd/` directory from `git add -A`. It is
  `/containerd` now. Check the file count of the patch after regenerating it.

## docker run -it and docker exec (2026-11-13)

Tested in headless Brave by typing in the terminal, as a user would (calling
`tabernetesTerminal.exec` does not route the keys to a running command):

- `docker run -it alpine sh`: the prompt shows at once, `tty` is
  `/dev/pts/0`, `stty size` is the size of the browser terminal, and
  `exit 3` gives `$?` 3. Ctrl-P Ctrl-Q detaches and the container runs on.
- `docker run` without `-d` stays attached and returns the exit code, also
  for a container that ends at once (`docker run alpine echo hi`).
  `--rm`, `--name`, `-e` and `-i` work; `docker run -i alpine cat` ends
  with Ctrl-D (StdinOnce).
- `docker exec`: stdout and stderr apart, the exit code, 126 and Docker's
  message for a missing command, `-it` with a shell, `-i` with a pipe
  (`echo x | docker exec -i web wc -c`) and `-i` in line mode on the
  terminal.
- `kubectl exec` and `kubectl exec -it` of pods still work.

How it works:

- **Output of attach from the console.** The VM starts the process at create
  time, so an attach always comes after start. `jsruntime.Runtime.Follow`
  reads the log and adds a follower under the lock that console output is
  written under, so nothing is lost or written twice (Docker's
  `attach?logs=1`). Stdin and the tty size go over the attach of init; its
  copy of the output is dropped.
- **Exec** goes over the exec protocol of init with the client of
  `kubectl exec` (`streamVM`, `dialVM`, taken out of `v86VM`), not over
  containerd's exec: containerd does not know these processes.
- **`v86.Streamer`** is the interface between the Docker API and that glue
  (`dockerStreams` in `docker.go`). The API (`dockerapi/stream.go`) serves
  attach, resize, exec, exec start/resize/json with the hijacked connection
  of the Docker API: raw with a tty, multiplexed without.
- `/etc/hostname` is the hostname now (`vmworker.js`), for pods too; it was
  the file of the image.

Bugs found on the way:

- **The end of stdin canceled the exec.** After `Hijack`, Go's HTTP server
  still reads through its buffered reader and cancels the request context
  when it reads EOF, which is when the client half-closes for the end of
  stdin. `hijack` now reads the buffered bytes and then the connection, and
  the streams use a context detached from the request. `TestDockerStreams`
  covers it (checked to fail with the old code).
- In line mode, the output of `exec -i` waited until the end: the shell
  holds back its stdout while the terminal is in raw mode. On the terminal
  the CLI now writes to the terminal, as `kubectl exec -i` does.

## The remaining gaps, closed in parallel (2026-11-13)

Four agents worked on four copies of the workspace at once (copy-on-write
clones, each with its own build, test server and browser), and their patches
were merged on one copy with `git apply -3`. Merging needed real decisions,
listed at the end. The merged result was tested again in headless Brave as a
whole: `run -d -p`, `ps`, `port`, `kill -s HUP`, `exec -i` with a pipe,
`run` with the pull progress, `stop -t 2` of a container that ignores
SIGTERM (2.02 s, exit 137), `ps -a`, `start` with its port, `images`,
`run -it`, `start -ai`, a reload (containers kept, logs kept) and `rm -f`.

### docker run -p

`docker run -p 8080:80 nginx` publishes a container port like Docker. The
Docker API keeps the bindings in the container label
`dev.tabernetes.v86.ports` (a JSON array of `{HostIp, HostPort,
ContainerPort, Protocol}`, see `v86.PortBinding`) and opens them while the
task runs: `v86.Ports` works like docker-proxy. It listens on
127.0.0.1:<host port> of the in-process network (never 0.0.0.0) and copies
each connection to the port of the VM through its network card (`dialVM`).
The ports open before the VM boots, so a taken port fails at once with
dockerd's message ("Bind for 0.0.0.0:8080 failed: port is already
allocated", or "Error starting userland proxy" when something else, such as
kubectl port-forward, listens there). They close when the container stops,
exits on its own or is removed, but not on `docker kill -s HUP`. `-p 80`
picks a free port from 32768, like Docker. Published ports show as links
under the terminal, named after the container (`web:80`), like those of
kubectl port-forward, so `proxy 8080`, a click on the link and
`wget localhost:8080` all reach nginx. `docker port` prints the mappings,
and inspect has `NetworkSettings.Ports`. UDP cannot be published: the
network of the VM carries TCP connections only.

### Graceful stop

`docker stop` sends the stop signal to the main process in the VM: `-s`,
else `--stop-signal` of the container, else the STOPSIGNAL of the image,
else SIGTERM. It waits up to `-t` seconds (default 10), then sends SIGKILL.
The exit code is the real one: `sleep` ends with 143, nginx with 0 (its
image stops it with SIGQUIT). `docker kill -s SIGNAL` only sends the signal,
so `docker kill -s HUP` reloads nginx and the container keeps running.
Plain `docker kill` is SIGKILL. `docker rm -f` kills, then removes.
`docker restart` uses the same stop. A deleted pod on the tab node is
stopped the same way, as the kubelet does: event `Killing` "Stopping
container X", the stop signal, up to the grace period (at least 2 s), then
SIGKILL. Init gets signals as the request `{"signal": N}` on its exec port.
SIGKILL ends every process in the VM but init, like the end of a PID
namespace. On js/wasm, `syscall.SIGKILL` is 3 and `syscall.SIGTERM` is 6:
the old code sent those numbers to the VM and faked the exit codes.

### Everyday docker commands

`docker ps` (`-a`, `-q`, `-l`, `-n`, `--no-trunc`, `--format`), `inspect`
(`-f`), `create`, `start` (`-a`, `-i`), `restart` (`-t`), `attach`,
`images`, `pull`, `rmi` (`-f`), `port`, `docker container …`,
`docker image …` and `docker help` work like Docker, with its texts, flags
and tables (taken from docker/cli v28 and dockerd). A container id is 64
random hex digits and the containerd id. A container without `--name` gets
a name like Docker's (`kind_mahavira`), from `crypto/rand`, because the heap
snapshot would repeat `math/rand`. Commands find a container by id, name or
unique id prefix. The VM of a task does not last, so labels on the
containerd container keep the name, the config of the image and the state
of the last run (start, end, exit code). When a task exits, the API writes
these labels and deletes the task, which frees the VM; `docker stop` and
`kill` wait for that, so the next `docker ps` shows the container exited.
`docker start` of an exited container boots a new VM for the same
container. `--rm` is done by the daemon, so `docker run -d --rm` works too.
Images stay in the image store of the browser (`layers.js`), which the pods
share, so `docker images` also lists the images of pods. `vm.js` pulls,
lists and removes images in a worker without a VM. `docker run` of a
missing image prints "Unable to find image … locally" and the pull progress
of each layer, as Docker does.

### Containers survive the page

containerd's data (`/var/lib/containerd`), its state (`/run/containerd`) and
the container logs (`/var/log/v86`) are kept in IndexedDB like `/root`
(`keptPaths` in `web/src/fs.js`). They load before Go runs, so containerd
always starts on them. A file over 64 KiB is saved in 64 KiB chunks, and a
save writes only the chunks that changed: a bbolt commit writes its meta
page and a few pages, a log grows at its end. The shim keeps
`v86-state.json` in the bundle of each task, and after a reload containerd
loads the in-process shims of the old bundles (`loadInProcessShim`). A
container that still ran shows Exited (255), containerd's code for a dead
shim; one whose tab closed shows 137 (`dockerTabGone` in `session.close`),
and the next container cannot go to the dead tab. A tab runs at most 8 VMs,
pods and containers together: `docker run` fails with the kubelet's message
(`Node didn't have enough resource: pods, ...`), the node shows allocatable
pods minus the running containers, and the tab kubelet rejects a pod with
`OutOfpods` when the tab is full. `forget`, `poweroff` and `reboot` wipe it
all with the rest of the database.

### What the merge decided

- `docker kill` closes the published ports only after SIGKILL: B made
  other signals leave the container running, and A had closed the ports on
  every kill.
- The ports of a container are opened in C's `startContainer` (before
  `NewTask`) and closed in C's watcher when the run ends, instead of A's
  own wait on the task.
- B's `stop` and `kill` wait for C's watcher (`waitRun`) and take a task
  that is already gone as stopped (C deletes the task when it ends).
- C's random ids broke A's assumption that the id is the name: the port
  links and dockerd's "endpoint" message now get the name from the label
  (`PortPublisher.Publish(id, name, ports)`), and a port error is not
  prefixed with "failed to create task" (dockerd sets up the network
  before the task).
- One `HostConfig` type for the CLI (A's `dockerHostConfig`, with C's
  `AutoRemove`) and one port binding type (`v86.PortBinding`).
- `restart` stops with B's stop (the image's signal, `?t=`) and starts with
  C's new VM.

## The everyday gaps, closed in parallel (2026-11-16)

Four agents worked at the same time, each in its own clone, and the results
were merged in the order CMDS, FLAGS, OPS, VOLS. Each tested its part in the
browser; the merged result passed one integrated browser test (volumes,
bind mounts, `docker cp`, run flags, restart policies, pause, top, stats,
rename, wait, filters, logs, events, tag, a reload) and a regression test
(a pod with port-forward, an `emptyDir` pod, `stop -t 2` = 137 in 2.02 s,
`stop` = 143, `run -it` with Ctrl-P Ctrl-Q, `attach`, `exit 7`, `start -ai`).

### docker run options

`-w`, `-u`, `-h`, `--entrypoint` (`""` resets it), `-l`, `--label-file`,
`--env-file` (Docker's format: comments, a bare `KEY` takes the value of the
shell), `-m`, `-P`, `--restart`, `--init`, `-a`, `--stop-signal`,
`--stop-timeout`, `--detach-keys`, `--sig-proxy`, `--log-driver`,
`--log-opt`, `-v` and `--mount` work on `docker run` (and on `docker create`
without the attach options), with the parser and the messages of the docker
CLI (`dockerrunflags.go`). The user and the entrypoint go to the VM as the
annotations `dev.tabernetes.v86.user` and `dev.tabernetes.v86.entrypoint`.
As in dockerd, an entrypoint drops the CMD of the image too. A missing
working directory is created like runc does. `-m` is the RAM of the VM: from
128 MiB (what a VM needs to boot) to 2 GiB (the limit of v86). `-P`
publishes every exposed TCP port of the image on a free host port. `--init`
does nothing: init of the VM already acts like tini.

### Restart policies

`no`, `on-failure[:N]`, `always` and `unless-stopped` follow dockerd's
restart manager: 100 ms before the first restart, doubled up to a minute,
back to 100 ms after a run of 10 s. While a container waits it shows
"Restarting (N) X ago". `docker stop` ends the restarts until the next
start by the user. The policy, the count and "stopped by the user" are
labels; the backoff lives in memory. If a tab reloads, its VMs die (137) and
the policy restarts them in another tab, or in the next tab that opens. If
the control plane starts fresh, a marker file in `/var/lib/containerd` makes
the first tab start containerd at once, and containers start again as after
a restart of dockerd.

### More commands

`docker wait`, `rename`, `top`, `pause`, `unpause`, `tag`, `container
prune`, `image prune`, `system df`, `system prune`, `cp`, `stats`, `events`
and `volume` work like Docker, with its texts, prompts and outputs.
`docker pause` stops the emulator of the container's VM: every process in
it stands still, like under the cgroup freezer. `docker top` asks init for
the processes (`{"ps": true}`), because many images have no `ps`.
`docker tag` adds a second reference to the same manifest in the image
store of the browser. Container sizes are 0B: what a container writes lives
in its VM.

`docker ps --filter` works in the Docker API with dockerd's rules, and
`--format` takes `table`, `json`, `raw` and every field of the docker CLI.
`--detach-keys` works for `attach`, `start -ai` and `run`; without a tty,
Ctrl-C sends SIGINT to the container (`--sig-proxy`).

### docker cp, stats, logs and events

`docker cp` copies files and directories both ways, also `dir/.` and `-`
for a tar. For a running container the page reads and writes the 9p tree of
the VM in its worker (`cpfs.js`), so the image needs no `tar`; init first
syncs and drops its caches (`{"sync": true}`), because the root is mounted
with `cache=loose`. A stopped container has no VM: a worker rebuilds its
image. What `docker cp` puts into a container is kept as tars under
`/var/lib/containerd/io.tabernetes.docker-cp/<id>/` and goes on top of the
image at every start.

`docker stats` reads the kernel of each VM: init answers `{"stats": true}`
with `/proc/stat`, `/proc/meminfo`, `/proc/net/dev` and the process count.
BLOCK I/O is always 0: the root is 9p, not a block device.

The console log is written in Docker's json-file format and rotated: 1 MiB
x 3 files by default, or `--log-opt max-size/max-file`. This makes
`docker logs --since/--until/-t/--tail` work. `logs -f` and attach take
their output from the console stream, so a rotation does not break them.
`docker events` keeps the last 256 events.

### Volumes and bind mounts

`docker volume create/ls/inspect/rm/prune`, `-v` and `--mount` (volume,
bind, tmpfs) work like Docker. v86 has one 9p device, so a mount is a second
v86 file system that the root file system of the VM mounts at the path
(`FS.Mount`, `web/src/volumes.js`); the guest sees one tree. The files of a
volume are in OPFS (`tabernetes/volumes/<name>/`: an index and a data file
per file, never changed once written); its metadata is in
`/var/lib/docker/volumes`, a kept path. The VM worker saves what changed
every 2 s, before it reports the exit and when the page stops the VM. Two
containers share a volume, but not live: a container sees the changes of
others when it starts. A new empty volume gets the files of the image at
the path (copy-up). `-v /path`, `--mount` without a source and the VOLUMEs
of the image make anonymous volumes; `docker rm -v` and `--rm` remove them.
A bind mount copies the files of the terminal (`/root/...`) into the VM at
start and writes the changes of the container back every 2 s and at stop.
`:ro` is a read-only bind remount by init, so writes fail with "Read-only
file system". `forget`, `poweroff` and `reboot` delete the volumes. Pods on
the tab node get `emptyDir` volumes the same way.

### Tabs and saves

Each docker CLI request carries `X-Tabernetes-Tab`, and a container runs in
the tab that started it, so two tabs no longer race. A tab that is hidden or
closing asks the shared worker to save the kept files at once. The data of
containerd is saved 50 ms after a change, at most once per second: a bbolt
commit dirties about 0.5 MB.

### What the merge decided

- One flag table for `docker run` (FLAGS' `dockerrunflags.go`); the attach
  options of CMDS, the log options of OPS and `-v`/`--mount` of VOLS became
  rows of it. `docker create` leaves out the attach options, like Docker.
- The container state has both `Paused` and `Restarting`; a restarting
  container shows as such before a paused one.
- `docker rm` cancels restarts, removes the log files and the `docker cp`
  files, and with `-v` the anonymous volumes.
- One init with all the new requests (`ps`, `stats`, `sync`, `detachable`,
  read-only remounts).
- `docker system df` counts the volumes (size -1: Go cannot measure OPFS).

## Remaining limitations

- **exec processes are not containerd processes**: their stdio cannot go
  through containerd's fifos in wasm, so the Docker API keeps its own exec
  store over init, as dockerd keeps its own exec store over containerd.
  This is by design.
- Not done: `docker build`, networks between containers (each VM reaches
  only the outside through its own `fetch` relay), `compose`, `push` (no
  registry takes writes from a browser), `docker update`.
- Volumes: no live sharing between running VMs and no live sync from the
  terminal into a running container; a changed file is saved whole.
  `HostConfig.Binds` and `Mounts` are not in inspect. Volume sizes are not
  known (`system df`).
- `docker top` refuses a paused container (init is frozen with it).
  `ancestor=` matches only the image itself. `docker images` has no
  `--filter`. Port links keep the old name after `docker rename` until the
  next start.
- Files a stopped container wrote itself cannot be copied out: they lived
  in its VM.
- `-P` bindings also show in `HostConfig.PortBindings`.
- No IPv6 lines in `docker port`.
- The init with all the requests is built from `vm/init/init.c` by the `vm`
  task; a deployment needs the new `dist/vm/init`. `vm/test` (Linux only)
  was not run on the Mac.

## Resume commands

```bash
cd /Users/rafa/Documents/self-projects/tabernetes

# reset the checkout and re-run the patch (this is what every iteration did)
cd .build/kubernetes && git checkout -- . && git clean -fd -e ".version-v1.37.2" && rm -f .patched
cd /Users/rafa/Documents/self-projects/tabernetes && mise run patch

# build the wasm binary (the real test)
cd .build/kubernetes && GOOS=js GOARCH=wasm go build -o /tmp/tabernetes.wasm ./cmd/tabernetes

# the full build with the heap snapshot (~3 min); not into dist/
k8s/build.sh .build/kubernetes /tmp/tabernetes-full.wasm

# the tests of the vendoring
go -C k8s test .
```

## Key files

- `k8s/patch.go` — `vendorContainerd`, `mergeVendor`, `addPackages`,
  `takesContainerdVersion`, `setRequire`, `setWorkspaceRequires`,
  `setModule`, `moduleBlock`, `parseModules`, `hasLocalReplace`.
- `k8s/containerd-js.patch` — the whole containerd port (regenerate from
  `.build/containerd` with `git add -A && git diff --cached --binary` after
  any change to the containerd checkout).
- `docs/docker-in-wasm.md` — the design and the full migration plan.

## Note on the vendoring approach

Kubernetes uses a `go.work` (the `staging/` modules) and manual vendoring
(`k8s/patch.go` edits `vendor/`, `go.mod`, `go.sum` by hand). `go get` /
`go mod vendor` corrupt the go.mod (`k8s.io/api v0.0.0` → a published version,
breaking the `replace` to `./staging`), so the vendoring must stay manual. The
`mergeVendor` rules are: skip `k8s.io/*` (local `replace`), keep the
checkout's version of shared modules (adding missing packages and the bbolt
edits of the patch), and overwrite only `github.com/containerd/*` (which
Kubernetes vendors at the old v1 versions) and logrus.
