// docker stats: the CPU, memory, network and processes of containers. The
// numbers come from the kernel of each VM: init answers the request
// {"stats": true} on its exec port with /proc (vm/init/init.c), the Docker
// API turns them into dockerd's StatsResponse (dockerapi/stats.go), and
// this command shows them like the docker CLI, refreshed every second
// until Ctrl-C, or once with --no-stream.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/runtime/v2/v86"
	units "github.com/docker/go-units"
	"mvdan.cc/sh/v3/interp"
)

// errOldInit is the answer of an init that does not know the request: it
// took it for an exec without a command. The VM is older than the page.
var errOldInit = errors.New("the init of the VM is too old for this request")

// initRequest sends one request line to init and returns what it wrote on
// stdout before its exit code 0 (stats_report, sync_caches).
func initRequest(ctx context.Context, dial func(int32) (net.Conn, error), req any) ([]byte, error) {
	conn, err := dial(vmExecPort)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to the container: %w", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	line, _ := json.Marshal(req)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	var out []byte
	for {
		var h [5]byte
		if _, err := io.ReadFull(conn, h[:]); err != nil {
			return nil, fmt.Errorf("the connection to the container broke: %w", err)
		}
		b := make([]byte, binary.BigEndian.Uint32(h[1:]))
		if _, err := io.ReadFull(conn, b); err != nil {
			return nil, fmt.Errorf("the connection to the container broke: %w", err)
		}
		switch {
		case h[0] == vmStdout:
			out = append(out, b...)
		case h[0] == vmExit && len(b) == 4 && binary.BigEndian.Uint32(b) == 0:
			return out, nil
		case h[0] == vmError && string(b) == "exec: no command specified":
			return nil, errOldInit
		case h[0] == vmError:
			return nil, errors.New(string(b))
		case h[0] == vmExit:
			return nil, fmt.Errorf("exit code %d", int32(binary.BigEndian.Uint32(b)))
		}
	}
}

// Stats reads the numbers of docker stats in the VM of container id
// (dockerapi/stats.go).
func (d dockerStreams) Stats(ctx context.Context, id string) (v86.Stats, error) {
	dial, err := d.dialer(id)
	if err != nil {
		return v86.Stats{}, err
	}
	b, err := initRequest(ctx, dial, map[string]bool{"stats": true})
	if err != nil {
		return v86.Stats{}, err
	}
	return v86.ParseStats(b, time.Now())
}

// ---- the command ----

// The flags of docker stats, as docker/cli v28 lists them.
var statsFlags = []flagSpec{
	{'a', "all", "", "Show all containers (default shows just running)"},
	{0, "format", "string", formatHelp},
	{0, "no-stream", "", "Disable streaming stats and only pull the first result"},
	{0, "no-trunc", "", "Do not truncate output"},
}

func init() {
	c := &dockerCommand{
		name: "stats", use: "stats [OPTIONS] [CONTAINER...]", short: "Display a live stream of container(s) resource usage statistics",
		aliases: []string{"docker container stats", "docker stats"},
		flags:   statsFlags, maxArgs: -1, run: dockerStats,
	}
	dockerCommands[c.name] = c
	if m, ok := dockerManagement["container"]; ok {
		m.subs["stats"] = c
	}
}

// statsRow is a line of docker stats, the fields of its templates. JSON
// lists them in this order, as the docker CLI does.
type statsRow struct {
	BlockIO, CPUPerc, Container, ID, MemPerc, MemUsage, Name, NetIO, PIDs string
}

var statsHeader = statsRow{BlockIO: "BLOCK I/O", CPUPerc: "CPU %", Container: "CONTAINER", ID: "CONTAINER ID",
	MemPerc: "MEM %", MemUsage: "MEM USAGE / LIMIT", Name: "NAME", NetIO: "NET I/O", PIDs: "PIDS"}

const statsTable = "table {{.ID}}\t{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.NetIO}}\t{{.BlockIO}}\t{{.PIDs}}"

// statsResponse is the part of the StatsResponse of the API that the docker
// CLI reads.
type statsResponse struct {
	Read     string `json:"read"`
	Name     string `json:"name"`
	ID       string `json:"id"`
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs  uint32 `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Stats map[string]uint64 `json:"stats"`
		Limit uint64            `json:"limit"`
	} `json:"memory_stats"`
	PidsStats struct {
		Current uint64 `json:"current"`
	} `json:"pids_stats"`
	Networks   map[string]statsNetwork `json:"networks"`
	BlkioStats struct {
		IoServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

type statsNetwork struct {
	RxBytes uint64 `json:"rx_bytes"`
	TxBytes uint64 `json:"tx_bytes"`
}

// statsEntry is what the docker CLI computes of a StatsResponse
// (container.StatsEntry).
type statsEntry struct {
	container, name, id                 string
	cpu, mem, memLimit, memPercent      float64
	netRx, netTx, blockRead, blockWrite float64
	pids                                uint64
	err                                 bool // the stats failed: "--"
}

// entryOf computes the numbers as the docker CLI does on Linux:
// calculateCPUPercentUnix and calculateMemUsageUnixNoCache.
func entryOf(container string, v statsResponse, preCPU, preSystem uint64) statsEntry {
	e := statsEntry{container: container, name: strings.TrimPrefix(v.Name, "/"), id: v.ID}
	cpuDelta := float64(v.CPUStats.CPUUsage.TotalUsage) - float64(preCPU)
	systemDelta := float64(v.CPUStats.SystemUsage) - float64(preSystem)
	if systemDelta > 0 && cpuDelta > 0 {
		e.cpu = cpuDelta / systemDelta * float64(v.CPUStats.OnlineCPUs) * 100
	}
	e.mem = float64(v.MemoryStats.Usage)
	if x, ok := v.MemoryStats.Stats["total_inactive_file"]; ok && x < v.MemoryStats.Usage {
		e.mem -= float64(x)
	} else if x := v.MemoryStats.Stats["inactive_file"]; x < v.MemoryStats.Usage {
		e.mem -= float64(x)
	}
	e.memLimit = float64(v.MemoryStats.Limit)
	if e.memLimit != 0 {
		e.memPercent = e.mem / e.memLimit * 100
	}
	for _, n := range v.Networks {
		e.netRx += float64(n.RxBytes)
		e.netTx += float64(n.TxBytes)
	}
	for _, b := range v.BlkioStats.IoServiceBytesRecursive {
		switch strings.ToLower(b.Op) {
		case "read":
			e.blockRead += float64(b.Value)
		case "write":
			e.blockWrite += float64(b.Value)
		}
	}
	e.pids = v.PidsStats.Current
	return e
}

// row formats an entry like the formatter of the docker CLI.
func (e statsEntry) row(trunc bool) statsRow {
	r := statsRow{Container: e.container, Name: e.name, ID: e.id}
	if trunc {
		r.ID = truncateID(e.id)
	}
	if r.Name == "" {
		r.Name = "--"
	}
	if e.err {
		r.CPUPerc, r.MemUsage, r.MemPerc, r.NetIO, r.BlockIO, r.PIDs = "--", "-- / --", "--", "--", "--", "--"
		return r
	}
	r.CPUPerc = fmt.Sprintf("%.2f%%", e.cpu)
	r.MemUsage = units.BytesSize(e.mem) + " / " + units.BytesSize(e.memLimit)
	r.MemPerc = fmt.Sprintf("%.2f%%", e.memPercent)
	r.NetIO = units.HumanSizeWithPrecision(e.netRx, 3) + " / " + units.HumanSizeWithPrecision(e.netTx, 3)
	r.BlockIO = units.HumanSizeWithPrecision(e.blockRead, 3) + " / " + units.HumanSizeWithPrecision(e.blockWrite, 3)
	r.PIDs = fmt.Sprintf("%d", e.pids)
	return r
}

// statsWatcher follows one container: a sample every second, with the
// CPU usage of the one before, as the docker CLI keeps it.
type statsWatcher struct {
	ref string // as the user gave it, or the id

	mu                 sync.Mutex
	entry              statsEntry
	preCPU, preSystem  uint64
	haveSample, failed bool
}

// sample asks the API once. With stream=0 and without one-shot, the API
// waits for a second sample, so the first CPU usage has a delta too.
func (w *statsWatcher) sample(ctx context.Context, oneShot bool) error {
	q := url.Values{"stream": {"0"}}
	if oneShot {
		q.Set("one-shot", "1")
	}
	var v statsResponse
	if _, err := dockerGet(ctx, "/containers/"+url.PathEscape(w.ref)+"/stats?"+q.Encode(), &v); err != nil {
		w.mu.Lock()
		w.entry.err, w.failed = true, true
		w.mu.Unlock()
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	pre, sys := w.preCPU, w.preSystem
	if !oneShot || !w.haveSample {
		// the API took both samples
		pre, sys = v.PreCPUStats.CPUUsage.TotalUsage, v.PreCPUStats.SystemUsage
	}
	w.entry = entryOf(w.ref, v, pre, sys)
	w.preCPU, w.preSystem = v.CPUStats.CPUUsage.TotalUsage, v.CPUStats.SystemUsage
	w.haveSample, w.failed = true, false
	return nil
}

func (w *statsWatcher) get() statsEntry {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.entry
}

// dockerStats is `docker stats [OPTIONS] [CONTAINER...]`.
func dockerStats(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, args []string) error {
	format := f["format"]
	if format == "" {
		format = statsTable
	}
	trunc := !f.bool("no-trunc")
	// the containers: those given, else those that run (all with -a)
	list := func() ([]string, error) {
		if len(args) > 0 {
			return args, nil
		}
		q := ""
		if f.bool("all") {
			q = "?all=1"
		}
		var cs []psContainer
		if _, err := dockerGet(ctx, "/containers/json"+q, &cs); err != nil {
			return nil, err
		}
		ids := make([]string, len(cs))
		for i, c := range cs {
			ids[i] = c.ID
		}
		return ids, nil
	}
	refs, err := list()
	if err != nil {
		return fail(hc, 1, "%v", err)
	}

	if f.bool("no-stream") {
		ws := make([]*statsWatcher, len(refs))
		errs := make([]error, len(refs))
		var wg sync.WaitGroup
		for i, ref := range refs {
			ws[i] = &statsWatcher{ref: ref}
			wg.Go(func() { errs[i] = ws[i].sample(ctx, false) })
		}
		wg.Wait()
		// the docker CLI fails for a container that is not there
		for _, err := range errs {
			if err != nil && len(args) > 0 {
				return fail(hc, 1, "%v", err)
			}
		}
		return writeStats(hc.Stdout, format, ws, trunc, false)
	}

	// Streaming: a watcher per container samples every second; the
	// screen is drawn again every 500 ms, as the docker CLI does.
	watchers := map[string]*statsWatcher{}
	var order []string
	var mu sync.Mutex
	start := func(ref string) {
		w := &statsWatcher{ref: ref}
		watchers[ref] = w
		order = append(order, ref)
		go func() {
			first := true
			for {
				err := w.sample(ctx, !first)
				first = false
				if err != nil && len(args) == 0 {
					// gone: the list drops it
					mu.Lock()
					delete(watchers, ref)
					mu.Unlock()
					return
				}
				select {
				case <-time.After(time.Second):
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	for _, r := range refs {
		start(r)
	}
	out := termFrom(ctx)
	var w io.Writer = hc.Stdout
	if fromTerminal(hc.Stdin) {
		w = out
	}
	io.WriteString(w, "\033[2J")
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	relist := 0
	for {
		mu.Lock()
		if len(args) == 0 {
			// new containers join, as with the events of the docker CLI
			if relist++; relist%2 == 0 {
				if ids, err := list(); err == nil {
					for _, id := range ids {
						if watchers[id] == nil {
							start(id)
						}
					}
				}
			}
		}
		var ws []*statsWatcher
		for _, r := range order {
			if x := watchers[r]; x != nil {
				ws = append(ws, x)
			}
		}
		mu.Unlock()
		writeStats(w, format, ws, trunc, true)
		select {
		case <-tick.C:
		case <-ctx.Done():
			return nil
		}
	}
}

// writeStats writes the table. live draws it over the last one, as the
// docker CLI does: cursor home, each line ends by clearing the rest of the
// line, and the rest of the screen is cleared.
func writeStats(w io.Writer, format string, ws []*statsWatcher, trunc, live bool) error {
	rows := make([]any, 0, len(ws))
	for _, x := range ws {
		e := x.get()
		if e.id == "" && !e.err {
			continue // no sample yet
		}
		rows = append(rows, e.row(trunc))
	}
	var buf bytes.Buffer
	if err := writeFormattedTo(&buf, format, statsHeader, rows); err != nil {
		return err
	}
	if !live {
		_, err := w.Write(buf.Bytes())
		return err
	}
	var b strings.Builder
	b.WriteString("\033[H")
	for _, l := range strings.SplitAfter(buf.String(), "\n") {
		if l == "" {
			continue
		}
		b.WriteString(strings.TrimSuffix(l, "\n") + "\033[K\r\n")
	}
	b.WriteString("\033[J")
	_, err := io.WriteString(w, b.String())
	return err
}
