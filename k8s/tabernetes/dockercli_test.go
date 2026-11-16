package main

import (
	"reflect"
	"testing"
)

func TestParseDockerRun(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		o     dockerRunOptions
		image string
		cmd   []string
		err   bool
	}{
		{args: []string{"nginx"}, image: "nginx"},
		{args: []string{"-it", "alpine", "sh"}, o: dockerRunOptions{interactive: true, tty: true}, image: "alpine", cmd: []string{"sh"}},
		{args: []string{"-dit", "--rm", "--name", "web", "nginx"}, o: dockerRunOptions{interactive: true, tty: true, detach: true, rm: true, name: "web"}, image: "nginx"},
		{args: []string{"-e", "A=1", "--env=B=2", "--name=x", "alpine", "env"}, o: dockerRunOptions{name: "x", env: []string{"A=1", "B=2"}}, image: "alpine", cmd: []string{"env"}},
		// options after the image belong to the command
		{args: []string{"alpine", "ls", "-l"}, image: "alpine", cmd: []string{"ls", "-l"}},
		{args: []string{"--", "alpine", "-x"}, image: "alpine", cmd: []string{"-x"}},
		{args: []string{"-d", "-p", "8080:80", "--publish=443", "-p8443:443/tcp", "--publish", "127.0.0.1::53", "-p=1:2", "nginx"},
			o: dockerRunOptions{detach: true, publish: []string{"8080:80", "443", "8443:443/tcp", "127.0.0.1::53", "1:2"}}, image: "nginx"},
		{args: []string{"-p", "x:80", "nginx"}, err: true},
		{args: []string{"-p"}, err: true},
		{args: []string{"--publish-all", "nginx"}, o: dockerRunOptions{publishAll: true}, image: "nginx"},
		{args: []string{"--no-such-flag", "nginx"}, err: true},
		{args: []string{"--name"}, err: true},
		{args: []string{"-it"}, err: true},
	} {
		o, image, cmd, err := parseDockerRun(tc.args)
		if (err != nil) != tc.err {
			t.Errorf("%q: error %v, want error %v", tc.args, err, tc.err)
			continue
		}
		if tc.err {
			continue
		}
		if !reflect.DeepEqual(o, tc.o) || image != tc.image || !reflect.DeepEqual(cmd, tc.cmd) && len(cmd)+len(tc.cmd) > 0 {
			t.Errorf("%q: got %+v %q %q, want %+v %q %q", tc.args, o, image, cmd, tc.o, tc.image, tc.cmd)
		}
	}
}

func TestDockerPublish(t *testing.T) {
	for _, tc := range []struct {
		specs    []string
		bindings map[string][]dockerPortBinding
		err      string
	}{
		{specs: []string{"8080:80"}, bindings: map[string][]dockerPortBinding{"80/tcp": {{"", "8080"}}}},
		{specs: []string{"80"}, bindings: map[string][]dockerPortBinding{"80/tcp": {{"", ""}}}},
		{specs: []string{"0.0.0.0:8080:80/tcp", "127.0.0.1:8081:80"}, bindings: map[string][]dockerPortBinding{"80/tcp": {{"0.0.0.0", "8080"}, {"127.0.0.1", "8081"}}}},
		{specs: []string{"127.0.0.1::80"}, bindings: map[string][]dockerPortBinding{"80/tcp": {{"127.0.0.1", ""}}}},
		{specs: []string{"[::1]:8080:80"}, bindings: map[string][]dockerPortBinding{"80/tcp": {{"::1", "8080"}}}},
		{specs: []string{"53:53/UDP"}, bindings: map[string][]dockerPortBinding{"53/udp": {{"", "53"}}}},
		{specs: []string{"8000-8001:80-81"}, bindings: map[string][]dockerPortBinding{"80/tcp": {{"", "8000"}}, "81/tcp": {{"", "8001"}}}},
		{specs: []string{"8000-8010:80"}, bindings: map[string][]dockerPortBinding{"80/tcp": {{"", "8000-8010"}}}},
		// the messages of the docker CLI
		{specs: []string{"x:80"}, err: "invalid hostPort: x"},
		{specs: []string{"8080:http"}, err: "invalid containerPort: http"},
		{specs: []string{"8080:"}, err: "no port specified: 8080:<empty>"},
		{specs: []string{"80/sctpx"}, err: "invalid proto: sctpx"},
		{specs: []string{"localhost:8080:80"}, err: "invalid IP address: localhost"},
		{specs: []string{"8000-8001:80-82"}, err: "invalid ranges specified for container and host Ports: 80-82 and 8000-8001"},
		{specs: []string{"70000"}, err: "invalid containerPort: 70000"},
	} {
		exposed, bindings, err := dockerPublish(tc.specs)
		if tc.err != "" {
			if err == nil || err.Error() != tc.err {
				t.Errorf("%q: error %v, want %q", tc.specs, err, tc.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(bindings, tc.bindings) {
			t.Errorf("%q: %v, %v; want %v", tc.specs, bindings, err, tc.bindings)
		}
		for port := range tc.bindings {
			if _, ok := exposed[port]; !ok {
				t.Errorf("%q: %s is not exposed", tc.specs, port)
			}
		}
	}
}

func TestDockerPortLines(t *testing.T) {
	ports := map[string][]dockerPortBinding{
		"443/tcp": {{"127.0.0.1", "32768"}},
		"80/tcp":  {{"0.0.0.0", "8080"}, {"::", "8080"}},
		"53/tcp":  nil, // exposed, not published
	}
	lines, err := dockerPortLines(ports, "web", nil)
	want := []string{"80/tcp -> 0.0.0.0:8080", "80/tcp -> [::]:8080", "443/tcp -> 127.0.0.1:32768"}
	if err != nil || !reflect.DeepEqual(lines, want) {
		t.Errorf("all: %q, %v; want %q", lines, err, want)
	}
	if lines, err := dockerPortLines(ports, "web", []string{"443/tcp"}); err != nil || !reflect.DeepEqual(lines, []string{"127.0.0.1:32768"}) {
		t.Errorf("443/tcp: %q, %v", lines, err)
	}
	if lines, err := dockerPortLines(ports, "web", []string{"80"}); err != nil || len(lines) != 2 {
		t.Errorf("80: %q, %v", lines, err)
	}
	for _, c := range []struct{ port, want string }{
		{"53", "Error: No public port '53' published for web"},
		{"81/udp", "Error: No public port '81/udp' published for web"},
		{"http", `Error: invalid port (http): strconv.ParseUint: parsing "http": invalid syntax`},
	} {
		if _, err := dockerPortLines(ports, "web", []string{c.port}); err == nil || err.Error() != c.want {
			t.Errorf("%s: %v; want %q", c.port, err, c.want)
		}
	}
	if lines, _ := dockerPortLines(map[string][]dockerPortBinding{}, "web", nil); len(lines) != 0 {
		t.Errorf("no ports: %q", lines)
	}
}
