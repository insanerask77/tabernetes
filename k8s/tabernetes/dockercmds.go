// The everyday commands of the `docker` builtin besides run and exec: ps,
// inspect, create, start, restart, attach, images, pull, rmi, the help, and
// the management commands `docker container ...` and `docker image ...`.
// The texts, the flags and the table formats are those of the docker CLI
// (github.com/docker/cli v28), so the output looks like Docker's.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"

	units "github.com/docker/go-units"
	"mvdan.cc/sh/v3/interp"
)

// flagSpec is a flag of a docker command, as pflag knows it.
type flagSpec struct {
	short byte   // 0: none
	long  string // without the dashes
	arg   string // the type of the value ("string", "int"), "" for a bool
	help  string
}

// dockerCommand is a command of the docker CLI. run takes the parsed flags
// and the arguments; raw commands (run, exec, logs, ... in dockercli.go)
// take the arguments as they are.
type dockerCommand struct {
	name    string
	use     string // after "docker "
	short   string
	aliases []string // other full names, for the help
	flags   []flagSpec
	minArgs int
	maxArgs int // -1: no limit
	run     func(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, args []string) error
	raw     func(ctx context.Context, hc interp.HandlerContext, args []string) error
}

// dockerFlagValues are the flags given, by long name ("true" for a bool).
type dockerFlagValues map[string]string

func (f dockerFlagValues) bool(name string) bool { return f[name] == "true" }

const (
	formatHelp = `Format output using a custom template:
'table':            Print output in table format with column headers (default)
'table TEMPLATE':   Print output in table format using the given Go template
'json':             Print in JSON format
'TEMPLATE':         Print output using the given Go template.
Refer to https://docs.docker.com/go/formatting/ for more information about formatting output with templates`
	inspectFormatHelp = `Format output using a custom template:
'json':             Print in JSON format
'TEMPLATE':         Print output using the given Go template.
Refer to https://docs.docker.com/go/formatting/ for more information about formatting output with templates`
)

// The commands, by the name after "docker". The texts are Docker's.
var (
	psCommand = &dockerCommand{
		name: "ps", use: "ps [OPTIONS]", short: "List containers",
		aliases: []string{"docker container ls", "docker container list", "docker container ps", "docker ps"},
		flags: []flagSpec{
			{'a', "all", "", "Show all containers (default shows just running)"},
			{0, "format", "string", formatHelp},
			{'n', "last", "int", "Show n last created containers (includes all states) (default -1)"},
			{'l', "latest", "", "Show the latest created container (includes all states)"},
			{0, "no-trunc", "", "Don't truncate output"},
			{'q', "quiet", "", "Only display container IDs"},
		},
		maxArgs: 0, run: dockerPS,
	}
	imagesCommand = &dockerCommand{
		name: "images", use: "images [OPTIONS] [REPOSITORY[:TAG]]", short: "List images",
		aliases: []string{"docker image ls", "docker image list", "docker images"},
		flags: []flagSpec{
			{0, "digests", "", "Show digests"},
			{0, "format", "string", formatHelp},
			{0, "no-trunc", "", "Don't truncate output"},
			{'q', "quiet", "", "Only show image IDs"},
		},
		maxArgs: 1, run: dockerImages,
	}
	containerInspectCommand = &dockerCommand{
		name: "inspect", use: "inspect [OPTIONS] CONTAINER [CONTAINER...]", short: "Display detailed information on one or more containers",
		flags:   []flagSpec{{'f', "format", "string", inspectFormatHelp}},
		minArgs: 1, maxArgs: -1, run: dockerInspectOf("container"),
	}
	imageInspectCommand = &dockerCommand{
		name: "inspect", use: "inspect [OPTIONS] IMAGE [IMAGE...]", short: "Display detailed information on one or more images",
		flags:   []flagSpec{{'f', "format", "string", inspectFormatHelp}},
		minArgs: 1, maxArgs: -1, run: dockerInspectOf("image"),
	}
	rmiCommand = &dockerCommand{
		name: "rmi", use: "rmi [OPTIONS] IMAGE [IMAGE...]", short: "Remove one or more images",
		aliases: []string{"docker image rm", "docker image remove", "docker rmi"},
		flags:   []flagSpec{{'f', "force", "", "Force removal of the image"}},
		minArgs: 1, maxArgs: -1, run: dockerRMI,
	}
	pullCommand = &dockerCommand{
		name: "pull", use: "pull [OPTIONS] NAME[:TAG|@DIGEST]", short: "Download an image from a registry",
		aliases: []string{"docker image pull", "docker pull"},
		flags:   []flagSpec{{'q', "quiet", "", "Suppress verbose output"}},
		minArgs: 1, maxArgs: 1, run: dockerPull,
	}
)

// dockerCommands are the commands of `docker`; dockerTopCommands those that
// Docker lists as "Common Commands".
var dockerCommands = map[string]*dockerCommand{}

var dockerTopCommands = []string{"run", "exec", "ps", "pull", "images", "version"}

func init() {
	for _, c := range []*dockerCommand{
		{name: "run", use: "run [OPTIONS] IMAGE [COMMAND] [ARG...]", short: "Create and run a new container from an image",
			aliases: []string{"docker container run", "docker run"}, raw: dockerRun},
		{name: "exec", use: "exec [OPTIONS] CONTAINER COMMAND [ARG...]", short: "Execute a command in a running container",
			aliases: []string{"docker container exec", "docker exec"}, raw: dockerExec},
		psCommand, imagesCommand, pullCommand, rmiCommand,
		{name: "version", use: "version [OPTIONS]", short: "Show the Docker version information", raw: dockerVersionCmd},
		{name: "create", use: "create [OPTIONS] IMAGE [COMMAND] [ARG...]", short: "Create a new container",
			aliases: []string{"docker container create", "docker create"}, raw: dockerCreate},
		{name: "start", use: "start [OPTIONS] CONTAINER [CONTAINER...]", short: "Start one or more stopped containers",
			aliases: []string{"docker container start", "docker start"},
			flags: []flagSpec{
				{'a', "attach", "", "Attach STDOUT/STDERR and forward signals"},
				{'i', "interactive", "", "Attach container's STDIN"},
			},
			minArgs: 1, maxArgs: -1, run: dockerStart},
		{name: "restart", use: "restart [OPTIONS] CONTAINER [CONTAINER...]", short: "Restart one or more containers",
			aliases: []string{"docker container restart", "docker restart"},
			flags:   []flagSpec{{'t', "timeout", "int", "Seconds to wait before killing the container"}},
			minArgs: 1, maxArgs: -1, run: dockerRestart},
		{name: "attach", use: "attach [OPTIONS] CONTAINER", short: "Attach local standard input, output, and error streams to a running container",
			aliases: []string{"docker container attach", "docker attach"},
			flags:   []flagSpec{{0, "no-stdin", "", "Do not attach STDIN"}},
			minArgs: 1, maxArgs: 1, run: dockerAttach},
		{name: "inspect", use: "inspect [OPTIONS] NAME|ID [NAME|ID...]", short: "Return low-level information on Docker objects",
			flags: []flagSpec{
				{'f', "format", "string", inspectFormatHelp},
				{0, "type", "string", "Only inspect objects of the given type"},
			},
			minArgs: 1, maxArgs: -1, run: dockerInspectOf("")},
		{name: "logs", use: "logs [OPTIONS] CONTAINER", short: "Fetch the logs of a container",
			aliases: []string{"docker container logs", "docker logs"}, raw: dockerLogs},
		{name: "stop", use: "stop [OPTIONS] CONTAINER [CONTAINER...]", short: "Stop one or more running containers",
			aliases: []string{"docker container stop", "docker stop"}, raw: dockerStop},
		{name: "kill", use: "kill [OPTIONS] CONTAINER [CONTAINER...]", short: "Kill one or more running containers",
			aliases: []string{"docker container kill", "docker kill"}, raw: dockerKill},
		{name: "rm", use: "rm [OPTIONS] CONTAINER [CONTAINER...]", short: "Remove one or more containers",
			aliases: []string{"docker container rm", "docker container remove", "docker rm"}, raw: dockerRM},
		{name: "port", use: "port CONTAINER [PRIVATE_PORT[/PROTO]]", short: "List port mappings or a specific mapping for the container",
			aliases: []string{"docker container port", "docker port"}, raw: dockerPortCmd},
	} {
		dockerCommands[c.name] = c
	}
}

// dockerManagement are the management commands: their subcommands, by
// name, and the command of `docker` each one is.
var dockerManagement = map[string]struct {
	short string
	subs  map[string]*dockerCommand
}{}

func init() {
	sub := func(names ...string) map[string]*dockerCommand {
		m := map[string]*dockerCommand{}
		for _, n := range names {
			m[n] = dockerCommands[n]
		}
		return m
	}
	containers := sub("attach", "create", "exec", "kill", "logs", "port", "restart", "rm", "run", "start", "stop")
	containers["ls"], containers["inspect"] = psCommand, containerInspectCommand
	containers["list"], containers["ps"], containers["remove"] = psCommand, psCommand, dockerCommands["rm"]
	images := map[string]*dockerCommand{
		"ls": imagesCommand, "list": imagesCommand, "pull": pullCommand, "rm": rmiCommand, "remove": rmiCommand,
		"inspect": imageInspectCommand,
	}
	dockerManagement["container"] = struct {
		short string
		subs  map[string]*dockerCommand
	}{"Manage containers", containers}
	dockerManagement["image"] = struct {
		short string
		subs  map[string]*dockerCommand
	}{"Manage images", images}
}

// dockerUsage is the help of `docker`, in the layout of the docker CLI.
func dockerUsage() string {
	var b strings.Builder
	b.WriteString("\nUsage:  docker [OPTIONS] COMMAND\n\nA self-sufficient runtime for containers\n\nCommon Commands:\n")
	for _, n := range dockerTopCommands {
		fmt.Fprintf(&b, "  %-12s%s\n", n, dockerCommands[n].short)
	}
	b.WriteString("\nManagement Commands:\n")
	for _, n := range []string{"container", "image"} {
		fmt.Fprintf(&b, "  %-12s%s\n", n, dockerManagement[n].short)
	}
	b.WriteString("\nCommands:\n")
	var rest []string
	for n := range dockerCommands {
		if !contains(dockerTopCommands, n) {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	for _, n := range rest {
		fmt.Fprintf(&b, "  %-12s%s\n", n, dockerCommands[n].short)
	}
	b.WriteString("\nRun 'docker COMMAND --help' for more information on a command.\n")
	return b.String()
}

// managementUsage is the help of `docker container` or `docker image`.
func managementUsage(name string) string {
	m := dockerManagement[name]
	var b strings.Builder
	fmt.Fprintf(&b, "\nUsage:  docker %s COMMAND\n\n%s\n\nCommands:\n", name, m.short)
	seen := map[*dockerCommand]bool{}
	var names []string
	for n, c := range m.subs {
		// an alias (list, ps, remove) is not listed again
		if n == "list" || n == "ps" || n == "remove" || seen[c] && n != c.name {
			continue
		}
		seen[c] = true
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "  %-12s%s\n", n, m.subs[n].short)
	}
	fmt.Fprintf(&b, "\nRun 'docker %s COMMAND --help' for more information on a command.\n", name)
	return b.String()
}

// commandUsage is the help of one command, like `docker ps --help`.
func commandUsage(path string, c *dockerCommand) string {
	var b strings.Builder
	use := strings.TrimPrefix(c.use, c.name)
	fmt.Fprintf(&b, "\nUsage:  %s%s\n\n%s\n", path, use, c.short)
	if len(c.aliases) > 0 {
		fmt.Fprintf(&b, "\nAliases:\n  %s\n", strings.Join(c.aliases, ", "))
	}
	if len(c.flags) > 0 {
		b.WriteString("\nOptions:\n")
		width := 0
		lefts := make([]string, len(c.flags))
		for i, f := range c.flags {
			l := "    "
			if f.short != 0 {
				l = "-" + string(f.short) + ", "
			}
			l += "--" + f.long
			if f.arg != "" {
				l += " " + f.arg
			}
			lefts[i] = l
			width = max(width, len(l))
		}
		for i, f := range c.flags {
			lines := strings.Split(f.help, "\n")
			fmt.Fprintf(&b, "  %-*s   %s\n", width, lefts[i], lines[0])
			for _, l := range lines[1:] {
				fmt.Fprintf(&b, "  %-*s   %s\n", width, "", l)
			}
		}
	}
	return b.String()
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// parseDockerFlags parses the flags of c like pflag: -a, --all, -aq,
// --format=x, --format x, -n 3, -n3; flags may come between the arguments,
// "--" ends them.
func parseDockerFlags(c *dockerCommand, args []string) (dockerFlagValues, []string, error) {
	f := dockerFlagValues{}
	var rest []string
	byLong := map[string]flagSpec{}
	byShort := map[byte]flagSpec{}
	for _, s := range c.flags {
		byLong[s.long] = s
		if s.short != 0 {
			byShort[s.short] = s
		}
	}
	set := func(s flagSpec, v string, name string) error {
		if s.arg == "int" {
			if _, err := strconv.Atoi(v); err != nil {
				flag := "--" + s.long
				if s.short != 0 {
					flag = "-" + string(s.short) + ", " + flag
				}
				return fmt.Errorf("invalid argument %q for %q flag: strconv.ParseInt: parsing %q: invalid syntax", v, flag, v)
			}
		}
		f[s.long] = v
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return f, append(rest, args[i+1:]...), nil
		case strings.HasPrefix(a, "--"):
			name, v, hasV := strings.Cut(a[2:], "=")
			s, ok := byLong[name]
			if !ok {
				return nil, nil, fmt.Errorf("unknown flag: --%s", name)
			}
			if s.arg == "" {
				if hasV && v != "true" && v != "false" {
					return nil, nil, fmt.Errorf("invalid argument %q for \"--%s\" flag: strconv.ParseBool: parsing %q: invalid syntax", v, name, v)
				}
				if !hasV {
					v = "true"
				}
			} else if !hasV {
				if i+1 >= len(args) {
					return nil, nil, fmt.Errorf("flag needs an argument: --%s", name)
				}
				i++
				v = args[i]
			}
			if err := set(s, v, a); err != nil {
				return nil, nil, err
			}
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				s, ok := byShort[a[j]]
				if !ok {
					return nil, nil, fmt.Errorf("unknown shorthand flag: '%c' in %s", a[j], a)
				}
				if s.arg == "" {
					f[s.long] = "true"
					continue
				}
				v := a[j+1:]
				if v == "" {
					if i+1 >= len(args) {
						return nil, nil, fmt.Errorf("flag needs an argument: '%c' in %s", a[j], a)
					}
					i++
					v = args[i]
				}
				if err := set(s, strings.TrimPrefix(v, "="), a); err != nil {
					return nil, nil, err
				}
				break
			}
		default:
			rest = append(rest, a)
		}
	}
	return f, rest, nil
}

// runDockerCommand runs c, called as path ("docker ps", "docker container
// ls"), with the checks and the error texts of the docker CLI.
func runDockerCommand(ctx context.Context, hc interp.HandlerContext, path string, c *dockerCommand, args []string) error {
	if len(args) > 0 && args[0] == "--help" {
		fmt.Fprint(hc.Stdout, commandUsage(path, c))
		return nil
	}
	if c.raw != nil {
		return c.raw(ctx, hc, args)
	}
	f, args, err := parseDockerFlags(c, args)
	if err != nil {
		return fail(hc, 125, "%v\n\nUsage:  %s%s\n\nRun '%s --help' for more information", err, path, strings.TrimPrefix(c.use, c.name), path)
	}
	if f.bool("help") {
		fmt.Fprint(hc.Stdout, commandUsage(path, c))
		return nil
	}
	usage := path + strings.TrimPrefix(c.use, c.name)
	switch {
	case len(args) < c.minArgs:
		plural := "argument"
		if c.minArgs > 1 {
			plural += "s"
		}
		if c.minArgs == c.maxArgs {
			return fail(hc, 1, "docker: '%s' requires %d %s\n\nUsage:  %s\n\nRun '%s --help' for more information", path, c.minArgs, plural, usage, path)
		}
		return fail(hc, 1, "docker: '%s' requires at least %d %s\n\nUsage:  %s\n\nSee '%s --help' for more information", path, c.minArgs, plural, usage, path)
	case c.maxArgs == 0 && len(args) > 0:
		return fail(hc, 1, "docker: '%s' accepts no arguments\n\nUsage:  %s\n\nRun '%s --help' for more information", path, usage, path)
	case c.maxArgs > 0 && len(args) > c.maxArgs:
		plural := "argument"
		if c.maxArgs > 1 {
			plural += "s"
		}
		if c.minArgs == c.maxArgs {
			return fail(hc, 1, "docker: '%s' requires %d %s\n\nUsage:  %s\n\nRun '%s --help' for more information", path, c.maxArgs, plural, usage, path)
		}
		return fail(hc, 1, "docker: '%s' requires at most %d %s\n\nUsage:  %s\n\nRun '%s --help' for more information", path, c.maxArgs, plural, usage, path)
	}
	return c.run(ctx, hc, f, args)
}

// daemonError is how the docker CLI shows an error of the API.
func daemonError(body []byte, status int) string {
	return "Error response from daemon: " + dockerErr(body, status)
}

// dockerGet sends a GET and decodes the JSON answer into v; a status other
// than 200 is an error with the message of the daemon.
func dockerGet(ctx context.Context, path string, v any) (int, error) {
	body, status, err := dockerCall(ctx, http.MethodGet, path, nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return status, errors.New(daemonError(body, status))
	}
	return status, json.Unmarshal(body, v)
}

// ---- ps ----

// psContainer is an entry of GET /containers/json.
type psContainer struct {
	ID      string `json:"Id"`
	Names   []string
	Image   string
	ImageID string
	Command string
	Created int64
	Ports   []psPort
	State   string
	Status  string
}

type psPort struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort uint16
	PublicPort  uint16 `json:",omitempty"`
	Type        string
}

// psRow is a line of docker ps, the fields its templates know.
type psRow struct {
	ID, Image, Command, CreatedAt, RunningFor, Ports, State, Status, Names string
}

var psHeader = psRow{ID: "CONTAINER ID", Image: "IMAGE", Command: "COMMAND", CreatedAt: "CREATED AT", RunningFor: "CREATED",
	Ports: "PORTS", State: "STATE", Status: "STATUS", Names: "NAMES"}

const psTable = "table {{.ID}}\t{{.Image}}\t{{.Command}}\t{{.RunningFor}}\t{{.Status}}\t{{.Ports}}\t{{.Names}}"

func psRowOf(c psContainer, trunc bool, now time.Time) psRow {
	r := psRow{
		ID: c.ID, Image: c.Image, Command: c.Command,
		CreatedAt:  time.Unix(c.Created, 0).String(),
		RunningFor: units.HumanDuration(now.Sub(time.Unix(c.Created, 0))) + " ago",
		Ports:      displayablePorts(c.Ports), State: c.State, Status: c.Status,
	}
	if trunc {
		r.ID = truncateID(c.ID)
		r.Command = ellipsis(c.Command, 20)
		if r.Image == "" {
			r.Image = "<no image>"
		}
	}
	r.Command = strconv.Quote(r.Command)
	var names []string
	for _, n := range c.Names {
		names = append(names, strings.TrimPrefix(n, "/"))
	}
	r.Names = strings.Join(names, ",")
	return r
}

// dockerPS lists the containers: `docker ps`.
func dockerPS(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, _ []string) error {
	q := url.Values{}
	if f.bool("all") {
		q.Set("all", "1")
	}
	if n, _ := strconv.Atoi(f["last"]); n > 0 {
		q.Set("limit", strconv.Itoa(n))
	}
	if f.bool("latest") {
		q.Set("limit", "1")
	}
	var cs []psContainer
	if _, err := dockerGet(ctx, "/containers/json?"+q.Encode(), &cs); err != nil {
		return fail(hc, 1, "%v", err)
	}
	format := f["format"]
	switch {
	case f.bool("quiet"):
		format = "{{.ID}}"
	case format == "":
		format = psTable
	}
	now := time.Now()
	rows := make([]any, len(cs))
	for i, c := range cs {
		rows[i] = psRowOf(c, !f.bool("no-trunc"), now)
	}
	return writeFormatted(hc, format, psHeader, rows)
}

// writeFormatted writes rows with a template of the docker CLI: "table
// TEMPLATE" (columns with a header made of header), "json" or a template.
func writeFormatted(hc interp.HandlerContext, format string, header any, rows []any) error {
	if err := writeFormattedTo(hc.Stdout, format, header, rows); err != nil {
		code := 1
		if strings.HasPrefix(err.Error(), "template parsing error") {
			code = 64
		}
		return fail(hc, code, "%v", err)
	}
	return nil
}

func writeFormattedTo(w io.Writer, format string, header any, rows []any) error {
	table := false
	switch {
	case format == "table":
		return errors.New("table needs a template")
	case strings.HasPrefix(format, "table "):
		table, format = true, format[6:]
	case format == "json":
		format = "{{json .}}"
	}
	format = strings.NewReplacer(`\t`, "\t", `\n`, "\n").Replace(format)
	tmpl, err := template.New("").Funcs(templateFuncs).Parse(format)
	if err != nil {
		return fmt.Errorf("template parsing error: %w", err)
	}
	var buf bytes.Buffer
	var out io.Writer = &buf
	var tw *tabwriter.Writer
	if table {
		// the settings of the docker CLI (formatter.Context)
		tw = tabwriter.NewWriter(&buf, 10, 1, 3, ' ', 0)
		out = tw
		if err := tmpl.Execute(out, header); err != nil {
			return err
		}
		io.WriteString(out, "\n")
	}
	for _, r := range rows {
		if err := tmpl.Execute(out, r); err != nil {
			return err
		}
		io.WriteString(out, "\n")
	}
	if tw != nil {
		tw.Flush()
	}
	_, err = w.Write(buf.Bytes())
	return err
}

// templateFuncs are the functions of the templates of the docker CLI.
var templateFuncs = template.FuncMap{
	"json": func(v any) string {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			panic(err)
		}
		return strings.TrimSpace(b.String())
	},
	"split": strings.Split,
	"join":  strings.Join,
	"title": func(s string) string {
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	},
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	"pad": func(s string, l, r int) string {
		return strings.Repeat(" ", l) + s + strings.Repeat(" ", r)
	},
	"truncate": func(s string, n int) string {
		if len(s) > n {
			return s[:n]
		}
		return s
	},
	"println": fmt.Sprintln,
}

// truncateID is an id as Docker shows it: 12 hex digits, no "sha256:".
func truncateID(id string) string {
	if i := strings.IndexByte(id, ':'); i >= 0 {
		id = id[i+1:]
	}
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// ellipsis shortens s to n runes, the last one "…" (formatter.Ellipsis).
func ellipsis(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
}

// displayablePorts is the PORTS column (formatter.DisplayablePorts): the
// ports sorted, consecutive ones as a range, published ones as
// 0.0.0.0:8080->80/tcp.
func displayablePorts(ports []psPort) string {
	type group struct{ first, last uint16 }
	groups := map[string]*group{}
	var keys, result, mappings []string
	ps := append([]psPort(nil), ports...)
	sort.Slice(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if a.PrivatePort != b.PrivatePort {
			return a.PrivatePort < b.PrivatePort
		}
		if a.IP != b.IP {
			return a.IP < b.IP
		}
		if a.PublicPort != b.PublicPort {
			return a.PublicPort < b.PublicPort
		}
		return a.Type < b.Type
	})
	form := func(key string, g *group) string {
		ip, typ, ok := strings.Cut(key, "/")
		if !ok {
			ip, typ = "", key
		}
		s := strconv.Itoa(int(g.first))
		if g.first != g.last {
			s = fmt.Sprintf("%s-%d", s, g.last)
		}
		if ip != "" {
			s = fmt.Sprintf("%s->%s", net.JoinHostPort(ip, s), s)
		}
		return s + "/" + typ
	}
	for _, p := range ps {
		key := p.Type
		if p.IP != "" {
			if p.PublicPort != p.PrivatePort {
				mappings = append(mappings, fmt.Sprintf("%s->%d/%s", net.JoinHostPort(p.IP, strconv.Itoa(int(p.PublicPort))), p.PrivatePort, p.Type))
				continue
			}
			key = p.IP + "/" + p.Type
		}
		g := groups[key]
		if g == nil {
			groups[key] = &group{p.PrivatePort, p.PrivatePort}
			keys = append(keys, key)
			continue
		}
		if p.PrivatePort == g.last+1 {
			g.last = p.PrivatePort
			continue
		}
		result = append(result, form(key, g))
		groups[key] = &group{p.PrivatePort, p.PrivatePort}
	}
	for _, k := range keys {
		result = append(result, form(k, groups[k]))
	}
	return strings.Join(append(result, mappings...), ", ")
}

// ---- inspect ----

// dockerInspectOf returns docker inspect for objects of kind: "container",
// "image" or "" for both (docker inspect: a container, else an image).
func dockerInspectOf(kind string) func(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, args []string) error {
	return func(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, args []string) error {
		if t := f["type"]; t != "" {
			if t != "container" && t != "image" {
				return fail(hc, 1, "unknown type: %q: must be one of \"container\", \"image\"", t)
			}
			kind = t
		}
		var tmpl *template.Template
		if format := f["format"]; format != "" {
			if format == "json" {
				format = "{{json .}}"
			}
			var err error
			if tmpl, err = template.New("").Funcs(templateFuncs).Parse(format); err != nil {
				return fail(hc, 64, "template parsing error: %v", err)
			}
		}
		var raws []json.RawMessage
		var errs []string
		for _, ref := range args {
			raw, err := inspectObject(ctx, kind, ref)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			raws = append(raws, raw)
		}
		var out bytes.Buffer
		if tmpl == nil {
			// the layout of the docker CLI: an indented JSON array
			b, _ := json.Marshal(raws)
			if len(raws) == 0 {
				b = []byte("[]")
			}
			json.Indent(&out, b, "", "    ")
			out.WriteString("\n")
		} else {
			for _, raw := range raws {
				var v any
				d := json.NewDecoder(bytes.NewReader(raw))
				d.UseNumber() // 143, not 1.43e+02
				d.Decode(&v)
				if err := tmpl.Execute(&out, v); err != nil {
					errs = append(errs, "template parsing error: "+err.Error())
					continue
				}
				out.WriteString("\n")
			}
		}
		hc.Stdout.Write(out.Bytes())
		if len(errs) > 0 {
			return fail(hc, 1, "%s", strings.Join(errs, "\n"))
		}
		return nil
	}
}

func inspectObject(ctx context.Context, kind, ref string) (json.RawMessage, error) {
	try := func(path string) (json.RawMessage, int, error) {
		body, status, err := dockerCall(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, 0, err
		}
		if status != http.StatusOK {
			return nil, status, errors.New(daemonError(body, status))
		}
		return json.RawMessage(bytes.TrimSpace(body)), status, nil
	}
	if kind != "image" {
		raw, status, err := try("/containers/" + url.PathEscape(ref) + "/json")
		if err == nil || kind == "container" {
			return raw, err
		}
		if status != http.StatusNotFound {
			return nil, err
		}
	}
	raw, status, err := try("/images/" + imagePath(ref) + "/json")
	if err != nil && kind == "" && status == http.StatusNotFound {
		return nil, fmt.Errorf("Error: No such object: %s", ref)
	}
	return raw, err
}

// imagePath is an image reference in the path of a request: its slashes
// stay, as the docker CLI sends them.
func imagePath(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// ---- create, start, restart, attach ----

// dockerInspectState reads what start and attach need of a container.
type dockerInspectState struct {
	ID    string `json:"Id"`
	Name  string
	State struct {
		Running bool
	}
	Config struct {
		Tty       bool
		OpenStdin bool
	}
}

func dockerInspectContainer(ctx context.Context, ref string) (dockerInspectState, error) {
	var c dockerInspectState
	_, err := dockerGet(ctx, "/containers/"+url.PathEscape(ref)+"/json", &c)
	return c, err
}

// dockerCreateContainer creates a container, like the docker CLI: an image
// that is not there is pulled first, with its progress on stderr. cmdName
// is the command for the error texts (run or create).
func dockerCreateContainer(ctx context.Context, hc interp.HandlerContext, req dockerCreateRequest, name, cmdName string) (string, error) {
	path := "/containers/create"
	if name != "" {
		path += "?name=" + url.QueryEscape(name)
	}
	createErr := func(msg string) error {
		return fail(hc, 125, "docker: %s\n\nRun 'docker %s --help' for more information", msg, cmdName)
	}
	for pulled := false; ; pulled = true {
		body, status, err := dockerCall(ctx, http.MethodPost, path, req)
		if err != nil {
			return "", createErr(err.Error())
		}
		if status == http.StatusNotFound && !pulled && strings.HasPrefix(dockerErr(body, status), "No such image") {
			ref := familiarRef(req.Image)
			fmt.Fprintf(hc.Stderr, "Unable to find image '%s' locally\n", ref)
			if err := dockerPullImage(ctx, hc, hc.Stderr, req.Image, false); err != nil {
				return "", createErr(err.Error())
			}
			continue
		}
		if status != http.StatusOK && status != http.StatusCreated {
			return "", createErr(daemonError(body, status))
		}
		var created dockerCreateResponse
		if err := json.Unmarshal(body, &created); err != nil {
			return "", createErr(err.Error())
		}
		for _, w := range created.Warnings {
			fmt.Fprintln(hc.Stderr, "WARNING:", w)
		}
		return created.ID, nil
	}
}

// dockerCreate creates a container without starting it: `docker create`.
func dockerCreate(ctx context.Context, hc interp.HandlerContext, args []string) error {
	o, image, cmd, err := parseDockerRun(args)
	if err != nil {
		return fail(hc, 125, "docker: %v", err)
	}
	if o.detach {
		return fail(hc, 125, "unknown shorthand flag: 'd' in -d\n\nUsage:  docker create [OPTIONS] IMAGE [COMMAND] [ARG...]\n\nRun 'docker create --help' for more information")
	}
	req := dockerCreateRequest{Image: image, Cmd: cmd, Env: o.env, Tty: o.tty, OpenStdin: o.interactive}
	setHostConfig(&req, o)
	id, err := dockerCreateContainer(ctx, hc, req, o.name, "create")
	if err != nil {
		return err
	}
	fmt.Fprintln(hc.Stdout, id)
	return nil
}

// dockerStart starts containers: `docker start [-a] [-i] CONTAINER...`. With
// -a it stays attached like docker run and returns the exit code.
func dockerStart(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, args []string) error {
	attach, interactive := f.bool("attach"), f.bool("interactive")
	if attach || interactive {
		if len(args) > 1 {
			return fail(hc, 1, "you cannot start and attach multiple containers at once")
		}
		c, err := dockerInspectContainer(ctx, args[0])
		if err != nil {
			return fail(hc, 1, "%v", err)
		}
		body, status, err := dockerCall(ctx, http.MethodPost, "/containers/"+c.ID+"/start", nil)
		if err != nil {
			return fail(hc, 1, "%v", err)
		}
		if status != http.StatusNoContent && status != http.StatusNotModified {
			return fail(hc, 1, "%s", daemonError(body, status))
		}
		// a container that ran already: what comes from now on
		logs := status == http.StatusNoContent
		return dockerAttachTo(ctx, hc, c, interactive && c.Config.OpenStdin, logs)
	}
	var failed []string
	for _, ref := range args {
		body, status, err := dockerCall(ctx, http.MethodPost, "/containers/"+url.PathEscape(ref)+"/start", nil)
		if err == nil && status != http.StatusNoContent && status != http.StatusNotModified {
			err = errors.New(daemonError(body, status))
		}
		if err != nil {
			fmt.Fprintln(hc.Stderr, err)
			failed = append(failed, ref)
			continue
		}
		fmt.Fprintln(hc.Stdout, ref)
	}
	if len(failed) > 0 {
		return fail(hc, 1, "Error: failed to start containers: %s", strings.Join(failed, ", "))
	}
	return nil
}

// dockerRestart restarts containers: `docker restart [-t N] CONTAINER...`.
func dockerRestart(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, args []string) error {
	q := ""
	if t, ok := f["timeout"]; ok {
		q = "?t=" + t
	}
	var errs []string
	for _, ref := range args {
		body, status, err := dockerCall(ctx, http.MethodPost, "/containers/"+url.PathEscape(ref)+"/restart"+q, nil)
		if err == nil && status != http.StatusNoContent {
			err = errors.New(daemonError(body, status))
		}
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		fmt.Fprintln(hc.Stdout, ref)
	}
	if len(errs) > 0 {
		return fail(hc, 1, "%s", strings.Join(errs, "\n"))
	}
	return nil
}

// dockerAttach attaches to a running container: `docker attach CONTAINER`.
// Ctrl-P Ctrl-Q detaches.
func dockerAttach(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, args []string) error {
	c, err := dockerInspectContainer(ctx, args[0])
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if !c.State.Running {
		return fail(hc, 1, "You cannot attach to a stopped container, start it first")
	}
	return dockerAttachTo(ctx, hc, c, !f.bool("no-stdin") && c.Config.OpenStdin, false)
}

// dockerAttachTo streams a container until it exits and returns its exit
// code, or until the user detaches. logs: from the start of the run, for a
// container that was just started (see attach in dockerapi).
func dockerAttachTo(ctx context.Context, hc interp.HandlerContext, c dockerInspectState, stdin, logs bool) error {
	q := "stream=1&stdout=1&stderr=1"
	if stdin {
		q += "&stdin=1"
	}
	if logs {
		q += "&logs=1"
	}
	detached, err := dockerStream(ctx, hc, "/containers/"+c.ID+"/attach?"+q, nil, c.Config.Tty, stdin, "/containers/"+c.ID+"/resize")
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if detached || ctx.Err() != nil {
		return nil
	}
	body, status, err := dockerCall(ctx, http.MethodPost, "/containers/"+c.ID+"/wait", nil)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if status != http.StatusOK {
		return fail(hc, 1, "%s", daemonError(body, status))
	}
	var waited struct{ StatusCode int }
	json.Unmarshal(body, &waited)
	return exitCode(waited.StatusCode)
}

// ---- images, pull, rmi ----

type dockerImageSummary struct {
	ID          string `json:"Id"`
	RepoTags    []string
	RepoDigests []string
	Created     int64
	Size        int64
}

// imageRow is a line of docker images, the fields its templates know.
type imageRow struct {
	Repository, Tag, Digest, ID, CreatedSince, CreatedAt, Size string
}

var imageHeader = imageRow{Repository: "REPOSITORY", Tag: "TAG", Digest: "DIGEST", ID: "IMAGE ID", CreatedSince: "CREATED", CreatedAt: "CREATED AT", Size: "SIZE"}

// imageRows are the lines of docker images: one per tag, or one with tag
// <none> for an image that has only a digest.
func imageRows(imgs []dockerImageSummary, filter string, trunc bool, now time.Time) []imageRow {
	var rows []imageRow
	for _, img := range imgs {
		base := imageRow{
			ID:           img.ID,
			CreatedSince: units.HumanDuration(now.Sub(time.Unix(img.Created, 0))) + " ago",
			CreatedAt:    time.Unix(img.Created, 0).String(),
			Size:         units.HumanSizeWithPrecision(float64(img.Size), 3),
			Digest:       "<none>",
		}
		if trunc {
			base.ID = truncateID(img.ID)
		}
		digests := map[string]string{} // repo -> digest
		for _, d := range img.RepoDigests {
			repo, dg, _ := strings.Cut(d, "@")
			digests[repo] = dg
		}
		tagged := map[string]bool{}
		for _, t := range img.RepoTags {
			i := strings.LastIndexByte(t, ':')
			r := base
			r.Repository, r.Tag = t[:i], t[i+1:]
			if d, ok := digests[r.Repository]; ok {
				r.Digest = d
			}
			tagged[r.Repository] = true
			rows = append(rows, r)
		}
		for repo, d := range digests {
			if !tagged[repo] {
				r := base
				r.Repository, r.Tag, r.Digest = repo, "<none>", d
				rows = append(rows, r)
			}
		}
		if len(img.RepoTags) == 0 && len(digests) == 0 {
			r := base
			r.Repository, r.Tag = "<none>", "<none>"
			rows = append(rows, r)
		}
	}
	if filter == "" {
		return rows
	}
	repo, tag, _ := strings.Cut(filter, ":")
	var out []imageRow
	for _, r := range rows {
		if (r.Repository == repo || r.Repository == strings.TrimPrefix(repo, "library/")) && (tag == "" || r.Tag == tag) {
			out = append(out, r)
		}
	}
	return out
}

// dockerImages lists the images of the tab: `docker images`. The VM worker
// pulls them into the image store of the browser, which the pods use too.
func dockerImages(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, args []string) error {
	var imgs []dockerImageSummary
	if _, err := dockerGet(ctx, "/images/json", &imgs); err != nil {
		return fail(hc, 1, "%v", err)
	}
	filter := ""
	if len(args) > 0 {
		filter = args[0]
	}
	rows := imageRows(imgs, filter, !f.bool("no-trunc"), time.Now())
	format := f["format"]
	switch {
	case f.bool("quiet"):
		// each image once
		seen := map[string]bool{}
		var b strings.Builder
		for _, r := range rows {
			if !seen[r.ID] {
				seen[r.ID] = true
				fmt.Fprintln(&b, r.ID)
			}
		}
		io.WriteString(hc.Stdout, b.String())
		return nil
	case format == "" && f.bool("digests"):
		format = "table {{.Repository}}\t{{.Tag}}\t{{.Digest}}\t{{.ID}}\t{{.CreatedSince}}\t{{.Size}}"
	case format == "":
		format = "table {{.Repository}}\t{{.Tag}}\t{{.ID}}\t{{.CreatedSince}}\t{{.Size}}"
	}
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return writeFormatted(hc, format, imageHeader, out)
}

// familiarRef is an image reference as Docker shows it, with its tag:
// nginx:latest for docker.io/library/nginx.
func familiarRef(ref string) string {
	name, digest, _ := strings.Cut(ref, "@")
	tag := ""
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name, tag = name[:i], name[i+1:]
	}
	name = strings.TrimPrefix(strings.TrimPrefix(name, "docker.io/"), "library/")
	if tag == "" && digest == "" {
		tag = "latest"
	}
	if tag != "" {
		name += ":" + tag
	}
	if digest != "" {
		name += "@" + digest
	}
	return name
}

// fullRef is an image reference with its registry: docker.io/library/nginx:latest.
func fullRef(ref string) string {
	f := familiarRef(ref)
	first, _, ok := strings.Cut(f, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return f
	}
	if !ok {
		f = "library/" + f
	}
	return "docker.io/" + f
}

// dockerPull pulls an image: `docker pull IMAGE`.
func dockerPull(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, args []string) error {
	ref := args[0]
	name, digest, _ := strings.Cut(ref, "@")
	if i := strings.LastIndexByte(name, ':'); i <= strings.LastIndexByte(name, '/') && digest == "" {
		fmt.Fprintln(hc.Stdout, "Using default tag: latest")
	}
	if err := dockerPullImage(ctx, hc, hc.Stdout, ref, f.bool("quiet")); err != nil {
		return fail(hc, 1, "%v", err)
	}
	fmt.Fprintln(hc.Stdout, fullRef(ref))
	return nil
}

// dockerPullImage pulls ref through the API (POST /images/create) and shows
// its progress on out like the docker CLI: on the terminal a line per layer
// that is redrawn in place, elsewhere only the steps without progress bars.
func dockerPullImage(ctx context.Context, hc interp.HandlerContext, out io.Writer, ref string, quiet bool) error {
	full := fullRef(ref)
	q := url.Values{}
	name, digest, _ := strings.Cut(full, "@")
	tag := ""
	if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
		name, tag = name[:i], name[i+1:]
	}
	if digest != "" {
		tag = digest
	}
	q.Set("fromImage", name)
	q.Set("tag", tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dockerAPIBase+"/images/create?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := localClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return errors.New(daemonError(b, resp.StatusCode))
	}
	if quiet {
		out = io.Discard
	}
	width := 80
	term := toTerminal(out)
	if t := termFrom(ctx); t != nil && term {
		width, _ = t.size()
	}
	return displayJSONMessages(resp.Body, out, term, width)
}

// pullMessage is a line of the progress stream (jsonmessage.JSONMessage).
type pullMessage struct {
	Status         string        `json:"status"`
	ID             string        `json:"id"`
	ProgressDetail *pullProgress `json:"progressDetail"`
	Error          string        `json:"error"`
	ErrorDetail    *struct {
		Message string `json:"message"`
	} `json:"errorDetail"`
}

type pullProgress struct {
	Current int64 `json:"current"`
	Total   int64 `json:"total"`
}

// String is the progress bar of the docker CLI (JSONProgress.String): the
// bar only on a terminal wider than 110 columns.
func (p *pullProgress) String(width int) string {
	if p.Current <= 0 && p.Total <= 0 {
		return ""
	}
	if p.Total <= 0 {
		return fmt.Sprintf("%8v", units.HumanSize(float64(p.Current)))
	}
	percentage := min(int(float64(p.Current)/float64(p.Total)*100)/2, 50)
	bar := ""
	if width > 110 {
		bar = fmt.Sprintf("[%s>%s] ", strings.Repeat("=", percentage), strings.Repeat(" ", max(50-percentage, 0)))
	}
	numbers := fmt.Sprintf("%8v/%v", units.HumanSize(float64(p.Current)), units.HumanSize(float64(p.Total)))
	if p.Current > p.Total {
		numbers = fmt.Sprintf("%8v", units.HumanSize(float64(p.Current)))
	}
	return bar + numbers
}

// displayJSONMessages shows a progress stream like the docker CLI
// (jsonmessage.DisplayJSONMessagesStream). On a terminal every layer keeps
// its line: the cursor goes up to it, redraws it and comes back.
func displayJSONMessages(r io.Reader, out io.Writer, term bool, width int) error {
	dec := json.NewDecoder(bufio.NewReader(r))
	ids := map[string]int{}
	for {
		var m pullMessage
		if err := dec.Decode(&m); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if m.ErrorDetail != nil || m.Error != "" {
			msg := m.Error
			if m.ErrorDetail != nil && m.ErrorDetail.Message != "" {
				msg = m.ErrorDetail.Message
			}
			return errors.New(msg)
		}
		diff := 0
		if m.ID != "" && m.ProgressDetail != nil {
			line, ok := ids[m.ID]
			if !ok {
				line = len(ids)
				ids[m.ID] = line
				if term {
					io.WriteString(out, "\n")
				}
			}
			diff = len(ids) - line
			if term && diff > 0 {
				fmt.Fprintf(out, "\x1b[%dA", diff)
			}
		} else {
			// a line that is not a layer ends the lines of the layers
			ids = map[string]int{}
		}
		progress := ""
		if m.ProgressDetail != nil {
			progress = m.ProgressDetail.String(width)
		}
		var b strings.Builder
		if term && m.ProgressDetail != nil {
			b.WriteString("\x1b[2K\r")
		}
		if m.ID != "" {
			b.WriteString(m.ID + ": ")
		}
		switch {
		case m.ProgressDetail != nil && term:
			fmt.Fprintf(&b, "%s %s\r", m.Status, progress)
		case progress != "":
			continue // no progress bars off the terminal
		default:
			end := "\n"
			if term && m.ProgressDetail != nil {
				end = "\r"
			}
			b.WriteString(m.Status + end)
		}
		io.WriteString(out, b.String())
		if m.ID != "" && term && diff > 0 {
			fmt.Fprintf(out, "\x1b[%dB", diff)
		}
	}
}

// toTerminal tells whether w writes to the terminal of the shell (not a
// pipe or a file).
func toTerminal(w io.Writer) bool {
	for range 8 {
		switch v := w.(type) {
		case ctxWriter:
			w = v.w
			continue
		case holdWriter, *terminal:
			return true
		}
		// the interpreter wraps its writers in structs of its own
		rv := reflect.ValueOf(w)
		if rv.Kind() == reflect.Pointer {
			rv = rv.Elem()
		}
		if rv.Kind() != reflect.Struct {
			return false
		}
		f := rv.FieldByName("Writer")
		if !f.IsValid() || !f.CanInterface() {
			return false
		}
		next, ok := f.Interface().(io.Writer)
		if !ok {
			return false
		}
		w = next
	}
	return false
}

// dockerRMI removes images: `docker rmi [-f] IMAGE...`.
func dockerRMI(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, args []string) error {
	q := ""
	if f.bool("force") {
		q = "?force=1"
	}
	var errs []string
	for _, ref := range args {
		body, status, err := dockerCall(ctx, http.MethodDelete, "/images/"+imagePath(ref)+q, nil)
		if err == nil && status != http.StatusOK {
			err = errors.New(daemonError(body, status))
		}
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		var items []struct{ Untagged, Deleted string }
		json.Unmarshal(body, &items)
		for _, it := range items {
			if it.Deleted != "" {
				fmt.Fprintln(hc.Stdout, "Deleted:", it.Deleted)
			} else {
				fmt.Fprintln(hc.Stdout, "Untagged:", it.Untagged)
			}
		}
	}
	if len(errs) > 0 {
		return fail(hc, 1, "%s", strings.Join(errs, "\n"))
	}
	return nil
}
