// The `docker` builtin. It is a thin HTTP client for the Docker Engine API
// that startDocker serves on 127.0.0.1:2375 (dockerapi, vendored with
// containerd in patch.go): run (attached, -it, -d, --rm, --name, -e, -p),
// exec (-i, -t), logs and version here; port in dockerports.go; stop, kill
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
	// docker run -p (dockerports.go)
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig   *dockerHostConfig   `json:"HostConfig,omitempty"`
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
		return fail(hc, 125, "docker: %v", err)
	}
	// docker run -dit keeps stdin open (for a later attach), but not only
	// for the first attach.
	req := dockerCreateRequest{Image: image, Cmd: cmd, Env: o.env, Tty: o.tty, OpenStdin: o.interactive, StdinOnce: o.interactive && !o.detach}
	setHostConfig(&req, o)
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
	// so far too (logs=1), so nothing is missed.
	q := "stream=1&stdout=1&stderr=1&logs=1"
	if o.interactive {
		q += "&stdin=1"
	}
	detached, err := dockerStream(ctx, hc, "/containers/"+id+"/attach?"+q, nil, o.tty, o.interactive, "/containers/"+id+"/resize")
	if err != nil {
		return fail(hc, 1, "docker: %v", err)
	}
	if detached {
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

type dockerRunOptions struct {
	interactive, tty, detach, rm bool
	name                         string
	env                          []string
	publish                      []string // -p, as given
}

// parseDockerRun reads the options of docker run up to the image.
func parseDockerRun(args []string) (o dockerRunOptions, image string, cmd []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		value := func(name string) (string, error) {
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				return v, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag needs an argument: %s", name)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--":
			if i+1 >= len(args) {
				return o, "", nil, errors.New("\"docker run\" requires at least 1 argument")
			}
			return o, args[i+1], args[i+2:], nil
		case !strings.HasPrefix(a, "-") || a == "-":
			return o, a, args[i+1:], nil
		case a == "--interactive":
			o.interactive = true
		case a == "--tty":
			o.tty = true
		case a == "--detach":
			o.detach = true
		case a == "--rm":
			o.rm = true
		case a == "--name" || strings.HasPrefix(a, "--name="):
			if o.name, err = value("--name"); err != nil {
				return
			}
		case a == "-e" || a == "--env" || strings.HasPrefix(a, "--env=") || strings.HasPrefix(a, "-e="):
			name := "-e"
			if strings.HasPrefix(a, "--env") {
				name = "--env"
			}
			var v string
			if v, err = value(name); err != nil {
				return
			}
			o.env = append(o.env, v)
		case a == "-p" || a == "--publish" || strings.HasPrefix(a, "-p") || strings.HasPrefix(a, "--publish="):
			var v string
			switch {
			case a == "-p" || strings.HasPrefix(a, "-p="):
				v, err = value("-p")
			case strings.HasPrefix(a, "--publish"):
				v, err = value("--publish")
			default:
				v = a[2:] // -p8080:80
			}
			if err != nil {
				return
			}
			if _, err = parsePortSpec(v); err != nil {
				return
			}
			o.publish = append(o.publish, v)
		case len(a) > 1 && a[1] != '-' && strings.Trim(a[1:], "itd") == "":
			// -i, -t, -d and their combinations: -it, -dit
			o.interactive = o.interactive || strings.Contains(a, "i")
			o.tty = o.tty || strings.Contains(a, "t")
			o.detach = o.detach || strings.Contains(a, "d")
		default:
			return o, "", nil, fmt.Errorf("unknown flag: %s", a)
		}
	}
	return o, "", nil, errors.New("\"docker run\" requires at least 1 argument")
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

// dockerStream runs an attach or an exec: the output to the terminal (or
// stdout and stderr), the input from the terminal or a pipe. With a tty and
// the terminal as stdin, every key goes to the container, Ctrl-P Ctrl-Q
// detaches, and resizePath gets the size of the terminal. It reports
// whether the user detached.
func dockerStream(ctx context.Context, hc interp.HandlerContext, path string, body any, tty, stdin bool, resizePath string) (bool, error) {
	conn, br, ct, err := dockerHijack(ctx, path, body)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	t := termFrom(ctx)
	fromTerm := fromTerminal(hc.Stdin)
	// On the terminal in raw or line mode, the shell holds back what is
	// written to its stdout until the command ends: write to the terminal
	// itself, as kubectl exec -i does (execLines).
	var stdout, stderr io.Writer = hc.Stdout, hc.Stderr
	if fromTerm && (tty || stdin) {
		stdout, stderr = t, t
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
		dockerCall(ctx, http.MethodPost, fmt.Sprintf("%s?h=%d&w=%d", resizePath, rows, cols), nil)
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
		ctrlP := false
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
				// Ctrl-P Ctrl-Q detaches, as in Docker; a single Ctrl-P is
				// sent on with the next key.
				var out []byte
				for _, c := range []byte(s) {
					switch {
					case ctrlP && c == 0x11:
						io.WriteString(t, "\r\nread escape sequence\r\n")
						return true, nil
					case ctrlP:
						out = append(out, 0x10, c)
						ctrlP = false
					case c == 0x10:
						ctrlP = true
					default:
						out = append(out, c)
					}
				}
				if len(out) > 0 {
					if _, err := conn.Write(out); err != nil {
						return false, nil
					}
				}
			}
		}
	default:
		// -i without -t from the terminal: the terminal edits lines and
		// echoes, the container gets whole lines; Ctrl-D ends stdin.
		return false, dockerLines(ctx, t, conn, closeWrite, outDone)
	}
	select {
	case <-outDone:
	case <-ctx.Done():
	}
	return false, nil
}

// dockerLines sends lines edited on the terminal to w, like kubectl exec -i
// (execLines), until the output ends.
func dockerLines(ctx context.Context, t *terminal, w io.Writer, closeWrite func(), outDone <-chan struct{}) error {
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

// dockerLogs prints the console of a container: `docker logs CONTAINER`.
// Without a tty the API multiplexes the streams: every frame starts with 8
// bytes, the stream (1 stdout, 2 stderr) and the size.
func dockerLogs(ctx context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) != 1 {
		return fail(hc, 1, "docker: \"logs\" requires exactly one container")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dockerAPIBase+"/containers/"+args[0]+"/logs?stdout=1&stderr=1", nil)
	if err != nil {
		return fail(hc, 1, "docker: %v", err)
	}
	resp, err := localClient.Do(req)
	if err != nil {
		return fail(hc, 1, "docker: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return fail(hc, 1, "docker: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fail(hc, 1, "docker: %s", dockerErr(b, resp.StatusCode))
	}
	if resp.Header.Get("Content-Type") != "application/vnd.docker.multiplexed-stream" {
		hc.Stdout.Write(b)
		return nil
	}
	for len(b) >= 8 {
		n := int(binary.BigEndian.Uint32(b[4:8]))
		if n > len(b)-8 {
			n = len(b) - 8
		}
		out := hc.Stdout
		if b[0] == 2 {
			out = hc.Stderr
		}
		out.Write(b[8 : 8+n])
		b = b[8+n:]
	}
	return nil
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
