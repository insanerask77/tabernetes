package main

// The output formats of the docker builtin: the --format of docker ps,
// docker images and docker system df, as the formatter package of the
// docker CLI (github.com/docker/cli v28, cli/command/formatter) writes them.
// A row is a context with one method per field ({{.ID}}, {{.Label "k"}});
// "json" marshals every field method; "table TEMPLATE" adds a header row
// from a map of the titles, executed with the same template.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"
	"unicode"

	units "github.com/docker/go-units"
	"mvdan.cc/sh/v3/interp"
)

// The format keys of the docker CLI.
const (
	tableFormatKey = "table"
	rawFormatKey   = "raw"
	jsonFormatKey  = "json"
	quietFormat    = "{{.ID}}"
)

// templateFuncs are the functions of the templates of the docker CLI
// (templates.basicFunctions).
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
	"title": strings.Title, //nolint:staticcheck // the docker CLI uses it, for ASCII
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
	"pad": func(s string, prefix, suffix int) string {
		if s == "" {
			return s
		}
		return strings.Repeat(" ", prefix) + s + strings.Repeat(" ", suffix)
	},
	"truncate": func(s string, n int) string {
		if len(s) < n {
			return s
		}
		return s[:n]
	},
}

// headerFuncs replace the functions for the header row: a title stays as
// it is (templates.HeaderFunctions).
var headerFuncs = template.FuncMap{
	"json":     func(v string) string { return v },
	"split":    func(v string, _ string) string { return v },
	"join":     func(v string, _ string) string { return v },
	"title":    func(v string) string { return v },
	"lower":    func(v string) string { return v },
	"upper":    func(v string) string { return v },
	"truncate": func(v string, _ int) string { return v },
}

// parseTemplate parses a template of the docker CLI.
func parseTemplate(format string) (*template.Template, error) {
	return template.New("").Funcs(templateFuncs).Parse(format)
}

// formatWriter is formatter.Context: it writes rows with format, a table
// with the titles of header.
type formatWriter struct {
	format string
	header map[string]string
}

func (f formatWriter) isTable() bool { return strings.HasPrefix(f.format, tableFormatKey) }

// final is the template that runs for every row (Context.preFormat).
func (f formatWriter) final() string {
	s := f.format
	switch {
	case f.isTable():
		s = s[len(tableFormatKey):]
	case s == jsonFormatKey:
		s = "{{json .}}"
	}
	s = strings.Trim(s, " ")
	return strings.NewReplacer(`\t`, "\t", `\n`, "\n").Replace(s)
}

// write writes the rows. A template that does not parse or fails on a row
// is "template parsing error", as in the docker CLI.
func (f formatWriter) write(w io.Writer, rows []any) error {
	tmpl, err := parseTemplate(f.final())
	if err != nil {
		return fmt.Errorf("template parsing error: %w", err)
	}
	var buf bytes.Buffer
	for _, r := range rows {
		if err := tmpl.Execute(&buf, r); err != nil {
			return fmt.Errorf("template parsing error: %w", err)
		}
		buf.WriteString("\n")
	}
	if !f.isTable() {
		_, err := w.Write(buf.Bytes())
		return err
	}
	// The header row runs the same template on the map of titles; like the
	// docker CLI, a field it cannot show ends the row there.
	tw := tabwriter.NewWriter(w, 10, 1, 3, ' ', 0)
	var head bytes.Buffer
	tmpl.Funcs(headerFuncs).Execute(&head, f.header)
	tw.Write(head.Bytes())
	tw.Write([]byte("\n"))
	tw.Write(buf.Bytes())
	return tw.Flush()
}

// marshalMethods is formatter.MarshalJSON: every exported method without
// arguments and with one result is a field.
func marshalMethods(x any) ([]byte, error) {
	v := reflect.ValueOf(x)
	m := map[string]any{}
	for i := 0; i < v.NumMethod(); i++ {
		name, fn := v.Type().Method(i).Name, v.Method(i)
		if !unicode.IsUpper(rune(name[0])) || name == "MarshalJSON" || fn.Type().NumIn() != 0 || fn.Type().NumOut() != 1 {
			continue
		}
		m[name] = fn.Call(nil)[0].Interface()
	}
	return json.Marshal(m)
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

// ---- docker ps ----

// psContainer is an entry of GET /containers/json.
type psContainer struct {
	ID         string `json:"Id"`
	Names      []string
	Image      string
	ImageID    string
	Command    string
	Created    int64
	Ports      []psPort
	Labels     map[string]string
	State      string
	Status     string
	SizeRw     int64
	SizeRootFs int64
	Mounts     []struct {
		Name, Source, Destination, Driver string
	}
	NetworkSettings *struct {
		Networks map[string]json.RawMessage
	}
}

type psPort struct {
	IP          string `json:"IP,omitempty"`
	PrivatePort uint16
	PublicPort  uint16 `json:",omitempty"`
	Type        string
}

// defaultPSTable is the table of docker ps.
const defaultPSTable = "table {{.ID}}\t{{.Image}}\t{{.Command}}\t{{.RunningFor}}\t{{.Status}}\t{{.Ports}}\t{{.Names}}"

// psHeader are the titles of the fields of docker ps.
var psHeader = map[string]string{
	"ID": "CONTAINER ID", "Names": "NAMES", "Image": "IMAGE", "Command": "COMMAND", "CreatedAt": "CREATED AT",
	"RunningFor": "CREATED", "Ports": "PORTS", "State": "STATE", "Status": "STATUS", "Size": "SIZE",
	"Labels": "LABELS", "Mounts": "MOUNTS", "LocalVolumes": "LOCAL VOLUMES", "Networks": "NETWORKS", "Platform": "PLATFORM",
}

// psFormat is formatter.NewContainerFormat: the template for a --format.
func psFormat(source string, quiet, size bool) string {
	switch source {
	case tableFormatKey, "":
		if quiet {
			return quietFormat
		}
		if size {
			return defaultPSTable + `\t{{.Size}}`
		}
		return defaultPSTable
	case rawFormatKey:
		if quiet {
			return `container_id: {{.ID}}`
		}
		format := "container_id: {{.ID}}\nimage: {{.Image}}\ncommand: {{.Command}}\ncreated_at: {{.CreatedAt}}\nstate: {{- pad .State 1 0}}\nstatus: {{- pad .Status 1 0}}\nnames: {{.Names}}\nlabels: {{- pad .Labels 1 0}}\nports: {{- pad .Ports 1 0}}\n"
		if size {
			format += `size: {{.Size}}\n`
		}
		return format
	}
	if quiet {
		return quietFormat
	}
	return source
}

// psCtx is a row of docker ps (formatter.ContainerContext).
type psCtx struct {
	trunc      bool
	c          psContainer
	now        time.Time
	fieldsUsed map[string]bool
}

func (c *psCtx) MarshalJSON() ([]byte, error) { return marshalMethods(c) }

func (c *psCtx) ID() string {
	if c.trunc {
		return truncateID(c.c.ID)
	}
	return c.c.ID
}

func (c *psCtx) Names() string {
	var names []string
	for _, n := range c.c.Names {
		names = append(names, strings.TrimPrefix(n, "/"))
	}
	if c.trunc {
		for _, n := range names {
			if !strings.Contains(n, "/") {
				names = []string{n}
				break
			}
		}
	}
	return strings.Join(names, ",")
}

func (c *psCtx) Image() string {
	if c.c.Image == "" {
		return "<no image>"
	}
	if c.trunc {
		if t := truncateID(c.c.ImageID); t == truncateID(c.c.Image) {
			return t
		}
		// without the digest: nginx:1.27, not nginx:1.27@sha256:…
		name, _, _ := strings.Cut(c.c.Image, "@")
		repo, tag := name, ""
		if i := strings.LastIndexByte(name, ':'); i > strings.LastIndexByte(name, '/') {
			repo, tag = name[:i], name[i+1:]
		}
		repo = strings.TrimPrefix(strings.TrimPrefix(repo, "docker.io/"), "library/")
		if tag != "" {
			return repo + ":" + tag
		}
		return repo
	}
	return c.c.Image
}

func (c *psCtx) Command() string {
	cmd := c.c.Command
	if c.trunc {
		cmd = ellipsis(cmd, 20)
	}
	return strconv.Quote(cmd)
}

func (c *psCtx) CreatedAt() string { return time.Unix(c.c.Created, 0).String() }

func (c *psCtx) RunningFor() string {
	return units.HumanDuration(c.now.UTC().Sub(time.Unix(c.c.Created, 0))) + " ago"
}

// psPlatform is the platform of the image of a container; the Docker API
// of the tab does not report it, so it is nil, as with dockerd's classic
// image store.
type psPlatform struct{ OS, Architecture string }

func (p *psPlatform) String() string { return p.OS + "/" + p.Architecture }

func (c *psCtx) Platform() *psPlatform { return nil }

func (c *psCtx) Ports() string { return displayablePorts(c.c.Ports) }

func (c *psCtx) State() string { return c.c.State }

func (c *psCtx) Status() string { return c.c.Status }

func (c *psCtx) Size() string {
	if c.fieldsUsed == nil {
		c.fieldsUsed = map[string]bool{}
	}
	c.fieldsUsed["Size"] = true
	srw := units.HumanSizeWithPrecision(float64(c.c.SizeRw), 3)
	if c.c.SizeRootFs > 0 {
		return fmt.Sprintf("%s (virtual %s)", srw, units.HumanSizeWithPrecision(float64(c.c.SizeRootFs), 3))
	}
	return srw
}

func (c *psCtx) Labels() string {
	if c.c.Labels == nil {
		return ""
	}
	var kv []string
	for k, v := range c.c.Labels {
		kv = append(kv, k+"="+v)
	}
	sort.Strings(kv)
	return strings.Join(kv, ",")
}

func (c *psCtx) Label(name string) string { return c.c.Labels[name] }

func (c *psCtx) Mounts() string {
	var out []string
	for _, m := range c.c.Mounts {
		name := m.Name
		if name == "" {
			name = m.Source
		}
		if c.trunc {
			name = ellipsis(name, 15)
		}
		out = append(out, name)
	}
	return strings.Join(out, ",")
}

func (c *psCtx) LocalVolumes() string {
	n := 0
	for _, m := range c.c.Mounts {
		if m.Driver == "local" {
			n++
		}
	}
	return strconv.Itoa(n)
}

func (c *psCtx) Networks() string {
	if c.c.NetworkSettings == nil {
		return ""
	}
	var out []string
	for k := range c.c.NetworkSettings.Networks {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
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

// psOptions are the flags of docker ps that change the request.
type psOptions struct {
	format      string
	quiet, size bool
	sizeChanged bool // --size given, also as --size=false
}

// prepare checks a --format as the docker CLI does before the request, and
// asks for the sizes when the template shows them (buildContainerListOptions).
func (o *psOptions) prepare() error {
	if o.format == "" {
		return nil
	}
	tmpl, err := parseTemplate(o.format)
	if err != nil {
		return fmt.Errorf("failed to parse template: %w", err)
	}
	probe := &psCtx{}
	if err := tmpl.Execute(io.Discard, probe); err != nil {
		return fmt.Errorf("failed to execute template: %w", err)
	}
	if !o.quiet && !o.size && !o.sizeChanged && probe.fieldsUsed["Size"] {
		o.size = true
	}
	return nil
}

// writePS writes the containers of GET /containers/json.
func writePS(w io.Writer, o psOptions, cs []psContainer, trunc bool, now time.Time) error {
	rows := make([]any, len(cs))
	for i, c := range cs {
		rows[i] = &psCtx{trunc: trunc, c: c, now: now}
	}
	return formatWriter{format: psFormat(o.format, o.quiet, o.size), header: psHeader}.write(w, rows)
}

// ---- docker images ----

type dockerImageSummary struct {
	ID          string `json:"Id"`
	RepoTags    []string
	RepoDigests []string
	Created     int64
	Size        int64
	SharedSize  int64
	Containers  int64
}

const (
	defaultImageTable           = "table {{.Repository}}\t{{.Tag}}\t{{.ID}}\t{{if .CreatedSince }}{{.CreatedSince}}{{else}}N/A{{end}}\t{{.Size}}"
	defaultImageTableWithDigest = "table {{.Repository}}\t{{.Tag}}\t{{.Digest}}\t{{.ID}}\t{{if .CreatedSince }}{{.CreatedSince}}{{else}}N/A{{end}}\t{{.Size}}"
)

var imageHeader = map[string]string{
	"ID": "IMAGE ID", "Repository": "REPOSITORY", "Tag": "TAG", "Digest": "DIGEST", "CreatedSince": "CREATED",
	"CreatedAt": "CREATED AT", "Size": "SIZE", "Containers": "CONTAINERS", "VirtualSize": "SIZE",
	"SharedSize": "SHARED SIZE", "UniqueSize": "UNIQUE SIZE",
}

// imageFormat is formatter.NewImageFormat.
func imageFormat(source string, quiet, digest bool) string {
	switch source {
	case tableFormatKey:
		switch {
		case quiet:
			return quietFormat
		case digest:
			return defaultImageTableWithDigest
		}
		return defaultImageTable
	case rawFormatKey:
		switch {
		case quiet:
			return `image_id: {{.ID}}`
		case digest:
			return "repository: {{ .Repository }}\ntag: {{.Tag}}\ndigest: {{.Digest}}\nimage_id: {{.ID}}\ncreated_at: {{.CreatedAt}}\nvirtual_size: {{.Size}}\n"
		}
		return "repository: {{ .Repository }}\ntag: {{.Tag}}\nimage_id: {{.ID}}\ncreated_at: {{.CreatedAt}}\nvirtual_size: {{.Size}}\n"
	}
	if strings.HasPrefix(source, tableFormatKey) && digest && !strings.Contains(source, "{{.Digest}}") {
		source += "\t{{.Digest}}"
	}
	return source
}

// imageCtx is a row of docker images (formatter.imageContext).
type imageCtx struct {
	trunc             bool
	i                 dockerImageSummary
	repo, tag, digest string
	now               time.Time
}

func (c *imageCtx) MarshalJSON() ([]byte, error) { return marshalMethods(c) }

func (c *imageCtx) ID() string {
	if c.trunc {
		return truncateID(c.i.ID)
	}
	return c.i.ID
}

func (c *imageCtx) Repository() string { return c.repo }
func (c *imageCtx) Tag() string        { return c.tag }
func (c *imageCtx) Digest() string     { return c.digest }

func (c *imageCtx) CreatedSince() string {
	t := time.Unix(c.i.Created, 0)
	if t.IsZero() {
		return ""
	}
	return units.HumanDuration(c.now.UTC().Sub(t)) + " ago"
}

func (c *imageCtx) CreatedAt() string { return time.Unix(c.i.Created, 0).String() }

func (c *imageCtx) Size() string { return units.HumanSizeWithPrecision(float64(c.i.Size), 3) }

func (c *imageCtx) Containers() string {
	if c.i.Containers == -1 {
		return "N/A"
	}
	return strconv.FormatInt(c.i.Containers, 10)
}

func (c *imageCtx) VirtualSize() string { return units.HumanSize(float64(c.i.Size)) }

func (c *imageCtx) SharedSize() string {
	if c.i.SharedSize == -1 {
		return "N/A"
	}
	return units.HumanSize(float64(c.i.SharedSize))
}

func (c *imageCtx) UniqueSize() string {
	if c.i.Size == -1 || c.i.SharedSize == -1 {
		return "N/A"
	}
	return units.HumanSize(float64(c.i.Size - c.i.SharedSize))
}

// imageRows are the rows of docker images (formatter.imageFormat): one per
// tag, with its digest if asked for, and one with tag <none> for a
// repository that has only a digest. The docker CLI walks maps here, so
// its order varies; this one is sorted by repository.
func imageRows(imgs []dockerImageSummary, trunc, digest, needDigest bool, now time.Time) []any {
	var rows []any
	add := func(img dockerImageSummary, repo, tag, dg string) {
		rows = append(rows, &imageCtx{trunc: trunc, i: img, repo: repo, tag: tag, digest: dg, now: now})
	}
	for _, img := range imgs {
		if len(img.RepoTags) == 0 && len(img.RepoDigests) == 0 {
			add(img, "<none>", "<none>", "<none>")
			continue
		}
		tags, digests := map[string][]string{}, map[string][]string{}
		for _, t := range img.RepoTags {
			i := strings.LastIndexByte(t, ':')
			if i < 0 {
				continue
			}
			tags[t[:i]] = append(tags[t[:i]], t[i+1:])
		}
		for _, d := range img.RepoDigests {
			repo, dg, ok := strings.Cut(d, "@")
			if ok {
				digests[repo] = append(digests[repo], dg)
			}
		}
		for _, repo := range sortedKeys(tags) {
			dgs := digests[repo]
			delete(digests, repo)
			if !needDigest {
				dgs = nil
			}
			for _, tag := range tags[repo] {
				if len(dgs) == 0 {
					add(img, repo, tag, "<none>")
					continue
				}
				for _, dg := range dgs {
					add(img, repo, tag, dg)
				}
			}
		}
		for _, repo := range sortedKeys(digests) {
			if digest {
				for _, dg := range digests[repo] {
					add(img, repo, "<none>", dg)
				}
			} else {
				add(img, repo, "<none>", "")
			}
		}
	}
	return rows
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// matchImageRef keeps the tags of the images that match the argument of
// docker images, as dockerd's reference filter does: a glob on the familiar
// reference, or on the repository when the pattern has no tag.
func matchImageRef(imgs []dockerImageSummary, pattern string) []dockerImageSummary {
	var out []dockerImageSummary
	for _, img := range imgs {
		var tags []string
		for _, t := range img.RepoTags {
			repo := t[:strings.LastIndexByte(t, ':')]
			if ok, _ := path.Match(pattern, t); ok {
				tags = append(tags, t)
			} else if ok, _ := path.Match(pattern, repo); ok {
				tags = append(tags, t)
			}
		}
		if len(tags) == 0 {
			continue
		}
		img.RepoTags = tags
		out = append(out, img)
	}
	return out
}

// writeImages writes the images of GET /images/json.
func writeImages(w io.Writer, format string, quiet, digest, trunc bool, imgs []dockerImageSummary, now time.Time) error {
	if format == "" {
		format = tableFormatKey
	}
	f := imageFormat(format, quiet, digest)
	rows := imageRows(imgs, trunc, digest, digest || strings.Contains(f, "{{.Digest}}"), now)
	return formatWriter{format: f, header: imageHeader}.write(w, rows)
}

// writeFormatted is writeFormattedTo on stdout, with the exit codes of the
// docker CLI: 64 for a template that does not parse, else 1.
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

// writeFormattedTo writes rows with a --format of docker stats and docker
// events: "table TEMPLATE" with a header row, "json", or a template.
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
