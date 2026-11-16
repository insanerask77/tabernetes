// The options of docker run and docker create: a parser like the one of
// the docker CLI (pflag, cli/command/container/opts.go), the files of
// --env-file and --label-file (docker/cli pkg/kvfile), and the create
// request they make. The texts are those of docker/cli v28.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	units "github.com/docker/go-units"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// dockerRunFlags are the flags of docker run, sorted like in its help.
// Flags of create are the same without --detach.
var dockerRunFlags = []flagSpec{
	{'a', "attach", "list", "Attach to STDIN, STDOUT or STDERR"},
	{'d', "detach", "", "Run container in background and print container ID"},
	{0, "detach-keys", "string", "Override the key sequence for detaching a container"},
	{0, "entrypoint", "string", "Overwrite the default ENTRYPOINT of the image"},
	{'e', "env", "list", "Set environment variables"},
	{0, "env-file", "list", "Read in a file of environment variables"},
	{'h', "hostname", "string", "Container host name"},
	{0, "init", "", "Run an init inside the container that forwards signals and reaps processes"},
	{'i', "interactive", "", "Keep STDIN open even if not attached"},
	{'l', "label", "list", "Set meta data on a container"},
	{0, "label-file", "list", "Read in a line delimited file of labels"},
	{'m', "memory", "bytes", "Memory limit"},
	{0, "mount", "mount", "Attach a filesystem mount to the container"},
	{0, "log-driver", "string", "Logging driver for the container"},
	{0, "log-opt", "list", "Log driver options"},
	{0, "name", "string", "Assign a name to the container"},
	{'p', "publish", "list", "Publish a container's port(s) to the host"},
	{'P', "publish-all", "", "Publish all exposed ports to random ports"},
	{0, "restart", "string", "Restart policy to apply when a container exits (default \"no\")"},
	{0, "sig-proxy", "", "Proxy received signals to the process (default true)"},
	{0, "rm", "", "Automatically remove the container and its associated anonymous volumes when it exits"},
	{0, "stop-signal", "string", "Signal to stop the container"},
	{0, "stop-timeout", "int", "Timeout (in seconds) to stop a container"},
	{'t', "tty", "", "Allocate a pseudo-TTY"},
	{'u', "user", "string", "Username or UID (format: <name|uid>[:<group|gid>])"},
	{'v', "volume", "list", "Bind mount a volume"},
	{'w', "workdir", "string", "Working directory inside the container"},
}

// dockerCreateFlags are the flags of docker create.
var dockerCreateFlags = func() []flagSpec {
	var out []flagSpec
	for _, f := range dockerRunFlags {
		// docker create has no attach: no detach keys, no signal proxy
		if f.long != "detach" && f.long != "detach-keys" && f.long != "sig-proxy" {
			out = append(out, f)
		}
	}
	return out
}()

// dockerRestartPolicy is Docker's RestartPolicy of the create request.
type dockerRestartPolicy struct {
	Name              string `json:"Name"`
	MaximumRetryCount int    `json:"MaximumRetryCount,omitempty"`
}

func (p dockerRestartPolicy) isNone() bool { return p.Name == "" || p.Name == "no" }

// parseRestartPolicy is opts.ParseRestartPolicy of the docker CLI. The
// daemon checks the name and the count (dockerapi).
func parseRestartPolicy(policy string) (dockerRestartPolicy, error) {
	if policy == "" {
		return dockerRestartPolicy{}, nil
	}
	var p dockerRestartPolicy
	k, v, ok := strings.Cut(policy, ":")
	if ok && k == "" {
		return dockerRestartPolicy{}, errors.New("invalid restart policy format: no policy provided before colon")
	}
	if v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return dockerRestartPolicy{}, errors.New("invalid restart policy format: maximum retry count must be an integer")
		}
		p.MaximumRetryCount = n
	}
	p.Name = k
	return p, nil
}

type dockerRunOptions struct {
	interactive, tty, detach, rm bool
	publishAll                   bool
	// init (--init) is taken and does nothing: init of the VM (vm/init)
	// already does what docker-init (tini) does, it sends signals to the
	// main process and reaps orphans.
	init       bool
	name       string
	env        []string // -e, as given
	envFiles   []string
	publish    []string // -p, as given
	workdir    string
	user       string
	hostname   string
	entrypoint *string // --entrypoint; "" resets the one of the image
	labels     []string
	labelFiles []string
	memory     int64 // -m, in bytes
	restart    dockerRestartPolicy
	attach     []string // -a: stdin, stdout, stderr
	detachKeys string   // --detach-keys
	logDriver  string   // --log-driver
	logOpts    []string // --log-opt, as given (dockerlogs.go)
	volumes    []string // -v, as given (dockervolumes.go)
	mounts     []string // --mount, as given
	noSigProxy bool     // --sig-proxy=false
	stopSignal string
	// stopTimeout is --stop-timeout, nil when not given.
	stopTimeout *int
}

// flagName is how pflag names a flag in its errors: "-m, --memory".
func flagName(f flagSpec) string {
	if f.short != 0 {
		return fmt.Sprintf("-%c, --%s", f.short, f.long)
	}
	return "--" + f.long
}

// set applies one flag with its value ("true" for a bool) to o, with the
// checks of the docker CLI.
func (o *dockerRunOptions) set(f flagSpec, v string) error {
	invalid := func(err error) error {
		return fmt.Errorf("invalid argument %q for %q flag: %v", v, flagName(f), err)
	}
	if f.arg == "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return invalid(err)
		}
		switch f.long {
		case "interactive":
			o.interactive = b
		case "tty":
			o.tty = b
		case "detach":
			o.detach = b
		case "rm":
			o.rm = b
		case "publish-all":
			o.publishAll = b
		case "init":
			o.init = b
		case "sig-proxy":
			o.noSigProxy = !b
		}
		return nil
	}
	switch f.long {
	case "name":
		o.name = v
	case "detach-keys":
		o.detachKeys = v
	case "volume":
		return dockerMountFlag(o, "-v", v)
	case "mount":
		return dockerMountFlag(o, "--mount", v)
	case "log-driver":
		o.logDriver = v
	case "log-opt":
		// opts.ValidateLogOpt... the CLI only checks the key=value form
		if !strings.Contains(v, "=") {
			return invalid(errors.New("invalid key value pair format in log options"))
		}
		o.logOpts = append(o.logOpts, v)
	case "env":
		// opts.ValidateEnv; the value of a bare name comes from the shell
		// later (dockerRunRequest)
		if k, _, _ := strings.Cut(v, "="); k == "" {
			return invalid(errors.New("invalid environment variable: " + v))
		}
		o.env = append(o.env, v)
	case "env-file":
		o.envFiles = append(o.envFiles, v)
	case "publish":
		if _, err := parsePortSpec(v); err != nil {
			return err
		}
		o.publish = append(o.publish, v)
	case "workdir":
		o.workdir = v
	case "user":
		o.user = v
	case "hostname":
		o.hostname = v
	case "entrypoint":
		o.entrypoint = &v
	case "label":
		// opts.ValidateLabel
		key, _, _ := strings.Cut(v, "=")
		key = strings.TrimLeft(key, " \t")
		if key == "" {
			return invalid(fmt.Errorf("invalid label '%s': empty name", v))
		}
		if strings.ContainsAny(key, " \t") {
			return invalid(fmt.Errorf("label '%s' contains whitespaces", key))
		}
		o.labels = append(o.labels, v)
	case "label-file":
		o.labelFiles = append(o.labelFiles, v)
	case "memory":
		// opts.MemBytes: b, k, m, g with or without "b", in powers of 1024
		n, err := units.RAMInBytes(v)
		if err != nil {
			return invalid(err)
		}
		o.memory = n
	case "restart":
		p, err := parseRestartPolicy(v)
		if err != nil {
			return invalid(err)
		}
		o.restart = p
	case "attach":
		s := strings.ToLower(v)
		if s != "stdin" && s != "stdout" && s != "stderr" {
			return invalid(errors.New("valid streams are STDIN, STDOUT and STDERR"))
		}
		o.attach = append(o.attach, s)
	case "stop-signal":
		o.stopSignal = v
	case "stop-timeout":
		n, err := strconv.ParseInt(v, 0, 0)
		if err != nil {
			return invalid(err)
		}
		t := int(n)
		o.stopTimeout = &t
	}
	return nil
}

// parseDockerRun reads the options of docker run up to the image, like
// pflag with interspersed flags off: -it, -dit, -p8080:80, -p=8080:80,
// -w /tmp, --name=x, --name x, --rm=false. The rest is the command.
func parseDockerRun(args []string) (o dockerRunOptions, image string, cmd []string, err error) {
	byLong := map[string]flagSpec{}
	byShort := map[byte]flagSpec{}
	for _, f := range dockerRunFlags {
		byLong[f.long] = f
		if f.short != 0 {
			byShort[f.short] = f
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 >= len(args) {
				return o, "", nil, errors.New("\"docker run\" requires at least 1 argument")
			}
			return o, args[i+1], args[i+2:], nil
		case !strings.HasPrefix(a, "-") || a == "-":
			return o, a, args[i+1:], o.check()
		case strings.HasPrefix(a, "--"):
			name, v, hasValue := strings.Cut(a[2:], "=")
			f, ok := byLong[name]
			if !ok {
				return o, "", nil, fmt.Errorf("unknown flag: --%s", name)
			}
			switch {
			case f.arg == "" && !hasValue:
				v = "true"
			case f.arg != "" && !hasValue:
				if i+1 >= len(args) {
					return o, "", nil, fmt.Errorf("flag needs an argument: --%s", name)
				}
				i++
				v = args[i]
			}
			if err := o.set(f, v); err != nil {
				return o, "", nil, err
			}
		default:
			// shorthands: bools may be grouped, the first flag with a value
			// takes the rest of the word or the next argument
			for j := 1; j < len(a); j++ {
				f, ok := byShort[a[j]]
				if !ok {
					return o, "", nil, fmt.Errorf("unknown shorthand flag: '%c' in %s", a[j], a)
				}
				rest := a[j+1:]
				if f.arg == "" {
					v := "true"
					if strings.HasPrefix(rest, "=") {
						v, j = rest[1:], len(a)
					}
					if err := o.set(f, v); err != nil {
						return o, "", nil, err
					}
					continue
				}
				v := strings.TrimPrefix(rest, "=")
				if rest == "" {
					if i+1 >= len(args) {
						return o, "", nil, fmt.Errorf("flag needs an argument: '%c' in -%c", a[j], a[j])
					}
					i++
					v = args[i]
				}
				if err := o.set(f, v); err != nil {
					return o, "", nil, err
				}
				break
			}
		}
	}
	return o, "", nil, errors.New("\"docker run\" requires at least 1 argument")
}

// check runs the checks of the docker CLI across flags (opts.go parse and
// run.go).
func (o dockerRunOptions) check() error {
	if o.rm && !o.restart.isNone() {
		return errors.New("conflicting options: cannot specify both --restart and --rm")
	}
	if o.detach && len(o.attach) > 0 {
		return errors.New("conflicting options: cannot specify both --attach and --detach")
	}
	return nil
}

// parseKVFile reads an env file or a label file like kvfile.Parse of the
// docker CLI: KEY=VALUE lines, # comments and empty lines skipped, leading
// white space removed. A KEY without "=" takes its value from lookup, and is
// left out if lookup has none (or is nil, for labels).
func parseKVFile(r io.Reader, lookup func(string) (string, bool)) ([]string, error) {
	lines := []string{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		b := sc.Bytes()
		if !utf8.Valid(b) {
			return []string{}, fmt.Errorf("invalid utf8 bytes at line %d: %v", n, b)
		}
		if n == 1 {
			b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
		}
		line := strings.TrimLeftFunc(string(b), unicode.IsSpace)
		if line == "" || line[0] == '#' {
			continue
		}
		key, _, hasValue := strings.Cut(line, "=")
		if key == "" {
			return []string{}, fmt.Errorf("no variable name on line '%s'", line)
		}
		if strings.ContainsAny(key, " \t") {
			return []string{}, fmt.Errorf("variable '%s' contains whitespaces", key)
		}
		if hasValue {
			lines = append(lines, line)
			continue
		}
		if lookup != nil {
			if v, ok := lookup(line); ok {
				lines = append(lines, key+"="+v)
			}
		}
	}
	return lines, sc.Err()
}

// readKVFiles reads the files of --env-file or --label-file, then adds
// override (-e or -l), as opts.ReadKVStrings does.
func readKVFiles(open func(string) (io.ReadCloser, error), files, override []string, lookup func(string) (string, bool)) ([]string, error) {
	var out []string
	for _, name := range files {
		f, err := open(name)
		if err != nil {
			return nil, err
		}
		vars, err := parseKVFile(f, lookup)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("invalid env file (%s): %v", name, err)
		}
		out = append(out, vars...)
	}
	return append(out, override...), nil
}

// shellLookup looks a name up in the exported variables of the shell, the
// environment of the docker CLI.
func shellLookup(hc interp.HandlerContext) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v := hc.Env.Get(name)
		if !v.Exported || v.Kind != expand.String {
			return "", false
		}
		return v.Str, true
	}
}

// dockerRunRequest makes the create request of docker run or create: the
// env (--env-file, then -e; a bare NAME takes the value of the shell, like
// opts.ValidateEnv), the labels, and the options of the host config.
func dockerRunRequest(hc interp.HandlerContext, o dockerRunOptions, image string, cmd []string) (dockerCreateRequest, error) {
	lookup := shellLookup(hc)
	open := func(name string) (io.ReadCloser, error) { return os.Open(abs(hc, name)) }
	// -e NAME: the value of the shell, else the name alone (left out)
	env := make([]string, 0, len(o.env))
	for _, e := range o.env {
		if !strings.Contains(e, "=") {
			if v, ok := lookup(e); ok {
				e += "=" + v
			}
		}
		env = append(env, e)
	}
	env, err := readKVFiles(open, o.envFiles, env, lookup)
	if err != nil {
		return dockerCreateRequest{}, err
	}
	labelList, err := readKVFiles(open, o.labelFiles, o.labels, nil)
	if err != nil {
		return dockerCreateRequest{}, err
	}
	var labels map[string]string
	if len(labelList) > 0 {
		labels = map[string]string{}
		for _, l := range labelList {
			k, v, _ := strings.Cut(l, "=")
			labels[k] = v
		}
	}
	// the env goes without the bare names that the shell does not have:
	// dockerd would drop them too
	kept := env[:0]
	for _, e := range env {
		if strings.Contains(e, "=") {
			kept = append(kept, e)
		}
	}
	req := dockerCreateRequest{
		Image: image, Cmd: cmd, Env: kept, Tty: o.tty, OpenStdin: o.interactive,
		WorkingDir: o.workdir, User: o.user, Hostname: o.hostname, Labels: labels,
		StopSignal: o.stopSignal, StopTimeout: o.stopTimeout,
	}
	if o.entrypoint != nil {
		// --entrypoint "" sends [""]: the daemon then uses no entrypoint
		req.Entrypoint = []string{*o.entrypoint}
	}
	// docker run -i attached: stdin closes with the attach (StdinOnce)
	attachStdin := o.interactive && (len(o.attach) == 0 || contains(o.attach, "stdin"))
	req.StdinOnce = attachStdin && !o.detach
	setHostConfig(&req, o)
	return req, nil
}
