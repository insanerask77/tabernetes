package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseDockerRunFlags(t *testing.T) {
	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }
	for _, tc := range []struct {
		args  []string
		o     dockerRunOptions
		image string
		cmd   []string
		err   string
	}{
		{args: []string{"-w", "/tmp", "-u", "nobody", "alpine", "sh", "-c", "pwd; id"},
			o: dockerRunOptions{workdir: "/tmp", user: "nobody"}, image: "alpine", cmd: []string{"sh", "-c", "pwd; id"}},
		{args: []string{"--workdir=/srv", "--user=1000:1000", "--hostname", "box", "-hweb", "alpine"},
			o: dockerRunOptions{workdir: "/srv", user: "1000:1000", hostname: "web"}, image: "alpine"},
		{args: []string{"-w/tmp", "-uroot", "-itd", "alpine"},
			o: dockerRunOptions{workdir: "/tmp", user: "root", interactive: true, tty: true, detach: true}, image: "alpine"},
		// -it grouped with a flag that takes a value: the rest of the word
		{args: []string{"-itw", "/x", "alpine"}, o: dockerRunOptions{interactive: true, tty: true, workdir: "/x"}, image: "alpine"},
		{args: []string{"-itw/x", "alpine"}, o: dockerRunOptions{interactive: true, tty: true, workdir: "/x"}, image: "alpine"},
		{args: []string{"--entrypoint", "sh", "alpine", "-c", "echo"}, o: dockerRunOptions{entrypoint: str("sh")}, image: "alpine", cmd: []string{"-c", "echo"}},
		{args: []string{"--entrypoint=", "nginx", "ls"}, o: dockerRunOptions{entrypoint: str("")}, image: "nginx", cmd: []string{"ls"}},
		{args: []string{"--entrypoint", "", "nginx"}, o: dockerRunOptions{entrypoint: str("")}, image: "nginx"},
		{args: []string{"-l", "a=1", "--label=b", "--label-file", "l.txt", "alpine"},
			o: dockerRunOptions{labels: []string{"a=1", "b"}, labelFiles: []string{"l.txt"}}, image: "alpine"},
		{args: []string{"--env-file", ".env", "-e", "HOME", "alpine"}, o: dockerRunOptions{envFiles: []string{".env"}, env: []string{"HOME"}}, image: "alpine"},
		// -m in Docker's units: powers of 1024, with or without b
		{args: []string{"-m", "512m", "alpine"}, o: dockerRunOptions{memory: 512 << 20}, image: "alpine"},
		{args: []string{"-m1g", "alpine"}, o: dockerRunOptions{memory: 1 << 30}, image: "alpine"},
		{args: []string{"--memory=300MB", "alpine"}, o: dockerRunOptions{memory: 300 << 20}, image: "alpine"},
		{args: []string{"--memory", "134217728", "alpine"}, o: dockerRunOptions{memory: 128 << 20}, image: "alpine"},
		{args: []string{"--memory", "256k", "alpine"}, o: dockerRunOptions{memory: 256 << 10}, image: "alpine"},
		{args: []string{"-P", "nginx"}, o: dockerRunOptions{publishAll: true}, image: "nginx"},
		{args: []string{"-dP", "nginx"}, o: dockerRunOptions{publishAll: true, detach: true}, image: "nginx"},
		{args: []string{"--init", "--rm", "alpine"}, o: dockerRunOptions{init: true, rm: true}, image: "alpine"},
		{args: []string{"--rm=false", "-t=false", "alpine"}, image: "alpine"},
		{args: []string{"--restart", "on-failure:2", "alpine"}, o: dockerRunOptions{restart: dockerRestartPolicy{Name: "on-failure", MaximumRetryCount: 2}}, image: "alpine"},
		{args: []string{"--restart=always", "alpine"}, o: dockerRunOptions{restart: dockerRestartPolicy{Name: "always"}}, image: "alpine"},
		{args: []string{"--restart", "no", "--rm", "alpine"}, o: dockerRunOptions{restart: dockerRestartPolicy{Name: "no"}, rm: true}, image: "alpine"},
		{args: []string{"-a", "stdout", "-a", "STDERR", "alpine"}, o: dockerRunOptions{attach: []string{"stdout", "stderr"}}, image: "alpine"},
		{args: []string{"--stop-signal", "SIGINT", "--stop-timeout", "3", "alpine"}, o: dockerRunOptions{stopSignal: "SIGINT", stopTimeout: num(3)}, image: "alpine"},

		// the messages of the docker CLI
		{args: []string{"--restart", "always", "--rm", "alpine"}, err: "conflicting options: cannot specify both --restart and --rm"},
		{args: []string{"--restart", ":1", "alpine"}, err: `invalid argument ":1" for "--restart" flag: invalid restart policy format: no policy provided before colon`},
		{args: []string{"--restart", "on-failure:x", "alpine"}, err: `invalid argument "on-failure:x" for "--restart" flag: invalid restart policy format: maximum retry count must be an integer`},
		{args: []string{"-m", "lots", "alpine"}, err: `invalid argument "lots" for "-m, --memory" flag: invalid size: 'lots'`},
		{args: []string{"-a", "stdpot", "alpine"}, err: `invalid argument "stdpot" for "-a, --attach" flag: valid streams are STDIN, STDOUT and STDERR`},
		{args: []string{"-d", "-a", "stdout", "alpine"}, err: "conflicting options: cannot specify both --attach and --detach"},
		{args: []string{"-e", "=x", "alpine"}, err: `invalid argument "=x" for "-e, --env" flag: invalid environment variable: =x`},
		{args: []string{"-l", "=x", "alpine"}, err: `invalid argument "=x" for "-l, --label" flag: invalid label '=x': empty name`},
		{args: []string{"-l", "a b=x", "alpine"}, err: `invalid argument "a b=x" for "-l, --label" flag: label 'a b' contains whitespaces`},
		{args: []string{"--stop-timeout", "soon", "alpine"}, err: `invalid argument "soon" for "--stop-timeout" flag: strconv.ParseInt: parsing "soon": invalid syntax`},
		{args: []string{"-w"}, err: "flag needs an argument: 'w' in -w"},
		{args: []string{"--user"}, err: "flag needs an argument: --user"},
		{args: []string{"-x", "alpine"}, err: "unknown shorthand flag: 'x' in -x"},
		{args: []string{"-itx", "alpine"}, err: "unknown shorthand flag: 'x' in -itx"},
		{args: []string{"--publish-everything", "alpine"}, err: "unknown flag: --publish-everything"},
	} {
		o, image, cmd, err := parseDockerRun(tc.args)
		if got := ""; err != nil || tc.err != "" {
			if err != nil {
				got = err.Error()
			}
			if got != tc.err {
				t.Errorf("%q: error %q, want %q", tc.args, got, tc.err)
			}
			continue
		}
		if !reflect.DeepEqual(o, tc.o) || image != tc.image || !reflect.DeepEqual(cmd, tc.cmd) && len(cmd)+len(tc.cmd) > 0 {
			t.Errorf("%q: got %+v %q %q, want %+v %q %q", tc.args, o, image, cmd, tc.o, tc.image, tc.cmd)
		}
	}
}

func TestParseRestartPolicy(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want dockerRestartPolicy
	}{
		{"", dockerRestartPolicy{}},
		{"no", dockerRestartPolicy{Name: "no"}},
		{"always", dockerRestartPolicy{Name: "always"}},
		{"unless-stopped", dockerRestartPolicy{Name: "unless-stopped"}},
		{"on-failure", dockerRestartPolicy{Name: "on-failure"}},
		{"on-failure:5", dockerRestartPolicy{Name: "on-failure", MaximumRetryCount: 5}},
		// the daemon checks names and counts, as dockerd does
		{"always:3", dockerRestartPolicy{Name: "always", MaximumRetryCount: 3}},
		{"sometimes", dockerRestartPolicy{Name: "sometimes"}},
	} {
		got, err := parseRestartPolicy(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%q: %+v %v, want %+v", tc.in, got, err, tc.want)
		}
	}
}

// TestParseKVFile follows the tests of kvfile in the docker CLI.
func TestParseKVFile(t *testing.T) {
	env := map[string]string{"HOME": "/root", "EMPTY": ""}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	in := "\xEF\xBB\xBF# a comment\n\nA=1\n  B=two words  \nHOME\nEMPTY\nMISSING\nC=x=y\n\t# indented comment\nD=\n"
	got, err := parseKVFile(strings.NewReader(in), lookup)
	want := []string{"A=1", "B=two words  ", "HOME=/root", "EMPTY=", "C=x=y", "D="}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("env file: %q %v, want %q", got, err, want)
	}
	// labels: a bare key is left out
	got, _ = parseKVFile(strings.NewReader("a=1\nb\n"), nil)
	if !reflect.DeepEqual(got, []string{"a=1"}) {
		t.Errorf("label file: %q", got)
	}
	for in, msg := range map[string]string{
		"=x\n":         "no variable name on line '=x'",
		"A B=x\n":      "variable 'A B' contains whitespaces",
		"A=\xff\xfe\n": "invalid utf8 bytes at line 1: [65 61 255 254]",
	} {
		if _, err := parseKVFile(strings.NewReader(in), lookup); err == nil || err.Error() != msg {
			t.Errorf("%q: %v, want %q", in, err, msg)
		}
	}
}

func TestReadKVFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.env"), []byte("A=1\nB=2\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "bad.env"), []byte("x y=1\n"), 0o644)
	open := func(name string) (io.ReadCloser, error) { return os.Open(filepath.Join(dir, name)) }
	got, err := readKVFiles(open, []string{"a.env"}, []string{"B=3"}, nil)
	if err != nil || !reflect.DeepEqual(got, []string{"A=1", "B=2", "B=3"}) {
		t.Errorf("files then -e: %q %v", got, err)
	}
	if _, err := readKVFiles(open, []string{"bad.env"}, nil, nil); err == nil || err.Error() != "invalid env file (bad.env): variable 'x y' contains whitespaces" {
		t.Errorf("bad file: %v", err)
	}
	if _, err := readKVFiles(open, []string{"none.env"}, nil, nil); !os.IsNotExist(err) {
		t.Errorf("missing file: %v", err)
	}
}

func TestDockerAttachQuery(t *testing.T) {
	for _, tc := range []struct {
		o     dockerRunOptions
		q     string
		stdin bool
	}{
		{dockerRunOptions{}, "stream=1&logs=1&stdout=1&stderr=1", false},
		{dockerRunOptions{interactive: true}, "stream=1&logs=1&stdout=1&stderr=1&stdin=1", true},
		{dockerRunOptions{attach: []string{"stderr"}}, "stream=1&logs=1&stderr=1", false},
		{dockerRunOptions{interactive: true, attach: []string{"stdout"}}, "stream=1&logs=1&stdout=1", false},
		{dockerRunOptions{interactive: true, attach: []string{"stdin", "stdout"}}, "stream=1&logs=1&stdout=1&stdin=1", true},
	} {
		q, stdin := dockerAttachQuery(tc.o)
		if q != tc.q || stdin != tc.stdin {
			t.Errorf("%+v: %q %v, want %q %v", tc.o, q, stdin, tc.q, tc.stdin)
		}
	}
}

func TestSetHostConfigRunFlags(t *testing.T) {
	o, image, cmd, err := parseDockerRun([]string{"-P", "-m", "512m", "--restart", "on-failure:2", "--init", "nginx"})
	if err != nil {
		t.Fatal(err)
	}
	req := dockerCreateRequest{Image: image, Cmd: cmd}
	setHostConfig(&req, o)
	h := req.HostConfig
	if !h.PublishAllPorts || h.Memory != 512<<20 || h.RestartPolicy == nil || *h.RestartPolicy != (dockerRestartPolicy{Name: "on-failure", MaximumRetryCount: 2}) || h.Init == nil || !*h.Init {
		t.Errorf("host config: %+v", h)
	}
}
