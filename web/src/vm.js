// The container side of the tab node (k8s/tabernetes/v86.go), as host.vm
// of the session of a tab. Every pod with a container image gets its own
// worker (vmworker.js) with an x86 emulator in it. Stopping the pod ends the
// worker, and with it the VM.

// spawn starts the worker. In the shared worker it is a worker of the tab
// (bridge.js), since a shared worker cannot start workers.
export function vmAPI({ version, workerURL, wasmURL, kernelURL, initURL, registry, spawn = (u) => new Worker(u) }) {
  const abs = (u) => new URL(u, location.href).href;
  // ask sends one request to a worker of its own, which starts no VM, and
  // resolves with the answer of type done. Docker uses it for pull, images
  // and rmi; the worker ends with the answer (it holds the layers of a pull
  // in use until then).
  const ask = (msg, done, onMessage = () => {}) => new Promise((resolve, reject) => {
    const worker = spawn(abs(workerURL));
    const end = () => worker.terminate();
    worker.onmessage = ({ data: m }) => {
      if (m.type === done) { end(); resolve(m); }
      else if (m.type === "error") { end(); reject(new Error(m.message)); }
      else onMessage(m);
    };
    worker.onerror = (e) => { e.preventDefault(); end(); reject(new Error(e.message || "the VM worker failed")); };
    worker.postMessage({ ...msg, registry, version });
  });
  return {
    version, // of v86, for the container runtime of the node
    registry,

    // pull pulls an image without starting it (docker pull). progress(step)
    // gets {layer, status, current, total}. It resolves with {image,
    // downloaded}; image is described in dockerapi/images.go (Image).
    pull(image, progress) {
      return ask({ type: "pull", image }, "pulled", (m) => { if (m.type === "progress") progress(m); })
        .then((m) => JSON.stringify({ image: m.image, downloaded: m.downloaded }));
    },
    // images lists the images of the browser (docker images), as JSON.
    images() {
      return ask({ type: "images" }, "images").then((m) => JSON.stringify(m.images));
    },
    // removeImage forgets a reference (docker rmi) and resolves with the
    // digests it deleted, as JSON.
    removeImage(image) {
      return ask({ type: "rmi", image }, "removed").then((m) => JSON.stringify(m.deleted));
    },
    // tagImage gives the image of the reference source the name target too
    // (docker tag). Both are normalized references.
    tagImage(source, target) {
      return ask({ type: "tag", source, target }, "tagged").then(() => "");
    },
    // pruneLayers deletes the layers that no reference uses (docker image
    // prune) and resolves with {deleted, reclaimed} as JSON.
    pruneLayers() {
      return ask({ type: "prune" }, "pruned").then((m) => JSON.stringify({ deleted: m.deleted, reclaimed: m.reclaimed }));
    },
    // imageFiles reads path in image with files on top, for docker cp of a
    // stopped container (cpfs.js): {stat, entries} or {notFound}; with
    // stat, only the stat.
    imageFiles(image, files, path, follow, stat) {
      return ask({ type: "imageFiles", image, files, path, follow, stat }, "files").then((m) => m.value);
    },
    // removeVolume deletes the data of a volume in the browser (docker
    // volume rm, volumes.js) and resolves with the bytes it held.
    removeVolume(name) {
      return ask({ type: "volume-rm", name }, "volume-removed").then((m) => m.size);
    },

    // start pulls the image and boots it. opts: image, container (command,
    // args, env as "NAME=value", workingDir), hostname, ip, gateway, memory
    // (bytes, 0 for automatic). on: pulling(), pulled(info), started(info),
    // console(text), exit(code, message: why the entrypoint did not start),
    // error(message, pull, never: not present with the pull policy Never),
    // bind(mount, set, del): what the container changed in a bind mount
    // (opts.mounts, volumes.js).
    start(opts, on) {
      const worker = spawn(abs(workerURL));
      const conns = new Map();
      const calls = new Map(); // docker cp: the requests to the worker
      let next = 1;
      const call = (msg, transfer = []) => new Promise((resolve, reject) => {
        const seq = next++;
        calls.set(seq, { resolve, reject });
        worker.postMessage({ ...msg, seq }, transfer);
      });
      // docker pause and unpause: the worker answers once the emulator has
      // stopped or runs again
      const waiting = { paused: [], resumed: [] };
      let synced = null;
      worker.onmessage = ({ data: m }) => {
        switch (m.type) {
          case "paused": case "resumed": waiting[m.type].splice(0).forEach((w) => w.resolve()); break;
          case "pause-error": waiting.paused.splice(0).concat(waiting.resumed.splice(0)).forEach((w) => w.reject(new Error(m.message))); break;
          case "reply": {
            const c = calls.get(m.seq);
            calls.delete(m.seq);
            if (c && m.error) c.reject(new Error(m.error));
            else if (c) c.resolve(m.value);
            break;
          }
          case "pulling": on.pulling(); break;
          case "pulled": on.pulled(m); break;
          case "started": on.started(m); break;
          case "console": on.console(m.data); break;
          case "exit": on.exit(m.code, m.message || ""); break;
          case "error": on.error(m.message, !!m.pull, !!m.never); break;
          case "bind": on.bind?.(m.mount, m.set, m.del); break;
          case "synced": synced?.(); break;
          case "conn": {
            const c = conns.get(m.id);
            if (!c) break;
            if (m.event === "connect") c.onConnect();
            else if (m.event === "data") c.onData(m.data);
            else { conns.delete(m.id); c.onClose(); }
          }
        }
      };
      worker.onerror = (e) => { e.preventDefault(); on.error(e.message || "the VM worker failed", false); };
      worker.postMessage({
        type: "start",
        ...opts,
        registry,
        version,
        wasmURL: abs(wasmURL),
        kernelURL: abs(kernelURL),
        initURL: abs(initURL),
      });
      return {
        // connect opens a TCP connection to port of the pod.
        connect(port, onConnect, onData, onClose) {
          const id = next++;
          conns.set(id, { onConnect, onData, onClose });
          worker.postMessage({ type: "connect", id, port });
          return {
            write(data) {
              const b = data.slice();
              worker.postMessage({ type: "write", id, data: b }, [b.buffer]);
            },
            close() {
              if (conns.delete(id)) worker.postMessage({ type: "close", id });
            },
          };
        },
        // pause stops the emulator of the VM (docker pause), resume runs it
        // again. Both resolve once it is done.
        pause() {
          return new Promise((resolve, reject) => { waiting.paused.push({ resolve, reject }); worker.postMessage({ type: "pause" }); });
        },
        resume() {
          return new Promise((resolve, reject) => { waiting.resumed.push({ resolve, reject }); worker.postMessage({ type: "resume" }); });
        },
        // docker cp of the running container: the 9p tree of its VM
        // (cpfs.js). statFile and readFiles resolve with {stat, entries}
        // or {notFound}; writeFiles puts entries into the directory dir.
        statFile: (path, follow) => call({ type: "statFile", path, follow }),
        readFiles: (path, follow) => call({ type: "readFiles", path, follow }),
        writeFiles: (dir, entries) => call({ type: "writeFiles", dir, entries }, entries.filter((e) => e.data).map((e) => e.data.buffer)),
        stop() {
          for (const c of calls.values()) c.reject(new Error("the container is not running"));
          calls.clear();
          for (const w of waiting.paused.splice(0).concat(waiting.resumed.splice(0))) w.reject(new Error("the VM was stopped"));
          for (const c of conns.values()) c.onClose();
          conns.clear();
          if (!opts.mounts?.length) { worker.terminate(); return; }
          // The worker saves the files of the volumes first, for at most
          // 10 s (volumes.js); a second stop does not wait again.
          if (synced) return;
          const end = setTimeout(() => worker.terminate(), 10000);
          synced = () => { clearTimeout(end); worker.terminate(); };
          worker.postMessage({ type: "sync" });
        },
      };
    },
  };
}
