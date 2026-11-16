package main

// docker top. dockerd runs ps on the host and keeps the lines of the
// processes of the container. Here the container is a VM, and many images
// have no ps (nginx has none, busybox's has other columns), so init lists
// the processes from /proc ({"ps": true} on its exec port, vm/init/init.c)
// and this file formats them as procps' ps does for the options of docker
// top: -ef (the default), -e, -f and the BSD forms aux, ax, u.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
)

// errNoPs is the answer of an init that does not know the ps request: it
// took it for an exec without a command. The VM is older than the page.
var errNoPs = errors.New("the init of the VM cannot list processes")

// psVM asks init of a VM for its processes (ps_report in vm/init/init.c).
func psVM(ctx context.Context, dial func(int32) (net.Conn, error)) (string, error) {
	conn, err := dial(vmExecPort)
	if err != nil {
		return "", fmt.Errorf("failed to connect to the container: %w", err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	line, _ := json.Marshal(map[string]bool{"ps": true})
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return "", err
	}
	var out strings.Builder
	for {
		var h [5]byte
		if _, err := io.ReadFull(conn, h[:]); err != nil {
			return "", fmt.Errorf("the connection to the container broke: %w", err)
		}
		b := make([]byte, binary.BigEndian.Uint32(h[1:]))
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", fmt.Errorf("the connection to the container broke: %w", err)
		}
		switch h[0] {
		case vmStdout:
			out.Write(b)
		case vmExit:
			return out.String(), nil
		case vmError:
			if string(b) == "exec: no command specified" {
				return "", errNoPs
			}
			return "", errors.New(string(b))
		}
	}
}

// vmProcess is a process of the VM, read from /proc/<pid>/stat.
type vmProcess struct {
	pid, ppid, pgrp, session, tpgid int
	uid                             int
	state                           string
	comm, args                      string
	tty                             int
	utime, stime                    int64 // clock ticks
	nice, threads                   int
	start                           int64 // clock ticks after boot
	vsize                           int64 // bytes
	rss                             int64 // pages
}

// vmHz is USER_HZ of Linux on x86: the unit of the times in /proc.
const vmHz = 100

// psReport is what init reports: the uptime of the VM, its memory and the
// processes.
type psReport struct {
	uptime   float64 // seconds
	memTotal int64   // kB
	procs    []vmProcess
}

func parsePsReport(s string) (psReport, error) {
	var r psReport
	for _, line := range strings.Split(s, "\n") {
		switch {
		case line == "":
		case strings.HasPrefix(line, "uptime "):
			r.uptime, _ = strconv.ParseFloat(line[7:], 64)
		case strings.HasPrefix(line, "memtotal "):
			r.memTotal, _ = strconv.ParseInt(line[9:], 10, 64)
		default:
			uid, rest, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			stat, args, _ := strings.Cut(rest, "\t")
			p, err := parseStat(stat)
			if err != nil {
				continue // a process that ended while init read it
			}
			p.uid, _ = strconv.Atoi(uid)
			p.args = args
			r.procs = append(r.procs, p)
		}
	}
	sort.Slice(r.procs, func(i, j int) bool { return r.procs[i].pid < r.procs[j].pid })
	return r, nil
}

// parseStat reads a line of /proc/<pid>/stat (proc(5)). The name of the
// command is in parentheses and may hold spaces.
func parseStat(s string) (vmProcess, error) {
	var p vmProcess
	open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || end < open {
		return p, errors.New("bad stat line")
	}
	p.pid, _ = strconv.Atoi(strings.TrimSpace(s[:open]))
	p.comm = s[open+1 : end]
	f := strings.Fields(s[end+1:])
	if len(f) < 22 {
		return p, errors.New("short stat line")
	}
	num := func(i int) int64 { n, _ := strconv.ParseInt(f[i], 10, 64); return n }
	p.state = f[0]
	p.ppid, p.pgrp, p.session, p.tty, p.tpgid = int(num(1)), int(num(2)), int(num(3)), int(num(4)), int(num(5))
	p.utime, p.stime = num(11), num(12)
	p.nice, p.threads = int(num(16)), int(num(17))
	p.start, p.vsize, p.rss = num(19), num(20), num(21)
	return p, nil
}

// psStyle is the output format of the ps options.
type psStyle int

const (
	psDefault psStyle = iota // PID TTY TIME CMD
	psFull                   // -f: UID PID PPID C STIME TTY TIME CMD
	psBSD                    // ax: PID TTY STAT TIME COMMAND
	psBSDUser                // aux: USER PID %CPU %MEM VSZ RSS TTY STAT START TIME COMMAND
)

// parsePsArgs reads the options of docker top. Like dockerd, which adds
// the pids of the container, every option selects only the processes of
// the container, so only the format matters.
func parsePsArgs(args string) (psStyle, error) {
	style := psDefault
	for _, a := range strings.Fields(args) {
		if strings.HasPrefix(a, "-") {
			for _, c := range a[1:] {
				switch c {
				case 'e', 'A', 'a', 'd', 'N', 'x':
				case 'f':
					style = psFull
				default:
					return 0, fmt.Errorf("ps: error: unsupported SysV option: -%c (docker top in the tab knows -e, -f, a, u and x)", c)
				}
			}
			continue
		}
		bsd := psBSD
		for _, c := range a {
			switch c {
			case 'a', 'x', 'e', 'w':
			case 'u':
				bsd = psBSDUser
			default:
				return 0, fmt.Errorf("ps: error: unsupported option (BSD syntax): %c (docker top in the tab knows -e, -f, a, u and x)", c)
			}
		}
		style = bsd
	}
	return style, nil
}

// formatTop turns the report of init into the titles and rows of docker
// top. Init itself and the kernel threads are left out: dockerd shows the
// processes of the container only, not its shim or the host kernel.
func formatTop(report string, psArgs string, now time.Time) ([]string, [][]string, error) {
	style, err := parsePsArgs(psArgs)
	if err != nil {
		return nil, nil, err
	}
	r, err := parsePsReport(report)
	if err != nil {
		return nil, nil, err
	}
	var titles []string
	switch style {
	case psDefault:
		titles = []string{"PID", "TTY", "TIME", "CMD"}
	case psFull:
		titles = []string{"UID", "PID", "PPID", "C", "STIME", "TTY", "TIME", "CMD"}
	case psBSD:
		titles = []string{"PID", "TTY", "STAT", "TIME", "COMMAND"}
	case psBSDUser:
		titles = []string{"USER", "PID", "%CPU", "%MEM", "VSZ", "RSS", "TTY", "STAT", "START", "TIME", "COMMAND"}
	}
	rows := [][]string{}
	for _, p := range r.procs {
		if p.pid == 1 || p.pid == 2 || p.ppid == 2 {
			continue
		}
		// seconds of life and the CPU time, as procps computes them
		life := int64(r.uptime - float64(p.start)/vmHz)
		cpu := (p.utime + p.stime) / vmHz
		started := now.Add(-time.Duration(life) * time.Second)
		cmd := p.args
		if cmd == "" {
			cmd = "[" + p.comm + "]"
		}
		pid, tty := strconv.Itoa(p.pid), ttyName(p.tty)
		switch style {
		case psDefault:
			rows = append(rows, []string{pid, tty, cpuTime(cpu), p.comm})
		case psFull:
			c := int64(0)
			if life > 0 {
				c = min((p.utime+p.stime)*100/vmHz/life, 99)
			}
			rows = append(rows, []string{userName(p.uid), pid, strconv.Itoa(p.ppid), strconv.FormatInt(c, 10), startTime(started, now), tty, cpuTime(cpu), cmd})
		case psBSD:
			rows = append(rows, []string{pid, tty, p.stat(), bsdTime(cpu), cmd})
		case psBSDUser:
			pcpu, pmem := int64(0), int64(0)
			if life > 0 {
				pcpu = min((p.utime+p.stime)*1000/vmHz/life, 999)
			}
			rssKB := p.rss * 4
			if r.memTotal > 0 {
				pmem = rssKB * 1000 / r.memTotal
			}
			rows = append(rows, []string{userName(p.uid), pid, tenths(pcpu), tenths(pmem), strconv.FormatInt(p.vsize/1024, 10),
				strconv.FormatInt(rssKB, 10), tty, p.stat(), startTime(started, now), bsdTime(cpu), cmd})
		}
	}
	return titles, rows, nil
}

// userName is the user of a process. dockerd shows the names of the host;
// the VM has no users of its own besides those of the image, so root is
// root and any other user is its number.
func userName(uid int) string {
	if uid == 0 {
		return "root"
	}
	return strconv.Itoa(uid)
}

// ttyName is the terminal of a process as ps shows it: pts/0, ttyS0 or ?.
func ttyName(nr int) string {
	major, minor := (nr>>8)&0xfff, (nr&0xff)|((nr>>12)&0xfff00)
	switch {
	case nr == 0:
		return "?"
	case major >= 136 && major <= 143:
		return fmt.Sprintf("pts/%d", (major-136)*256+minor)
	case major == 4 && minor >= 64:
		return fmt.Sprintf("ttyS%d", minor-64)
	case major == 4:
		return fmt.Sprintf("tty%d", minor)
	case major == 5 && minor == 1:
		return "console"
	}
	return "?"
}

// startTime is STIME and START of procps: the time today, else the day,
// else the year.
func startTime(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	switch {
	case t.Year() != now.Year():
		return t.Format("2006")
	case t.YearDay() != now.YearDay():
		return t.Format("Jan02")
	}
	return t.Format("15:04")
}

// cpuTime is TIME of ps -f: [DD-]hh:mm:ss.
func cpuTime(s int64) string {
	if d := s / 86400; d > 0 {
		return fmt.Sprintf("%d-%02d:%02d:%02d", d, s%86400/3600, s%3600/60, s%60)
	}
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, s%3600/60, s%60)
}

// bsdTime is TIME of ps aux: minutes:seconds.
func bsdTime(s int64) string { return fmt.Sprintf("%d:%02d", s/60, s%60) }

func tenths(n int64) string { return fmt.Sprintf("%d.%d", n/10, n%10) }

// stat is STAT of ps aux: the state and the flags of procps.
func (p vmProcess) stat() string {
	s := p.state
	switch {
	case p.nice < 0:
		s += "<"
	case p.nice > 0:
		s += "N"
	}
	if p.session == p.pid {
		s += "s"
	}
	if p.threads > 1 {
		s += "l"
	}
	if p.tpgid > 0 && p.pgrp == p.tpgid {
		s += "+"
	}
	return s
}

// Top is docker top for the Docker API (dockerapi.Topper).
func (d dockerStreams) Top(ctx context.Context, id, psArgs string) ([]string, [][]string, error) {
	if _, err := parsePsArgs(psArgs); err != nil {
		return nil, nil, err
	}
	dial, err := d.dialer(id)
	if err != nil {
		return nil, nil, err
	}
	report, err := psVM(ctx, dial)
	if err != nil {
		return nil, nil, err
	}
	return formatTop(report, psArgs, time.Now())
}
