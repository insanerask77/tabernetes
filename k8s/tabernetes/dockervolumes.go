// Volumes of the `docker` builtin: docker volume create, ls, inspect, rm
// and prune, and the -v and --mount options of docker run and create. The
// texts, flags and formats are those of the docker CLI v28
// (cli/command/volume, opts/mount.go, internal/volumespec). The Docker API
// (dockerapi/volumes.go) keeps the volumes; the VM worker mounts them and
// keeps their files in the browser (web/src/volumes.js).
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"syscall/js"
	"unicode"

	units "github.com/docker/go-units"
	"mvdan.cc/sh/v3/interp"
)

// ---- docker run -v and --mount ----

// dockerMountConfig is the part of HostConfig with the mounts (embedded in
// dockerHostConfig).
type dockerMountConfig struct {
	Binds  []string      `json:"Binds,omitempty"`
	Mounts []dockerMount `json:"Mounts,omitempty"`
}

// dockerMount is a --mount (types/mount.Mount).
type dockerMount struct {
	Type          string               `json:"Type,omitempty"`
	Source        string               `json:"Source,omitempty"`
	Target        string               `json:"Target,omitempty"`
	ReadOnly      bool                 `json:"ReadOnly,omitempty"`
	Consistency   string               `json:"Consistency,omitempty"`
	BindOptions   *dockerBindOptions   `json:"BindOptions,omitempty"`
	VolumeOptions *dockerVolumeOptions `json:"VolumeOptions,omitempty"`
	TmpfsOptions  *dockerTmpfsOptions  `json:"TmpfsOptions,omitempty"`
}

type dockerBindOptions struct {
	Propagation      string `json:"Propagation,omitempty"`
	NonRecursive     bool   `json:"NonRecursive,omitempty"`
	CreateMountpoint bool   `json:"CreateMountpoint,omitempty"`
}

type dockerVolumeOptions struct {
	NoCopy       bool              `json:"NoCopy,omitempty"`
	Labels       map[string]string `json:"Labels,omitempty"`
	Subpath      string            `json:"Subpath,omitempty"`
	DriverConfig *struct {
		Name    string            `json:"Name,omitempty"`
		Options map[string]string `json:"Options,omitempty"`
	} `json:"DriverConfig,omitempty"`
}

type dockerTmpfsOptions struct {
	SizeBytes int64  `json:"SizeBytes,omitempty"`
	Mode      uint32 `json:"Mode,omitempty"`
}

// volumeSpec is a -v spec as volumespec.Parse of the CLI reads it.
type volumeSpec struct {
	source, target string
	bind           bool // the source is a path, not a volume name
}

// parseVolumeSpec is volumespec.Parse: SOURCE:TARGET[:OPTIONS] or TARGET.
func parseVolumeSpec(spec string) (volumeSpec, error) {
	var v volumeSpec
	switch len(spec) {
	case 0:
		return v, errors.New("invalid empty volume spec")
	case 1, 2:
		v.target = spec
		return v, nil
	}
	var fields []string
	buf := []rune{}
	for _, c := range spec + "\x00" {
		switch {
		case c == ':' && len(buf) == 1 && unicode.IsLetter(buf[0]): // a Windows drive
			buf = append(buf, c)
		case c == ':' || c == 0:
			if len(buf) == 0 {
				return v, fmt.Errorf("invalid spec: %s: empty section between colons", spec)
			}
			if len(fields) == 2 && c == ':' {
				return v, fmt.Errorf("invalid spec: %s: too many colons", spec)
			}
			fields = append(fields, string(buf))
			buf = buf[:0]
		default:
			buf = append(buf, c)
		}
	}
	if len(fields) == 1 {
		v.target = fields[0]
		return v, nil
	}
	v.source, v.target = fields[0], fields[1]
	switch v.source[0] {
	case '.', '/', '~':
		v.bind = true
	}
	return v, nil
}

// parseMountFlag is MountOpt.Set of the CLI: a --mount value.
func parseMountFlag(value string) (dockerMount, error) {
	fields, err := csv.NewReader(strings.NewReader(value)).Read()
	if err != nil {
		return dockerMount{}, err
	}
	m := dockerMount{Type: "volume"}
	volumeOptions := func() *dockerVolumeOptions {
		if m.VolumeOptions == nil {
			m.VolumeOptions = &dockerVolumeOptions{Labels: map[string]string{}}
		}
		return m.VolumeOptions
	}
	bindOptions := func() *dockerBindOptions {
		if m.BindOptions == nil {
			m.BindOptions = &dockerBindOptions{}
		}
		return m.BindOptions
	}
	tmpfsOptions := func() *dockerTmpfsOptions {
		if m.TmpfsOptions == nil {
			m.TmpfsOptions = &dockerTmpfsOptions{}
		}
		return m.TmpfsOptions
	}
	for _, field := range fields {
		key, val, ok := strings.Cut(field, "=")
		key = strings.ToLower(key)
		if !ok {
			switch key {
			case "readonly", "ro":
				m.ReadOnly = true
				continue
			case "volume-nocopy":
				volumeOptions().NoCopy = true
				continue
			case "bind-nonrecursive":
				bindOptions().NonRecursive = true
				continue
			default:
				return m, fmt.Errorf("invalid field '%s' must be a key=value pair", field)
			}
		}
		switch key {
		case "type":
			m.Type = strings.ToLower(val)
		case "source", "src":
			m.Source = val
		case "target", "dst", "destination":
			m.Target = val
		case "readonly", "ro":
			if m.ReadOnly, err = strconv.ParseBool(val); err != nil {
				return m, fmt.Errorf("invalid value for %s: %s", key, val)
			}
		case "consistency":
			m.Consistency = strings.ToLower(val)
		case "bind-propagation":
			bindOptions().Propagation = strings.ToLower(val)
		case "bind-nonrecursive":
			if bindOptions().NonRecursive, err = strconv.ParseBool(val); err != nil {
				return m, fmt.Errorf("invalid value for %s: %s", key, val)
			}
		case "bind-recursive":
			switch val {
			case "enabled", "writable", "readonly":
				bindOptions()
			case "disabled":
				bindOptions().NonRecursive = true
			default:
				return m, fmt.Errorf(`invalid value for %s: %s (must be "enabled", "disabled", "writable", or "readonly")`, key, val)
			}
		case "volume-subpath":
			volumeOptions().Subpath = val
		case "volume-nocopy":
			if volumeOptions().NoCopy, err = strconv.ParseBool(val); err != nil {
				return m, fmt.Errorf("invalid value for volume-nocopy: %s", val)
			}
		case "volume-label":
			if k, v, _ := strings.Cut(val, "="); k != "" {
				volumeOptions().Labels[k] = v
			}
		case "volume-driver", "volume-opt":
			o := volumeOptions()
			if o.DriverConfig == nil {
				o.DriverConfig = &struct {
					Name    string            `json:"Name,omitempty"`
					Options map[string]string `json:"Options,omitempty"`
				}{}
			}
			if key == "volume-driver" {
				o.DriverConfig.Name = val
			} else if k, v, _ := strings.Cut(val, "="); k != "" {
				if o.DriverConfig.Options == nil {
					o.DriverConfig.Options = map[string]string{}
				}
				o.DriverConfig.Options[k] = v
			}
		case "tmpfs-size":
			n, err := units.RAMInBytes(val)
			if err != nil {
				return m, fmt.Errorf("invalid value for %s: %s", key, val)
			}
			tmpfsOptions().SizeBytes = n
		case "tmpfs-mode":
			n, err := strconv.ParseUint(val, 8, 32)
			if err != nil {
				return m, fmt.Errorf("invalid value for %s: %s", key, val)
			}
			tmpfsOptions().Mode = uint32(n)
		default:
			return m, fmt.Errorf("unexpected key '%s' in '%s'", key, field)
		}
	}
	if m.Type == "" {
		return m, errors.New("type is required")
	}
	if m.Target == "" {
		return m, errors.New("target is required")
	}
	for _, x := range []struct {
		set  bool
		kind string
		typ  string
	}{{m.VolumeOptions != nil, "volume", "volume"}, {m.BindOptions != nil, "bind", "bind"}, {m.TmpfsOptions != nil, "tmpfs", "tmpfs"}} {
		if x.set && m.Type != x.typ {
			return m, fmt.Errorf("cannot mix '%s-*' options with mount type '%s'", x.kind, m.Type)
		}
	}
	return m, nil
}

// dockerMountFlag reads -v, --volume and --mount for parseDockerRun: name
// is the flag as given ("-v", "--mount"), value its value. It checks the
// value as the CLI does and keeps it in o.
func dockerMountFlag(o *dockerRunOptions, name, value string) error {
	if name == "--mount" {
		if _, err := parseMountFlag(value); err != nil {
			return fmt.Errorf("invalid argument %q for \"--mount\" flag: %v", value, err)
		}
		o.mounts = append(o.mounts, value)
		return nil
	}
	if _, err := parseVolumeSpec(value); err != nil {
		return err
	}
	o.volumes = append(o.volumes, value)
	return nil
}

// setMounts puts -v and --mount into the create request, as the CLI does:
// a -v with a source is a bind (Binds; a relative path that starts with
// "." becomes absolute against cwd), one without is an anonymous volume
// (Config.Volumes).
func setMounts(req *dockerCreateRequest, o dockerRunOptions, cwd string) {
	if len(o.volumes) == 0 && len(o.mounts) == 0 {
		return
	}
	if req.HostConfig == nil {
		req.HostConfig = &dockerHostConfig{}
	}
	abs := func(p string) string {
		if !path.IsAbs(p) && strings.HasPrefix(p, ".") {
			return path.Join(cwd, p)
		}
		return p
	}
	for _, v := range o.volumes {
		spec, err := parseVolumeSpec(v)
		if err != nil {
			continue // parseDockerRun checked them
		}
		if spec.source == "" {
			if req.Volumes == nil {
				req.Volumes = map[string]struct{}{}
			}
			req.Volumes[v] = struct{}{}
			continue
		}
		bind := v
		if spec.bind {
			if host, target, ok := strings.Cut(v, ":"); ok {
				bind = abs(host) + ":" + target
			}
		}
		req.HostConfig.Binds = append(req.HostConfig.Binds, bind)
	}
	for _, v := range o.mounts {
		m, err := parseMountFlag(v)
		if err != nil {
			continue
		}
		m.Source = abs(m.Source)
		req.HostConfig.Mounts = append(req.HostConfig.Mounts, m)
	}
}

// ---- docker volume ----

// dockerVolume is a volume of the API (types/volume.Volume).
type dockerVolume struct {
	CreatedAt  string `json:",omitempty"`
	Driver     string
	Labels     map[string]string
	Mountpoint string
	Name       string
	Options    map[string]string
	Scope      string
}

// volumeRow is a line of docker volume ls, the fields its templates know.
type volumeRow struct {
	Name, Driver, Scope, Mountpoint, Labels, Links, Size, Group, Availability, Status string
}

var volumeHeader = volumeRow{Name: "VOLUME NAME", Driver: "DRIVER", Scope: "SCOPE", Mountpoint: "MOUNTPOINT", Labels: "LABELS",
	Links: "LINKS", Size: "SIZE", Group: "GROUP", Availability: "AVAILABILITY", Status: "STATUS"}

func volumeRowOf(v dockerVolume) volumeRow {
	var labels []string
	for k, val := range v.Labels {
		labels = append(labels, k+"="+val)
	}
	sort.Strings(labels)
	return volumeRow{Name: v.Name, Driver: v.Driver, Scope: v.Scope, Mountpoint: v.Mountpoint, Labels: strings.Join(labels, ","),
		Links: "N/A", Size: "N/A", Group: "N/A", Availability: "N/A", Status: "N/A"}
}

// takeMulti takes the values of a flag that may repeat (--label, --filter)
// out of args, as pflag collects them, and returns the other arguments.
func takeMulti(args []string, short byte, long string) (values, rest []string, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return values, append(rest, args[i:]...), nil
		case a == "--"+long || short != 0 && a == "-"+string(short):
			if i+1 >= len(args) {
				if a[1] == '-' {
					return nil, nil, fmt.Errorf("flag needs an argument: --%s", long)
				}
				return nil, nil, fmt.Errorf("flag needs an argument: '%c' in -%c", short, short)
			}
			i++
			values = append(values, args[i])
		case strings.HasPrefix(a, "--"+long+"="):
			values = append(values, a[len(long)+3:])
		case short != 0 && len(a) > 2 && a[0] == '-' && a[1] == short:
			values = append(values, strings.TrimPrefix(a[2:], "="))
		default:
			rest = append(rest, a)
		}
	}
	return values, rest, nil
}

// filtersJSON encodes "key=value" filters for the API: {"key":{"value":true}}.
func filtersJSON(filters []string) (string, error) {
	m := map[string]map[string]bool{}
	for _, f := range filters {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return "", fmt.Errorf("bad format of filter (expected name=value)")
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if m[k] == nil {
			m[k] = map[string]bool{}
		}
		m[k][v] = true
	}
	if len(m) == 0 {
		return "", nil
	}
	b, _ := json.Marshal(m)
	return string(b), nil
}

// usageError is how the CLI reports a bad flag of command c.
func usageError(hc interp.HandlerContext, path string, c *dockerCommand, err error) error {
	return fail(hc, 125, "%v\n\nUsage:  %s%s\n\nRun '%s --help' for more information", err, path, strings.TrimPrefix(c.use, c.name), path)
}

func dockerVolumeCreate(ctx context.Context, hc interp.HandlerContext, args []string) error {
	const path = "docker volume create"
	c := volumeCreateCommand
	labels, args, err := takeMulti(args, 0, "label")
	if err == nil {
		var opts []string
		if opts, args, err = takeMulti(args, 'o', "opt"); err == nil {
			var f dockerFlagValues
			if f, args, err = parseDockerFlags(c, args); err == nil {
				return volumeCreate(ctx, hc, f, labels, opts, args)
			}
		}
	}
	return usageError(hc, path, c, err)
}

func volumeCreate(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, labels, opts, args []string) error {
	if len(args) > 1 {
		return fail(hc, 1, "docker: 'docker volume create' requires at most 1 argument\n\nUsage:  docker volume create [OPTIONS] [VOLUME]\n\nSee 'docker volume create --help' for more information")
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	driver := f["driver"]
	if driver == "" {
		driver = "local"
	}
	req := map[string]any{"Name": name, "Driver": driver, "DriverOpts": map[string]string{}, "Labels": map[string]string{}}
	for _, l := range labels {
		k, v, _ := strings.Cut(l, "=")
		req["Labels"].(map[string]string)[k] = v
	}
	for _, o := range opts {
		k, v, ok := strings.Cut(o, "=")
		if !ok {
			return fail(hc, 125, "invalid argument %q for \"-o, --opt\" flag: bad format for key-value pair\n\nUsage:  docker volume create [OPTIONS] [VOLUME]\n\nRun 'docker volume create --help' for more information", o)
		}
		req["DriverOpts"].(map[string]string)[k] = v
	}
	body, status, err := dockerCall(ctx, http.MethodPost, "/volumes/create", req)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return fail(hc, 1, "%s", daemonError(body, status))
	}
	var v dockerVolume
	json.Unmarshal(body, &v)
	fmt.Fprintln(hc.Stdout, v.Name)
	return nil
}

func dockerVolumeLS(ctx context.Context, hc interp.HandlerContext, args []string) error {
	const path = "docker volume ls"
	c := volumeLSCommand
	filters, args, err := takeMulti(args, 'f', "filter")
	if err != nil {
		return usageError(hc, path, c, err)
	}
	f, args, err := parseDockerFlags(c, args)
	if err != nil {
		return usageError(hc, path, c, err)
	}
	if len(args) > 0 {
		return fail(hc, 1, "docker: 'docker volume ls' accepts no arguments\n\nUsage:  docker volume ls [OPTIONS]\n\nRun 'docker volume ls --help' for more information")
	}
	fj, err := filtersJSON(filters)
	if err != nil {
		return fail(hc, 125, "invalid argument %q for \"-f, --filter\" flag: %v\n\nUsage:  docker volume ls [OPTIONS]\n\nRun 'docker volume ls --help' for more information", strings.Join(filters, ","), err)
	}
	q := ""
	if fj != "" {
		q = "?filters=" + url.QueryEscape(fj)
	}
	var list struct{ Volumes []dockerVolume }
	if _, err := dockerGet(ctx, "/volumes"+q, &list); err != nil {
		return fail(hc, 1, "%v", err)
	}
	sort.Slice(list.Volumes, func(i, j int) bool { return list.Volumes[i].Name < list.Volumes[j].Name })
	format := f["format"]
	switch {
	case f.bool("quiet") && (format == "" || format == "table"):
		format = "{{.Name}}"
	case format == "" || format == "table":
		format = "table {{.Driver}}\t{{.Name}}"
	}
	rows := make([]any, len(list.Volumes))
	for i, v := range list.Volumes {
		rows[i] = volumeRowOf(v)
	}
	return writeFormatted(hc, format, volumeHeader, rows)
}

func dockerVolumeRM(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, args []string) error {
	q := ""
	if f.bool("force") {
		q = "?force=1"
	}
	var errs []string
	for _, name := range args {
		body, status, err := dockerCall(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name)+q, nil)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if status != http.StatusNoContent && status != http.StatusOK {
			errs = append(errs, daemonError(body, status))
			continue
		}
		fmt.Fprintln(hc.Stdout, name)
	}
	if len(errs) > 0 {
		return fail(hc, 1, "%s", strings.Join(errs, "\n"))
	}
	return nil
}

func dockerVolumePrune(ctx context.Context, hc interp.HandlerContext, args []string) error {
	const path = "docker volume prune"
	c := volumePruneCommand
	filters, args, err := takeMulti(args, 0, "filter")
	if err != nil {
		return usageError(hc, path, c, err)
	}
	f, args, err := parseDockerFlags(c, args)
	if err != nil {
		return usageError(hc, path, c, err)
	}
	if len(args) > 0 {
		return fail(hc, 1, "docker: 'docker volume prune' accepts no arguments\n\nUsage:  docker volume prune [OPTIONS]\n\nRun 'docker volume prune --help' for more information")
	}
	warning := "WARNING! This will remove anonymous local volumes not used by at least one container.\nAre you sure you want to continue?"
	if f.bool("all") {
		for _, x := range filters {
			if strings.HasPrefix(strings.ToLower(x), "all=") {
				return fail(hc, 1, "conflicting options: cannot specify both --all and --filter all=1")
			}
		}
		filters = append(filters, "all=true")
		warning = "WARNING! This will remove all local volumes not used by at least one container.\nAre you sure you want to continue?"
	}
	if !f.bool("force") {
		fmt.Fprint(hc.Stdout, warning+" [y/N] ")
		yes := false
		if hc.Stdin != nil {
			s := bufio.NewScanner(hc.Stdin)
			if s.Scan() {
				yes = strings.EqualFold(strings.TrimSpace(s.Text()), "y")
			}
		}
		if !yes {
			return fail(hc, 1, "volume prune has been cancelled")
		}
	}
	fj, err := filtersJSON(filters)
	if err != nil {
		return fail(hc, 125, "invalid argument %q for \"--filter\" flag: %v", strings.Join(filters, ","), err)
	}
	q := ""
	if fj != "" {
		q = "?filters=" + url.QueryEscape(fj)
	}
	body, status, err := dockerCall(ctx, http.MethodPost, "/volumes/prune"+q, nil)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if status != http.StatusOK {
		return fail(hc, 1, "%s", daemonError(body, status))
	}
	var report struct {
		VolumesDeleted []string
		SpaceReclaimed uint64
	}
	json.Unmarshal(body, &report)
	if len(report.VolumesDeleted) > 0 {
		out := "Deleted Volumes:\n"
		for _, n := range report.VolumesDeleted {
			out += n + "\n"
		}
		fmt.Fprintln(hc.Stdout, out)
	}
	fmt.Fprintln(hc.Stdout, "Total reclaimed space:", units.HumanSize(float64(report.SpaceReclaimed)))
	return nil
}

var (
	volumeCreateCommand = &dockerCommand{
		name: "create", use: "create [OPTIONS] [VOLUME]", short: "Create a volume",
		flags: []flagSpec{
			{'d', "driver", "string", `Specify volume driver name (default "local")`},
			{0, "label", "list", "Set metadata for a volume"},
			{'o', "opt", "map", "Set driver specific options (default map[])"},
		},
	}
	volumeLSCommand = &dockerCommand{
		name: "ls", use: "ls [OPTIONS]", short: "List volumes",
		aliases: []string{"docker volume ls", "docker volume list"},
		flags: []flagSpec{
			{'f', "filter", "filter", `Provide filter values (e.g. "dangling=true")`},
			{0, "format", "string", formatHelp},
			{'q', "quiet", "", "Only display volume names"},
		},
	}
	volumeInspectCommand = &dockerCommand{
		name: "inspect", use: "inspect [OPTIONS] VOLUME [VOLUME...]", short: "Display detailed information on one or more volumes",
		flags:   []flagSpec{{'f', "format", "string", inspectFormatHelp}},
		minArgs: 1, maxArgs: -1, run: dockerInspectOf("volume"),
	}
	volumeRMCommand = &dockerCommand{
		name: "rm", use: "rm [OPTIONS] VOLUME [VOLUME...]", short: "Remove one or more volumes",
		aliases: []string{"docker volume rm", "docker volume remove"},
		flags:   []flagSpec{{'f', "force", "", "Force the removal of one or more volumes"}},
		minArgs: 1, maxArgs: -1, run: dockerVolumeRM,
	}
	volumePruneCommand = &dockerCommand{
		name: "prune", use: "prune [OPTIONS]", short: "Remove unused local volumes",
		flags: []flagSpec{
			{'a', "all", "", "Remove all unused volumes, not just anonymous ones"},
			{0, "filter", "filter", `Provide filter values (e.g. "label=<label>")`},
			{'f', "force", "", "Do not prompt for confirmation"},
		},
	}
)

func init() {
	// here, not in the vars: the commands refer to themselves (usage)
	volumeCreateCommand.raw = dockerVolumeCreate
	volumeLSCommand.raw = dockerVolumeLS
	volumePruneCommand.raw = dockerVolumePrune
	dockerManagement["volume"] = struct {
		short string
		subs  map[string]*dockerCommand
	}{"Manage volumes", map[string]*dockerCommand{
		"create": volumeCreateCommand, "ls": volumeLSCommand, "list": volumeLSCommand, "inspect": volumeInspectCommand,
		"rm": volumeRMCommand, "remove": volumeRMCommand, "prune": volumePruneCommand,
	}}
}

// ---- the files of the volumes ----

// dockerVolumeData deletes the files of a volume in the browser
// (dockerapi.VolumeData): vm.js asks a worker without a VM, as for docker
// rmi (web/src/volumes.js, removeVolume).
type dockerVolumeData struct{}

func (dockerVolumeData) Remove(ctx context.Context, name string) (int64, error) {
	vm := dockerVM()
	if vm.Type() != js.TypeObject || vm.Get("removeVolume").Type() != js.TypeFunction {
		return 0, errors.New("this tab cannot run container images")
	}
	v, err := await(ctx, vm.Call("removeVolume", name))
	if err != nil {
		return 0, err
	}
	if v.Type() != js.TypeNumber {
		return 0, nil
	}
	return int64(v.Float()), nil
}
