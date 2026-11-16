package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/runtime/v2/v86"
)

func TestGetTimestamp(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct{ in, want string }{
		{"10m", strconv.FormatInt(now.Add(-10*time.Minute).Unix(), 10)},
		{"1700000000", "1700000000"},
		{"1700000000.5", "1700000000.5"},
		{"2026-11-16T09:00:00Z", fmt.Sprintf("%d.000000000", time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC).Unix())},
		{"2026-11-16", fmt.Sprintf("%d.000000000", time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC).Unix())},
		{"2026-11-16T09:30", fmt.Sprintf("%d.000000000", time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC).Unix())},
	} {
		got, err := getTimestamp(c.in, now)
		if err != nil || got != c.want {
			t.Errorf("%s: %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if _, err := getTimestamp("yesterday", now); err == nil {
		t.Error("yesterday parsed")
	}
}

func TestLogsQuery(t *testing.T) {
	q, err := logsQuery(dockerFlagValues{"follow": "true", "tail": "3", "timestamps": "true", "since": "1700000000"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("follow") != "1" || q.Get("tail") != "3" || q.Get("timestamps") != "1" || q.Get("since") != "1700000000" || q.Get("stdout") != "1" {
		t.Errorf("query %v", q)
	}
	if q, _ := logsQuery(dockerFlagValues{}, time.Now()); q.Get("tail") != "all" {
		t.Errorf("default tail %q", q.Get("tail"))
	}
}

func TestSplitCpArg(t *testing.T) {
	for _, c := range []struct{ in, ctr, p string }{
		{"web:/etc", "web", "/etc"},
		{"/root/x", "", "/root/x"},
		{"./a:b", "", "./a:b"},
		{"file", "", "file"},
		{"web:", "web", ""},
	} {
		if ctr, p := splitCpArg(c.in); ctr != c.ctr || p != c.p {
			t.Errorf("%s: %q %q", c.in, ctr, p)
		}
	}
}

func TestPrepareCopy(t *testing.T) {
	dir := []v86.CopiedFile{{Path: "html", Mode: v86.ModeDir | 0o755}, {Path: "html/a", Mode: v86.ModeRegular | 0o644}}
	file := []v86.CopiedFile{{Path: "a.txt", Mode: v86.ModeRegular | 0o644}}
	names := func(fs []v86.CopiedFile) string {
		var s []string
		for _, f := range fs {
			s = append(s, f.Path)
		}
		return strings.Join(s, ",")
	}
	for _, c := range []struct {
		files    []v86.CopiedFile
		base     string
		srcDir   bool
		dst      string
		info     copyInfo
		wantDir  string
		wantName string
		err      error
	}{
		// into an existing directory: as it is
		{dir, "html", true, "/root", copyInfo{true, true}, "/root", "html,html/a", nil},
		// to a new name
		{dir, "html", true, "/root/site", copyInfo{}, "/root", "site,site/a", nil},
		{file, "a.txt", false, "/root/b.txt", copyInfo{true, false}, "/root", "b.txt", nil},
		{file, "a.txt", false, "/root/b.txt", copyInfo{}, "/root", "b.txt", nil},
		// errors of moby
		{dir, "html", true, "/root/f", copyInfo{true, false}, "", "", errCannotCopyDir},
		{file, "a.txt", false, "/root/new/", copyInfo{}, "", "", errDirNotExists},
	} {
		d, fs, err := prepareCopy(c.files, c.base, c.srcDir, c.dst, c.info)
		if err != c.err || d != c.wantDir || names(fs) != c.wantName {
			t.Errorf("%s -> %s: %q %q %v", c.base, c.dst, d, names(fs), err)
		}
	}
}

func TestStatsEntry(t *testing.T) {
	var v statsResponse
	v.Name, v.ID = "/web", "0123456789abcdef"
	v.CPUStats.CPUUsage.TotalUsage, v.CPUStats.SystemUsage, v.CPUStats.OnlineCPUs = 300, 2000, 1
	v.MemoryStats.Usage, v.MemoryStats.Limit = 10<<20, 100<<20
	v.PidsStats.Current = 3
	if err := json.Unmarshal([]byte(`{"networks":{"eth0":{"rx_bytes":1500,"tx_bytes":2048}}}`), &v); err != nil {
		t.Fatal(err)
	}
	e := entryOf("web", v, 100, 1000)
	r := e.row(true)
	want := statsRow{BlockIO: "0B / 0B", CPUPerc: "20.00%", Container: "web", ID: "0123456789ab", MemPerc: "10.00%",
		MemUsage: "10MiB / 100MiB", Name: "web", NetIO: "1.5kB / 2.05kB", PIDs: "3"}
	if r != want {
		t.Errorf("row %+v\nwant %+v", r, want)
	}
	if r := (statsEntry{id: "x", err: true}).row(true); r.CPUPerc != "--" || r.MemUsage != "-- / --" {
		t.Errorf("failed row %+v", r)
	}
}

func TestPrettyEvent(t *testing.T) {
	var e dockerEvent
	e.Type, e.Action, e.Actor.ID, e.TimeNano = "container", "die", "abc", time.Date(2026, 11, 16, 10, 0, 0, 1, time.UTC).UnixNano()
	e.Actor.Attributes = map[string]string{"name": "web", "image": "nginx", "exitCode": "0"}
	got := prettyEvent(e)
	if !strings.HasSuffix(got, " container die abc (exitCode=0, image=nginx, name=web)\n") || !strings.HasPrefix(got, "2026-11-16T") {
		t.Errorf("%q", got)
	}
	q, err := eventsQuery(dockerFlagValues{"filter": "event=die"}, time.Now())
	if err != nil || q.Get("filters") != `{"event":{"die":true}}` {
		t.Errorf("%v %v", q, err)
	}
}
