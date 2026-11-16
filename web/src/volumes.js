// volumes gives the VM of a container its volumes and bind mounts
// (docker run -v, --mount; vmworker.js).
//
// v86 has one virtio 9p device, so a VM has one 9p file system: the root
// of the container (rootfs.js). A mount is a second file system of v86 (an
// FS object with its own files) that the root mounts at the path of the
// mount, with FS.Mount. The guest sees one 9p tree; v86 sends the requests
// below the path to the FS of the mount. This needs no change of the
// kernel, of init or of v86, and every mount keeps its files apart from
// the root, so they can be saved on their own.
//
// The files of a mount stay in the memory of the worker while the
// container writes them, as all files of the VM do. Every 2 s, and before
// the VM ends, sync writes what changed to where the mount comes from:
//
//   - a named or anonymous volume: the origin private file system (OPFS),
//     tabernetes/volumes/<name>/, an index of the files (index.json) and a
//     file per written file (d/<id>). A file id is random and its data never
//     changes, so a VM can read old data while another one writes new data.
//     The data of a file that is saved goes out of the memory of the worker.
//   - a bind mount: the file system of the page (the files of the terminal,
//     web/src/fs.js), through the worker that runs Go (the Docker API writes
//     them, jsruntime). The files come from there when the VM starts.
//
// Two containers with the same volume share it, but not live: each VM has
// its own copy of the files, and sync merges the changes into the index by
// path. A container sees the changes of another one when it starts, as if
// the volume were on a network file system with a cache. A data file that
// a sync replaces goes at once when no other VM uses the volume (a shared
// Web Lock per volume and VM); else it goes when a VM mounts the volume
// alone.
import { Tree, clean, modes } from "./rootfs.js";

const S_IFMT = 0o170000;
const { S_IFDIR, S_IFREG, S_IFLNK } = modes;
const VERSION = 1; // of index.json
const STATUS_ON_SERVER = 2, STATUS_FORWARDING = 5; // of v86 inodes
export const SYNC_EVERY = 2000; // ms

const lockName = (name) => `tabernetes-volume:${name}`; // the index, exclusive
const useName = (name) => `tabernetes-volume-use:${name}`; // a VM uses it, shared

// volumesDir returns the directory of the volumes in OPFS, or null.
export async function volumesDir(create = true) {
  try {
    const root = await navigator.storage.getDirectory();
    return await (await root.getDirectoryHandle("tabernetes", { create })).getDirectoryHandle("volumes", { create });
  } catch {
    return null;
  }
}

const withLock = (name, fn, opts = {}) => (navigator.locks ? navigator.locks.request(name, opts, fn) : fn({}));

// Volume is the data of one volume in OPFS.
export class Volume {
  constructor(name, dir) {
    this.name = name;
    this.dir = dir; // tabernetes/volumes/<name>
    this.files = new Map(); // id -> File, the data files read so far
  }

  static async open(name) {
    const top = await volumesDir();
    if (!top) throw new Error("volumes need the origin private file system of the browser, which this window does not have");
    return new Volume(name, await top.getDirectoryHandle(name, { create: true }));
  }

  // index returns {version, files: {path: [mode, uid, gid, mtime, size,
  // key|target]}}; "" is the root of the volume.
  async index() {
    try {
      const f = await (await this.dir.getFileHandle("index.json")).getFile();
      const idx = JSON.parse(await f.text());
      if (idx.version === VERSION && idx.files) return idx;
    } catch { /* a new volume */ }
    return { version: VERSION, files: {} };
  }

  async writeIndex(idx) {
    await writeFile(this.dir, "index.json", new TextEncoder().encode(JSON.stringify(idx)));
  }

  async data() {
    return this.dir.getDirectoryHandle("d", { create: true });
  }

  async write(id, bytes) {
    await writeFile(await this.data(), id, bytes);
  }

  async read(id, offset, length) {
    let f = this.files.get(id);
    if (!f) {
      f = await (await (await this.data()).getFileHandle(id)).getFile();
      this.files.set(id, f);
    }
    return new Uint8Array(await f.slice(offset, offset + length).arrayBuffer());
  }

  // sweep deletes the data files that idx does not use.
  async sweep(idx) {
    const used = new Set();
    for (const e of Object.values(idx.files)) {
      if ((e[0] & S_IFMT) === S_IFREG) used.add(dataID(e[5]));
    }
    const d = await this.data();
    const gone = [];
    for await (const name of d.keys()) if (!used.has(name)) gone.push(name);
    for (const name of gone) await d.removeEntry(name).catch(() => {});
    return gone.length;
  }
}

// writeFile writes a whole file: with a sync access handle in a dedicated
// worker, else with a writable stream.
async function writeFile(dir, name, bytes) {
  const fh = await dir.getFileHandle(name, { create: true });
  if (fh.createSyncAccessHandle) {
    const h = await fh.createSyncAccessHandle();
    try {
      h.truncate(0);
      if (bytes.length && h.write(bytes, { at: 0 }) !== bytes.length) throw new DOMException("short write", "QuotaExceededError");
      h.flush();
    } finally {
      h.close();
    }
    return;
  }
  const w = await fh.createWritable();
  await w.write(bytes);
  await w.close();
}

// The key of a file in a volume: "vol:<name>/<id>:0", in the format of the
// keys of LayerStorage (rootfs.js): a "layer" with the data at offset 0.
export const volumeKey = (name, id) => `vol:${name}/${id}:0`;
const dataID = (key) => (typeof key === "string" && key.startsWith("vol:") ? key.slice(key.indexOf("/") + 1, key.lastIndexOf(":")) : "");

// volumeReaders answers LayerStorage for the "layers" of the volumes:
// get("vol:<name>/<id>") is a reader of that data file. The other ids go to
// the layers of the image.
export function volumeReaders(layers, volumes) {
  return {
    get(id) {
      if (!id.startsWith("vol:")) return layers.get(id);
      const slash = id.indexOf("/");
      const vol = volumes.get(id.slice(4, slash));
      const file = id.slice(slash + 1);
      return vol && { read: (offset, length) => vol.read(file, offset, length) };
    },
  };
}

// copyStorage returns a copy of what storage reads for a volume file: v86
// writes into the buffer it gets for a file (FS.Write), and the buffer of
// LayerStorage may be its cache, which other files with the same data use.
export function copyStorage(storage) {
  return {
    read: async (key, offset, count, size) => {
      const b = await storage.read(key, offset, count, size);
      return key.startsWith("vol:") || key.startsWith("bind:") ? b.slice() : b;
    },
    cache: () => storage.cache?.(),
    uncache: (k) => storage.uncache?.(k),
  };
}

// treeOf turns entries ({path: [mode, uid, gid, mtime, size, key|target]})
// into the file system JSON of v86, and returns the entry of the root too.
export function treeOf(files) {
  const tree = new Tree();
  for (const [path, [mode, uid, gid, mtime, size, x]] of Object.entries(files)) {
    if (path === "") continue;
    const fmt = mode & S_IFMT;
    const e = { mode, uid, gid, mtime };
    if (fmt === S_IFREG) Object.assign(e, { size, key: x });
    else if (fmt === S_IFLNK) e.target = x;
    else if (fmt !== S_IFDIR) continue;
    tree.set(path, e);
  }
  return { json: tree.v86(), root: files[""] };
}

// entriesOf turns what Tree.take returned (path below the mount -> tree
// entry) into the entries of an index.
export function entriesOf(taken) {
  const files = {};
  for (const [path, e] of taken) {
    const fmt = e.mode & S_IFMT;
    if (fmt === S_IFREG) files[path] = [e.mode, e.uid, e.gid, e.mtime, e.size, e.key];
    else if (fmt === S_IFLNK) files[path] = [e.mode, e.uid, e.gid, e.mtime, 0, e.target];
    else if (fmt === S_IFDIR) files[path] = [e.mode, e.uid, e.gid, e.mtime, 0, ""];
  }
  return files;
}

// scan lists the files of a v86 FS: path -> {idx, entry}. Mounts below
// this one (forwarding inodes) belong to other mounts and are left out.
export function scan(fs) {
  const out = new Map();
  const walk = (idx, path) => {
    const i = fs.inodes[idx];
    const fmt = i.mode & S_IFMT;
    let x = "", size = 0;
    // "*": the data is in the memory of the worker, not in a storage
    if (fmt === S_IFREG) { size = i.size; x = i.status === STATUS_ON_SERVER ? i.sha256sum : "*"; }
    else if (fmt === S_IFLNK) x = i.symlink;
    else if (fmt !== S_IFDIR) return; // devices, FIFOs: not kept
    out.set(path, { idx, entry: [i.mode, i.uid, i.gid, i.mtime, size, x] });
    if (fmt !== S_IFDIR) return;
    for (const [name, child] of i.direntries) {
      if (name === "." || name === "..") continue;
      if (fs.inodes[child].status === STATUS_FORWARDING) continue;
      walk(child, path ? `${path}/${name}` : name);
    }
  };
  walk(0, "");
  return out;
}

const same = (a, b) => a && b && a.length === b.length && a.every((v, i) => v === b[i]);

// diff compares the files of a mount with base, the entries of the last
// sync, and returns what changed: set (path -> {idx, entry, data}) and del
// (paths). data says that the data of the file must be saved: the guest
// wrote it (dirty), or its key is not one the target keeps (keeps).
export function diff(now, base, dirty, keeps) {
  const set = new Map(), del = [];
  for (const [path, f] of now) {
    const b = base[path];
    if ((f.entry[0] & S_IFMT) === S_IFREG && (dirty.has(f.idx) || !keeps(f.entry[5], b))) {
      set.set(path, { ...f, data: true });
    } else if (!same(b, f.entry)) {
      set.set(path, f);
    }
  }
  for (const path of Object.keys(base)) if (!now.has(path)) del.push(path);
  return { set, del };
}

// mountFS creates the v86 FS of a mount and mounts it at path in root.
// files are the entries of the mount; storage reads their data. dirty
// collects the inodes whose data the guest changed.
export function mountFS(root, path, files, storage) {
  const { json, root: top } = treeOf(files);
  const fs = new root.constructor(storage, root.qidcounter);
  fs.load_from_json(json);
  const r = fs.inodes[0];
  if (top) [r.mode, r.uid, r.gid, r.mtime] = [S_IFDIR | (top[0] & 0o7777), top[1], top[2], top[3]];
  else r.mode = S_IFDIR | 0o755;
  r.ctime = r.atime = r.mtime;
  const dirty = new Set();
  const setData = fs.set_data;
  fs.set_data = function (idx, buf) {
    dirty.add(idx);
    return setData.call(this, idx, buf);
  };
  ensureParent(root, path);
  const rc = root.Mount("/" + clean(path), fs);
  if (rc < 0) throw new Error(`mount ${path}: error ${-rc}`);
  return { fs, dirty };
}

// ensureParent makes the directories above path in the v86 FS root, for a
// mount below another mount (the parent is in the FS of the other mount).
function ensureParent(root, path) {
  const parts = clean(path).split("/");
  parts.pop();
  let id = 0;
  for (const p of parts) {
    let next = root.Search(id, p);
    if (next < 0) next = root.CreateDirectory(p, id);
    id = next;
  }
  // a directory of the image or of an outer mount that is in the way
  const name = clean(path).split("/").pop();
  const there = root.Search(id, name);
  if (there >= 0 && root.IsDirectory(there) && root.IsEmpty(there)) root.Unlink(id, name);
}

// Mount is one mount of a running VM: a volume or a bind mount.
export class Mount {
  // spec: {type, name, destination, readonly, copy, ...}; fs, dirty: of
  // mountFS; base: the entries as saved; target: {save(changes)}.
  constructor(spec, fs, dirty, base, target) {
    this.spec = spec;
    this.fs = fs;
    this.dirty = dirty;
    this.base = base;
    this.target = target;
    this.pending = Promise.resolve();
  }

  // sync saves what changed since the last sync. One sync at a time. A
  // read-only mount changes only when it was copied up (force).
  sync(force = false) {
    if (this.spec.readonly && !force) return Promise.resolve();
    this.pending = this.pending.then(() => this.#sync()).catch((e) => console.warn(`tabernetes: save the files of ${this.spec.destination}:`, e));
    return this.pending;
  }

  async #sync() {
    const now = scan(this.fs);
    const { set, del } = diff(now, this.base, this.dirty, this.target.keeps);
    if (!set.size && !del.length) return;
    // The data of a changed file, as it is now. The guest may write again
    // while it is saved: that marks the inode dirty again, and the next
    // sync saves it once more.
    const data = new Map();
    for (const [path, f] of set) {
      if (!f.data) continue;
      this.dirty.delete(f.idx);
      const mem = this.fs.inodedata[f.idx];
      data.set(path, mem ? mem.slice(0, f.entry[4]) : (await this.fs.Read(f.idx, 0, f.entry[4]))?.slice() || new Uint8Array(0));
    }
    const saved = await this.target.save({ set, del, data });
    const base = {};
    for (const [path, f] of now) {
      if (saved?.has(path)) {
        f.entry[5] = saved.get(path);
        // The data is in OPFS now: v86 reads it from there, and the memory
        // of the worker is free again, unless the guest wrote meanwhile.
        const i = this.fs.inodes[f.idx];
        if (!this.dirty.has(f.idx) && i.size === f.entry[4]) {
          i.sha256sum = f.entry[5];
          i.status = STATUS_ON_SERVER;
          delete this.fs.inodedata[f.idx];
        }
      }
      base[path] = f.entry;
    }
    this.base = base;
  }
}

// volumeTarget saves the changes of a mount into a volume: the data files
// first, then the index, under the lock of the volume. The changes of other
// VMs since are kept: the merge is by path.
export function volumeTarget(vol) {
  return {
    // only its own data files: the data of the image (copied up) is saved
    keeps: (key) => key.startsWith("vol:"),
    async save({ set, del, data }) {
      return withLock(lockName(vol.name), async () => {
        const keys = new Map();
        for (const [path, bytes] of data) {
          const id = crypto.randomUUID();
          await vol.write(id, bytes);
          keys.set(path, volumeKey(vol.name, id));
        }
        const idx = await vol.index();
        // the data files that this sync replaces
        const old = new Set();
        for (const path of [...del, ...set.keys()]) {
          const prev = idx.files[path];
          if (prev && (prev[0] & S_IFMT) === S_IFREG && dataID(prev[5])) old.add(dataID(prev[5]));
        }
        for (const path of del) delete idx.files[path];
        for (const [path, f] of set) {
          const e = [...f.entry];
          if (keys.has(path)) e[5] = keys.get(path);
          idx.files[path] = e;
        }
        await vol.writeIndex(idx);
        // Without another VM on the volume, nobody reads them any more: a
        // database that rewrites its files would fill OPFS with old data.
        if (old.size && (await alone(vol.name))) {
          for (const e of Object.values(idx.files)) if ((e[0] & S_IFMT) === S_IFREG) old.delete(dataID(e[5]));
          const d = await vol.data();
          for (const id of old) await d.removeEntry(id).catch(() => {});
        }
        return keys;
      });
    },
  };
}

// alone tells whether this worker is the only VM that uses a volume.
async function alone(name) {
  if (!navigator.locks?.query) return false;
  const { held } = await navigator.locks.query();
  return held.filter((l) => l.name === useName(name)).length <= 1;
}

// openVolume prepares a volume for a VM: it marks the volume used by this
// worker until it ends, sweeps old data if no other VM uses it, and returns
// its entries. With copy, an empty volume gets the files that the image
// has at the mount path (image: path -> entry), as Docker copies them on
// the first mount; they are saved at the first sync.
export async function openVolume(spec, image) {
  const vol = await Volume.open(spec.name);
  if (navigator.locks) {
    // nobody else: old data files can go
    await withLock(useName(vol.name), async (alone) => {
      if (alone) await withLock(lockName(vol.name), async () => vol.sweep(await vol.index()).catch(() => 0));
    }, { ifAvailable: true });
    await new Promise((granted) => navigator.locks.request(useName(vol.name), { mode: "shared" }, () => {
      granted();
      return new Promise(() => {}); // until the worker ends
    }));
  }
  const idx = await vol.index();
  const empty = !Object.keys(idx.files).some((p) => p !== "");
  let files = idx.files, base = idx.files;
  if (empty && spec.copy !== false && image?.size) {
    files = { ...entriesOf(image) };
    if (!files[""]) files[""] = idx.files[""] || [S_IFDIR | 0o755, 0, 0, Math.floor(Date.now() / 1000), 0, ""];
    base = {}; // all of it is new to the volume
  } else if (!files[""]) {
    files = { ...files, "": [S_IFDIR | 0o755, 0, 0, Math.floor(Date.now() / 1000), 0, ""] };
  }
  return { vol, files, base };
}

// bindFiles turns the files of a bind mount, as the Docker API sends them
// ([{path, mode, data|target}], jsruntime), into entries; the data goes
// into mem under "bind:<n>/<i>:0" keys.
export function bindFiles(spec, n, mem) {
  const now = Math.floor(Date.now() / 1000);
  const files = { "": [S_IFDIR | ((spec.mode ?? 0o755) & 0o7777), 0, 0, now, 0, ""] };
  (spec.files || []).forEach((f, i) => {
    const path = clean(f.path);
    if (!path) return;
    const fmt = f.mode & S_IFMT;
    if (fmt === S_IFREG) {
      const key = `bind:${n}/${i}:0`;
      const data = f.data || new Uint8Array(0);
      mem.set(`bind:${n}/${i}`, data);
      files[path] = [f.mode, 0, 0, f.mtime || now, data.length, key];
    } else if (fmt === S_IFLNK) {
      files[path] = [f.mode, 0, 0, f.mtime || now, 0, f.target];
    } else if (fmt === S_IFDIR) {
      files[path] = [f.mode, 0, 0, f.mtime || now, 0, ""];
    }
  });
  return files;
}

// memReaders serves the data of bind mounts to LayerStorage (as "layers").
export function memReaders(next, mem) {
  return {
    get(id) {
      const m = mem.get(id);
      if (m) return { read: async (offset, length) => m.subarray(offset, offset + length) };
      return next.get(id);
    },
  };
}

// bindTarget sends the changes of a bind mount to the worker that runs Go,
// which writes them below the source of the mount.
export function bindTarget(n, post) {
  return {
    // what came from the page, or what went there at the last sync
    keeps: (key, base) => key.startsWith("bind:") || (key === "*" && base?.[5] === "*"),
    async save({ set, del, data }) {
      const out = [];
      const transfer = [];
      for (const [path, f] of set) {
        const [mode, , , mtime, , x] = f.entry;
        const e = { path, mode, mtime };
        if ((mode & S_IFMT) === S_IFLNK) e.target = x;
        if (data.has(path)) {
          e.data = data.get(path);
          transfer.push(e.data.buffer);
        }
        out.push(e);
      }
      post({ type: "bind", mount: n, set: out, del }, transfer);
      // The data stays in memory: it came from there, and v86 keeps it.
      return null;
    },
  };
}

// removeVolume deletes the data of a volume (docker volume rm, the
// emptyDir of a deleted pod) and returns the bytes it held. It waits until
// no VM uses the volume, for at most 15 s: a VM that stops saves its files
// first (vm.js), and would bring the volume back.
export async function removeVolume(name) {
  const top = await volumesDir(false);
  if (!top) return 0;
  let size = 0;
  try {
    const vol = new Volume(name, await top.getDirectoryHandle(name));
    for (const e of Object.values((await vol.index()).files)) if ((e[0] & S_IFMT) === S_IFREG) size += e[4];
  } catch {
    return 0; // never mounted: nothing in OPFS
  }
  const rm = () => withLock(lockName(name), () => top.removeEntry(name, { recursive: true }).catch(() => {}));
  if (!navigator.locks) {
    await rm();
    return size;
  }
  const wait = typeof AbortSignal?.timeout === "function" ? { signal: AbortSignal.timeout(15000) } : { ifAvailable: true };
  await navigator.locks.request(useName(name), wait, () => rm()).catch(() => rm());
  return size;
}

// forgetVolumes deletes all volumes (forget, poweroff).
export async function forgetVolumes() {
  const root = await navigator.storage.getDirectory();
  const t = await root.getDirectoryHandle("tabernetes");
  await t.removeEntry("volumes", { recursive: true });
}

const depth = (p) => clean(p).split("/").filter(Boolean).length;
const under = (p, dir) => p === dir || p.startsWith(dir + "/");
const dirMounts = (plan) => plan.filter((p) => !p.file && !p.tmpfs);

// prepareMounts takes the mount points out of the root file system of the
// image (tree) before it goes to v86, opens the volumes and reads the
// files of bind mounts. specs are the mounts of the start request (from
// jsruntime): {type: "volume"|"bind", destination, readonly, name, copy}
// for a volume, {type: "bind", destination, readonly, mode, files} for a
// directory and {type: "bind", file: true, destination, readonly, mode,
// data} for a file. The data of bind mounts goes into mem.
export async function prepareMounts(specs, tree, mem) {
  const plan = [];
  const order = (specs || []).map((spec, n) => ({ spec, n })).sort((a, b) => depth(a.spec.destination) - depth(b.spec.destination));
  for (const { spec, n } of order) {
    const path = tree.resolve(spec.destination);
    const outer = dirMounts(plan).find((p) => under(path, p.path) && path !== p.path);
    if (spec.type === "tmpfs") {
      // an empty directory of the root file system: its files stay in
      // the memory of the worker and go with the VM, like a tmpfs
      if (outer) throw new Error(`cannot mount a tmpfs at ${spec.destination} inside the mount ${outer.spec.destination}: the tab does not support it`);
      tree.take(path);
      tree.set(path, { mode: S_IFDIR | ((spec.mode ?? 0o1777) & 0o7777), uid: 0, gid: 0, mtime: Math.floor(Date.now() / 1000) });
      plan.push({ spec, n, path, tmpfs: true });
      continue;
    }
    if (spec.file) {
      if (outer) throw new Error(`cannot mount the file ${spec.destination} inside the mount ${outer.spec.destination}: the tab does not support it`);
      const data = spec.data || new Uint8Array(0);
      mem.set(`bind:${n}/0`, data);
      tree.take(path);
      tree.set(path, { mode: S_IFREG | ((spec.mode ?? 0o644) & 0o7777), uid: 0, gid: 0, mtime: Math.floor(Date.now() / 1000), size: data.length, key: `bind:${n}/0:0` });
      plan.push({ spec, n, path, file: true });
      continue;
    }
    // What the image has at the path: the root of the image or what an
    // outer mount took of it. A volume copies it up when it is empty.
    let image;
    if (outer) {
      image = new Map();
      const rel = path.slice(outer.path.length + 1);
      for (const [k, v] of outer.image) {
        if (k === rel) image.set("", v);
        else if (k.startsWith(rel + "/")) image.set(k.slice(rel.length + 1), v);
      }
    } else {
      image = tree.take(path);
    }
    const p = { spec, n, path, image };
    if (spec.type === "volume") {
      const { vol, files, base } = await openVolume(spec, image);
      Object.assign(p, { vol, files, base, copied: files !== base });
    } else {
      p.files = bindFiles(spec, n, mem);
      p.base = p.files;
    }
    plan.push(p);
  }
  return plan;
}

// readonlyPaths are the mount points that init makes read-only
// (vm/init/init.c): a bind mount of the path onto itself, remounted with
// MS_RDONLY, so the guest gets EROFS ("Read-only file system").
export const readonlyPaths = (plan) => plan.filter((p) => p.spec.readonly).map((p) => "/" + p.path);

// FileMount is a bind mount of one file: it is a file of the root file
// system, and sync sends it to the page when its data or mode changed.
class FileMount {
  constructor(spec, n, root, path, post) {
    this.spec = spec;
    this.n = n;
    this.root = root;
    this.path = path;
    this.post = post;
    // a copy: v86 writes into the buffer that it read the file into
    this.last = (spec.data || new Uint8Array(0)).slice();
    this.mode = S_IFREG | ((spec.mode ?? 0o644) & 0o7777);
    this.pending = Promise.resolve();
  }

  sync() {
    if (this.spec.readonly) return Promise.resolve();
    this.pending = this.pending.then(async () => {
      const { id } = this.root.SearchPath("/" + this.path);
      if (id < 0) return; // removed: Docker keeps the file of the host
      const i = this.root.GetInode(id);
      if ((i.mode & S_IFMT) !== S_IFREG) return;
      const data = (await this.root.Read(id, 0, i.size))?.slice() || new Uint8Array(0);
      const changed = data.length !== this.last.length || data.some((b, k) => b !== this.last[k]);
      if (!changed && i.mode === this.mode) return;
      this.last = data;
      this.mode = i.mode;
      const copy = data.slice();
      this.post({ type: "bind", mount: this.n, set: [{ path: "", mode: i.mode, mtime: i.mtime, data: copy }], del: [] }, [copy.buffer]);
    }).catch((e) => console.warn(`tabernetes: save ${this.spec.destination}:`, e));
    return this.pending;
  }
}

// attachMounts mounts the plan of prepareMounts into the 9p file system of
// the VM (root, an FS of v86, after load_from_json) and returns the mounts
// for sync. storage reads the data of all of them. A copied-up volume is
// saved at once, as Docker fills the volume before the container starts.
export function attachMounts(root, plan, storage, post) {
  const mounts = [];
  for (const p of plan) {
    if (p.tmpfs) continue;
    if (p.file) {
      mounts.push(new FileMount(p.spec, p.n, root, p.path, post));
      continue;
    }
    const { fs, dirty } = mountFS(root, p.path, p.files, storage);
    const target = p.vol ? volumeTarget(p.vol) : bindTarget(p.n, post);
    const m = new Mount(p.spec, fs, dirty, p.base, target);
    if (p.copied) m.sync(true);
    mounts.push(m);
  }
  return mounts;
}
