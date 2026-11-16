import { test } from "node:test";
import assert from "node:assert/strict";
import { Tree, addWorkingDir, modes } from "./rootfs.js";

test("addWorkingDir creates a missing working directory like runc", () => {
  const t = new Tree();
  t.set("tmp", { mode: modes.S_IFDIR | 0o1777, uid: 0, gid: 0, mtime: 0 });
  t.set("var/run", { mode: modes.S_IFLNK | 0o777, uid: 0, gid: 0, mtime: 0, target: "/run" });
  t.set("etc/passwd", { mode: modes.S_IFREG | 0o644, uid: 0, gid: 0, mtime: 0, size: 0 });

  addWorkingDir(t, "/srv/app/data", 5);
  for (const p of ["srv", "srv/app", "srv/app/data"]) assert.equal(t.files.get(p).mode & 0o170000, modes.S_IFDIR, p);
  assert.equal(t.files.get("srv/app/data").mode & 0o7777, 0o755);

  // an existing directory keeps its mode
  addWorkingDir(t, "/tmp", 5);
  assert.equal(t.files.get("tmp").mode & 0o7777, 0o1777);
  // through a symbolic link or a file: left to init
  addWorkingDir(t, "/var/run/x", 5);
  assert.equal(t.files.has("var/run/x"), false);
  addWorkingDir(t, "/etc/passwd/x", 5);
  assert.equal(t.files.has("etc/passwd/x"), false);
  // nothing for / or an empty path
  const n = t.files.size;
  addWorkingDir(t, "/", 5);
  addWorkingDir(t, "", 5);
  assert.equal(t.files.size, n);
});
