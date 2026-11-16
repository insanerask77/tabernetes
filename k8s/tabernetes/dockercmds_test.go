package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseDockerFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		f    dockerFlagValues
		rest []string
		err  string
	}{
		{args: []string{"-a"}, f: dockerFlagValues{"all": "true"}},
		{args: []string{"-aq", "--no-trunc"}, f: dockerFlagValues{"all": "true", "quiet": "true", "no-trunc": "true"}},
		{args: []string{"-n", "3"}, f: dockerFlagValues{"last": "3"}},
		{args: []string{"-n3"}, f: dockerFlagValues{"last": "3"}},
		{args: []string{"--format={{.ID}}"}, f: dockerFlagValues{"format": "{{.ID}}"}},
		{args: []string{"--format", "{{.ID}}", "--all=false"}, f: dockerFlagValues{"format": "{{.ID}}", "all": "false"}},
		{args: []string{"x", "-a", "--", "-q"}, f: dockerFlagValues{"all": "true"}, rest: []string{"x", "-q"}},
		{args: []string{"--foo"}, err: "unknown flag: --foo"},
		{args: []string{"-x"}, err: "unknown shorthand flag: 'x' in -x"},
		{args: []string{"-ax"}, err: "unknown shorthand flag: 'x' in -ax"},
		{args: []string{"--format"}, err: "flag needs an argument: --format"},
		{args: []string{"-n"}, err: "flag needs an argument: 'n' in -n"},
		{args: []string{"-n", "x"}, err: `invalid argument "x" for "-n, --last" flag: strconv.ParseInt: parsing "x": invalid syntax`},
	} {
		f, rest, err := parseDockerFlags(psCommand, tc.args)
		if tc.err != "" {
			if err == nil || err.Error() != tc.err {
				t.Errorf("%q: error %v; want %q", tc.args, err, tc.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(f, tc.f) || !reflect.DeepEqual(rest, tc.rest) {
			t.Errorf("%q: %v %q %v; want %v %q", tc.args, f, rest, err, tc.f, tc.rest)
		}
	}
}

func TestDisplayablePorts(t *testing.T) {
	for _, tc := range []struct {
		ports []psPort
		want  string
	}{
		{nil, ""},
		{[]psPort{{PrivatePort: 80, Type: "tcp"}}, "80/tcp"},
		{[]psPort{{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080, Type: "tcp"}}, "0.0.0.0:8080->80/tcp"},
		{[]psPort{{IP: "::", PrivatePort: 80, PublicPort: 8080, Type: "tcp"}, {IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080, Type: "tcp"}}, "0.0.0.0:8080->80/tcp, [::]:8080->80/tcp"},
		{[]psPort{{PrivatePort: 81, Type: "tcp"}, {PrivatePort: 80, Type: "tcp"}, {PrivatePort: 443, Type: "tcp"}}, "80-81/tcp, 443/tcp"},
		{[]psPort{{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 80, Type: "tcp"}}, "0.0.0.0:80->80/tcp"},
	} {
		if got := displayablePorts(tc.ports); got != tc.want {
			t.Errorf("%+v: %q; want %q", tc.ports, got, tc.want)
		}
	}
}

func TestPSTable(t *testing.T) {
	now := time.Now()
	cs := []psContainer{
		{ID: "4f6e3c2b1a0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f", Names: []string{"/focused_turing"}, Image: "nginx",
			Command: "/docker-entrypoint.sh nginx -g 'daemon off;'", Created: now.Add(-2 * time.Minute).Unix(),
			Ports: []psPort{{IP: "0.0.0.0", PrivatePort: 80, PublicPort: 8080, Type: "tcp"}}, State: "running", Status: "Up 2 minutes"},
		{ID: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2", Names: []string{"/web"}, Image: "alpine",
			Command: "sh -c 'exit 3'", Created: now.Add(-90 * time.Second).Unix(), State: "exited", Status: "Exited (3) 5 seconds ago"},
	}
	rows := []any{psRowOf(cs[0], true, now), psRowOf(cs[1], true, now)}
	var out bytes.Buffer
	if err := writeFormattedTo(&out, psTable, psHeader, rows); err != nil {
		t.Fatal(err)
	}
	want := `CONTAINER ID   IMAGE     COMMAND                  CREATED              STATUS                     PORTS                  NAMES
4f6e3c2b1a0d   nginx     "/docker-entrypoint.…"   2 minutes ago        Up 2 minutes               0.0.0.0:8080->80/tcp   focused_turing
a1b2c3d4e5f6   alpine    "sh -c 'exit 3'"         About a minute ago   Exited (3) 5 seconds ago                          web
`
	if out.String() != want {
		t.Errorf("ps:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestImageRows(t *testing.T) {
	now := time.Now()
	imgs := []dockerImageSummary{
		{ID: "sha256:4f67c83422ec89a1b2c3d4e5f6", RepoTags: []string{"nginx:1.27", "nginx:latest"}, RepoDigests: []string{"nginx@sha256:abc"},
			Created: now.Add(-49 * time.Hour).Unix(), Size: 192_000_000},
		{ID: "sha256:0123456789abcdef", RepoDigests: []string{"alpine@sha256:def"}, Created: now.Add(-3 * 7 * 24 * time.Hour).Unix(), Size: 7_830_000},
	}
	rows := imageRows(imgs, "", true, now)
	var out bytes.Buffer
	var any []any
	for _, r := range rows {
		any = append(any, r)
	}
	if err := writeFormattedTo(&out, "table {{.Repository}}\t{{.Tag}}\t{{.ID}}\t{{.CreatedSince}}\t{{.Size}}", imageHeader, any); err != nil {
		t.Fatal(err)
	}
	want := `REPOSITORY   TAG       IMAGE ID       CREATED       SIZE
nginx        1.27      4f67c83422ec   2 days ago    192MB
nginx        latest    4f67c83422ec   2 days ago    192MB
alpine       <none>    0123456789ab   3 weeks ago   7.83MB
`
	if out.String() != want {
		t.Errorf("images:\n%s\nwant:\n%s", out.String(), want)
	}
	if got := imageRows(imgs, "nginx:latest", true, now); len(got) != 1 || got[0].Tag != "latest" {
		t.Errorf("filter nginx:latest: %+v", got)
	}
}

func TestImageRefs(t *testing.T) {
	for _, tc := range []struct{ in, familiar, full string }{
		{"nginx", "nginx:latest", "docker.io/library/nginx:latest"},
		{"nginx:1.27", "nginx:1.27", "docker.io/library/nginx:1.27"},
		{"docker.io/library/alpine", "alpine:latest", "docker.io/library/alpine:latest"},
		{"bitnami/redis:7", "bitnami/redis:7", "docker.io/bitnami/redis:7"},
		{"ghcr.io/a/b", "ghcr.io/a/b:latest", "ghcr.io/a/b:latest"},
		{"alpine@sha256:abc", "alpine@sha256:abc", "docker.io/library/alpine@sha256:abc"},
	} {
		if f, full := familiarRef(tc.in), fullRef(tc.in); f != tc.familiar || full != tc.full {
			t.Errorf("%s: %s %s; want %s %s", tc.in, f, full, tc.familiar, tc.full)
		}
	}
}

const pullStream = `{"status":"Pulling from library/busybox","id":"latest"}
{"status":"Pulling fs layer","progressDetail":{},"id":"aaaaaaaaaaaa"}
{"status":"Pulling fs layer","progressDetail":{},"id":"bbbbbbbbbbbb"}
{"status":"Downloading","progressDetail":{"current":1000000,"total":2000000},"id":"aaaaaaaaaaaa"}
{"status":"Download complete","progressDetail":{},"id":"bbbbbbbbbbbb"}
{"status":"Pull complete","progressDetail":{},"id":"aaaaaaaaaaaa"}
{"status":"Digest: sha256:123"}
{"status":"Status: Downloaded newer image for busybox:latest"}
`

func TestDisplayJSONMessages(t *testing.T) {
	// off the terminal: the steps, no progress bars
	var out bytes.Buffer
	if err := displayJSONMessages(strings.NewReader(pullStream), &out, false, 80); err != nil {
		t.Fatal(err)
	}
	want := `latest: Pulling from library/busybox
aaaaaaaaaaaa: Pulling fs layer
bbbbbbbbbbbb: Pulling fs layer
bbbbbbbbbbbb: Download complete
aaaaaaaaaaaa: Pull complete
Digest: sha256:123
Status: Downloaded newer image for busybox:latest
`
	if out.String() != want {
		t.Errorf("not a terminal:\n%s\nwant:\n%s", out.String(), want)
	}

	// on the terminal: each layer keeps its line
	out.Reset()
	if err := displayJSONMessages(strings.NewReader(pullStream), &out, true, 120); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\n\x1b[1A\x1b[2K\raaaaaaaaaaaa: Pulling fs layer \r\x1b[1B",
		"\n\x1b[1A\x1b[2K\rbbbbbbbbbbbb: Pulling fs layer \r\x1b[1B",
		"\x1b[2A\x1b[2K\raaaaaaaaaaaa: Downloading [=========================>                         ]      1MB/2MB\r\x1b[2B",
		"\x1b[1A\x1b[2K\rbbbbbbbbbbbb: Download complete \r\x1b[1B",
		"Digest: sha256:123\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("terminal output %q lacks %q", out.String(), want)
		}
	}

	err := displayJSONMessages(strings.NewReader(`{"errorDetail":{"message":"not found"},"error":"not found"}`), &out, false, 80)
	if err == nil || err.Error() != "not found" {
		t.Errorf("error %v; want not found", err)
	}
}

func TestDockerHelp(t *testing.T) {
	out, code := sh(t, "docker help")
	if code != 0 || !strings.HasPrefix(out, "\nUsage:  docker [OPTIONS] COMMAND\n\nA self-sufficient runtime for containers\n\nCommon Commands:\n  run         Create and run a new container from an image\n") ||
		!strings.Contains(out, "  attach      Attach local standard input, output, and error streams to a running container\n") ||
		!strings.Contains(out, "\nManagement Commands:\n  container   Manage containers\n  image       Manage images\n") {
		t.Errorf("docker help: %d\n%s", code, out)
	}
	if out, code := sh(t, "docker frobnicate"); code != 1 || out != "docker: unknown command: docker frobnicate\n\nRun 'docker --help' for more information\n" {
		t.Errorf("unknown command: %d %q", code, out)
	}
	if out, code := sh(t, "docker ps --help"); code != 0 || !strings.Contains(out, "Usage:  docker ps [OPTIONS]\n\nList containers\n\nAliases:\n  docker container ls, docker container list, docker container ps, docker ps\n\nOptions:\n  -a, --all             Show all containers (default shows just running)\n") {
		t.Errorf("docker ps --help: %d\n%s", code, out)
	}
	if out, code := sh(t, "docker container"); code != 0 || !strings.Contains(out, "Usage:  docker container COMMAND\n\nManage containers\n\nCommands:\n  attach ") || strings.Contains(out, "  list ") {
		t.Errorf("docker container: %d\n%s", code, out)
	}
	if out, code := sh(t, "docker image frob"); code != 1 || !strings.HasPrefix(out, "docker: unknown command: docker image frob\n") {
		t.Errorf("docker image frob: %d %q", code, out)
	}
	if out, code := sh(t, "docker help images"); code != 0 || !strings.Contains(out, "Usage:  docker images [OPTIONS] [REPOSITORY[:TAG]]") {
		t.Errorf("docker help images: %d\n%s", code, out)
	}
}
