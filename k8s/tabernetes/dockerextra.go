package main

// More commands of the docker builtin: wait, rename, top, pause, unpause,
// tag, container prune, image prune, system df and system prune. The
// texts, flags and outputs are those of the docker CLI (docker/cli v28).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	units "github.com/docker/go-units"
	"mvdan.cc/sh/v3/interp"
)

const pruneFilterHelp = `Provide filter values (e.g. "until=<timestamp>")`

var (
	containerPruneCommand = &dockerCommand{
		name: "prune", use: "prune [OPTIONS]", short: "Remove all stopped containers",
		flags: []flagSpec{
			{0, "filter", "filter", pruneFilterHelp},
			{'f', "force", "", "Do not prompt for confirmation"},
		},
		run: dockerContainerPrune,
	}
	imagePruneCommand = &dockerCommand{
		name: "prune", use: "prune [OPTIONS]", short: "Remove unused images",
		flags: []flagSpec{
			{'a', "all", "", "Remove all unused images, not just dangling ones"},
			{0, "filter", "filter", pruneFilterHelp},
			{'f', "force", "", "Do not prompt for confirmation"},
		},
		run: dockerImagePrune,
	}
	systemDFCommand = &dockerCommand{
		name: "df", use: "df [OPTIONS]", short: "Show docker disk usage",
		flags: []flagSpec{
			{0, "format", "string", formatHelp},
			{'v', "verbose", "", "Show detailed information on space usage"},
		},
		run: dockerSystemDF,
	}
	systemPruneCommand = &dockerCommand{
		name: "prune", use: "prune [OPTIONS]", short: "Remove unused data",
		flags: []flagSpec{
			{'a', "all", "", "Remove all unused images not just dangling ones"},
			{0, "filter", "filter", `Provide filter values (e.g. "label=<key>=<value>")`},
			{'f', "force", "", "Do not prompt for confirmation"},
			{0, "volumes", "", "Prune anonymous volumes"},
		},
		run: dockerSystemPrune,
	}
)

func init() {
	for _, c := range []*dockerCommand{
		{name: "wait", use: "wait CONTAINER [CONTAINER...]", short: "Block until one or more containers stop, then print their exit codes",
			aliases: []string{"docker container wait", "docker wait"}, minArgs: 1, maxArgs: -1, run: dockerWait},
		{name: "rename", use: "rename CONTAINER NEW_NAME", short: "Rename a container",
			aliases: []string{"docker container rename", "docker rename"}, minArgs: 2, maxArgs: 2, run: dockerRename},
		{name: "top", use: "top CONTAINER [ps OPTIONS]", short: "Display the running processes of a container",
			aliases: []string{"docker container top", "docker top"}, raw: dockerTop},
		{name: "pause", use: "pause CONTAINER [CONTAINER...]", short: "Pause all processes within one or more containers",
			aliases: []string{"docker container pause", "docker pause"}, minArgs: 1, maxArgs: -1, run: dockerPauseOf("pause")},
		{name: "unpause", use: "unpause CONTAINER [CONTAINER...]", short: "Unpause all processes within one or more containers",
			aliases: []string{"docker container unpause", "docker unpause"}, minArgs: 1, maxArgs: -1, run: dockerPauseOf("unpause")},
		{name: "tag", use: "tag SOURCE_IMAGE[:TAG] TARGET_IMAGE[:TAG]", short: "Create a tag TARGET_IMAGE that refers to SOURCE_IMAGE",
			aliases: []string{"docker image tag", "docker tag"}, minArgs: 2, maxArgs: 2, run: dockerTag},
	} {
		dockerCommands[c.name] = c
	}
	containers := dockerManagement["container"].subs
	for _, n := range []string{"wait", "rename", "top", "pause", "unpause"} {
		containers[n] = dockerCommands[n]
	}
	containers["prune"] = containerPruneCommand
	images := dockerManagement["image"].subs
	images["tag"], images["prune"] = dockerCommands["tag"], imagePruneCommand
	dockerManagement["system"] = struct {
		short string
		subs  map[string]*dockerCommand
	}{"Manage Docker", map[string]*dockerCommand{"df": systemDFCommand, "prune": systemPruneCommand}}
}

// dockerErrors prints errors as the docker CLI prints errors.Join: one per
// line, and the exit code 1.
func dockerErrors(hc interp.HandlerContext, errs []string) error {
	if len(errs) > 0 {
		return fail(hc, 1, "%s", strings.Join(errs, "\n"))
	}
	return nil
}

// dockerPost sends a POST without a body and returns the body; a status
// other than want is the error of the daemon.
func dockerPost(ctx context.Context, path string, want int) ([]byte, error) {
	body, status, err := dockerCall(ctx, http.MethodPost, path, nil)
	if err != nil {
		return nil, err
	}
	if status != want {
		return nil, errors.New(daemonError(body, status))
	}
	return body, nil
}

// dockerWait prints the exit code of each container once it stopped:
// `docker wait CONTAINER...`.
func dockerWait(ctx context.Context, hc interp.HandlerContext, _ dockerFlagValues, args []string) error {
	var errs []string
	for _, ref := range args {
		body, err := dockerPost(ctx, "/containers/"+url.PathEscape(ref)+"/wait", http.StatusOK)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		var w struct{ StatusCode int }
		json.Unmarshal(body, &w)
		fmt.Fprintf(hc.Stdout, "%d\n", w.StatusCode)
	}
	return dockerErrors(hc, errs)
}

// dockerRename renames a container: `docker rename CONTAINER NEW_NAME`.
func dockerRename(ctx context.Context, hc interp.HandlerContext, _ dockerFlagValues, args []string) error {
	oldName, newName := strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
	if oldName == "" || newName == "" {
		return fail(hc, 1, "Error: Neither old nor new names may be empty")
	}
	if _, err := dockerPost(ctx, "/containers/"+url.PathEscape(oldName)+"/rename?name="+url.QueryEscape(newName), http.StatusNoContent); err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return fail(hc, 1, "Error: failed to rename container named %s", oldName)
	}
	return nil
}

// dockerTop shows the processes of a container: `docker top CONTAINER [ps
// OPTIONS]`. Everything after the container is an option of ps.
func dockerTop(ctx context.Context, hc interp.HandlerContext, args []string) error {
	c := dockerCommands["top"]
	if len(args) == 0 {
		return fail(hc, 1, "docker: 'docker top' requires at least 1 argument\n\nUsage:  docker top CONTAINER [ps OPTIONS]\n\nSee 'docker top --help' for more information")
	}
	if strings.HasPrefix(args[0], "-") && args[0] != "-" {
		if args[0] == "-h" {
			fmt.Fprint(hc.Stdout, commandUsage("docker top", c))
			return nil
		}
		flag := args[0]
		if strings.HasPrefix(flag, "--") {
			flag = "unknown flag: " + flag
		} else {
			flag = fmt.Sprintf("unknown shorthand flag: '%c' in %s", flag[1], flag)
		}
		return fail(hc, 125, "%s\n\nUsage:  docker top CONTAINER [ps OPTIONS]\n\nRun 'docker top --help' for more information", flag)
	}
	q := url.Values{}
	if len(args) > 1 {
		q.Set("ps_args", strings.Join(args[1:], " "))
	}
	var top struct {
		Titles    []string
		Processes [][]string
	}
	if _, err := dockerGet(ctx, "/containers/"+url.PathEscape(args[0])+"/top?"+q.Encode(), &top); err != nil {
		return fail(hc, 1, "%v", err)
	}
	// the settings of docker top (runTop)
	var b bytes.Buffer
	w := tabwriter.NewWriter(&b, 20, 1, 3, ' ', 0)
	fmt.Fprintln(w, strings.Join(top.Titles, "\t"))
	for _, p := range top.Processes {
		fmt.Fprintln(w, strings.Join(p, "\t"))
	}
	w.Flush()
	hc.Stdout.Write(b.Bytes())
	return nil
}

// dockerPauseOf is docker pause or docker unpause: the names as given on
// success, the errors of the daemon after them.
func dockerPauseOf(op string) func(ctx context.Context, hc interp.HandlerContext, _ dockerFlagValues, args []string) error {
	return func(ctx context.Context, hc interp.HandlerContext, _ dockerFlagValues, args []string) error {
		var errs []string
		for _, ref := range args {
			if _, err := dockerPost(ctx, "/containers/"+url.PathEscape(ref)+"/"+op, http.StatusNoContent); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			fmt.Fprintln(hc.Stdout, ref)
		}
		return dockerErrors(hc, errs)
	}
}

// The grammar of image references (distribution/reference).
var (
	refComponent = `[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*`
	refDomain    = `(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*|\[[0-9a-fA-F:]+\])(?::[0-9]+)?`
	refPattern   = regexp.MustCompile(`^(?:(` + refDomain + `)/)?(` + refComponent + `(?:/` + refComponent + `)*)(?::([\w][\w.-]{0,127}))?(?:@(sha256:[0-9a-f]{64}))?$`)
	anyCase      = regexp.MustCompile(`^(?:(` + refDomain + `)/)?([a-zA-Z0-9]+(?:(?:[._]|__|[-]+)[a-zA-Z0-9]+)*(?:/[a-zA-Z0-9]+(?:(?:[._]|__|[-]+)[a-zA-Z0-9]+)*)*)(?::[\w][\w.-]{0,127})?(?:@sha256:[0-9a-f]{64})?$`)
)

// parseTagTarget checks the target of docker tag as the Docker client does
// (ParseNormalizedNamed) and returns its repository and tag ("latest" if
// none): "myapp:v1" is docker.io/library/myapp and v1.
func parseTagTarget(ref string) (repo, tag string, err error) {
	m := refPattern.FindStringSubmatch(ref)
	if m == nil {
		if a := anyCase.FindStringSubmatch(ref); a != nil {
			name := a[2]
			if a[1] == "" && !strings.Contains(name, "/") {
				name = "library/" + name
			}
			return "", "", fmt.Errorf("Error parsing reference: %q is not a valid repository/tag: invalid reference format: repository name (%s) must be lowercase", ref, name)
		}
		return "", "", fmt.Errorf("Error parsing reference: %q is not a valid repository/tag: invalid reference format", ref)
	}
	if m[4] != "" {
		return "", "", errors.New("refusing to create a tag with a digest reference")
	}
	domain, path := m[1], m[2]
	if domain != "" && !strings.ContainsAny(domain, ".:") && domain != "localhost" && strings.ToLower(domain) == domain {
		path, domain = domain+"/"+path, ""
	}
	if domain == "" {
		domain = "docker.io"
		if !strings.Contains(path, "/") {
			path = "library/" + path
		}
	}
	tag = m[3]
	if tag == "" {
		tag = "latest"
	}
	return domain + "/" + path, tag, nil
}

// dockerTag gives an image one more name: `docker tag SOURCE TARGET`.
func dockerTag(ctx context.Context, hc interp.HandlerContext, _ dockerFlagValues, args []string) error {
	source, target := args[0], args[1]
	if !anyCase.MatchString(source) && !regexp.MustCompile(`^(sha256:)?[0-9a-f]{4,64}$`).MatchString(source) {
		return fail(hc, 1, "Error parsing reference: %q is not a valid repository/tag: invalid reference format", source)
	}
	repo, tag, err := parseTagTarget(target)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	q := url.Values{"repo": {repo}, "tag": {tag}}
	if _, err := dockerPost(ctx, "/images/"+imagePath(source)+"/tag?"+q.Encode(), http.StatusCreated); err != nil {
		return fail(hc, 1, "%v", err)
	}
	return nil
}

// errPromptTerminated is the answer of a prompt ended with Ctrl-C.
var errPromptTerminated = errors.New("prompt terminated")

// dockerConfirm asks message with " [y/N] " and reads a line, as the
// docker CLI's prompt.Confirm: only y or Y confirm. On the terminal it
// edits the line itself (the shell gives commands no stdin); Ctrl-C ends
// the prompt.
func dockerConfirm(ctx context.Context, hc interp.HandlerContext, message string) (bool, error) {
	if message == "" {
		message = "Are you sure you want to proceed?"
	}
	message += " [y/N] "
	t := termFrom(ctx)
	if !fromTerminal(hc.Stdin) || t == nil {
		io.WriteString(hc.Stdout, message)
		sc := bufio.NewScanner(hc.Stdin)
		if sc.Scan() {
			return strings.EqualFold(strings.TrimSpace(sc.Text()), "y"), nil
		}
		return false, nil
	}
	leave := t.enterLine()
	defer leave()
	io.WriteString(t, message)
	var line []rune
	for {
		select {
		case <-ctx.Done():
			io.WriteString(t, "\r\n")
			return false, errPromptTerminated
		case s := <-t.keys:
			for _, k := range parseKeys(s) {
				switch {
				case k.name == "ctrl-c":
					io.WriteString(t, "^C\r\n")
					return false, errPromptTerminated
				case k.name == "enter" || k.name == "ctrl-d":
					io.WriteString(t, "\r\n")
					return strings.EqualFold(strings.TrimSpace(string(line)), "y"), nil
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

// pruneReport is the answer of POST /containers/prune and /images/prune.
type pruneReport struct {
	ContainersDeleted []string
	VolumesDeleted    []string
	ImagesDeleted     []struct{ Untagged, Deleted string }
	SpaceReclaimed    uint64
}

// runPrune sends a prune request and returns the reclaimed space and the
// output, as runPrune of the docker CLI.
func runPrune(ctx context.Context, path string, filters []string) (uint64, string, error) {
	if len(filters) > 0 {
		path += "?filters=" + url.QueryEscape(filterJSON(filters))
	}
	body, err := dockerPost(ctx, path, http.StatusOK)
	if err != nil {
		return 0, "", err
	}
	var r pruneReport
	json.Unmarshal(body, &r)
	var out strings.Builder
	if len(r.ContainersDeleted) > 0 {
		out.WriteString("Deleted Containers:\n")
		for _, id := range r.ContainersDeleted {
			out.WriteString(id + "\n")
		}
	}
	if len(r.VolumesDeleted) > 0 {
		out.WriteString("Deleted Volumes:\n")
		for _, v := range r.VolumesDeleted {
			out.WriteString(v + "\n")
		}
	}
	if len(r.ImagesDeleted) > 0 {
		out.WriteString("Deleted Images:\n")
		for _, d := range r.ImagesDeleted {
			if d.Untagged != "" {
				out.WriteString("untagged: " + d.Untagged + "\n")
			} else {
				out.WriteString("deleted: " + d.Deleted + "\n")
			}
		}
	}
	if out.Len() == 0 {
		return 0, "", nil
	}
	return r.SpaceReclaimed, out.String(), nil
}

// confirmed asks for the prompt of a prune unless -f: false and nil when
// the user said no or pressed Ctrl-C, which the docker CLI ends with exit
// code 0 and no message (a cancelled error).
func confirmed(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, warning string) bool {
	if f.bool("force") {
		return true
	}
	ok, _ := dockerConfirm(ctx, hc, warning)
	return ok
}

// dockerContainerPrune removes the stopped containers: `docker container
// prune`.
func dockerContainerPrune(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, _ []string) error {
	if !confirmed(ctx, hc, f, "WARNING! This will remove all stopped containers.\nAre you sure you want to continue?") {
		return nil
	}
	space, out, err := runPrune(ctx, "/containers/prune", f.list("filter"))
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if out != "" {
		fmt.Fprintln(hc.Stdout, out)
	}
	fmt.Fprintln(hc.Stdout, "Total reclaimed space:", units.HumanSize(float64(space)))
	return nil
}

// imagePruneFilters are the filters of docker image prune: dangling=true,
// or false with -a.
func imagePruneFilters(f dockerFlagValues, all bool) []string {
	return append(f.list("filter"), fmt.Sprintf("dangling=%t", !all))
}

// dockerImagePrune removes unused images: `docker image prune [-a]`.
func dockerImagePrune(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, _ []string) error {
	warning := "WARNING! This will remove all dangling images.\nAre you sure you want to continue?"
	if f.bool("all") {
		warning = "WARNING! This will remove all images without at least one container associated to them.\nAre you sure you want to continue?"
	}
	if !confirmed(ctx, hc, f, warning) {
		return nil
	}
	space, out, err := runPrune(ctx, "/images/prune", imagePruneFilters(f, f.bool("all")))
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if out != "" {
		fmt.Fprintln(hc.Stdout, out)
	}
	fmt.Fprintln(hc.Stdout, "Total reclaimed space:", units.HumanSize(float64(space)))
	return nil
}

// systemPruneWarning is the prompt of docker system prune
// (confirmationTemplate of the docker CLI).
func systemPruneWarning(all, volumes bool, filters []string) string {
	warnings := []string{"all stopped containers", "all networks not used by at least one container"}
	if volumes {
		warnings = append(warnings, "all anonymous volumes not used by at least one container")
	}
	if all {
		warnings = append(warnings, "all images without at least one container associated to them", "all build cache")
	} else {
		warnings = append(warnings, "all dangling images", "unused build cache")
	}
	var b strings.Builder
	b.WriteString("WARNING! This will remove:")
	for _, w := range warnings {
		b.WriteString("\n  - " + w)
	}
	var shown []string
	for _, f := range filters {
		if k, _, _ := strings.Cut(f, "="); k == "label" || k == "label!" || k == "until" {
			shown = append(shown, f)
		}
	}
	if len(shown) > 0 {
		b.WriteString("\n\n  Items to be pruned will be filtered with:")
		for _, f := range shown {
			b.WriteString("\n  - " + f)
		}
	}
	b.WriteString("\n\nAre you sure you want to continue?")
	return b.String()
}

// dockerSystemPrune removes stopped containers and unused images: `docker
// system prune`. The tab has no networks of its own and no build cache.
func dockerSystemPrune(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, _ []string) error {
	all, volumes, filters := f.bool("all"), f.bool("volumes"), f.list("filter")
	if volumes {
		for _, kv := range filters {
			if k, _, _ := strings.Cut(kv, "="); k == "until" {
				return fail(hc, 1, `ERROR: The "until" filter is not supported with "--volumes"`)
			}
		}
	}
	if !confirmed(ctx, hc, f, systemPruneWarning(all, volumes, filters)) {
		return nil
	}
	steps := []struct {
		path    string
		filters []string
	}{{"/containers/prune", filters}}
	if volumes {
		steps = append(steps, struct {
			path    string
			filters []string
		}{"/volumes/prune", filters})
	}
	steps = append(steps, struct {
		path    string
		filters []string
	}{"/images/prune", imagePruneFilters(f, all)})
	var total uint64
	for _, s := range steps {
		space, out, err := runPrune(ctx, s.path, s.filters)
		if err != nil {
			return fail(hc, 1, "%v", err)
		}
		total += space
		if out != "" {
			fmt.Fprintln(hc.Stdout, out)
		}
	}
	fmt.Fprintln(hc.Stdout, "Total reclaimed space:", units.HumanSize(float64(total)))
	return nil
}

// diskUsage is the answer of GET /system/df.
type diskUsage struct {
	LayersSize int64
	Images     []dockerImageSummary
	Containers []psContainer
	Volumes    []struct {
		Name      string
		UsageData struct{ Size, RefCount int64 }
	}
}

// dfCtx is a row of docker system df (formatter.diskUsage*Context).
type dfCtx struct {
	typ               string
	total, active     int
	size, reclaimable int64
	totalSize         int64 // for the percentage
	plain             bool  // no percentage (build cache)
}

func (c *dfCtx) MarshalJSON() ([]byte, error) { return marshalMethods(c) }
func (c *dfCtx) Type() string                 { return c.typ }
func (c *dfCtx) TotalCount() string           { return fmt.Sprint(c.total) }
func (c *dfCtx) Active() string               { return fmt.Sprint(c.active) }
func (c *dfCtx) Size() string                 { return units.HumanSize(float64(c.size)) }

func (c *dfCtx) Reclaimable() string {
	if c.totalSize > 0 && !c.plain {
		return fmt.Sprintf("%s (%v%%)", units.HumanSize(float64(c.reclaimable)), c.reclaimable*100/c.totalSize)
	}
	return units.HumanSize(float64(c.reclaimable))
}

var dfHeader = map[string]string{"Type": "TYPE", "TotalCount": "TOTAL", "Active": "ACTIVE", "Size": "SIZE", "Reclaimable": "RECLAIMABLE"}

// dockerSystemDF shows what images and containers take: `docker system df`.
func dockerSystemDF(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, _ []string) error {
	var du diskUsage
	if _, err := dockerGet(ctx, "/system/df", &du); err != nil {
		return fail(hc, 1, "%v", err)
	}
	var out bytes.Buffer
	var err error
	if f.bool("verbose") {
		err = writeDFVerbose(&out, f["format"], du, time.Now())
	} else {
		err = writeDF(&out, f["format"], du)
	}
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	hc.Stdout.Write(out.Bytes())
	return nil
}

// writeDF writes the summary of docker system df.
func writeDF(w io.Writer, format string, du diskUsage) error {
	switch format {
	case "", tableFormatKey:
		format = "table {{.Type}}\t{{.TotalCount}}\t{{.Active}}\t{{.Size}}\t{{.Reclaimable}}"
	case rawFormatKey:
		format = "type: {{.Type}}\ntotal: {{.TotalCount}}\nactive: {{.Active}}\nsize: {{.Size}}\nreclaimable: {{.Reclaimable}}\n"
	}
	images := &dfCtx{typ: "Images", total: len(du.Images), size: du.LayersSize, totalSize: du.LayersSize}
	var used int64
	for _, i := range du.Images {
		if i.Containers > 0 {
			images.active++
			if i.Size != -1 && i.SharedSize != -1 {
				used += i.Size - i.SharedSize
			}
		}
	}
	images.reclaimable = du.LayersSize - used
	containers := &dfCtx{typ: "Containers", total: len(du.Containers)}
	for _, c := range du.Containers {
		active := c.State == "running" || c.State == "paused" || c.State == "restarting"
		if active {
			containers.active++
		} else {
			containers.reclaimable += c.SizeRw
		}
		containers.size += c.SizeRw
	}
	containers.totalSize = containers.size
	volumes := &dfCtx{typ: "Local Volumes", total: len(du.Volumes)}
	for _, v := range du.Volumes {
		if v.UsageData.RefCount > 0 {
			volumes.active++
		}
		if v.UsageData.Size != -1 {
			if v.UsageData.RefCount == 0 {
				volumes.reclaimable += v.UsageData.Size
			}
			volumes.size += v.UsageData.Size
		}
	}
	volumes.totalSize = volumes.size
	cache := &dfCtx{typ: "Build Cache", plain: true}
	return formatWriter{format: format, header: dfHeader}.write(w, []any{images, containers, volumes, cache})
}

// writeDFVerbose writes docker system df -v: a table each for the images,
// the containers, the volumes and the build cache.
func writeDFVerbose(w io.Writer, format string, du diskUsage, now time.Time) error {
	if format != "" && format != tableFormatKey {
		return errors.New("the tab supports docker system df -v only as a table")
	}
	images := []any{}
	for _, i := range du.Images {
		repo, tag := "<none>", "<none>"
		if len(i.RepoTags) > 0 {
			t := i.RepoTags[0]
			repo, tag = t[:strings.LastIndexByte(t, ':')], t[strings.LastIndexByte(t, ':')+1:]
		}
		images = append(images, &imageCtx{trunc: true, i: i, repo: repo, tag: tag, now: now})
	}
	io.WriteString(w, "Images space usage:\n\n")
	if err := (formatWriter{format: "table {{.Repository}}\t{{.Tag}}\t{{.ID}}\t{{.CreatedSince}}\t{{.Size}}\t{{.SharedSize}}\t{{.UniqueSize}}\t{{.Containers}}", header: imageHeader}).write(w, images); err != nil {
		return err
	}
	containers := []any{}
	for _, c := range du.Containers {
		c.SizeRootFs = 0 // no virtual size here
		containers = append(containers, &psCtx{trunc: true, c: c, now: now})
	}
	io.WriteString(w, "\nContainers space usage:\n\n")
	if err := (formatWriter{format: "table {{.ID}}\t{{.Image}}\t{{.Command}}\t{{.LocalVolumes}}\t{{.Size}}\t{{.RunningFor}}\t{{.Status}}\t{{.Names}}", header: psHeader}).write(w, containers); err != nil {
		return err
	}
	io.WriteString(w, "\nLocal Volumes space usage:\n\n")
	var vol bytes.Buffer
	vt := tabwriter.NewWriter(&vol, 10, 1, 3, ' ', 0)
	fmt.Fprintln(vt, "VOLUME NAME\tLINKS\tSIZE")
	for _, v := range du.Volumes {
		size := "N/A"
		if v.UsageData.Size != -1 {
			size = units.HumanSizeWithPrecision(float64(v.UsageData.Size), 3)
		}
		fmt.Fprintf(vt, "%s\t%d\t%s\n", v.Name, v.UsageData.RefCount, size)
	}
	vt.Flush()
	w.Write(vol.Bytes())
	fmt.Fprintf(w, "\nBuild cache usage: %s\n\n", units.HumanSize(0))
	var bc bytes.Buffer
	bt := tabwriter.NewWriter(&bc, 10, 1, 3, ' ', 0)
	fmt.Fprintln(bt, "CACHE ID\tCACHE TYPE\tSIZE\tCREATED\tLAST USED\tUSAGE\tSHARED")
	bt.Flush()
	w.Write(bc.Bytes())
	return nil
}
