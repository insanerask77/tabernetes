package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseVolumeSpec(t *testing.T) {
	for spec, want := range map[string]string{
		"/d":                 "|/d|false",
		"data:/d":            "data|/d|false",
		"data:/d:ro":         "data|/d|false",
		"/root/site:/srv:ro": "/root/site|/srv|true",
		"./site:/srv":        "./site|/srv|true",
		"~/x:/x":             "~/x|/x|true",
		"c:/x":               "|c:/x|false", // a Windows drive, as the CLI reads it
		"d":                  "|d|false",
	} {
		v, err := parseVolumeSpec(spec)
		if err != nil {
			t.Errorf("%q: %v", spec, err)
			continue
		}
		if got := strings.Join([]string{v.source, v.target, map[bool]string{true: "true", false: "false"}[v.bind]}, "|"); got != want {
			t.Errorf("%q = %s, want %s", spec, got, want)
		}
	}
	for spec, want := range map[string]string{
		"":           "invalid empty volume spec",
		"ab::c":      "invalid spec: ab::c: empty section between colons",
		"ab:/b:ro:x": "invalid spec: ab:/b:ro:x: too many colons",
		"ab:/b:":     "invalid spec: ab:/b:: empty section between colons",
	} {
		if _, err := parseVolumeSpec(spec); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %q", spec, err, want)
		}
	}
}

func TestParseMountFlag(t *testing.T) {
	m, err := parseMountFlag("type=volume,src=data,dst=/d,readonly,volume-nocopy,volume-label=a=b")
	if err != nil || m.Type != "volume" || m.Source != "data" || m.Target != "/d" || !m.ReadOnly || !m.VolumeOptions.NoCopy || m.VolumeOptions.Labels["a"] != "b" {
		t.Errorf("volume: %+v %v", m, err)
	}
	m, err = parseMountFlag("type=bind,source=/root/x,target=/x,bind-propagation=rshared")
	if err != nil || m.Type != "bind" || m.BindOptions.Propagation != "rshared" {
		t.Errorf("bind: %+v %v", m, err)
	}
	m, err = parseMountFlag("type=tmpfs,dst=/t,tmpfs-mode=700,tmpfs-size=64m")
	if err != nil || m.TmpfsOptions.Mode != 0o700 || m.TmpfsOptions.SizeBytes != 64<<20 {
		t.Errorf("tmpfs: %+v %v", m, err)
	}
	// a volume by default
	if m, err := parseMountFlag("dst=/d"); err != nil || m.Type != "volume" {
		t.Errorf("default: %+v %v", m, err)
	}
	for value, want := range map[string]string{
		"type=volume":                    "target is required",
		"type=bind,src=/x,dst=/x,foo":    "invalid field 'foo' must be a key=value pair",
		"type=bind,dst=/x,volume-nocopy": "cannot mix 'volume-*' options with mount type 'bind'",
		"dst=/x,color=red":               "unexpected key 'color' in 'color=red'",
		"dst=/x,ro=maybe":                "invalid value for ro: maybe",
	} {
		if _, err := parseMountFlag(value); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %q", value, err, want)
		}
	}
}

func TestDockerRunMounts(t *testing.T) {
	o, image, _, err := parseDockerRun([]string{"-v", "data:/d", "--volume=/anon", "-v./site:/srv:ro", "--mount", "type=bind,src=./x,dst=/x", "alpine"})
	if err != nil || image != "alpine" {
		t.Fatalf("parse: %v %q", err, image)
	}
	if !reflect.DeepEqual(o.volumes, []string{"data:/d", "/anon", "./site:/srv:ro"}) || len(o.mounts) != 1 {
		t.Errorf("options: %+v", o)
	}
	req := dockerCreateRequest{Image: image}
	setHostConfig(&req, o)
	setMounts(&req, o, "/root/web")
	b, _ := json.Marshal(req)
	for _, want := range []string{
		`"Binds":["data:/d","/root/web/site:/srv:ro"]`,
		`"Mounts":[{"Type":"bind","Source":"/root/web/x","Target":"/x"}]`,
		`"Volumes":{"/anon":{}}`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("request %s lacks %s", b, want)
		}
	}
	if _, _, _, err := parseDockerRun([]string{"--mount", "type=volume", "alpine"}); err == nil ||
		err.Error() != `invalid argument "type=volume" for "--mount" flag: target is required` {
		t.Errorf("--mount without a target: %v", err)
	}
	if _, _, _, err := parseDockerRun([]string{"-v", "ab::c", "alpine"}); err == nil {
		t.Error("-v ab::c: no error")
	}
}

func TestTakeMultiAndFilters(t *testing.T) {
	vals, rest, err := takeMulti([]string{"--label", "a=1", "-q", "--label=b=2", "x"}, 0, "label")
	if err != nil || !reflect.DeepEqual(vals, []string{"a=1", "b=2"}) || !reflect.DeepEqual(rest, []string{"-q", "x"}) {
		t.Errorf("takeMulti: %q %q %v", vals, rest, err)
	}
	vals, _, _ = takeMulti([]string{"-f", "dangling=true", "-fname=x"}, 'f', "filter")
	if !reflect.DeepEqual(vals, []string{"dangling=true", "name=x"}) {
		t.Errorf("takeMulti short: %q", vals)
	}
	if _, _, err := takeMulti([]string{"--filter"}, 'f', "filter"); err == nil || err.Error() != "flag needs an argument: --filter" {
		t.Errorf("takeMulti without a value: %v", err)
	}
	if s, err := filtersJSON([]string{"dangling=true", "label=a=b"}); err != nil || s != `{"dangling":{"true":true},"label":{"a=b":true}}` {
		t.Errorf("filtersJSON: %s %v", s, err)
	}
	if _, err := filtersJSON([]string{"dangling"}); err == nil {
		t.Error("filtersJSON without =: no error")
	}
}

func TestVolumeRows(t *testing.T) {
	r := volumeRowOf(dockerVolume{Name: "data", Driver: "local", Scope: "local", Labels: map[string]string{"b": "2", "a": "1"}})
	if r.Labels != "a=1,b=2" || r.Links != "N/A" || r.Size != "N/A" {
		t.Errorf("row: %+v", r)
	}
	var b strings.Builder
	writeFormattedTo(&b, "table {{.Driver}}\t{{.Name}}", volumeHeader, []any{r})
	if b.String() != "DRIVER    VOLUME NAME\nlocal     data\n" {
		t.Errorf("table:\n%s", b.String())
	}
}

func TestPSMounts(t *testing.T) {
	c := psContainer{}
	c.Mounts = append(c.Mounts, struct{ Name, Source, Destination, Driver string }{Name: "0123456789abcdef0123"},
		struct{ Name, Source, Destination, Driver string }{Source: "/root/site"})
	if got := (&psCtx{c: c, trunc: true}).Mounts(); got != "0123456789abcd…,/root/site" {
		t.Errorf("Mounts = %q", got)
	}
}
