// The `docker` builtin. It is a thin HTTP client for the Docker Engine API
// that startDocker serves on 127.0.0.1:2375 (dockerapi, vendored with
// containerd in patch.go): run (its options in dockerrunflags.go), exec (-i, -t), logs and version here; port in dockerports.go; stop, kill
// and rm in dockerstop.go; ps, inspect, create, start, restart, attach,
// images, pull, rmi and the help in dockercmds.go. Attach and exec take over
// the HTTP connection, as the docker CLI does.
//
// This file stays free of containerd imports: the CLI only speaks HTTP.
// ensureDocker (docker.go) starts the daemon behind it.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

// dockerAPIBase is where startDocker serves the Docker Engine API.
const dockerAPIBase = "http://127.0.0.1:2375"

// dockerCreateRequest is the subset of the Docker create request this CLI
// sends. The server fills the rest (env, working dir, tty) with defaults.
type dockerCreateRequest struct {
	Image     string   `json:"Image"`
	Cmd       []string `json:"Cmd,omitempty"`
	Env       []string `json:"Env,omitempty"`
	Tty       bool     `json:"Tty"`
	OpenStdin bool     `json:"OpenStdin"`
	StdinOnce bool     `json:"StdinOnce"`
	// -w, -u, -h, --entrypoint ([""] for --entrypoint ""), -l,
	// --stop-signal and --stop-timeout (dockerrunflags.go)
	WorkingDir  string            `json:"WorkingDir,omitempty"`
	User        string            `json:"User,omitempty"`
	Hostname    string            `json:"Hostname,omitempty"`
	Entrypoint  []string          `json:"Entrypoint,omitempty"`
	Labels      map[string]string `json:"Labels,omitempty"`
	StopSignal  string            `json:"StopSignal,omitempty"`
	StopTimeout *int              `json:"StopTimeout,omitempty"`
	// docker run -p (dockerports.go)
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig   *dockerHostConfig   `json:"HostConfig,omitempty"`

	// docker run -v /path: anonymous volumes (dockervolumes.go)
	Volumes map[string]struct{} `json:"Volumes,omitempty"`
}

type dockerCreateResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

type dockerVersion struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
}

// dockerCall sends one request to the API and returns the body and status.
// It uses localClient (coreutils.go) so the request goes through the
// in-process network of the wasm, not the browser's fetch().
func dockerCall(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, dockerAPIBase+path, r)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// the tab that runs a container it starts (docker.go)
	if s := sessionFrom(ctx); s != nil {
		req.Header.Set(dockerTabHeader, s.name)
	}
	resp, err := localClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return b, resp.StatusCode, nil
}

// dockerErr turns a non-2xx response into a message for the terminal. The API
// writes its errors as plain text with http.Error.
func dockerErr(body []byte, status int) string {
	if msg := strings.TrimSpace(string(body)); msg != "" {
		return msg
	}
	return http.StatusText(status)
}

// dockerCmd is the `docker` builtin. It translates the subcommands of the
// docker CLI into HTTP requests against the Docker Engine API.
func dockerCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	// Like the docker CLI: no command prints the help on stderr, help and
	// --help on stdout (dockercmds.go has the commands and their texts).
	switch {
	case len(args) == 0:
		fmt.Fprint(hc.Stderr, dockerUsage())
		return nil
	case args[0] == "--help" || args[0] == "-h" || args[0] == "help" && len(args) == 1:
		fmt.Fprint(hc.Stdout, dockerUsage())
		return nil
	case args[0] == "help":
		args = append(args[1:], "--help")
	}
	path, cmd := "docker "+args[0], dockerCommands[args[0]]
	args = args[1:]
	if m, ok := dockerManagement[path[7:]]; ok {
		if len(args) == 0 || args[0] == "--help" {
			fmt.Fprint(hc.Stdout, managementUsage(path[7:]))
			return nil
		}
		if cmd = m.subs[args[0]]; cmd == nil {
			return fail(hc, 1, "docker: unknown command: %s %s\n\nUsage:  %s COMMAND\n\nRun '%s --help' for more information", path, args[0], path, path)
		}
		path, args = path+" "+args[0], args[1:]
	}
	if cmd == nil {
		return fail(hc, 1, "docker: unknown command: %s\n\nRun 'docker --help' for more information", path)
	}
	if len(args) > 0 && args[0] == "--help" {
		return runDockerCommand(ctx, hc, path, cmd, args)
	}
	// The first docker command starts containerd (docker.go), and every
	// command makes its tab the one that runs new containers.
	if err := ensureDocker(ctx); err != nil {
		return fail(hc, 1, "docker: %v", err)
	}
	return runDockerCommand(ctx, hc, path, cmd, args)
}

// dockerRun creates and starts a container: `docker run [OPTIONS] IMAGE
// [COMMAND] [ARG...]`. Like Docker it stays attached to the output until the
// container exits and returns its exit code, unless -d is given.
func dockerRun(ctx context.Context, hc interp.HandlerContext, args []string) error {
	o, image, cmd, err := parseDockerRun(args)
	if err != nil {
		return fail(hc, 125, "docker: %v\n\nRun 'docker run --help' for more information", err)
	}
	if o.detachKeys != "" && !o.detach {
		// dockerd refuses them at the attach; here before the VM boots
		if _, err := parseDetachKeys(o.detachKeys); err != nil {
			return fail(hc, 1, "docker: Error response from daemon: Invalid detach keys (%s) provided", o.detachKeys)
		}
	}
	// docker run -dit keeps stdin open (for a later attach), but not only
	// for the first attach (dockerRunRequest).
	req, err := dockerRunRequest(hc, o, image, cmd)
	if err != nil {
		return fail(hc, 125, "docker: %v\n\nRun 'docker run --help' for more information", err)
	}
	setMounts(&req, o, hc.Dir)
	// an image that is not there is pulled first (dockercmds.go)
	id, err := dockerCreateContainer(ctx, hc, req, o.name, "run")
	if err != nil {
		return err
	}

	body, status, err := dockerCall(ctx, http.MethodPost, "/containers/"+id+"/start", nil)
	if err != nil {
		return fail(hc, 125, "docker: %v", err)
	}
	if status != http.StatusNoContent {
		return fail(hc, 125, "docker: Error response from daemon: %s", dockerErr(body, status))
	}
	if o.detach {
		fmt.Fprintln(hc.Stdout, id)
		return nil
	}

	// The VM started the process already; the attach sends what it wrote
	// so far too (logs=1), so nothing is missed. -a picks the streams.
	q, stdin := dockerAttachQuery(o)
	so := streamOptions{tty: o.tty, stdin: stdin, resizePath: "/containers/" + id + "/resize"}
	if o.detachKeys != "" {
		// the daemon checks the keys (dockerapi), the CLI watches for them
		q += "&detachKeys=" + url.QueryEscape(o.detachKeys)
		so.detachKeys, _ = parseDetachKeys(o.detachKeys)
	}
	if !o.noSigProxy {
		so.sigProxy = id
	}
	detached, err := dockerStreamWith(ctx, hc, "/containers/"+id+"/attach?"+q, nil, so)
	if err != nil {
		return fail(hc, 1, "docker: %v", err)
	}
	if detached {
		// docker run says nothing when the user detaches
		io.WriteString(termFrom(ctx), "\r\n")
		return nil
	}
	body, status, err = dockerCall(ctx, http.MethodPost, "/containers/"+id+"/wait", nil)
	if err != nil || status != http.StatusOK {
		return fail(hc, 125, "docker: %s", dockerErr(body, status))
	}
	var waited struct{ StatusCode int }
	json.Unmarshal(body, &waited)
	// with --rm, the daemon removed the container already (AutoRemove)
	return exitCode(waited.StatusCode)
}

// dockerAttachQuery is the query of the attach of docker run: stdout and
// stderr, or the streams of -a, and stdin with -i. It reports whether stdin
// is sent.
func dockerAttachQuery(o dockerRunOptions) (string, bool) {
	q := "stream=1&logs=1"
	out := o.attach
	if len(out) == 0 {
		out = []string{"stdout", "stderr"}
	}
	for _, s := range []string{"stdout", "stderr"} {
		if contains(out, s) {
			q += "&" + s + "=1"
		}
	}
	stdin := o.interactive && (len(o.attach) == 0 || contains(o.attach, "stdin"))
	if stdin {
		q += "&stdin=1"
	}
	return q, stdin
}

// dockerExec runs a command in a running container: `docker exec [-i] [-t]
// CONTAINER COMMAND [ARG...]`, and returns its exit code.
func dockerExec(ctx context.Context, hc interp.HandlerContext, args []string) error {
	var interactive, tty bool
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		switch a := args[i]; {
		case a == "--interactive":
			interactive = true
		case a == "--tty":
			tty = true
		case len(a) > 1 && a[1] != '-' && strings.Trim(a[1:], "it") == "":
			interactive = interactive || strings.Contains(a, "i")
			tty = tty || strings.Contains(a, "t")
		default:
			return fail(hc, 125, "docker: unknown flag: %s", a)
		}
	}
	if len(args)-i < 2 {
		return fail(hc, 125, "docker: \"exec\" requires at least 2 arguments")
	}
	container, cmd := args[i], args[i+1:]
	body, status, err := dockerCall(ctx, http.MethodPost, "/containers/"+container+"/exec", map[string]any{
		"AttachStdin": interactive, "AttachStdout": true, "AttachStderr": true, "Tty": tty, "Cmd": cmd,
	})
	if err != nil {
		return fail(hc, 125, "docker: %v", err)
	}
	if status != http.StatusCreated {
		return fail(hc, 125, "Error response from daemon: %s", dockerErr(body, status))
	}
	var created struct{ Id string }
	json.Unmarshal(body, &created)
	if _, err := dockerStream(ctx, hc, "/exec/"+created.Id+"/start", map[string]any{"Detach": false, "Tty": tty}, tty, interactive, "/exec/"+created.Id+"/resize"); err != nil {
		return fail(hc, 126, "docker: %v", err)
	}
	body, status, err = dockerCall(ctx, http.MethodGet, "/exec/"+created.Id+"/json", nil)
	if err != nil || status != http.StatusOK {
		return fail(hc, 126, "docker: %s", dockerErr(body, status))
	}
	var inspect struct{ ExitCode int }
	json.Unmarshal(body, &inspect)
	return exitCode(inspect.ExitCode)
}

// dockerHijack sends a request that takes over the connection, as the docker
// CLI does for attach and exec, and returns the stream after the answer.
func dockerHijack(ctx context.Context, path string, body any) (net.Conn, *bufio.Reader, string, error) {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	conn, err := dialLocal(ctx, "tcp", strings.TrimPrefix(dockerAPIBase, "http://"))
	if err != nil {
		return nil, nil, "", err
	}
	fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: docker\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", path, len(b))
	conn.Write(b)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, nil, "", err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		conn.Close()
		return nil, nil, "", errors.New(dockerErr(msg, resp.StatusCode))
	}
	return conn, br, resp.Header.Get("Content-Type"), nil
}

// streamOptions are the options of an attach or an exec stream.
type streamOptions struct {
	tty, stdin bool
	resizePath string
	// detachKeys end the stream with a tty and the terminal as stdin, as
	// Docker's escape proxy does; nil: ctrl-p,ctrl-q.
	detachKeys []byte
	// sigProxy is the container whose main process gets SIGINT for Ctrl-C
	// (docker attach and run --sig-proxy, the default without a tty).
	sigProxy string
}

// defaultDetachKeys is Docker's detach sequence: ctrl-p, ctrl-q.
var defaultDetachKeys = []byte{0x10, 0x11}

// dockerStream runs an exec stream; see dockerStreamWith.
func dockerStream(ctx context.Context, hc interp.HandlerContext, path string, body any, tty, stdin bool, resizePath string) (bool, error) {
	return dockerStreamWith(ctx, hc, path, body, streamOptions{tty: tty, stdin: stdin, resizePath: resizePath})
}

// dockerStreamWith runs an attach or an exec: the output to the terminal
// (or stdout and stderr), the input from the terminal or a pipe. With a tty
// and the terminal as stdin, every key goes to the container, the detach
// keys detach, and resizePath gets the size of the terminal. With sigProxy
// and no tty, Ctrl-C sends SIGINT to the container instead of ending the
// command, as the docker CLI forwards the signals it gets. It reports
// whether the user detached.
func dockerStreamWith(ctx context.Context, hc interp.HandlerContext, path string, body any, o streamOptions) (bool, error) {
	conn, br, ct, err := dockerHijack(ctx, path, body)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	t := termFrom(ctx)
	fromTerm := fromTerminal(hc.Stdin)
	tty, stdin := o.tty, o.stdin
	// With sigProxy the terminal goes into line mode without stdin, to get
	// Ctrl-C as a key: the stream must outlive it.
	proxy := o.sigProxy != "" && !tty && fromTerm && t != nil
	// On the terminal in raw or line mode, the shell holds back what is
	// written to its stdout until the command ends: write to the terminal
	// itself, as kubectl exec -i does (execLines).
	var stdout, stderr io.Writer = hc.Stdout, hc.Stderr
	if fromTerm && (tty || stdin) {
		stdout, stderr = t, t
	} else if proxy {
		if toTerminal(stdout) {
			stdout = t
		}
		if toTerminal(stderr) {
			stderr = t
		}
	}
	interrupt := func() {
		// what the docker CLI does with the SIGINT of Ctrl-C
		io.WriteString(t, "^C\r\n")
		dockerCall(ctx, http.MethodPost, "/containers/"+o.sigProxy+"/kill?signal=INT", nil)
	}

	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		if ct != "application/vnd.docker.multiplexed-stream" {
			io.Copy(stdout, br)
			return
		}
		for {
			var h [8]byte
			if _, err := io.ReadFull(br, h[:]); err != nil {
				return
			}
			w := stdout
			if h[0] == 2 {
				w = stderr
			}
			if _, err := io.CopyN(w, br, int64(binary.BigEndian.Uint32(h[4:]))); err != nil {
				return
			}
		}
	}()
	resize := func() {
		cols, rows := t.size()
		dockerCall(ctx, http.MethodPost, fmt.Sprintf("%s?h=%d&w=%d", o.resizePath, rows, cols), nil)
	}
	closeWrite := func() {
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
	}

	switch {
	case !stdin:
		if tty && fromTerm {
			resize()
		}
		if proxy {
			leave := t.enterLine()
			defer leave()
			for {
				select {
				case <-outDone:
					return false, nil
				case <-ctx.Done():
					return false, nil
				case s := <-t.keys:
					if strings.ContainsRune(s, 0x03) {
						interrupt()
					}
				}
			}
		}
	case !fromTerm:
		// a pipe: send it, then end stdin
		go func() {
			io.Copy(conn, hc.Stdin)
			closeWrite()
		}()
	case tty:
		leave := t.enterLine()
		defer leave()
		resize()
		esc := &escapeKeys{keys: o.detachKeys}
		if esc.keys == nil {
			esc.keys = defaultDetachKeys
		}
		for {
			select {
			case <-outDone:
				return false, nil
			case <-ctx.Done():
				return false, nil
			case s := <-t.keys:
				if s == resizeKey {
					resize()
					continue
				}
				out, detach := esc.feed([]byte(s))
				if len(out) > 0 {
					if _, err := conn.Write(out); err != nil {
						return false, nil
					}
				}
				if detach {
					return true, nil
				}
			}
		}
	default:
		// -i without -t from the terminal: the terminal edits lines and
		// echoes, the container gets whole lines; Ctrl-D ends stdin.
		var onCtrlC func()
		if o.sigProxy != "" {
			onCtrlC = interrupt
		}
		return false, dockerLines(ctx, t, conn, closeWrite, outDone, onCtrlC)
	}
	select {
	case <-outDone:
	case <-ctx.Done():
	}
	return false, nil
}

// escapeKeys finds the detach sequence in the keys typed, like the escape
// proxy of moby/term: the keys of a sequence that has begun are held back,
// and sent on if the sequence breaks off.
type escapeKeys struct {
	keys []byte
	pos  int // keys of the sequence seen so far
}

// feed returns what goes to the container, and whether the whole sequence
// was typed.
func (e *escapeKeys) feed(b []byte) (out []byte, detach bool) {
	for _, c := range b {
		if c == e.keys[e.pos] {
			e.pos++
			if e.pos == len(e.keys) {
				return out, true
			}
			continue
		}
		if e.pos > 0 {
			out = append(out, e.keys[:e.pos]...)
			e.pos = 0
			if c == e.keys[0] {
				e.pos = 1
				continue
			}
		}
		out = append(out, c)
	}
	return out, false
}

// dockerLines sends lines edited on the terminal to w, like kubectl exec -i
// (execLines), until the output ends. Ctrl-C calls onCtrlC (docker attach:
// SIGINT to the container); without it, Ctrl-C ends stdin.
func dockerLines(ctx context.Context, t *terminal, w io.Writer, closeWrite func(), outDone <-chan struct{}, onCtrlC func()) error {
	leave := t.enterLine()
	defer leave()
	var line []rune
	closed := false
	for {
		select {
		case <-outDone:
			return nil
		case <-ctx.Done():
			return nil
		case s := <-t.keys:
			for _, k := range parseKeys(s) {
				switch {
				case k.name == "ctrl-c" && onCtrlC != nil:
					onCtrlC()
					line = nil // what a terminal in line mode does
				case closed:
				case k.name == "ctrl-d" && len(line) == 0:
					io.WriteString(t, "\r\n")
					closed = true
					closeWrite()
				case k.name == "ctrl-c":
					io.WriteString(t, "^C\r\n")
					closed = true
					closeWrite()
				case k.name == "enter":
					io.WriteString(t, "\r\n")
					io.WriteString(w, string(line)+"\n")
					line = nil
				case k.name == "backspace":
					if len(line) > 0 {
						line = line[:len(line)-1]
						io.WriteString(t, "\b \b")
					}
				case k.name == "" && k.r != 0:
					line = append(line, k.r)
					io.WriteString(t, string(k.r))
				}
			}
		}
	}
}

// dockerVersionCmd prints the version of the API server: `docker version`.
func dockerVersionCmd(ctx context.Context, hc interp.HandlerContext, args []string) error {
	body, status, err := dockerCall(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return fail(hc, 1, "docker: %v", err)
	}
	if status != http.StatusOK {
		return fail(hc, 1, "docker: %s", dockerErr(body, status))
	}
	var v dockerVersion
	if err := json.Unmarshal(body, &v); err != nil {
		return fail(hc, 1, "docker: %v", err)
	}
	fmt.Fprintf(hc.Stdout, "Version:    %s\n", v.Version)
	fmt.Fprintf(hc.Stdout, "ApiVersion: %s\n", v.APIVersion)
	return nil
}
