// cpfs reads and writes the files of a container for docker cp
// (k8s/tabernetes/dockercp.go). A running container has its root file
// system in the 9p tree of its VM (v86's FS, in the worker of the VM); a
// stopped one only has its image, which the worker merges again (rootfs.js)
// with the files that docker cp put into the container (addFiles).
//
// Both are seen through a view, so that the walk is the same: root(),
// child(node, name), info(node) and read(node). Paths resolve symbolic
// links like the kernel, also in the middle of a path (/bin is a link to
// usr/bin in Debian), but the last part only with follow (docker cp -L).
import { clean, modes } from "./rootfs.js";

const S_IFMT = 0o170000;
const { S_IFDIR, S_IFREG, S_IFLNK } = modes;

class NotFound extends Error {}

// v86View sees the 9p tree of a running VM (emulator.fs9p).
export function v86View(fs) {
  return {
    root: () => 0,
    child(node, name) {
      const id = fs.Search(node, name);
      return id < 0 ? null : id;
    },
    children: (node) => fs.GetChildren(node),
    info(node) {
      const i = fs.GetInode(node);
      return { mode: i.mode, uid: i.uid, gid: i.gid, mtime: i.mtime, size: i.size, target: i.symlink || "" };
    },
    read: async (node) => {
      const i = fs.GetInode(node);
      if (!i.size) return new Uint8Array(0);
      const d = await fs.Read(node, 0, i.size);
      return d ? d.slice() : new Uint8Array(i.size);
    },
  };
}

// treeView sees a Tree of rootfs.js (an image with the files of docker cp
// on top) and its storage (LayerStorage).
export function treeView(tree, storage) {
  let kids = null;
  const children = (node) => {
    if (!kids) {
      kids = new Map();
      for (const p of tree.files.keys()) {
        const dir = p.slice(0, Math.max(p.lastIndexOf("/"), 0));
        if (!kids.has(dir)) kids.set(dir, []);
        kids.get(dir).push(p.slice(p.lastIndexOf("/") + 1));
      }
    }
    return kids.get(node) || [];
  };
  return {
    root: () => "",
    child(node, name) {
      const p = node ? `${node}/${name}` : name;
      return tree.files.has(p) ? p : null;
    },
    children,
    info(node) {
      if (node === "") return { mode: S_IFDIR | 0o755, uid: 0, gid: 0, mtime: 0, size: 0, target: "" };
      const e = tree.files.get(node);
      return { mode: e.mode, uid: e.uid || 0, gid: e.gid || 0, mtime: e.mtime || 0, size: e.size || 0, target: e.target || "" };
    },
    read: async (node) => {
      const e = tree.files.get(node);
      if (!e.size) return new Uint8Array(0);
      return (await storage.read(e.key, 0, e.size, e.size)).slice();
    },
  };
}

// resolve finds path in view: {node, name} of the last part, or throws
// NotFound. Links in the middle always resolve, the last only with follow.
function resolve(view, path, follow) {
  let parts = clean(path).split("/").filter(Boolean);
  let node = view.root(), stack = [];
  let hops = 0;
  for (let i = 0; i < parts.length; i++) {
    const name = parts[i];
    const next = view.child(node, name);
    if (next === null) throw new NotFound(path);
    const info = view.info(next);
    const last = i === parts.length - 1;
    if ((info.mode & S_IFMT) === S_IFLNK && (!last || follow)) {
      if (++hops > 40) throw new Error(`too many levels of symbolic links: ${path}`);
      // go on from the target, relative to the directory of the link
      const base = info.target.startsWith("/") ? [] : stack.map((s) => s.name);
      parts = clean([...base, info.target, ...parts.slice(i + 1)].join("/")).split("/").filter(Boolean);
      node = view.root();
      stack = [];
      i = -1;
      continue;
    }
    stack.push({ node, name });
    node = next;
  }
  return { node, name: stack.length ? stack[stack.length - 1].name : "/" };
}

// readFiles returns path as the entries of a tar: the path itself, named
// by its base name, and what is below it. stat is its PathStat.
export async function readFiles(view, path, follow) {
  let found;
  try {
    found = resolve(view, path, follow);
  } catch (e) {
    if (e instanceof NotFound) return { notFound: true };
    throw e;
  }
  const entries = [];
  const top = found.name === "/" ? "." : found.name;
  const walk = async (node, name) => {
    const i = view.info(node);
    const fmt = i.mode & S_IFMT;
    const e = { name, mode: i.mode, uid: i.uid, gid: i.gid, mtime: i.mtime, size: 0, target: i.target };
    if (fmt === S_IFREG) {
      e.data = await view.read(node);
      e.size = e.data.length;
    }
    if (fmt === S_IFREG || fmt === S_IFDIR || fmt === S_IFLNK) entries.push(e);
    if (fmt === S_IFDIR) {
      for (const c of [...view.children(node)].sort()) await walk(view.child(node, c), `${name}/${c}`);
    }
  };
  await walk(found.node, top);
  const root = view.info(found.node);
  return { stat: { name: found.name, mode: root.mode, size: root.size, mtime: root.mtime, target: root.target }, entries };
}

// statFile returns the PathStat of path, or {notFound}.
export function statFile(view, path, follow) {
  try {
    const { node, name } = resolve(view, path, follow);
    const i = view.info(node);
    return { stat: { name, mode: i.mode, size: i.size, mtime: i.mtime, target: i.target } };
  } catch (e) {
    if (e instanceof NotFound) return { notFound: true };
    throw e;
  }
}

// writeFiles puts entries ([{path, mode, uid, gid, mtime, data?, target?}],
// paths relative to dir) into the 9p tree of a running VM. dir must be a directory. A file
// replaces what was there; a directory over a directory keeps its contents.
export async function writeFiles(fs, dir, entries) {
  const view = v86View(fs);
  const base = resolve(view, dir, true);
  if ((view.info(base.node).mode & S_IFMT) !== S_IFDIR) throw new Error("extraction point is not a directory");
  for (const e of entries) {
    const parts = clean(e.path).split("/").filter(Boolean);
    if (!parts.length) continue;
    let parent = base.node;
    for (const p of parts.slice(0, -1)) {
      let id = fs.Search(parent, p);
      if (id < 0) id = fs.CreateDirectory(p, parent);
      parent = id;
    }
    const name = parts[parts.length - 1];
    const fmt = e.mode & S_IFMT;
    let id = fs.Search(parent, name);
    if (id >= 0) {
      const old = fs.GetInode(id).mode & S_IFMT;
      if (!(old === S_IFDIR && fmt === S_IFDIR)) {
        if (old === S_IFDIR) await removeTree(fs, id);
        fs.Unlink(parent, name);
        id = -1;
      }
    }
    if (fmt === S_IFDIR) {
      if (id < 0) id = fs.CreateDirectory(name, parent);
    } else if (fmt === S_IFLNK) {
      id = fs.CreateSymlink(name, parent, e.target);
    } else if (fmt === S_IFREG) {
      id = await fs.CreateBinaryFile(name, parent, e.data || new Uint8Array(0));
    } else continue;
    const i = fs.GetInode(id);
    i.mode = (i.mode & S_IFMT) | (e.mode & 0o7777);
    i.uid = e.uid;
    i.gid = e.gid;
    if (e.mtime) i.mtime = e.mtime;
  }
}

async function removeTree(fs, id) {
  for (const c of fs.GetChildren(id)) {
    const cid = fs.Search(id, c);
    if ((fs.GetInode(cid).mode & S_IFMT) === S_IFDIR) await removeTree(fs, cid);
    fs.Unlink(id, c);
  }
}

// addFiles puts the files that docker cp copied into a container on top of
// its root file system, before it boots: files is [{path, mode, uid, gid,
// mtime, data?, target?}] in the order of the copies.
export function addFiles(tree, mem, files) {
  for (const f of files || []) {
    const fmt = f.mode & S_IFMT;
    const meta = { uid: f.uid || 0, gid: f.gid || 0, mtime: f.mtime || 0, mode: f.mode };
    if (fmt === S_IFDIR) {
      tree.set(f.path, meta);
    } else if (fmt === S_IFLNK) {
      tree.set(f.path, { ...meta, target: f.target });
    } else if (fmt === S_IFREG) {
      const key = `mem:cp:${mem.size}`;
      const data = f.data || new Uint8Array(0);
      mem.set(key, data);
      tree.set(f.path, { ...meta, size: data.length, key });
    }
  }
}
