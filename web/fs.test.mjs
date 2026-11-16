// Tests of the files that survive a reload (src/fs.js, saveHome in
// src/state.js): big files are saved in chunks, and a save writes only the
// chunks that changed.
import assert from "node:assert/strict";
import test from "node:test";

// IndexedDB is not in Node.js: a range of keys is enough for applyHome.
globalThis.IDBKeyRange ??= { bound: (lower, upper) => ({ lower, upper }) };
const { createFS, CHUNK } = await import("./src/fs.js");
const { applyHome, homeKey, sortHome } = await import("./src/state.js");

// store is an object store in memory, with the keys of the puts counted.
function store() {
  const m = new Map();
  const s = {
    m,
    puts: [],
    put(r, key) { m.set(key, structuredClone(r)); s.puts.push(key); },
    delete(key) {
      if (typeof key === "string") m.delete(key);
      else for (const k of [...m.keys()]) if (k >= key.lower && k <= key.upper) m.delete(k);
    },
  };
  return s;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const saved = () => sleep(400); // a save runs 300 ms after the last change

// fsys returns a file system that keeps /var/log/v86 in st, with the
// records restored from it, and Go's callback API made synchronous.
function fsys(st) {
  const fs = createFS({
    persist: {
      keep: ["/var/log/v86"],
      skip: [],
      save: async (put, del) => applyHome(st, put, del),
    },
  });
  fs.mkdirSync("/var/log/v86");
  fs.restore(sortHome([...st.m.values()]));
  const sync = (name) => (...a) => {
    let out, err;
    fs[name](...a, (e, r) => { err = e; out = r; });
    if (err) throw err;
    return out;
  };
  const f = Object.fromEntries(["open", "write", "close", "ftruncate", "unlink", "rename", "mkdir"].map((n) => [n, sync(n)]));
  f.read = (path) => fs.readFileSync(path);
  f.writeAt = (path, bytes, pos) => {
    const fd = f.open(path, 0o101, 0o644); // O_WRONLY|O_CREAT
    f.write(fd, bytes, 0, bytes.length, pos);
    f.close(fd);
  };
  return f;
}

const bytes = (n, seed) => Uint8Array.from({ length: n }, (_, i) => (i * 7 + seed) & 0xff);

test("a big file is saved in chunks, and only the chunks that changed", async () => {
  const st = store();
  const f = fsys(st);
  const log = "/var/log/v86/c1.log";

  f.writeAt(log, bytes(100, 1), 0);
  await saved();
  assert.deepEqual([...st.m.keys()], [log], "a small file is one record");

  const big = bytes(3 * CHUNK + 100, 2);
  f.writeAt(log, big, 0);
  await saved();
  assert.equal(st.m.get(log).chunks, 4);
  assert.equal(st.m.get(log).data, undefined, "the data is in the chunks");
  assert.equal(st.m.size, 5);

  // a log grows at its end: one chunk
  st.puts.length = 0;
  f.writeAt(log, bytes(10, 3), big.length);
  await saved();
  assert.deepEqual(st.puts, [log, homeKey({ path: log, chunk: 3 })]);

  // bbolt writes its meta page at the start and a page in the middle
  st.puts.length = 0;
  f.writeAt(log, bytes(16, 4), 0);
  f.writeAt(log, bytes(16, 5), 2 * CHUNK + 5);
  await saved();
  assert.deepEqual(st.puts, [log, homeKey({ path: log, chunk: 0 }), homeKey({ path: log, chunk: 2 })]);

  // the next start finds the same bytes
  const want = f.read(log);
  assert.deepEqual(fsys(st).read(log), want);
});

test("a file that shrinks, moves or goes leaves no chunks behind", async () => {
  const st = store();
  const f = fsys(st);
  f.mkdir("/var/log/v86/a", 0o755);
  const p = "/var/log/v86/a/db";
  f.writeAt(p, bytes(5 * CHUNK, 6), 0);
  await saved();
  assert.equal(st.m.size, 1 + 1 + 5);

  // truncate to two chunks and a bit: the chunks after go
  const fd = f.open(p, 2, 0);
  f.ftruncate(fd, 2 * CHUNK + 1);
  f.close(fd);
  await saved();
  assert.equal(st.m.get(p).chunks, 3);
  assert.equal([...st.m.keys()].filter((k) => k.startsWith(p + "\0")).length, 3);
  assert.deepEqual(fsys(st).read(p), f.read(p));

  // a moved directory takes all chunks of its files along
  f.rename("/var/log/v86/a", "/var/log/v86/b");
  await saved();
  assert.deepEqual([...st.m.keys()].filter((k) => k.startsWith("/var/log/v86/a")), []);
  assert.deepEqual(fsys(st).read("/var/log/v86/b/db"), f.read("/var/log/v86/b/db"));

  // small again: one record with the data
  const fd2 = f.open("/var/log/v86/b/db", 2, 0);
  f.ftruncate(fd2, 10);
  f.close(fd2);
  await saved();
  assert.deepEqual([...st.m.keys()].sort(), ["/var/log/v86/b", "/var/log/v86/b/db"]);
  assert.equal(st.m.get("/var/log/v86/b/db").data.length, 10);

  f.unlink("/var/log/v86/b/db");
  await saved();
  assert.deepEqual([...st.m.keys()], ["/var/log/v86/b"]);
});

test("a write after the end leaves zeros, not old bytes", async () => {
  const st = store();
  const f = fsys(st);
  const p = "/var/log/v86/hole";
  f.writeAt(p, bytes(100, 7), 0);
  const fd = f.open(p, 2, 0);
  f.ftruncate(fd, 10);
  f.close(fd);
  f.writeAt(p, bytes(1, 8), 50);
  assert.deepEqual([...f.read(p).subarray(10, 50)], new Array(40).fill(0));
});

test("the data of containerd is saved at once, even while a log grows", async () => {
  const st = store();
  const fs = createFS({
    persist: { keep: ["/var/log/v86", "/run/containerd"], skip: [], save: async (put, del) => applyHome(st, put, del) },
  });
  fs.mkdirSync("/var/log/v86");
  fs.mkdirSync("/run/containerd");
  const write = (path, s) => fs.writeFileSync(path, s);
  write("/run/containerd/state.json", "{}");
  // a log that writes every 20 ms would put a plain save off for 3 s
  const t0 = Date.now();
  let got = 0;
  while (Date.now() - t0 < 200 && !got) {
    write("/var/log/v86/c.log", String(Date.now()));
    await sleep(20);
    if (st.m.has("/run/containerd/state.json")) got = Date.now() - t0;
  }
  assert.ok(got > 0 && got < 150, `saved after ${got} ms`);
  assert.equal(fs.saveStats.saves >= 1, true);

  // flush saves what changed at once
  write("/var/log/v86/d.log", "x");
  fs.flush();
  await sleep(0);
  assert.ok(st.m.has("/var/log/v86/d.log"));
});
