// Command patch prepares a Kubernetes checkout for GOOS=js GOARCH=wasm:
//
//	go run . <kubernetes checkout>
//
// kube-apiserver, kube-controller-manager, kube-scheduler and kubectl then
// build and run in the browser. The command
//
//   - excludes unix-only files from js with build tags,
//   - copies _overlay/ (js stubs and helpers) into the tree,
//   - applies a few content patches,
//   - vendors mvdan.cc/sh (the terminal's shell) at MVDAN_SH_VERSION and
//   - copies tabernetes/ (the main package) to cmd/tabernetes.
//
// Every patch fails when its target is gone. A Kubernetes update shows exactly
// what needs attention.
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed all:_overlay all:tabernetes containerd-js.patch
var files embed.FS

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run . <kubernetes checkout>")
		os.Exit(2)
	}
	if err := os.Chdir(os.Args[1]); err != nil {
		fail("%v", err)
	}

	// Exclude unix-only files from js. The js variant comes from _overlay/ or
	// is a copy of the windows variant.
	for _, f := range []string{
		"vendor/github.com/coreos/go-systemd/v22/journal/journal_unix.go",
		"vendor/go.etcd.io/etcd/client/pkg/v3/fileutil/lock_flock.go",
		"vendor/go.etcd.io/etcd/client/pkg/v3/fileutil/lock_unix.go",
		"vendor/github.com/moby/term/term_unix.go",
		"vendor/github.com/moby/term/termios_unix.go",
		"vendor/github.com/moby/term/termios_nonbsd.go",
		"pkg/probe/dialer_others.go",
	} {
		buildTag(f, "(%s) && !js")
	}
	for unix, windows := range map[string]string{
		"vendor/github.com/coreos/go-systemd/v22/journal/journal_unix.go": "vendor/github.com/coreos/go-systemd/v22/journal/journal_windows.go",
		"staging/src/k8s.io/apiserver/pkg/server/options/serving_unix.go": "staging/src/k8s.io/apiserver/pkg/server/options/serving_windows.go",
		"staging/src/k8s.io/kubectl/pkg/util/umask.go":                    "staging/src/k8s.io/kubectl/pkg/util/umask_windows.go",
		"pkg/scheduler/backend/cache/debugger/signal.go":                  "pkg/scheduler/backend/cache/debugger/signal_windows.go",
		"staging/src/k8s.io/kubectl/pkg/util/term/resizeevents.go":        "staging/src/k8s.io/kubectl/pkg/util/term/resizeevents_windows.go",
	} {
		jsFromWindows(unix, windows)
	}
	// kubectl edit: no editor process in js. _overlay/ has a launcher that
	// runs the editor in-process (the vi of cmd/tabernetes).
	buildTag("staging/src/k8s.io/kubectl/pkg/cmd/util/editor/launcher_others.go", "(%s) && !js")

	// The plain file system helpers work in js as well.
	buildTag("pkg/util/filesystem/util_unix.go", "%s || js")

	copyTree("_overlay", ".")

	// mount-utils: move the unix syscalls into mount_helper_sys_{unix,js}.go.
	edit("staging/src/k8s.io/mount-utils/mount_helper_unix.go",
		"\t\terr = syscall.Access(path, syscall.F_OK)\n\t\tif err == nil {",
		"\t\tif accessExists(path) {",
		"\t\tuname := unix.Utsname{}\n\t\terr := unix.Uname(&uname)\n\t\tif err != nil {\n\t\t\treturn\n\t\t}\n\n\t\tend := bytes.IndexByte(uname.Release[:], 0)\n\t\tv := bytes.SplitN(uname.Release[:end]",
		"\t\trelease, ok := unameRelease()\n\t\tif !ok {\n\t\t\treturn\n\t\t}\n\n\t\tv := bytes.SplitN(release",
		"\t\"golang.org/x/sys/unix\"\n", "",
	)

	// kubectl: no SIGHUP/SIGQUIT in js, and no syscall.Exec for plugins.
	edit("staging/src/k8s.io/kubectl/pkg/util/interrupt/interrupt.go",
		"syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT",
		"syscall.SIGINT, syscall.SIGTERM",
	)
	edit("staging/src/k8s.io/kubectl/pkg/cmd/plugin.go",
		"return syscall.Exec(executablePath,",
		"return syscallExec(executablePath,",
	)
	dropUnusedImport("staging/src/k8s.io/kubectl/pkg/cmd/plugin.go", "syscall")
	// kubectl translations: the first i18n.T() call, in the package init of
	// kubectl/pkg/cmd/options, zips the .po and .mo files and parses them
	// (~0.1 s in Wasm). The terminal sets no LANG, so kubectl always speaks
	// English anyway. Without a loaded locale, i18n.T returns the English text.
	edit("staging/src/k8s.io/kubectl/pkg/util/i18n/i18n.go",
		"\t\treturn LoadTranslations(\"kubectl\", nil)\n",
		"\t\treturn nil\n",
	)

	// RSA key generation is painfully slow in single-threaded wasm: use ECDSA
	// for self-signed certificates.
	edit("staging/src/k8s.io/client-go/util/cert/cert.go",
		"rsa.GenerateKey(cryptorand.Reader, 2048)",
		"ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)",
		"\tif err := pem.Encode(&keyBuffer, &pem.Block{Type: keyutil.RSAPrivateKeyBlockType, Bytes: x509.MarshalPKCS1PrivateKey(priv)}); err != nil {",
		"\tprivDER, err := x509.MarshalECPrivateKey(priv)\n\tif err != nil {\n\t\treturn nil, nil, err\n\t}\n\tif err := pem.Encode(&keyBuffer, &pem.Block{Type: keyutil.ECPrivateKeyBlockType, Bytes: privDER}); err != nil {",
		"\t\"crypto/rsa\"\n",
		"\t\"crypto/ecdsa\"\n\t\"crypto/elliptic\"\n",
	)

	// wasm has no preemption: give the informer a chance to catch up instead of
	// busy-looping.
	edit("pkg/registry/flowcontrol/ensurer/strategy.go",
		"\t\tklog.V(5).InfoS(fmt.Sprintf(\"Something created the %T concurrently\", bootstrap), \"type\", configurationType, \"name\", name)\n\t}",
		"\t\tklog.V(5).InfoS(fmt.Sprintf(\"Something created the %T concurrently\", bootstrap), \"type\", configurationType, \"name\", name)\n\t\ttime.Sleep(100 * time.Millisecond)\n\t}",
	)
	addImport("pkg/registry/flowcontrol/ensurer/strategy.go", "time")

	// All components run in one process: make the signal handler setup
	// idempotent.
	edit("staging/src/k8s.io/apiserver/pkg/server/signal.go",
		"func SetupSignalContext() context.Context {\n\tclose(onlyOneSignalHandler) // panics when called twice\n",
		"func SetupSignalContext() context.Context {\n\tsignalOnce.Do(func() { signalCtx = setupSignalContext() })\n\treturn signalCtx\n}\n\nvar (\n\tsignalOnce sync.Once\n\tsignalCtx  context.Context\n)\n\nfunc setupSignalContext() context.Context {\n\tclose(onlyOneSignalHandler) // panics when called twice\n",
		"\t\"os/signal\"\n",
		"\t\"os/signal\"\n\t\"sync\"\n",
	)

	// js has no fsnotify. The certificate and CA file watchers retry every
	// minute and log an error each time. The periodic check before the watch
	// still reloads the files, so skip the watch quietly.
	for _, f := range []string{
		"staging/src/k8s.io/apiserver/pkg/server/dynamiccertificates/dynamic_cafile_content.go",
		"staging/src/k8s.io/apiserver/pkg/server/dynamiccertificates/dynamic_serving_content.go",
	} {
		edit(f,
			"\tw, err := fsnotify.NewWatcher()\n\tif err != nil {\n\t\treturn fmt.Errorf(\"error creating fsnotify watcher: %v\", err)\n\t}",
			"\tw, err := fsnotify.NewWatcher()\n\tif err != nil {\n\t\treturn nil // tabernetes: no fsnotify in js\n\t}",
		)
	}

	// The shell of the terminal. Its only other dependencies, golang.org/x/sys
	// and golang.org/x/term, are vendored by Kubernetes already.
	vendorModule("mvdan.cc/sh/v3", os.Getenv("MVDAN_SH_VERSION"),
		"expand", "fileutil", "internal", "interp", "pattern", "syntax")

	// A background job ("k port-forward ... &") runs with the context of its
	// command line, which ends with the line. The hook lets tabernetes give each
	// job its own context (tabernetes/jobs.go).
	edit("vendor/mvdan.cc/sh/v3/interp/runner.go",
		"\t\tgo func() {\n\t\t\tr2.Run(ctx, &st2)\n",
		"\t\tctx, done := BackgroundContext(ctx, &st2) // tabernetes\n\t\tgo func() {\n\t\t\tdefer done()\n\t\t\tr2.Run(ctx, &st2)\n",
	)
	write("vendor/mvdan.cc/sh/v3/interp/tabernetes.go", `package interp

import (
	"context"

	"mvdan.cc/sh/v3/syntax"
)

// BackgroundContext returns the context of a background job and the func
// called when the job ends. Added by tabernetes (k8s/patch.go).
var BackgroundContext = func(ctx context.Context, stmt *syntax.Stmt) (context.Context, func()) {
	return ctx, func() {}
}
`)

	// port-forward and proxy serve until the process ends. Here the process is
	// the browser tab, so stop them with the command's context (Ctrl-C,
	// kill %1). port-forward has RunPortForwardContext, the command does not
	// use it.
	edit("staging/src/k8s.io/kubectl/pkg/cmd/portforward/portforward.go",
		"cmdutil.CheckErr(opts.RunPortForward())", "cmdutil.CheckErr(opts.RunPortForwardContext(cmd.Context()))",
	)
	// The status line under the terminal links every forwarded port
	// (tabernetes/forwards.go).
	edit("staging/src/k8s.io/kubectl/pkg/cmd/portforward/portforward.go",
		"\treturn fw.ForwardPorts()\n}",
		`	stopped := make(chan struct{}) // tabernetes: report the ports while they listen
	defer close(stopped)
	go func() {
		select {
		case <-opts.ReadyChannel:
		case <-stopped:
			return
		}
		ports, err := fw.GetPorts()
		if err != nil {
			return
		}
		done := OnForward(opts.Namespace, opts.PodName, ports)
		<-stopped
		done()
	}()
	return fw.ForwardPorts()
}`,
	)
	write("staging/src/k8s.io/kubectl/pkg/cmd/portforward/tabernetes.go", `package portforward

import "k8s.io/client-go/tools/portforward"

// OnForward is called when the local ports listen and returns the func called
// when they stop. Added by tabernetes (k8s/patch.go).
var OnForward = func(namespace, pod string, ports []portforward.ForwardedPort) func() {
	return func() {}
}
`)
	edit("staging/src/k8s.io/kubectl/pkg/cmd/proxy/proxy.go",
		"import (\n", "import (\n\t\"context\"\n",
		"cmdutil.CheckErr(o.RunProxy())", "cmdutil.CheckErr(o.runProxyContext(cmd.Context()))",
		"func (o ProxyOptions) RunProxy() error {\n",
		"func (o ProxyOptions) RunProxy() error { return o.runProxyContext(context.Background()) }\n\nfunc (o ProxyOptions) runProxyContext(ctx context.Context) error {\n",
		"\treturn server.ServeOnListener(l)\n",
		"\tstop := context.AfterFunc(ctx, func() { l.Close() })\n\tdefer stop()\n\tif err := server.ServeOnListener(l); ctx.Err() == nil {\n\t\treturn err\n\t}\n\treturn nil\n",
	)

	// The tabernetes main package.
	if err := os.RemoveAll("cmd/tabernetes"); err != nil {
		fail("%v", err)
	}
	copyTree("tabernetes", "cmd/tabernetes")

	// containerd for the docker-in-wasm spike (docs/docker-in-wasm.md). It is
	// patched for js/wasm and vendored so cmd/tabernetes can import the v86 shim.
	vendorContainerd()
}

// vendorContainerd clones containerd at the pinned commit, applies the
// containerd-js patch and vendors containerd plus its dependencies into the
// checkout, the way "go mod vendor" would for a module Kubernetes does not use.
//
// go.mod, go.sum and vendor/modules.txt are edited by hand, like vendorModule
// does, because "go mod vendor" would rewrite the go.mod of the workspace.
func vendorContainerd() {
	commit := os.Getenv("CONTAINERD_COMMIT")
	if commit == "" {
		fail("vendor containerd: no CONTAINERD_COMMIT set")
	}
	if _, err := os.Stat("vendor/github.com/containerd/containerd/v2"); err == nil {
		fail("vendor containerd: already vendored, drop vendorContainerd")
	}

	// Clone containerd at the commit and apply the js/wasm patch. Do not use
	// "go get" or "go mod vendor": Kubernetes vendors by hand (replace to the
	// staging dirs), and those commands would rewrite its go.mod.
	dir, err := os.MkdirTemp("", "tabernetes-containerd-")
	must(err)
	defer os.RemoveAll(dir)
	must(sh("git", "clone", "--quiet", "--filter=blob:none", "https://github.com/containerd/containerd.git", dir))
	must(shIn(dir, "git", "checkout", "--quiet", commit))
	patch, err := files.ReadFile("containerd-js.patch")
	must(err)
	patchFile := filepath.Join(dir, "containerd-js.patch")
	must(os.WriteFile(patchFile, patch, 0o644))
	must(shIn(dir, "git", "apply", patchFile))

	// Copy containerd's own packages and its vendored dependencies into the
	// checkout's vendor tree (the patch is already applied). The shared modules
	// (grpc, protobuf, x/sys, ...) keep the checkout's versions.
	root := filepath.Join("vendor", "github.com", "containerd", "containerd", "v2")
	// cmd/containerd/server runs the daemon in-process (v86/daemon); the
	// commands themselves (containerd, ctr) are not needed.
	for _, pkg := range []string{"client", "cmd/containerd/server", "core", "defaults", "internal", "pkg", "plugins", "version"} {
		if _, err := os.Stat(filepath.Join(dir, pkg)); err == nil {
			must(copyDir(filepath.Join(dir, pkg), filepath.Join(root, pkg)))
		}
	}
	// The "api" directory is a separate module (github.com/containerd/containerd/api),
	// vendored like any other dependency by mergeVendor below.
	mergeVendor(filepath.Join(dir, "vendor"), "vendor", patchedFiles(string(patch)))

	// containerd itself: directly required (not "// indirect"), with its packages
	// listed in modules.txt as explicit. The version is the pseudo-version of the
	// pinned commit (a raw commit hash is not a valid go.mod version).
	version, sum, goModSum := moduleVersion("github.com/containerd/containerd/v2", commit)
	gomod := read("go.mod")
	if !strings.Contains(gomod, "\tgithub.com/containerd/containerd/v2 ") {
		write("go.mod", strings.Replace(gomod, "\nrequire (\n", "\nrequire (\n\tgithub.com/containerd/containerd/v2 "+version+"\n", 1))
	}
	write("go.sum", read("go.sum")+"github.com/containerd/containerd/v2 "+version+" "+sum+"\ngithub.com/containerd/containerd/v2 "+version+"/go.mod "+goModSum+"\n")
	goVersion := regexp.MustCompile(`(?m)^go (\S+)`).FindStringSubmatch(read(filepath.Join(dir, "go.mod")))
	if goVersion == nil {
		fail("vendor containerd: no go directive in go.mod")
	}
	write("vendor/modules.txt", setModule(read("vendor/modules.txt"), vendorModuleEntry{
		path:      "github.com/containerd/containerd/v2",
		version:   version,
		goVersion: goVersion[1],
		explicit:  true,
		pkgs:      packagesIn(root),
	}))
}

// moduleVersion resolves a commit to its pseudo-version and go.sum entries.
func moduleVersion(path, commit string) (version, sum, goModSum string) {
	cmd := exec.Command("go", "mod", "download", "-json", path+"@"+commit)
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	out, err := cmd.Output()
	if err != nil {
		fail("go mod download %s@%s: %v", path, commit, err)
	}
	var mod struct{ Version, Sum, GoModSum string }
	if err := json.Unmarshal(out, &mod); err != nil {
		fail("go mod download %s: %v", path, err)
	}
	return mod.Version, mod.Sum, mod.GoModSum
}

// mergeVendor copies the dependencies that the checkout does not vendor yet
// from containerd's vendor tree into the checkout's, and merges modules.txt.
// Shared modules (grpc, protobuf, x/sys, ...) keep the checkout's versions.
func mergeVendor(srcDir, dstDir string, patched []string) {
	dstModules := filepath.Join(dstDir, "modules.txt")
	dst := read(dstModules)
	gomod := read("go.mod")

	// The modules the checkout already vendors, by path.
	have := make(map[string]vendorModuleEntry)
	for _, m := range parseModules(dst) {
		have[m.path] = m
	}

	for _, m := range parseModules(read(filepath.Join(srcDir, "modules.txt"))) {
		// Modules replaced with a local directory (k8s.io/* => ./staging) are
		// resolved by the replace, not the vendor tree.
		if hasLocalReplace(gomod, m.path) {
			continue
		}
		// Keep the checkout's version for shared modules (grpc, x/sys, otel,
		// ...), except where takesContainerdVersion says otherwise.
		old, vendored := have[m.path]
		if vendored && !takesContainerdVersion(m.path) {
			// Kubernetes vendors only the packages it uses. Add the others that
			// containerd lists, at the version of the checkout.
			if pkgs := addPackages(old, m.pkgs, dstDir); len(pkgs) > len(old.pkgs) {
				old.pkgs = pkgs
				dst = setModule(dst, old)
			}
			// The containerd-js patch also edits some vendored modules (bbolt
			// for js/wasm). Those edits must reach the checkout's copy, which
			// works only at the same version.
			for _, f := range patched {
				if !strings.HasPrefix(f, "vendor/"+m.path+"/") {
					continue
				}
				if old.version != m.version {
					fail("vendor containerd: the patch edits %s, but the checkout vendors %s %s, not %s", f, m.path, old.version, m.version)
				}
				rel := strings.TrimPrefix(f, "vendor/")
				b, err := os.ReadFile(filepath.Join(srcDir, rel))
				must(err)
				must(os.MkdirAll(filepath.Dir(filepath.Join(dstDir, rel)), 0o755))
				must(os.WriteFile(filepath.Join(dstDir, rel), b, 0o644))
			}
			continue
		}
		// An overwritten module loses the files of its old version first: a file
		// that the new version dropped would still compile.
		for _, pkg := range old.pkgs {
			removeGoFiles(filepath.Join(dstDir, pkg))
		}
		for _, pkg := range m.pkgs {
			must(copyDir(filepath.Join(srcDir, pkg), filepath.Join(dstDir, pkg)))
		}
		gomod = setRequire(gomod, m.path, m.version)
		setWorkspaceRequires(m.path, m.version)
		m.explicit = true // like the require
		dst = setModule(dst, m)
	}
	write("go.mod", gomod)
	write(dstModules, dst)
}

// addPackages vendors the packages of want that the module m does not vendor
// yet, from the module cache at m's version, and returns the new package list.
// A package that this version does not have is left out: the build fails on it
// only if containerd really imports it on js/wasm (most are Windows or test
// packages).
func addPackages(m vendorModuleEntry, want []string, dstDir string) []string {
	have := make(map[string]bool)
	for _, pkg := range m.pkgs {
		have[pkg] = true
	}
	var missing []string
	for _, pkg := range want {
		if !have[pkg] {
			missing = append(missing, pkg)
		}
	}
	if len(missing) == 0 {
		return m.pkgs
	}
	cmd := exec.Command("go", "mod", "download", "-json", m.path+"@"+m.version)
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fail("vendor %s@%s: go mod download: %v", m.path, m.version, err)
	}
	var mod struct{ Dir string }
	must(json.Unmarshal(out, &mod))

	pkgs := m.pkgs
	for _, pkg := range missing {
		src := filepath.Join(mod.Dir, strings.TrimPrefix(strings.TrimPrefix(pkg, m.path), "/"))
		entries, err := os.ReadDir(src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "patch: %s@%s has no package %s, left out\n", m.path, m.version, pkg)
			continue
		}
		dst := filepath.Join(dstDir, pkg)
		must(os.MkdirAll(dst, 0o755))
		for _, e := range entries {
			// Like "go mod vendor": the files of the package, without tests and
			// without files that build with the ignore tag.
			if e.IsDir() || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(src, e.Name()))
			must(err)
			if strings.HasSuffix(e.Name(), ".go") && bytes.Contains(b, []byte("//go:build ignore")) {
				continue
			}
			must(os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644))
		}
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)
	return pkgs
}

// removeGoFiles removes the .go files of one package directory, not of the
// packages below it.
func removeGoFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return
	}
	must(err)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			must(os.Remove(filepath.Join(dir, e.Name())))
		}
	}
}

// takesContainerdVersion reports whether a module that both vendor trees have
// is overwritten with containerd's version. That is containerd's own modules,
// which Kubernetes vendors at the versions of the old (v1) containerd, and
// logrus: containerd/log v0.2.0 imports logrus/hooks/slog (logrus v1.10).
func takesContainerdVersion(path string) bool {
	return strings.HasPrefix(path, "github.com/containerd/") || path == "github.com/sirupsen/logrus"
}

// patchedFiles lists the files that a unified diff changes or adds.
func patchedFiles(patch string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`(?m)^diff --git a/(\S+) b/`).FindAllStringSubmatch(patch, -1) {
		out = append(out, m[1])
	}
	return out
}

// hasLocalReplace reports whether go.mod replaces path with a local directory
// (e.g. "k8s.io/api => ./staging/src/k8s.io/api").
func hasLocalReplace(gomod, path string) bool {
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(path) + `(?: v\S+)? => ./`)
	return re.MatchString(gomod)
}

// setWorkspaceRequires sets the version of path in the go.mod of every other
// module of go.work that requires it (the staging modules): in workspace mode,
// "go build" checks every require against vendor/modules.txt.
func setWorkspaceRequires(path, version string) {
	use := regexp.MustCompile(`(?s)\nuse \((.*?)\n\)`).FindStringSubmatch(read("go.work"))
	if use == nil {
		fail("go.work: patch target not found: use block")
	}
	line := regexp.MustCompile(`(?m)^(\t` + regexp.QuoteMeta(path) + `) v\S+`)
	for _, dir := range strings.Fields(use[1]) {
		if dir == "." {
			continue // the main go.mod, see setRequire
		}
		gomod := filepath.Join(dir, "go.mod")
		if s := read(gomod); line.MatchString(s) {
			write(gomod, line.ReplaceAllString(s, "${1} "+version))
		}
	}
}

// setRequire sets the require version of path in a go.mod. The "// indirect"
// comment is not added: in workspace mode every require counts as explicit.
func setRequire(gomod, path, version string) string {
	line := regexp.MustCompile(`(?m)^\t` + regexp.QuoteMeta(path) + ` v\S+.*\n`)
	gomod = line.ReplaceAllString(gomod, "")
	if !strings.Contains(gomod, "\nrequire (\n") {
		fail("go.mod: patch target not found: require block")
	}
	return strings.Replace(gomod, "\nrequire (\n", "\nrequire (\n\t"+path+" "+version+"\n", 1)
}

// setModule replaces the block of m.path in a modules.txt, or inserts it where
// the sort order by module path puts it. A required module must be marked
// explicit (m.explicit). The go version of the module must stay:
// without "go X" the compiler builds its packages as go1.16 (no generics).
//
// The blocks of modules.txt are not separated by blank lines. A block ends
// where the next "# " header starts, so every other block stays byte for byte.
func setModule(dst string, m vendorModuleEntry) string {
	block := []string{"# " + m.path + " " + m.version}
	var notes []string
	if m.explicit {
		notes = append(notes, "explicit")
	}
	if m.goVersion != "" {
		notes = append(notes, "go "+m.goVersion)
	}
	if len(notes) > 0 {
		block = append(block, "## "+strings.Join(notes, "; "))
	}
	block = append(block, m.pkgs...)

	lines := strings.Split(dst, "\n")
	start, end, found := moduleBlock(lines, m.path)
	if !found {
		// Insert before the first module that sorts after m.path, else at the
		// end (before the empty string after the final newline).
		start = len(lines)
		if start > 0 && lines[start-1] == "" {
			start--
		}
		for i, line := range lines {
			if path, ok := moduleHeader(line); ok && path > m.path {
				start = i
				break
			}
		}
		end = start
	}
	out := make([]string, 0, len(lines)+len(block))
	out = append(out, lines[:start]...)
	out = append(out, block...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n")
}

// moduleBlock finds the lines [start, end) of the block of path: its header,
// its "## " annotations and its packages, up to the next header.
func moduleBlock(lines []string, path string) (start, end int, found bool) {
	for i, line := range lines {
		if p, ok := moduleHeader(line); ok && p == path {
			end = i + 1
			for end < len(lines) {
				if _, ok := moduleHeader(lines[end]); ok || lines[end] == "" {
					break
				}
				end++
			}
			return i, end, true
		}
	}
	return 0, 0, false
}

// moduleHeader returns the module path of a "# path version" or
// "# path [version] => replacement" line of modules.txt.
func moduleHeader(line string) (string, bool) {
	if !strings.HasPrefix(line, "# ") {
		return "", false
	}
	fields := strings.Fields(line[2:])
	if len(fields) == 0 {
		return "", false
	}
	return fields[0], true
}

// packagesIn lists the packages (directories with non-test .go files) under a
// vendored module root, as full module package paths.
func packagesIn(root string) []string {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
				out = append(out, filepath.ToSlash(strings.TrimPrefix(p, "vendor"+string(filepath.Separator))))
				break
			}
		}
		return nil
	})
	if err != nil {
		fail("%v", err)
	}
	return out
}

type vendorModuleEntry struct {
	path, version string
	goVersion     string // from "## explicit; go X"
	explicit      bool
	pkgs          []string
}

// parseModules parses a vendor/modules.txt into its module blocks. Lines
// before the first header ("## workspace") belong to no block.
func parseModules(src string) []vendorModuleEntry {
	var out []vendorModuleEntry
	var cur *vendorModuleEntry
	for _, line := range strings.Split(src, "\n") {
		switch {
		case strings.HasPrefix(line, "# "):
			path, _ := moduleHeader(line)
			e := vendorModuleEntry{path: path}
			if fields := strings.Fields(line[2:]); len(fields) > 1 && fields[1] != "=>" {
				e.version = fields[1]
			}
			out = append(out, e)
			cur = &out[len(out)-1]
		case strings.HasPrefix(line, "## "):
			if cur == nil {
				continue
			}
			for _, part := range strings.Split(line[3:], ";") {
				part = strings.TrimSpace(part)
				switch {
				case part == "explicit":
					cur.explicit = true
				case strings.HasPrefix(part, "go "):
					cur.goVersion = strings.TrimPrefix(part, "go ")
				}
			}
		case line != "":
			if cur != nil {
				cur.pkgs = append(cur.pkgs, line)
			}
		}
	}
	return out
}

func cwd() string {
	d, err := os.Getwd()
	must(err)
	return d
}

func sh(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "GOWORK=off")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func shIn(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// copyDir copies src into dst recursively, overwriting what exists (the same
// way "cp -r" merges directories). dst's parent is created.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// vendorModule adds packages of a module that Kubernetes does not use to the
// vendor directory, the way "go mod vendor" would: go.mod, go.sum,
// vendor/modules.txt and the sources without tests.
func vendorModule(path, version string, pkgs ...string) {
	if version == "" {
		fail("vendor %s: no version set", path)
	}
	cmd := exec.Command("go", "mod", "download", "-json", path+"@"+version)
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fail("vendor %s: go mod download: %v", path, err)
	}
	var mod struct{ Dir, GoMod, Sum, GoModSum string }
	if err := json.Unmarshal(out, &mod); err != nil {
		fail("vendor %s: %v", path, err)
	}
	goVersion := regexp.MustCompile(`(?m)^go (\S+)`).FindStringSubmatch(read(mod.GoMod))
	if goVersion == nil {
		fail("vendor %s: no go directive in go.mod", path)
	}

	modules := read("vendor/modules.txt")
	if strings.Contains(modules, "\n# "+path+" ") {
		fail("vendor %s: Kubernetes vendors it already, drop vendorModule", path)
	}
	entry := fmt.Sprintf("# %s %s\n## explicit; go %s\n", path, version, goVersion[1])
	for _, p := range pkgs {
		entry += path + "/" + p + "\n"
	}
	// modules.txt is sorted by module path.
	at := len(modules)
	for _, m := range regexp.MustCompile(`(?m)^# (\S+) `).FindAllStringSubmatchIndex(modules, -1) {
		if modules[m[2]:m[3]] > path {
			at = m[0]
			break
		}
	}
	write("vendor/modules.txt", modules[:at]+entry+modules[at:])
	gomod := read("go.mod")
	if !strings.Contains(gomod, "\nrequire (\n") {
		fail("go.mod: patch target not found: require block")
	}
	write("go.mod", strings.Replace(gomod, "\nrequire (\n", "\nrequire (\n\t"+path+" "+version+"\n", 1))
	write("go.sum", read("go.sum")+fmt.Sprintf("%s %s %s\n%s %s/go.mod %s\n", path, version, mod.Sum, path, version, mod.GoModSum))

	dst := filepath.Join("vendor", path)
	for _, p := range pkgs {
		entries, err := os.ReadDir(filepath.Join(mod.Dir, p))
		if err != nil {
			fail("vendor %s: %v", path, err)
		}
		must(os.MkdirAll(filepath.Join(dst, p), 0o755))
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(mod.Dir, p, e.Name()))
			must(err)
			// "go mod vendor" drops files with an ignore build tag as well.
			if bytes.Contains(b, []byte("//go:build ignore")) {
				continue
			}
			must(os.WriteFile(filepath.Join(dst, p, e.Name()), b, 0o644))
		}
	}
	b, err := os.ReadFile(filepath.Join(mod.Dir, "LICENSE"))
	must(err)
	must(os.WriteFile(filepath.Join(dst, "LICENSE"), b, 0o644))
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, "patch: "+msg)
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		// An annotation puts the failing patch at the top of the run and the PR.
		fmt.Printf("::error file=tabernetes/k8s/patch.go,title=Kubernetes patch failed::%s\n", msg)
	}
	os.Exit(1)
}

func read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		fail("%v", err)
	}
	return string(b)
}

func write(path, s string) {
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		fail("%v", err)
	}
}

// edit replaces each old with its new in path. pairs is old, new, old, new, ...
func edit(path string, pairs ...string) {
	s := read(path)
	for i := 0; i < len(pairs); i += 2 {
		if !strings.Contains(s, pairs[i]) {
			fail("%s: patch target not found: %q", path, pairs[i])
		}
		s = strings.ReplaceAll(s, pairs[i], pairs[i+1])
	}
	write(path, s)
}

var (
	goBuild   = regexp.MustCompile(`(?m)^//go:build (.+)$`)
	plusBuild = regexp.MustCompile(`(?m)^// \+build .*\n`)
)

// buildTag rewrites the //go:build line of path with format, where %s is the
// old expression. Legacy // +build lines are dropped.
func buildTag(path, format string) {
	s := read(path)
	m := goBuild.FindStringSubmatchIndex(s)
	if m == nil {
		fail("%s: patch target not found: //go:build line", path)
	}
	expr := s[m[2]:m[3]]
	if strings.Contains(expr, "js") {
		fail("%s: already patched: %s", path, expr)
	}
	s = s[:m[2]] + fmt.Sprintf(format, expr) + s[m[3]:]
	write(path, plusBuild.ReplaceAllString(s, ""))
}

// jsFromWindows excludes unix from js and copies windows to a _js.go file
// with a js build tag.
func jsFromWindows(unix, windows string) {
	if !strings.Contains(read(unix), "&& !js") {
		buildTag(unix, "(%s) && !js")
	}
	s := plusBuild.ReplaceAllString(goBuild.ReplaceAllString(read(windows), ""), "")
	write(strings.TrimSuffix(windows, "_windows.go")+"_js.go", "//go:build js\n\n"+strings.TrimLeft(s, "\n"))
}

func dropUnusedImport(path, pkg string) {
	if regexp.MustCompile(`\b` + pkg + `\.[A-Z]`).MatchString(read(path)) {
		fail("%s: %s is still used", path, pkg)
	}
	edit(path, "\t\""+pkg+"\"\n", "")
}

func addImport(path, pkg string) {
	if !strings.Contains(read(path), "\t\""+pkg+"\"\n") {
		edit(path, "import (\n", "import (\n\t\""+pkg+"\"\n")
	}
}

// copyTree copies the embedded directory src into dst.
func copyTree(src, dst string) {
	err := fs.WalkDir(files, src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, strings.TrimPrefix(path, src+"/"))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		fail("%v", err)
	}
}
