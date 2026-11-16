// Tiny in-memory file system for Go's js/wasm syscall layer.
// Go expects a subset of the Node.js callback based fs API on globalThis.fs.
// The files below /root, and the data of Docker (keptPaths), survive a
// reload: every change goes to IndexedDB a moment later (state.js), and the
// next start loads them before Go runs. Other tabs see a change after their
// next reload.

const S_IFMT = 0o170000, S_IFREG = 0o100000, S_IFDIR = 0o040000, S_IFLNK = 0o120000;
const constants = {
  O_RDONLY: 0, O_WRONLY: 1, O_RDWR: 2, O_CREAT: 0o100, O_EXCL: 0o200, O_TRUNC: 0o1000,
  O_APPEND: 0o2000, O_DIRECTORY: 0o200000, O_SYNC: 0o4010000,
};

// Node.js applies the umask in the kernel, here open and mkdir do: a new
// file is -rw-r--r--, not -rw-rw-rw-.
const UMASK = 0o022;

function fsErr(code, path) {
  const e = new Error(`${code}: ${path ?? ""}`);
  e.code = code;
  return e;
}

let inoCounter = 1;
function newNode(mode, extra = {}) {
  const now = Date.now();
  return { ino: inoCounter++, mode, nlink: 1, uid: 0, gid: 0, atime: now, mtime: now, ctime: now, ...extra };
}
const isDir = (n) => (n.mode & S_IFMT) === S_IFDIR;
const isLink = (n) => (n.mode & S_IFMT) === S_IFLNK;

// What survives a reload: the files of the terminal, and Docker's: the data
// and the state of containerd, the consoles of the containers and the
// metadata of the volumes, whose files are in OPFS (volumes.js)
// (k8s/tabernetes/docker.go has the same paths). The kubeconfig belongs to
// the cluster of this visit.
export const keptPaths = ["/root", "/var/lib/containerd", "/run/containerd", "/var/log/v86", "/var/lib/docker"];
const skippedPaths = ["/root/.kube"];

// A file up to CHUNK bytes is saved as one record. A bigger one is a record
// without the data and a record per CHUNK bytes of it, and a save writes
// only the chunks that changed: a log grows at its end, and bbolt (the
// database of containerd) writes a few pages of its file per transaction.
// Saving all of such a file after every change would write it again and
// again.
export const CHUNK = 64 << 10;

// The data of containerd changes with the state of a container: it is saved
// SOON ms after a change, not up to 3 s later as other kept files, but at
// most once per SOON_GAP ms: a bbolt commit dirties about 0.5 MB of
// chunks, and docker run, stop and rm each commit several times.
const SOON_PATHS = ["/var/lib/containerd", "/run/containerd"];
const SOON = 50, SOON_GAP = 1000;

// markDirty notes that bytes [from, to) of a file changed since the last
// save. n.dirty is a Set of chunk numbers, or undefined for all of them (a
// new file, or one that was never saved).
function markDirty(n, from, to) {
  if (!n.dirty) return;
  for (let i = Math.floor(from / CHUNK); i * CHUNK < to; i++) n.dirty.add(i);
}

// persist: {keep: [paths], skip: [paths], save(put, del)} keeps the files
// below the paths of keep, except the ones below skip. save gets records
// (see record and fileRecords) and the paths of removed files.
export function createFS({ cwd = "/", onOutput = () => {}, persist = null } = {}) {
  let root = newNode(S_IFDIR | 0o755, { children: new Map() });
  const fds = new Map();
  let nextFd = 10;
  const dec = new TextDecoder();

  const split = (p) => {
    if (!p.startsWith("/")) p = cwd.replace(/\/$/, "") + "/" + p;
    const parts = [];
    for (const s of p.split("/")) {
      if (!s || s === ".") continue;
      if (s === "..") parts.pop();
      else parts.push(s);
    }
    return parts;
  };

  // resolve returns {node, parent, name}; node may be undefined if it does not exist.
  function resolve(path, follow = true, depth = 0) {
    if (depth > 32) throw fsErr("ELOOP", path);
    const parts = split(path);
    let node = root, parent = null, name = "";
    for (let i = 0; i < parts.length; i++) {
      if (!isDir(node)) throw fsErr("ENOTDIR", path);
      parent = node;
      name = parts[i];
      node = node.children.get(name);
      if (!node) {
        if (i < parts.length - 1) throw fsErr("ENOENT", path);
        return { node: undefined, parent, name };
      }
      if (isLink(node) && (follow || i < parts.length - 1)) {
        const base = "/" + parts.slice(0, i).join("/");
        const target = node.target.startsWith("/") ? node.target : base + "/" + node.target;
        const rest = parts.slice(i + 1).join("/");
        return resolve(rest ? target + "/" + rest : target, follow, depth + 1);
      }
    }
    return { node, parent, name };
  }

  const must = (path, follow = true) => {
    const r = resolve(path, follow);
    if (!r.node) throw fsErr("ENOENT", path);
    return r;
  };

  const stat = (n) => {
    const size = isDir(n) ? 4096 : isLink(n) ? n.target.length : n.size;
    return {
      dev: 1, ino: n.ino, mode: n.mode, nlink: n.nlink, uid: n.uid, gid: n.gid, rdev: 0,
      size, blksize: 4096, blocks: Math.ceil(size / 512),
      atimeMs: n.atime, mtimeMs: n.mtime, ctimeMs: n.ctime,
      isDirectory: () => isDir(n), isFile: () => (n.mode & S_IFMT) === S_IFREG,
    };
  };

  const ensureCap = (n, size) => {
    if (n.data.length >= size) return;
    const buf = new Uint8Array(Math.max(size, n.data.length * 2, 256));
    buf.set(n.data.subarray(0, n.size));
    n.data = buf;
  };

  // changed paths, saved together a moment after the last change, but no
  // later than a few seconds after the first one: a program that writes all
  // the time (a download with wget, a log) would put the save off forever.
  // A reload right after a change, from vi say, saves on pagehide, and a
  // tab that is hidden saves too (the tab asks the shared worker, tabs.js).
  // The data of containerd changes when a container is created, started,
  // stopped or removed: it is saved after SOON ms, so that a container
  // survives a reload that comes right after docker run.
  const changed = new Set();
  let timer = 0, since = 0, soonDue = Infinity, lastFlush = 0;
  if (persist && typeof addEventListener === "function") {
    addEventListener("pagehide", () => changed.size && flush());
    if (typeof document === "object") addEventListener("visibilitychange", () => document.visibilityState === "hidden" && changed.size && flush());
  }
  const abs = (path) => "/" + split(path).join("/");
  // below a path of keep: these are always there
  const kept = (path) => persist && persist.keep.some((k) => path.startsWith(k + "/")) &&
    !persist.skip.some((s) => path === s || path.startsWith(s + "/"));
  const soon = (path) => SOON_PATHS.some((k) => path.startsWith(k + "/"));
  const touch = (path) => {
    if (!persist) return;
    path = abs(path);
    if (!kept(path)) return;
    const now = Date.now();
    if (!changed.size) since = now;
    changed.add(path);
    // a change of containerd's data must be saved by soonDue, whatever
    // changes after it
    if (soon(path)) soonDue = Math.min(soonDue, Math.max(now + SOON, lastFlush + SOON_GAP));
    const at = Math.min(now + 300, since + 3000, soonDue);
    clearTimeout(timer);
    timer = setTimeout(flush, Math.max(0, at - now));
  };
  // record is a directory, a link, or a small file with its data.
  // fileRecords are those of a file: a big one has its size and the number
  // of its chunks, and a record {path, chunk, data} per chunk that changed
  // (all of them with all). chunks tells the save which chunks to delete.
  const record = (path, n) => {
    const r = { path, mode: n.mode, mtime: n.mtime };
    if (isLink(n)) r.target = n.target;
    else if (!isDir(n)) {
      r.data = n.data.slice(0, n.size);
      r.chunks = 0;
    }
    return r;
  };
  const fileRecords = (path, n, all) => {
    const dirty = all ? undefined : n.dirty;
    n.dirty = new Set();
    if (n.size <= CHUNK) return [record(path, n)];
    const chunks = Math.ceil(n.size / CHUNK);
    const out = [{ path, mode: n.mode, mtime: n.mtime, size: n.size, chunks }];
    for (let i = 0; i < chunks; i++) {
      if (dirty && !dirty.has(i)) continue;
      out.push({ path, chunk: i, data: n.data.slice(i * CHUNK, Math.min(n.size, (i + 1) * CHUNK)) });
    }
    return out;
  };
  function flush() {
    clearTimeout(timer);
    timer = 0;
    soonDue = Infinity;
    lastFlush = Date.now();
    const put = [], del = [];
    // all: below a changed directory (moved, say) every chunk is new
    const walk = (path, n, all) => {
      if (!kept(path)) return;
      if (isDir(n) || isLink(n)) put.push(record(path, n));
      else put.push(...fileRecords(path, n, all));
      if (isDir(n)) for (const [name, c] of n.children) walk(path + "/" + name, c, true);
    };
    for (const path of changed) {
      let n;
      try { n = resolve(path, false).node; } catch { n = undefined; }
      if (n) walk(path, n, false);
      else del.push(path);
    }
    changed.clear();
    if (!put.length && !del.length) return;
    stats.saves++;
    stats.records += put.length + del.length;
    for (const r of put) stats.bytes += r.data ? r.data.length : 0;
    persist.save(put, del).catch((e) => console.warn("tabernetes: saving files:", e));
  }
  // how much the saves cost, for measurements
  const stats = { saves: 0, records: 0, bytes: 0 };

  // resize is truncate: a file that grows gets zeros.
  const resize = (n, len) => {
    ensureCap(n, len);
    if (len > n.size) n.data.fill(0, n.size, len);
    markDirty(n, Math.min(len, n.size), Math.max(len, n.size, Math.min(len, n.size) + 1));
    n.size = len;
  };

  const getFd = (fd) => {
    const f = fds.get(fd);
    if (!f) throw fsErr("EBADF");
    return f;
  };

  const ops = {
    open(path, flags, mode) {
      let { node, parent, name } = resolve(path);
      if (node && (flags & constants.O_CREAT) && (flags & constants.O_EXCL)) throw fsErr("EEXIST", path);
      if (!node) {
        if (!(flags & constants.O_CREAT)) throw fsErr("ENOENT", path);
        if (!parent || !isDir(parent)) throw fsErr("ENOENT", path);
        node = newNode(S_IFREG | (mode & 0o7777 & ~UMASK), { data: new Uint8Array(0), size: 0 });
        parent.children.set(name, node);
        parent.mtime = Date.now();
      }
      if (isDir(node) && (flags & 3) !== 0) throw fsErr("EISDIR", path);
      if (!isDir(node) && (flags & constants.O_DIRECTORY)) throw fsErr("ENOTDIR", path);
      if ((flags & constants.O_TRUNC) && !isDir(node)) { node.size = 0; node.mtime = Date.now(); markDirty(node, 0, 1); }
      const fd = nextFd++;
      const p = abs(path);
      fds.set(fd, { node, pos: 0, flags, path: p });
      if (flags & (constants.O_CREAT | constants.O_TRUNC)) touch(p);
      return fd;
    },
    close(fd) { getFd(fd); fds.delete(fd); },
    read(fd, buf, offset, length, position) {
      const f = getFd(fd), n = f.node;
      if (isDir(n)) throw fsErr("EISDIR");
      const pos = position ?? f.pos;
      const cnt = Math.max(0, Math.min(length, n.size - pos));
      buf.set(n.data.subarray(pos, pos + cnt), offset);
      if (position == null) f.pos += cnt;
      n.atime = Date.now();
      return cnt;
    },
    write(fd, buf, offset, length, position) {
      if (fd === 1 || fd === 2) {
        onOutput(fd, dec.decode(buf.subarray(offset, offset + length)));
        return length;
      }
      const f = getFd(fd), n = f.node;
      if ((f.flags & 3) === 0) throw fsErr("EBADF");
      let pos = position ?? ((f.flags & constants.O_APPEND) ? n.size : f.pos);
      ensureCap(n, pos + length);
      n.data.set(buf.subarray(offset, offset + length), pos);
      if (pos > n.size) { // a hole: zeros, not what a truncate left behind
        n.data.fill(0, n.size, pos);
        markDirty(n, n.size, pos);
      }
      markDirty(n, pos, pos + length);
      n.size = Math.max(n.size, pos + length);
      if (position == null) f.pos = pos + length;
      n.mtime = Date.now();
      touch(f.path);
      return length;
    },
    fstat(fd) { return stat(getFd(fd).node); },
    stat(path) { return stat(must(path).node); },
    lstat(path) { return stat(must(path, false).node); },
    readdir(path) {
      const { node } = must(path);
      if (!isDir(node)) throw fsErr("ENOTDIR", path);
      return [...node.children.keys()];
    },
    mkdir(path, perm) {
      const { node, parent, name } = resolve(path);
      if (node) throw fsErr("EEXIST", path);
      if (!parent) throw fsErr("EEXIST", path);
      parent.children.set(name, newNode(S_IFDIR | (perm & 0o7777 & ~UMASK), { children: new Map() }));
      touch(path);
    },
    rmdir(path) {
      const { node, parent, name } = must(path, false);
      if (!isDir(node)) throw fsErr("ENOTDIR", path);
      if (node.children.size) throw fsErr("ENOTEMPTY", path);
      parent.children.delete(name);
      touch(path);
    },
    unlink(path) {
      const { node, parent, name } = must(path, false);
      if (isDir(node)) throw fsErr("EISDIR", path);
      parent.children.delete(name);
      node.nlink--;
      touch(path);
    },
    rename(from, to) {
      const src = must(from, false);
      const dst = resolve(to, false);
      if (!dst.parent) throw fsErr("EBUSY", to);
      if (dst.node && isDir(dst.node) && dst.node.children.size) throw fsErr("ENOTEMPTY", to);
      src.parent.children.delete(src.name);
      dst.parent.children.set(dst.name, src.node);
      touch(from);
      touch(to);
    },
    link(from, to) {
      const src = must(from);
      const dst = resolve(to);
      if (dst.node) throw fsErr("EEXIST", to);
      dst.parent.children.set(dst.name, src.node);
      src.node.nlink++;
      touch(to);
    },
    symlink(target, path) {
      const dst = resolve(path, false);
      if (dst.node) throw fsErr("EEXIST", path);
      dst.parent.children.set(dst.name, newNode(S_IFLNK | 0o777, { target }));
      touch(path);
    },
    readlink(path) {
      const { node } = must(path, false);
      if (!isLink(node)) throw fsErr("EINVAL", path);
      return node.target;
    },
    chmod(path, mode) { const { node } = must(path); node.mode = (node.mode & S_IFMT) | (mode & 0o7777); touch(path); },
    fchmod(fd, mode) { const f = getFd(fd), n = f.node; n.mode = (n.mode & S_IFMT) | (mode & 0o7777); touch(f.path); },
    chown(path, uid, gid) { const { node } = must(path); node.uid = uid; node.gid = gid; },
    fchown(fd, uid, gid) { const n = getFd(fd).node; n.uid = uid; n.gid = gid; },
    lchown(path, uid, gid) { const { node } = must(path, false); node.uid = uid; node.gid = gid; },
    utimes(path, atime, mtime) { const { node } = must(path); node.atime = atime * 1000; node.mtime = mtime * 1000; touch(path); },
    truncate(path, len) { const { node } = must(path); resize(node, len); touch(path); },
    ftruncate(fd, len) { const f = getFd(fd), n = f.node; resize(n, len); touch(f.path); },
    fsync() {},
  };

  const fs = { constants };
  // save what changed now (a tab that goes away, cluster.js), and what the
  // saves cost so far
  fs.flush = () => { if (changed.size) flush(); };
  fs.saveStats = stats;
  for (const [name, fn] of Object.entries(ops)) {
    fs[name] = (...args) => {
      const cb = args.pop();
      let res, err = null;
      try { res = fn(...args); } catch (e) { err = e; }
      if (name === "read" || name === "write") cb(err, res ?? 0);
      else cb(err, res);
    };
  }
  fs.writeSync = (fd, buf) => ops.write(fd, buf, 0, buf.length, null);
  fs.readFileSync = (path) => { const { node } = must(path); return node.data.slice(0, node.size); };
  fs.writeFileSync = (path, data) => {
    const fd = ops.open(path, constants.O_WRONLY | constants.O_CREAT | constants.O_TRUNC, 0o644);
    const b = typeof data === "string" ? new TextEncoder().encode(data) : data;
    ops.write(fd, b, 0, b.length, null);
    ops.close(fd);
  };
  // process.cwd/chdir: Go's os.Chdir and os.Getwd end up here.
  fs.cwd = () => cwd;
  fs.chdir = (path) => {
    const { node } = must(path);
    if (!isDir(node)) throw fsErr("ENOTDIR", path);
    cwd = "/" + split(path).join("/");
  };
  fs.mkdirSync = (path) => {
    let cur = "";
    for (const part of split(path)) {
      cur += "/" + part;
      if (!resolve(cur).node) ops.mkdir(cur, 0o755);
    }
  };
  // restore puts saved records (state.js) back, parents first and the
  // chunks of a file after it, without saving them again.
  fs.restore = (records) => {
    const keep = persist;
    persist = null;
    for (const r of records) {
      try {
        const { node, parent, name } = resolve(r.path, false);
        if (!parent) continue;
        if (r.chunk != null) {
          // a chunk of a big file, or a stale one past its end
          const at = r.chunk * CHUNK;
          if (node?.data && at < node.size) node.data.set(r.data.subarray(0, node.size - at), at);
          continue;
        }
        let n = node;
        if (!n) {
          const fmt = r.mode & S_IFMT;
          if (fmt === S_IFDIR) n = newNode(r.mode, { children: new Map() });
          else if (fmt === S_IFLNK) n = newNode(r.mode, { target: r.target });
          else if (r.data) n = newNode(r.mode, { data: r.data, size: r.data.length });
          else n = newNode(r.mode, { data: new Uint8Array(r.size), size: r.size });
          parent.children.set(name, n);
        }
        if (!isDir(n) && !isLink(n)) n.dirty = new Set(); // saved as it is
        n.mtime = r.mtime;
      } catch { /* its parent is gone */ }
    }
    persist = keep;
  };
  // dump and load carry the files and the open file descriptors over to
  // another instance, for the hibernation of the control plane
  // (hibernate.js). structuredClone keeps hard links one node.
  fs.dump = () => structuredClone({ root, fds, nextFd, ino: inoCounter, cwd });
  fs.load = (d) => {
    root = d.root;
    fds.clear();
    for (const [k, v] of d.fds) fds.set(k, v);
    nextFd = d.nextFd;
    inoCounter = Math.max(inoCounter, d.ino);
    cwd = d.cwd;
  };
  return fs;
}

// installFS puts the file system on globalThis.fs. home (optional):
// {records, save(put, del)} keeps keptPaths, see state.js.
export function installFS(onOutput, home = null) {
  const fs = createFS({
    cwd: "/root",
    onOutput,
    persist: home && { keep: keptPaths, skip: skippedPaths, save: home.save },
  });
  fs.mkdirSync("/tmp");
  for (const p of keptPaths) fs.mkdirSync(p);
  if (home) fs.restore(home.records);
  globalThis.fs = fs;
  globalThis.process ??= {};
  Object.assign(globalThis.process, {
    getuid: () => 0, getgid: () => 0, geteuid: () => 0, getegid: () => 0,
    getgroups: () => [0], pid: 1, ppid: 0, umask: () => UMASK,
    cwd: () => fs.cwd(), chdir: (path) => fs.chdir(path),
  });
  return fs;
}
