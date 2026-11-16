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

    // start pulls the image and boots it. opts: image, container (command,
    // args, env as "NAME=value", workingDir), hostname, ip, gateway, memory
    // (bytes, 0 for automatic). on: pulling(), pulled(info), started(info),
    // console(text), exit(code, message: why the entrypoint did not start),
    // error(message, pull, never: not present with the pull policy Never).
    start(opts, on) {
      const worker = spawn(abs(workerURL));
      const conns = new Map();
      let next = 1;
      worker.onmessage = ({ data: m }) => {
        switch (m.type) {
          case "pulling": on.pulling(); break;
          case "pulled": on.pulled(m); break;
          case "started": on.started(m); break;
          case "console": on.console(m.data); break;
          case "exit": on.exit(m.code, m.message || ""); break;
          case "error": on.error(m.message, !!m.pull, !!m.never); break;
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
        stop() {
          worker.terminate();
          for (const c of conns.values()) c.onClose();
          conns.clear();
        },
      };
    },
  };
}
