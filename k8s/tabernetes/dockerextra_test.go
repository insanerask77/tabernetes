package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testContainers(now time.Time) []psContainer {
	c := psContainer{
		ID: "4f6e3c2b1a0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f", Names: []string{"/web"}, Image: "docker.io/library/nginx:1.27",
		ImageID: "sha256:b2b2", Command: "/docker-entrypoint.sh nginx -g 'daemon off;'", Created: now.Add(-2 * time.Minute).Unix(),
		Ports: []psPort{{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080, Type: "tcp"}}, State: "paused", Status: "Up 2 minutes (Paused)",
		Labels: map[string]string{"maintainer": "NGINX <docker-maint@nginx.com>", "tier": "web"}, SizeRootFs: 192_000_000,
	}
	c.NetworkSettings = &struct{ Networks map[string]json.RawMessage }{Networks: map[string]json.RawMessage{"bridge": json.RawMessage(`{}`)}}
	return []psContainer{c}
}

func TestPSFormats(t *testing.T) {
	now := time.Now()
	cs := testContainers(now)
	for _, tc := range []struct {
		o    psOptions
		want string
	}{
		{psOptions{format: "{{.Names}} {{.State}} {{.Label \"tier\"}} {{.Networks}} {{.Ports}}"}, "web paused web bridge 0.0.0.0:8080->80/tcp\n"},
		{psOptions{format: "{{.Labels}}"}, "maintainer=NGINX <docker-maint@nginx.com>,tier=web\n"},
		{psOptions{format: "{{.Image}}|{{.Mounts}}|{{.LocalVolumes}}|{{.Platform}}|{{.Size}}"}, "nginx:1.27||0|<nil>|0B (virtual 192MB)\n"},
		{psOptions{format: "{{.ID}}", quiet: true}, "4f6e3c2b1a0d\n"},
		{psOptions{format: "{{.Names}}", quiet: true}, "4f6e3c2b1a0d\n"}, // -q wins over --format
		{psOptions{format: "table {{.Names}}\t{{.Status}}"}, "NAMES     STATUS\nweb       Up 2 minutes (Paused)\n"},
		{psOptions{format: "table"}, ""}, // the default table, checked below
		{psOptions{format: "table {{.Names}}\t{{.Label \"tier\"}}"}, "NAMES     \nweb       web\n"},
		{psOptions{format: "{{upper .Names}} {{pad .State 1 1}}|{{truncate .ID 4}}|{{title .State}}|{{join (split .Names \"e\") \"-\"}}"}, "WEB  paused |4f6e|Paused|w-b\n"},
		{psOptions{format: "raw"}, "container_id: 4f6e3c2b1a0d\nimage: nginx:1.27\ncommand: \"/docker-entrypoint.…\"\ncreated_at: " +
			time.Unix(cs[0].Created, 0).String() + "\nstate: paused\nstatus: Up 2 minutes (Paused)\nnames: web\nlabels: maintainer=NGINX <docker-maint@nginx.com>,tier=web\nports: 0.0.0.0:8080->80/tcp\n\n"},
	} {
		var out bytes.Buffer
		if err := writePS(&out, tc.o, cs, true, now); err != nil {
			t.Errorf("%+v: %v", tc.o, err)
			continue
		}
		if tc.o.format == "table" {
			if !strings.HasPrefix(out.String(), "CONTAINER ID   IMAGE        COMMAND") {
				t.Errorf("table: %q", out.String())
			}
			continue
		}
		if out.String() != tc.want {
			t.Errorf("%q:\n%q\nwant\n%q", tc.o.format, out.String(), tc.want)
		}
	}

	// json: every field, keys sorted, HTML escaped like the docker CLI
	var out bytes.Buffer
	writePS(&out, psOptions{format: "json"}, cs, true, now)
	want := `{"Command":"\"/docker-entrypoint.…\"","CreatedAt":"` + time.Unix(cs[0].Created, 0).String() + `","ID":"4f6e3c2b1a0d","Image":"nginx:1.27",` +
		`"Labels":"maintainer=NGINX \u003cdocker-maint@nginx.com\u003e,tier=web","LocalVolumes":"0","Mounts":"","Names":"web","Networks":"bridge",` +
		`"Platform":null,"Ports":"0.0.0.0:8080-\u003e80/tcp","RunningFor":"2 minutes ago","Size":"0B (virtual 192MB)","State":"paused","Status":"Up 2 minutes (Paused)"}` + "\n"
	if out.String() != want {
		t.Errorf("json:\n%s\nwant:\n%s", out.String(), want)
	}

	// -s adds SIZE; .Size in a template asks for the sizes
	out.Reset()
	writePS(&out, psOptions{size: true}, cs, true, now)
	if !strings.Contains(strings.SplitN(out.String(), "\n", 2)[0], "NAMES     SIZE") || !strings.Contains(out.String(), "0B (virtual 192MB)") {
		t.Errorf("ps -s:\n%s", out.String())
	}
	o := psOptions{format: "{{.Names}} {{.Size}}"}
	if err := o.prepare(); err != nil || !o.size {
		t.Errorf("prepare: %v %v", o.size, err)
	}
	o = psOptions{format: "{{.Names}} {{.Size}}", sizeChanged: true}
	if o.prepare(); o.size {
		t.Error("--size=false must win")
	}
	o = psOptions{format: "{{.Names"}
	if err := o.prepare(); err == nil || !strings.HasPrefix(err.Error(), "failed to parse template: template: :1: unclosed action") {
		t.Errorf("bad template: %v", err)
	}
	o = psOptions{format: "{{.Nope}}"}
	if err := o.prepare(); err == nil || !strings.HasPrefix(err.Error(), "failed to execute template:") {
		t.Errorf("unknown field: %v", err)
	}
}

func TestFilterFlag(t *testing.T) {
	f, _, err := parseDockerFlags(psCommand, []string{"-f", "name=web", "--filter", "status=exited", "--filter=label=a=b", "-s"})
	if err != nil || !reflect.DeepEqual(f.list("filter"), []string{"name=web", "status=exited", "label=a=b"}) || !f.bool("size") {
		t.Fatalf("%v %v", f, err)
	}
	if got := filterJSON(f.list("filter")); got != `{"label":{"a=b":true},"name":{"web":true},"status":{"exited":true}}` {
		t.Errorf("filterJSON: %s", got)
	}
	if _, _, err := parseDockerFlags(psCommand, []string{"-f", "web"}); err == nil ||
		err.Error() != `invalid argument "web" for "-f, --filter" flag: bad format of filter (expected name=value)` {
		t.Errorf("bad filter: %v", err)
	}
}

func TestEscapeKeys(t *testing.T) {
	for _, tc := range []struct {
		keys   []byte
		in     []string
		out    string
		detach bool
	}{
		{defaultDetachKeys, []string{"ls\r"}, "ls\r", false},
		{defaultDetachKeys, []string{"\x10\x11"}, "", true},
		{defaultDetachKeys, []string{"a\x10", "\x11"}, "a", true},
		{defaultDetachKeys, []string{"\x10x"}, "\x10x", false},
		{defaultDetachKeys, []string{"\x10\x10\x11"}, "\x10", true},
		{[]byte("ab"), []string{"xa", "c", "ab"}, "xac", true},
	} {
		e := &escapeKeys{keys: tc.keys}
		var out []byte
		detach := false
		for _, s := range tc.in {
			o, d := e.feed([]byte(s))
			out = append(out, o...)
			if d {
				detach = true
				break
			}
		}
		if string(out) != tc.out || detach != tc.detach {
			t.Errorf("%q %q: %q %v; want %q %v", tc.keys, tc.in, out, detach, tc.out, tc.detach)
		}
	}
	if k, err := parseDetachKeys("ctrl-a,x,DEL,ctrl-\\"); err != nil || !reflect.DeepEqual(k, []byte{1, 'x', 127, 28}) {
		t.Errorf("parseDetachKeys: %v %v", k, err)
	}
	if _, err := parseDetachKeys("ctrl-p,foo"); err == nil || err.Error() != "Unknown character: 'foo'" {
		t.Errorf("parseDetachKeys foo: %v", err)
	}
}

func TestParseTagTarget(t *testing.T) {
	for _, tc := range []struct{ in, repo, tag, err string }{
		{"myapp", "docker.io/library/myapp", "latest", ""},
		{"myapp:v1", "docker.io/library/myapp", "v1", ""},
		{"me/app:1.0", "docker.io/me/app", "1.0", ""},
		{"localhost:5000/app", "localhost:5000/app", "latest", ""},
		{"ghcr.io/a/b:c", "ghcr.io/a/b", "c", ""},
		{"MyApp", "", "", `Error parsing reference: "MyApp" is not a valid repository/tag: invalid reference format: repository name (library/MyApp) must be lowercase`},
		{"a:b:c", "", "", `Error parsing reference: "a:b:c" is not a valid repository/tag: invalid reference format`},
		{"app@sha256:" + strings.Repeat("a", 64), "", "", "refusing to create a tag with a digest reference"},
	} {
		repo, tag, err := parseTagTarget(tc.in)
		if tc.err != "" {
			if err == nil || err.Error() != tc.err {
				t.Errorf("%s: %v", tc.in, err)
			}
			continue
		}
		if err != nil || repo != tc.repo || tag != tc.tag {
			t.Errorf("%s: %s %s %v", tc.in, repo, tag, err)
		}
	}
}

// A report of init (ps_report in vm/init/init.c) with init, a kernel
// thread, nginx and a shell of docker exec -it.
const testPsReport = "uptime 125.50\nmemtotal 245000\n" +
	"0\t1 (init) S 0 1 1 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 1 1000000 50 18446744073709551615\t/init\n" +
	"0\t2 (kthreadd) S 0 0 0 0 -1 2129984 0 0 0 0 0 0 0 0 20 0 1 0 1 0 0 18446744073709551615\t\n" +
	"0\t3 (kworker/0:0) I 2 0 0 0 -1 69238880 0 0 0 0 0 0 0 0 20 0 1 0 1 0 0 18446744073709551615\t\n" +
	"0\t40 (nginx) S 1 40 40 0 -1 4194560 300 0 0 0 150 50 0 0 20 0 1 0 2550 11210752 1250 18446744073709551615\tnginx: master process nginx -g daemon off;\n" +
	"101\t41 (nginx) S 40 40 40 0 -1 4194624 30 0 0 0 0 0 0 0 20 0 1 0 2560 11640832 400 18446744073709551615\tnginx: worker process\n" +
	"0\t52 (sh) S 1 52 52 34816 52 4194560 30 0 0 0 0 0 0 0 20 0 1 0 12000 1700000 150 18446744073709551615\tsh\n"

func TestFormatTop(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 30, 0, 0, time.Local)
	titles, rows, err := formatTop(testPsReport, "-ef", now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(titles, " ") != "UID PID PPID C STIME TTY TIME CMD" {
		t.Errorf("titles: %q", titles)
	}
	want := [][]string{
		{"root", "40", "1", "2", "10:28", "?", "00:00:02", "nginx: master process nginx -g daemon off;"},
		{"101", "41", "40", "0", "10:28", "?", "00:00:00", "nginx: worker process"},
		{"root", "52", "1", "0", "10:29", "pts/0", "00:00:00", "sh"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Errorf("-ef:\n%q\nwant:\n%q", rows, want)
	}
	titles, rows, _ = formatTop(testPsReport, "aux", now)
	if strings.Join(titles, " ") != "USER PID %CPU %MEM VSZ RSS TTY STAT START TIME COMMAND" ||
		!reflect.DeepEqual(rows[0], []string{"root", "40", "2.0", "2.0", "10948", "5000", "?", "Ss", "10:28", "0:02", "nginx: master process nginx -g daemon off;"}) ||
		rows[2][7] != "Ss+" {
		t.Errorf("aux: %q %q", titles, rows)
	}
	titles, rows, _ = formatTop(testPsReport, "", now)
	if strings.Join(titles, " ") != "PID TTY TIME CMD" || !reflect.DeepEqual(rows[1], []string{"41", "?", "00:00:00", "nginx"}) {
		t.Errorf("default: %q %q", titles, rows)
	}
	if _, _, err := formatTop(testPsReport, "-o pid", now); err == nil || !strings.HasPrefix(err.Error(), "ps: error: unsupported SysV option: -o") {
		t.Errorf("-o: %v", err)
	}
	if got := startTime(now.Add(-25*time.Hour), now); got != "Nov15" {
		t.Errorf("STIME of yesterday: %s", got)
	}
	if got := cpuTime(90061); got != "1-01:01:01" {
		t.Errorf("cpuTime: %s", got)
	}
}

func TestSystemPruneWarning(t *testing.T) {
	want := `WARNING! This will remove:
  - all stopped containers
  - all networks not used by at least one container
  - all dangling images
  - unused build cache

Are you sure you want to continue?`
	if got := systemPruneWarning(false, false, nil); got != want {
		t.Errorf("system prune:\n%s", got)
	}
	want = `WARNING! This will remove:
  - all stopped containers
  - all networks not used by at least one container
  - all anonymous volumes not used by at least one container
  - all images without at least one container associated to them
  - all build cache

  Items to be pruned will be filtered with:
  - label=a

Are you sure you want to continue?`
	if got := systemPruneWarning(true, true, []string{"label=a"}); got != want {
		t.Errorf("system prune -a --volumes:\n%s", got)
	}
}

func TestWriteDF(t *testing.T) {
	du := diskUsage{
		LayersSize: 200_000_000,
		Images: []dockerImageSummary{
			{ID: "sha256:aa", RepoTags: []string{"nginx:latest"}, Size: 190_000_000, SharedSize: 0, Containers: 1},
			{ID: "sha256:bb", RepoTags: []string{"alpine:latest"}, Size: 10_000_000, SharedSize: 0, Containers: 0},
		},
		Containers: []psContainer{{State: "running"}, {State: "exited"}},
	}
	var out bytes.Buffer
	if err := writeDF(&out, "", du); err != nil {
		t.Fatal(err)
	}
	want := `TYPE            TOTAL     ACTIVE    SIZE      RECLAIMABLE
Images          2         1         200MB     10MB (5%)
Containers      2         1         0B        0B
Local Volumes   0         0         0B        0B
Build Cache     0         0         0B        0B
`
	if out.String() != want {
		t.Errorf("df:\n%s\nwant:\n%s", out.String(), want)
	}
	out.Reset()
	writeDF(&out, "{{.Type}}: {{.Reclaimable}}", du)
	if !strings.HasPrefix(out.String(), "Images: 10MB (5%)\nContainers: 0B\n") {
		t.Errorf("df --format: %q", out.String())
	}
}

func TestDockerHelpNewCommands(t *testing.T) {
	out, code := sh(t, "docker help")
	for _, s := range []string{"  pause       Pause all processes within one or more containers\n", "  system      Manage Docker\n",
		"  tag         Create a tag TARGET_IMAGE that refers to SOURCE_IMAGE\n", "  wait        Block until one or more containers stop, then print their exit codes\n"} {
		if code != 0 || !strings.Contains(out, s) {
			t.Errorf("docker help has no %q:\n%s", s, out)
		}
	}
	if out, _ := sh(t, "docker container --help"); !strings.Contains(out, "  prune       Remove all stopped containers\n") || !strings.Contains(out, "  unpause     ") {
		t.Errorf("docker container --help:\n%s", out)
	}
	if out, _ := sh(t, "docker system"); !strings.Contains(out, "  df          Show docker disk usage\n  prune       Remove unused data\n") {
		t.Errorf("docker system:\n%s", out)
	}
	if out, _ := sh(t, "docker attach --help"); !strings.Contains(out, "      --detach-keys string   Override the key sequence for detaching a container\n      --no-stdin             Do not attach STDIN\n      --sig-proxy            Proxy all received signals to the process (default true)\n") {
		t.Errorf("docker attach --help:\n%s", out)
	}
	if out, _ := sh(t, "docker image prune --help"); !strings.Contains(out, "Usage:  docker image prune [OPTIONS]\n\nRemove unused images\n\nOptions:\n  -a, --all             Remove all unused images, not just dangling ones\n      --filter filter   Provide filter values (e.g. \"until=<timestamp>\")\n  -f, --force           Do not prompt for confirmation\n") {
		t.Errorf("docker image prune --help:\n%s", out)
	}
}

func TestParseDockerRunAttachFlags(t *testing.T) {
	o, image, _, err := parseDockerRun([]string{"-it", "--detach-keys", "ctrl-x,x", "--sig-proxy=false", "alpine"})
	if err != nil || image != "alpine" || o.detachKeys != "ctrl-x,x" || !o.noSigProxy {
		t.Errorf("%+v %q %v", o, image, err)
	}
	if o, _, _, _ := parseDockerRun([]string{"--detach-keys=a", "--sig-proxy", "alpine"}); o.detachKeys != "a" || o.noSigProxy {
		t.Errorf("%+v", o)
	}
	if _, _, _, err := parseDockerRun([]string{"--sig-proxy=maybe", "alpine"}); err == nil ||
		err.Error() != `invalid argument "maybe" for "--sig-proxy" flag: strconv.ParseBool: parsing "maybe": invalid syntax` {
		t.Errorf("bad --sig-proxy: %v", err)
	}
}
