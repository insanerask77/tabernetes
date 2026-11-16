// docker cp: files and directories between a container and the file system
// of the shell, both ways, and "-" for a tar on stdin or stdout.
//
// The Docker API (dockerapi/archive.go) asks dockerFiles for the files. A
// running container has them in the 9p tree of its VM, in the worker of the
// VM (web/src/cpfs.js): init first syncs and drops its caches, so the tree
// and the VM agree (sync_caches in vm/init/init.c). This needs no tar in the
// image. A stopped container has no VM: a worker without one merges its
// image again, with the files that docker cp put into the container on top
// (v86.CopiedFiles), which also go into the container when it starts again.
//
// The command follows the docker CLI (cli/command/container/cp.go and
// moby's pkg/archive/copy.go): the destination decides whether the files go
// into a directory or take a new name, and it prints "Successfully copied".
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall/js"
	"time"

	"github.com/containerd/containerd/v2/core/runtime/v2/v86"
	"github.com/containerd/containerd/v2/core/runtime/v2/v86/dockerapi"
	"github.com/containerd/containerd/v2/core/runtime/v2/v86/jsruntime"
	"github.com/containerd/errdefs"
	units "github.com/docker/go-units"
	"mvdan.cc/sh/v3/interp"
)

// ---- the side of the Docker API ----

// dockerFiles is the ContainerFiles of the Docker API.
type dockerFiles struct{ rt *jsruntime.Runtime }

// syncVM makes the 9p tree and the VM of container id agree. An older init
// does not know the request; then the files written last may be missing.
func (d dockerFiles) syncVM(ctx context.Context, id string) {
	dial, err := dockerStreams{d.rt}.dialer(id)
	if err != nil {
		return
	}
	if _, err := initRequest(ctx, dial, map[string]bool{"sync": true}); err != nil && !errors.Is(err, errOldInit) {
		fmt.Printf("tabernetes: docker cp: sync container %s: %v\n", shortID(id), err)
	}
}

// read asks the VM worker for path: readFiles, or statFile with stat.
func (d dockerFiles) read(ctx context.Context, id, image, p string, running, stat bool) (js.Value, error) {
	var v js.Value
	var err error
	if h, ok := d.rt.VM(id); running && ok {
		d.syncVM(ctx, id)
		method := "readFiles"
		if stat {
			method = "statFile"
		}
		v, err = await(ctx, h.Call(method, p, false))
	} else {
		vm := dockerVMFor(id)
		if vm.Type() != js.TypeObject {
			vm = anyTabVM()
		}
		if vm.Type() != js.TypeObject {
			return js.Undefined(), errors.New("docker cp of a stopped container needs a tab")
		}
		files, _ := v86.CopiedFiles(id)
		v, err = await(ctx, vm.Call("imageFiles", image, jsruntime.JSCopied(files), p, false, stat))
	}
	if err != nil {
		return js.Undefined(), err
	}
	if v.Get("notFound").Truthy() {
		return js.Undefined(), errdefs.ErrNotFound
	}
	return v, nil
}

// anyTabVM returns the vm object of an open tab, or undefined.
func anyTabVM() js.Value {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	for _, s := range sessions {
		if vm := s.host.Get("vm"); vm.Type() == js.TypeObject {
			return vm
		}
	}
	return js.Undefined()
}

func statOf(v js.Value) dockerapi.PathStat {
	st := v.Get("stat")
	return dockerapi.PathStat{
		Name:       st.Get("name").String(),
		Size:       int64(st.Get("size").Int()),
		Mode:       dockerapi.FileModeOf(uint32(st.Get("mode").Int())),
		Mtime:      time.Unix(int64(st.Get("mtime").Int()), 0),
		LinkTarget: st.Get("target").String(),
	}
}

func (d dockerFiles) Stat(ctx context.Context, id, image, p string, running bool) (dockerapi.PathStat, error) {
	v, err := d.read(ctx, id, image, p, running, true)
	if err != nil {
		return dockerapi.PathStat{}, err
	}
	return statOf(v), nil
}

func (d dockerFiles) Archive(ctx context.Context, id, image, p string, running bool) (dockerapi.PathStat, []byte, error) {
	v, err := d.read(ctx, id, image, p, running, false)
	if err != nil {
		return dockerapi.PathStat{}, nil, err
	}
	entries := v.Get("entries")
	files := make([]v86.CopiedFile, entries.Length())
	for i := range files {
		e := entries.Index(i)
		f := v86.CopiedFile{
			Path: e.Get("name").String(), Mode: uint32(e.Get("mode").Int()),
			UID: e.Get("uid").Int(), GID: e.Get("gid").Int(), Mtime: int64(e.Get("mtime").Int()),
			LinkTarget: e.Get("target").String(),
		}
		if data := e.Get("data"); data.Type() == js.TypeObject {
			f.Data = make([]byte, data.Get("length").Int())
			js.CopyBytesToGo(f.Data, data)
		}
		files[i] = f
	}
	// "dir/." is the contents of dir: their names start with "." (moby's
	// TarResourceRebase with the base name ".")
	if strings.HasSuffix(p, "/.") && len(files) > 0 {
		files = rebase(files, files[0].Path, ".")
	}
	var b bytes.Buffer
	if err := v86.WriteTar(&b, files); err != nil {
		return dockerapi.PathStat{}, nil, err
	}
	return statOf(v), b.Bytes(), nil
}

func (d dockerFiles) Extract(ctx context.Context, id, image, dir string, running bool, files []v86.CopiedFile) error {
	if h, ok := d.rt.VM(id); running && ok {
		d.syncVM(ctx, id)
		if _, err := await(ctx, h.Call("writeFiles", dir, jsruntime.JSCopied(files))); err != nil {
			return err
		}
		// the kernel of the VM reads the tree again
		d.syncVM(ctx, id)
	}
	// kept for the next start of the container (cpfs.js addFiles)
	kept := make([]v86.CopiedFile, 0, len(files))
	for _, f := range files {
		f.Path = path.Join("/", dir, f.Path)
		kept = append(kept, f)
	}
	return v86.AddCopied(id, kept)
}

// rebase renames the entry old and what is below it to new, as moby's
// RebaseArchiveEntries does.
func rebase(files []v86.CopiedFile, old, new string) []v86.CopiedFile {
	out := make([]v86.CopiedFile, 0, len(files))
	for _, f := range files {
		switch {
		case f.Path == old:
			f.Path = new
		case strings.HasPrefix(f.Path, old+"/"):
			f.Path = new + f.Path[len(old):]
		}
		out = append(out, f)
	}
	return out
}

// ---- the command ----

func init() {
	c := &dockerCommand{
		name: "cp", use: "cp [OPTIONS] CONTAINER:SRC_PATH DEST_PATH|-\n\tdocker cp [OPTIONS] SRC_PATH|- CONTAINER:DEST_PATH",
		short:   "Copy files/folders between a container and the local filesystem",
		aliases: []string{"docker container cp", "docker cp"},
		flags: []flagSpec{
			{'a', "archive", "", "Archive mode (copy all uid/gid information)"},
			{'L', "follow-link", "", "Always follow symbol link in SRC_PATH"},
			{'q', "quiet", "", "Suppress progress output during copy. Progress output is automatically suppressed if no terminal is attached"},
		},
		minArgs: 2, maxArgs: 2, run: dockerCp,
	}
	dockerCommands[c.name] = c
	if m, ok := dockerManagement["container"]; ok {
		m.subs["cp"] = c
	}
}

// The errors of moby's pkg/archive.
var (
	errCannotCopyDir = errors.New("cannot copy directory")
	errDirNotExists  = errors.New("no such directory")
)

// splitCpArg splits CONTAINER:PATH, as the docker CLI does: an absolute
// path or one that starts with "." is local.
func splitCpArg(arg string) (ctr, p string) {
	if filepath.IsAbs(arg) {
		return "", arg
	}
	ctr, p, ok := strings.Cut(arg, ":")
	if !ok || strings.HasPrefix(ctr, ".") {
		return "", arg
	}
	return ctr, p
}

// dockerCp is `docker cp`.
func dockerCp(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, args []string) error {
	srcCtr, src := splitCpArg(args[0])
	dstCtr, dst := splitCpArg(args[1])
	switch {
	case srcCtr != "" && dstCtr != "":
		return fail(hc, 1, "copying between containers is not supported")
	case srcCtr == "" && dstCtr == "":
		return fail(hc, 1, "must specify at least one container source")
	case src == "" && srcCtr != "" || dst == "" && dstCtr != "":
		return fail(hc, 1, "filesystem path cannot be empty")
	}
	var err error
	if srcCtr != "" {
		err = copyFromContainer(ctx, hc, srcCtr, src, dst, f.bool("follow-link"), f.bool("quiet"))
	} else {
		err = copyToContainer(ctx, hc, src, dstCtr, dst, f.bool("follow-link"), f.bool("quiet"))
	}
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	return nil
}

// containerStat is HEAD /containers/{id}/archive: the stat of a path in
// a container, or an error with the text of the docker CLI.
func containerStat(ctx context.Context, ctr, p string) (dockerapi.PathStat, error) {
	var st dockerapi.PathStat
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, dockerAPIBase+"/containers/"+url.PathEscape(ctr)+"/archive?path="+url.QueryEscape(p), nil)
	if err != nil {
		return st, err
	}
	resp, err := localClient.Do(req)
	if err != nil {
		return st, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		// a HEAD has no body: the docker CLI says this
		return st, fmt.Errorf("Error response from daemon: Could not find the file %s in container %s", p, ctr)
	default:
		return st, fmt.Errorf("Error response from daemon: %s", http.StatusText(resp.StatusCode))
	}
	return st, decodeStat(resp.Header, &st)
}

func decodeStat(h http.Header, st *dockerapi.PathStat) error {
	b, err := base64.StdEncoding.DecodeString(h.Get("X-Docker-Container-Path-Stat"))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, st)
}

// copyFromContainer is `docker cp CONTAINER:SRC DEST`.
func copyFromContainer(ctx context.Context, hc interp.HandlerContext, ctr, src, dst string, follow, quiet bool) error {
	if dst != "-" {
		dst = abs(hc, dst)
	}
	// -L: a link is copied as the file it points to, under its own name
	rebaseName := ""
	if follow {
		if st, err := containerStat(ctx, ctr, src); err == nil && st.Mode&os.ModeSymlink != 0 {
			target := st.LinkTarget
			if !path.IsAbs(target) {
				target = path.Join(path.Dir(src), target)
			}
			rebaseName = path.Base(src)
			src = target
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dockerAPIBase+"/containers/"+url.PathEscape(ctr)+"/archive?path="+url.QueryEscape(src), nil)
	if err != nil {
		return err
	}
	resp, err := localClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New(daemonError(body, resp.StatusCode))
	}
	if dst == "-" {
		_, err := hc.Stdout.Write(body)
		return err
	}
	var st dockerapi.PathStat
	if err := decodeStat(resp.Header, &st); err != nil {
		return err
	}
	files, err := v86.ReadTar(bytes.NewReader(body))
	if err != nil {
		return err
	}
	srcBase := path.Base(src)
	if strings.HasSuffix(src, "/.") {
		srcBase = "."
	}
	if rebaseName != "" {
		files = rebase(files, srcBase, rebaseName)
		srcBase = rebaseName
	}
	dir, files, err := prepareCopy(files, srcBase, st.Mode.IsDir(), dst, localStat(dst))
	if err != nil {
		return err
	}
	if err := untarLocal(dir, files); err != nil {
		return err
	}
	if !quiet {
		fmt.Fprintf(hc.Stderr, "Successfully copied %s to %s\n", units.HumanSize(float64(len(body))), dst)
	}
	return nil
}

// copyInfo is what is known of a destination (archive.CopyInfo).
type copyInfo struct{ exists, isDir bool }

func localStat(p string) copyInfo {
	fi, err := os.Stat(p)
	if err != nil {
		return copyInfo{}
	}
	return copyInfo{exists: true, isDir: fi.IsDir()}
}

// prepareCopy is moby's PrepareArchiveCopy: where to extract the files of
// the source (named srcBase) so that they end up at dst, and their names.
func prepareCopy(files []v86.CopiedFile, srcBase string, srcIsDir bool, dst string, info copyInfo) (string, []v86.CopiedFile, error) {
	dstDir, dstBase := path.Dir(strings.TrimSuffix(dst, "/")), path.Base(dst)
	switch {
	case info.exists && info.isDir:
		return dst, files, nil
	case info.exists && srcIsDir:
		return "", nil, errCannotCopyDir
	case info.exists, srcIsDir:
		return dstDir, rebase(files, srcBase, dstBase), nil
	case strings.HasSuffix(dst, "/") || strings.HasSuffix(dst, "/."):
		return "", nil, errDirNotExists
	default:
		return dstDir, rebase(files, srcBase, dstBase), nil
	}
}

// untarLocal writes files into the directory dir of the shell's file
// system, as the docker CLI extracts the tar.
func untarLocal(dir string, files []v86.CopiedFile) error {
	if fi, err := os.Stat(dir); err != nil {
		return err
	} else if !fi.IsDir() {
		return errors.New("not a directory")
	}
	type stamp struct {
		p string
		t time.Time
	}
	var dirs []stamp
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		if rel, err := filepath.Rel(dir, p); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		mode := os.FileMode(f.Mode & 0o777)
		switch f.Mode & v86.ModeType {
		case v86.ModeDir:
			if fi, err := os.Lstat(p); err == nil && !fi.IsDir() {
				os.Remove(p)
			}
			if err := os.MkdirAll(p, mode|0o700); err != nil {
				return err
			}
			os.Chmod(p, mode)
			dirs = append(dirs, stamp{p, time.Unix(f.Mtime, 0)})
		case v86.ModeSymlink:
			os.Remove(p)
			if err := os.Symlink(f.LinkTarget, p); err != nil {
				return err
			}
		case v86.ModeRegular:
			if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
				return fmt.Errorf("cannot overwrite directory %q with non-directory %q", p, f.Path)
			}
			os.Remove(p)
			if err := os.WriteFile(p, f.Data, mode); err != nil {
				return err
			}
			os.Chtimes(p, time.Unix(f.Mtime, 0), time.Unix(f.Mtime, 0))
		}
	}
	// directories last: what was written into them changed their times
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Chtimes(dirs[i].p, dirs[i].t, dirs[i].t)
	}
	return nil
}

// tarLocal reads src of the shell's file system as files named by base
// (moby's TarResourceRebase). follow resolves src itself if it is a link.
func tarLocal(src, base string, follow bool) ([]v86.CopiedFile, bool, error) {
	stat := os.Lstat
	if follow {
		stat = os.Stat
	}
	fi, err := stat(src)
	if err != nil {
		return nil, false, err
	}
	var files []v86.CopiedFile
	add := func(p, name string, fi os.FileInfo) error {
		f := v86.CopiedFile{Path: name, Mode: uint32(fi.Mode().Perm()), Mtime: fi.ModTime().Unix()}
		switch {
		case fi.IsDir():
			f.Mode |= v86.ModeDir
		case fi.Mode()&os.ModeSymlink != 0:
			f.Mode |= v86.ModeSymlink
			if f.LinkTarget, err = os.Readlink(p); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			f.Mode |= v86.ModeRegular
			if f.Data, err = os.ReadFile(p); err != nil {
				return err
			}
		default:
			return nil
		}
		files = append(files, f)
		return nil
	}
	if !fi.IsDir() {
		return files, false, add(src, base, fi)
	}
	err = filepath.Walk(src, func(p string, wfi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == src {
			return add(p, base, fi)
		}
		rel, _ := filepath.Rel(src, p)
		return add(p, path.Join(base, filepath.ToSlash(rel)), wfi)
	})
	return files, true, err
}

// copyToContainer is `docker cp SRC CONTAINER:DEST`.
func copyToContainer(ctx context.Context, hc interp.HandlerContext, src, ctr, dst string, follow, quiet bool) error {
	if src != "-" {
		src = abs(hc, src)
	}
	dstPath := dst
	var info copyInfo
	st, err := containerStat(ctx, ctr, dst)
	if err == nil && st.Mode&os.ModeSymlink != 0 {
		target := st.LinkTarget
		if !path.IsAbs(target) {
			target = path.Join(path.Dir(dst), target)
		}
		dstPath = target
		st, err = containerStat(ctx, ctr, target)
	}
	if err == nil {
		if !st.Mode.IsDir() && !st.Mode.IsRegular() {
			return fmt.Errorf("destination \"%s:%s\" must be a directory or a regular file", ctr, dst)
		}
		info = copyInfo{exists: true, isDir: st.Mode.IsDir()}
	} else if strings.Contains(err.Error(), "No such container") {
		return err
	}

	var tarball []byte
	var dir string
	if src == "-" {
		if !info.isDir {
			return fmt.Errorf("destination \"%s:%s\" must be a directory", ctr, dst)
		}
		if tarball, err = io.ReadAll(hc.Stdin); err != nil {
			return err
		}
		dir = dstPath
	} else {
		base := filepath.Base(src)
		if strings.HasSuffix(src, string(filepath.Separator)+".") || strings.HasSuffix(src, "/.") {
			base = "."
		}
		files, isDir, err := tarLocal(strings.TrimSuffix(src, "/."), base, follow)
		if err != nil {
			return err
		}
		var prepared []v86.CopiedFile
		if dir, prepared, err = prepareCopy(files, base, isDir, dstPath, info); err != nil {
			return err
		}
		var b bytes.Buffer
		if err := v86.WriteTar(&b, prepared); err != nil {
			return err
		}
		tarball = b.Bytes()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, dockerAPIBase+"/containers/"+url.PathEscape(ctr)+"/archive?path="+url.QueryEscape(dir)+"&noOverwriteDirNonDir=true", bytes.NewReader(tarball))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := localClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return errors.New(daemonError(b, resp.StatusCode))
	}
	if !quiet {
		fmt.Fprintf(hc.Stderr, "Successfully copied %s to %s\n", units.HumanSize(float64(len(tarball))), ctr+":"+dstPath)
	}
	return nil
}
