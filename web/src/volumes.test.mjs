import assert from "node:assert/strict";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { V86 } from "v86";
import { Tree, LayerStorage, modes } from "./rootfs.js";
import { prepareMounts, attachMounts, readonlyPaths, volumeReaders, memReaders, copyStorage, removeVolume, diff, scan } from "./volumes.js";

const enc = new TextEncoder(), dec = new TextDecoder();

// A directory of the origin private file system, in memory.
class Dir {
  constructor() { this.entries = new Map(); }
  async getDirectoryHandle(n, { create } = {}) {
    let d = this.entries.get(n);
    if (!d) {
      if (!create) throw new DOMException(n, "NotFoundError");
      this.entries.set(n, (d = new Dir()));
    }
    return d;
  }
  async getFileHandle(n, { create } = {}) {
    let f = this.entries.get(n);
    if (!f) {
      if (!create) throw new DOMException(n, "NotFoundError");
      this.entries.set(n, (f = new File()));
    }
    return f;
  }
  async removeEntry(n) { if (!this.entries.delete(n)) throw new DOMException(n, "NotFoundError"); }
  async *keys() { yield* [...this.entries.keys()]; }
}
class File {
  constructor() { this.data = new Uint8Array(0); }
  async createWritable() {
    return { write: async (b) => { this.data = new Uint8Array(b); }, close: async () => {} };
  }
  async getFile() { return new Blob([this.data]); }
}

let opfs, holders;
test.beforeEach((t) => {
  opfs = new Dir();
  holders = 1;
  const old = Object.getOwnPropertyDescriptor(globalThis, "navigator");
  Object.defineProperty(globalThis, "navigator", { configurable: true, value: {
    storage: { getDirectory: async () => opfs },
    locks: {
      // every lock is free, and a shared lock is held for ever
      request: async (_name, opts, fn) => (fn ?? opts)(opts?.ifAvailable ? {} : {}),
      // one VM per volume, unless a test says otherwise
      query: async () => ({ held: [...new Array(holders)].map(() => ({ name: "tabernetes-volume-use:data" })) }),
    },
  } });
  t.after(() => {
    if (old) Object.defineProperty(globalThis, "navigator", old);
    else delete globalThis.navigator;
  });
});

let FS;
test.before(async () => {
  const wasm = fileURLToPath(new URL("../node_modules/v86/build/v86.wasm", import.meta.url));
  const e = new V86({ wasm_path: wasm, autostart: false, filesystem: {}, memory_size: 32 << 20, disable_keyboard: true, disable_mouse: true, disable_speaker: true });
  await new Promise((r) => e.add_listener("emulator-loaded", r));
  FS = e.fs9p.constructor;
  e.destroy();
});

// image is a root file system with /data/hello from a layer "L".
function image() {
  const tree = new Tree();
  tree.addLayer("L", [
    ["data/", "5", 0o750, 33, 33, 100, 0, 0, ""],
    ["data/hello", "0", 0o644, 33, 33, 100, 6, 0, ""],
    ["etc/", "5", 0o755, 0, 0, 100, 0, 0, ""],
    ["var/run", "2", 0o777, 0, 0, 100, 0, 0, "../run"],
  ]);
  const layers = new Map([["L", { read: async (o, n) => enc.encode("hello\n").subarray(o, o + n) }]]);
  return { tree, layers };
}

// boot loads the tree into a v86 FS and mounts the plan, like vmworker.js.
async function boot(specs, { posted = [] } = {}) {
  const { tree, layers } = image();
  const mem = new Map(), volumes = new Map();
  const plan = await prepareMounts(specs, tree, mem);
  for (const p of plan) if (p.vol) volumes.set(p.vol.name, p.vol);
  const storage = new LayerStorage(volumeReaders(memReaders(layers, mem), volumes), new Map());
  const root = new FS(storage);
  root.load_from_json(tree.v86());
  const mounts = attachMounts(root, plan, copyStorage(storage), (m) => posted.push(m));
  await Promise.all(mounts.map((m) => m.pending));
  return { root, mounts, plan, tree };
}

async function readFile(fs, path) {
  const { id } = fs.SearchPath(path);
  assert.ok(id >= 0, `${path} exists`);
  return dec.decode(await fs.Read(id, 0, fs.GetInode(id).size));
}

async function writeFile(fs, path, text) {
  const { id, parentid, name } = fs.SearchPath(path);
  const f = id >= 0 ? id : fs.CreateFile(name, parentid);
  const b = enc.encode(text);
  await fs.Write(f, 0, b.length, b);
  return f;
}

async function index(name) {
  const d = await (await opfs.getDirectoryHandle("tabernetes")).getDirectoryHandle("volumes");
  const f = await (await (await d.getDirectoryHandle(name)).getFileHandle("index.json")).getFile();
  return JSON.parse(await f.text());
}

test("Tree.resolve follows links above the path and take keeps the parents", () => {
  const { tree } = image();
  assert.equal(tree.resolve("/var/run/x/y"), "run/x/y");
  assert.equal(tree.resolve("/data/hello"), "data/hello");
  const taken = tree.take("data");
  assert.deepEqual([...taken.keys()].sort(), ["", "hello"]);
  assert.equal(tree.files.has("data"), false);
  tree.take("a/b/c");
  assert.equal(tree.files.has("a/b"), true);
  assert.equal(tree.files.has("a/b/c"), false);
});

test("a new volume gets the files of the image, and a second container sees what the first wrote", async () => {
  const vol = { type: "volume", name: "data", destination: "/data" };
  const first = await boot([vol]);
  // copied up at once, with the mode and the owner of the directory
  let idx = await index("data");
  assert.equal(idx.files.hello[5].startsWith("vol:data/"), true);
  assert.deepEqual(idx.files[""].slice(0, 3), [modes.S_IFDIR | 0o750, 33, 33]);
  assert.equal(await readFile(first.root, "/data/hello"), "hello\n");

  await writeFile(first.root, "/data/f", "hi\n");
  first.root.CreateDirectory("sub", first.root.SearchPath("/data").id);
  await first.mounts[0].sync();
  idx = await index("data");
  assert.ok(idx.files.f && idx.files.sub);
  // saved: the data left the memory of the worker
  const { id } = first.root.SearchPath("/data/f");
  assert.equal(first.root.GetInode(id).status, 2);
  assert.equal(await readFile(first.root, "/data/f"), "hi\n");

  // a second container: the volume is not empty, nothing is copied
  const second = await boot([vol]);
  assert.equal(await readFile(second.root, "/data/f"), "hi\n");
  assert.equal(await readFile(second.root, "/data/hello"), "hello\n");

  // both write: the merge is by path
  await writeFile(first.root, "/data/a", "A");
  await writeFile(second.root, "/data/b", "B");
  const unlinked = second.root.SearchPath("/data/hello");
  second.root.Unlink(unlinked.parentid, "hello");
  await first.mounts[0].sync();
  await second.mounts[0].sync();
  idx = await index("data");
  assert.ok(idx.files.a && idx.files.b);
  assert.equal(idx.files.hello, undefined);

  const third = await boot([{ ...vol, copy: false }]);
  assert.equal(await readFile(third.root, "/data/a"), "A");
  assert.equal(await readFile(third.root, "/data/b"), "B");
});

test("nocopy leaves a new volume empty, and the image file stays hidden", async () => {
  const { root } = await boot([{ type: "volume", name: "empty", destination: "/data", copy: false }]);
  assert.equal(root.SearchPath("/data/hello").id, -1);
  assert.ok(root.SearchPath("/data").id >= 0);
});

test("an unchanged volume is not written again", async () => {
  const { mounts } = await boot([{ type: "volume", name: "v", destination: "/data" }]);
  const before = (await index("v")).files.hello[5];
  await mounts[0].sync();
  assert.equal((await index("v")).files.hello[5], before);
});

test("a bind mount sends what changed, once", async () => {
  const posted = [];
  const spec = { type: "bind", destination: "/srv", mode: 0o755, files: [{ path: "index.html", mode: modes.S_IFREG | 0o644, data: enc.encode("old") }] };
  const { root, mounts } = await boot([spec], { posted });
  assert.equal(await readFile(root, "/srv/index.html"), "old");
  await writeFile(root, "/srv/new.txt", "new");
  await mounts[0].sync();
  assert.equal(posted.length, 1);
  assert.deepEqual(posted[0].set.map((e) => e.path), ["new.txt"]);
  assert.equal(dec.decode(posted[0].set[0].data), "new");
  await mounts[0].sync();
  assert.equal(posted.length, 1, "nothing changed since");
  const f = root.SearchPath("/srv/index.html");
  root.Unlink(f.parentid, "index.html");
  await mounts[0].sync();
  assert.deepEqual(posted[1].del, ["index.html"]);
});

test("a bind mount of a file replaces the file of the image and syncs back", async () => {
  const posted = [];
  const { root, mounts } = await boot([{ type: "bind", file: true, destination: "/data/hello", mode: 0o600, data: enc.encode("mine") }], { posted });
  assert.equal(await readFile(root, "/data/hello"), "mine");
  await mounts[0].sync();
  assert.equal(posted.length, 0);
  await writeFile(root, "/data/hello", "MINE");
  await mounts[0].sync();
  assert.equal(dec.decode(posted[0].set[0].data), "MINE");
});

test("mounts inside mounts, read-only paths and links in the path", async () => {
  const { root, plan, mounts } = await boot([
    { type: "volume", name: "inner", destination: "/data/inner" },
    { type: "volume", name: "outer", destination: "/data", readonly: true },
    { type: "volume", name: "run", destination: "/var/run/app" },
  ]);
  assert.deepEqual(readonlyPaths(plan), ["/data"]);
  await writeFile(root, "/data/inner/x", "x");
  await Promise.all(mounts.map((m) => m.sync()));
  assert.ok((await index("inner")).files.x);
  // the outer volume was copied up (read-only or not), without the inner one
  const outer = await index("outer");
  assert.ok(outer.files.hello);
  assert.equal(outer.files["inner/x"], undefined);
  assert.ok(root.SearchPath("/run/app").id >= 0);
});

test("diff saves the data of dirty files and of keys the target does not keep", () => {
  const now = new Map([
    ["", { idx: 0, entry: [modes.S_IFDIR | 0o755, 0, 0, 1, 0, ""] }],
    ["a", { idx: 1, entry: [modes.S_IFREG | 0o644, 0, 0, 1, 1, "vol:v/1:0"] }],
    ["b", { idx: 2, entry: [modes.S_IFREG | 0o644, 0, 0, 1, 1, "L:0"] }],
    ["c", { idx: 3, entry: [modes.S_IFREG | 0o644, 0, 0, 1, 1, "vol:v/3:0"] }],
  ]);
  const base = { "": [modes.S_IFDIR | 0o755, 0, 0, 1, 0, ""], a: [modes.S_IFREG | 0o644, 0, 0, 1, 1, "vol:v/1:0"], c: [modes.S_IFREG | 0o644, 0, 0, 1, 1, "vol:v/3:0"], gone: [modes.S_IFREG, 0, 0, 1, 1, "vol:v/9:0"] };
  const { set, del } = diff(now, base, new Set([3]), (k) => k.startsWith("vol:"));
  assert.deepEqual([...set.keys()], ["b", "c"]);
  assert.ok(set.get("b").data && set.get("c").data);
  assert.deepEqual(del, ["gone"]);
});

test("removeVolume deletes the data and tells its size", async () => {
  const { mounts } = await boot([{ type: "volume", name: "gone", destination: "/data" }]);
  await mounts[0].sync();
  assert.equal(await removeVolume("gone"), 6);
  const d = await (await opfs.getDirectoryHandle("tabernetes")).getDirectoryHandle("volumes");
  await assert.rejects(d.getDirectoryHandle("gone"));
  assert.equal(await removeVolume("never"), 0);
});

test("a tmpfs is an empty directory of the root that is not saved", async () => {
  const { root, mounts, plan } = await boot([{ type: "tmpfs", destination: "/data", mode: 0o700, readonly: true }]);
  assert.equal(mounts.length, 0);
  assert.equal(root.SearchPath("/data/hello").id, -1);
  assert.equal(root.GetInode(root.SearchPath("/data").id).mode, modes.S_IFDIR | 0o700);
  assert.deepEqual(readonlyPaths(plan), ["/data"]);
});

test("old data files go at once when no other VM uses the volume", async () => {
  const dataFiles = async () => {
    const d = await (await (await (await opfs.getDirectoryHandle("tabernetes")).getDirectoryHandle("volumes")).getDirectoryHandle("data")).getDirectoryHandle("d");
    return [...d.entries.keys()].length;
  };
  const { root, mounts } = await boot([{ type: "volume", name: "data", destination: "/data" }]);
  assert.equal(await dataFiles(), 1); // hello, copied up
  await writeFile(root, "/data/hello", "HELLO!");
  await mounts[0].sync();
  assert.equal(await dataFiles(), 1);
  // another VM uses it: the old file stays for it
  holders = 2;
  await writeFile(root, "/data/hello", "hello?");
  await mounts[0].sync();
  assert.equal(await dataFiles(), 2);
  assert.equal(await readFile(root, "/data/hello"), "hello?");
});
