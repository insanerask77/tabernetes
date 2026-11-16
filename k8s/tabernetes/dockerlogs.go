// docker logs, with the flags of the docker CLI: -f, --since, --until, -t,
// -n/--tail and --details. The Docker API (dockerapi/logs.go) keeps the log
// in the format of Docker's json-file driver, rotated, and filters it.
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// The flags of docker logs, as docker/cli v28 lists them.
var logsFlags = []flagSpec{
	{0, "details", "", "Show extra details provided to logs"},
	{'f', "follow", "", "Follow log output"},
	{0, "since", "string", `Show logs since timestamp (e.g. "2013-01-02T13:23:37Z") or relative (e.g. "42m" for 42 minutes)`},
	{'n', "tail", "string", `Number of lines to show from the end of the logs (default "all")`},
	{'t', "timestamps", "", "Show timestamps"},
	{0, "until", "string", `Show logs before a timestamp (e.g. "2013-01-02T13:23:37Z") or relative (e.g. "42m" for 42 minutes)`},
}

func init() {
	// dockercmds.go registers the command; the flags are here, with the
	// command (files init in name order).
	if c := dockerCommands["logs"]; c != nil {
		c.flags = logsFlags
	}
}

// dockerLogs prints the log of a container: `docker logs [OPTIONS]
// CONTAINER`. Without a tty the API multiplexes the streams: every frame
// starts with 8 bytes, the stream (1 stdout, 2 stderr) and the size. With
// -f it prints what comes until the container stops or Ctrl-C.
func dockerLogs(ctx context.Context, hc interp.HandlerContext, args []string) error {
	const path = "docker logs"
	c := &dockerCommand{name: "logs", use: "logs [OPTIONS] CONTAINER", flags: logsFlags}
	f, args, err := parseDockerFlags(c, args)
	if err != nil {
		return fail(hc, 125, "%v\n\nUsage:  %s [OPTIONS] CONTAINER\n\nRun '%s --help' for more information", err, path, path)
	}
	if len(args) != 1 {
		return fail(hc, 1, "docker: '%s' requires 1 argument\n\nUsage:  %s [OPTIONS] CONTAINER\n\nRun '%s --help' for more information", path, path, path)
	}
	q, err := logsQuery(f, time.Now())
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dockerAPIBase+"/containers/"+url.PathEscape(args[0])+"/logs?"+q.Encode(), nil)
	if err != nil {
		return fail(hc, 1, "docker: %v", err)
	}
	resp, err := localClient.Do(req)
	if err != nil {
		return fail(hc, 1, "docker: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fail(hc, 1, "%s", daemonError(b, resp.StatusCode))
	}
	if resp.Header.Get("Content-Type") != "application/vnd.docker.multiplexed-stream" {
		io.Copy(hc.Stdout, resp.Body)
		return nil
	}
	demux(resp.Body, hc.Stdout, hc.Stderr)
	return nil
}

// demux copies the multiplexed stream of the Docker API to stdout and
// stderr, frame by frame, until it ends.
func demux(r io.Reader, stdout, stderr io.Writer) {
	var h [8]byte
	for {
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return
		}
		w := stdout
		if h[0] == 2 {
			w = stderr
		}
		if _, err := io.CopyN(w, r, int64(binary.BigEndian.Uint32(h[4:]))); err != nil {
			return
		}
	}
}

// logsQuery turns the flags of docker logs into the query of the API, as
// the docker CLI does: since and until become Unix timestamps.
func logsQuery(f dockerFlagValues, now time.Time) (url.Values, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}}
	for _, name := range []string{"since", "until"} {
		if v, ok := f[name]; ok && v != "" {
			ts, err := getTimestamp(v, now)
			if err != nil {
				return nil, err
			}
			q.Set(name, ts)
		}
	}
	if f.bool("timestamps") {
		q.Set("timestamps", "1")
	}
	if f.bool("follow") {
		q.Set("follow", "1")
	}
	if f.bool("details") {
		q.Set("details", "1")
	}
	tail := "all"
	if v, ok := f["tail"]; ok {
		tail = v
	}
	q.Set("tail", tail)
	return q, nil
}

// getTimestamp is GetTimestamp of moby's api/types/time: a duration ("10m")
// is that long before now, else an RFC 3339 time or date (in the local zone
// without one), else Unix seconds as they are. The result is
// "seconds.nanoseconds".
func getTimestamp(value string, reference time.Time) (string, error) {
	if d, err := time.ParseDuration(value); value != "0" && err == nil {
		return strconv.FormatInt(reference.Add(-d).Unix(), 10), nil
	}
	const (
		rfc3339Local     = "2006-01-02T15:04:05"
		rfc3339NanoLocal = "2006-01-02T15:04:05.999999999"
		dateWithZone     = "2006-01-02Z07:00"
		dateLocal        = "2006-01-02"
	)
	var format string
	parseInLocation := !(strings.ContainsAny(value, "zZ+") || strings.Count(value, "-") == 3)
	switch {
	case strings.Contains(value, "."):
		format = time.RFC3339Nano
		if parseInLocation {
			format = rfc3339NanoLocal
		}
	case strings.Contains(value, "T"):
		tcolons := strings.Count(value, ":")
		if !parseInLocation && !strings.ContainsAny(value, "zZ") && tcolons > 0 {
			tcolons--
		}
		if parseInLocation {
			switch tcolons {
			case 0:
				format = "2006-01-02T15"
			case 1:
				format = "2006-01-02T15:04"
			default:
				format = rfc3339Local
			}
		} else {
			switch tcolons {
			case 0:
				format = "2006-01-02T15Z07:00"
			case 1:
				format = "2006-01-02T15:04Z07:00"
			default:
				format = time.RFC3339
			}
		}
	case parseInLocation:
		format = dateLocal
	default:
		format = dateWithZone
	}
	var t time.Time
	var err error
	if parseInLocation {
		t, err = time.ParseInLocation(format, value, time.FixedZone(reference.Zone()))
	} else {
		t, err = time.Parse(format, value)
	}
	if err != nil {
		if strings.Contains(value, "-") {
			return "", err
		}
		sec, frac, _ := strings.Cut(value, ".")
		if _, err := strconv.ParseInt(sec, 10, 64); err != nil {
			return "", fmt.Errorf("failed to parse value as time or duration: %q", value)
		}
		if frac != "" {
			if _, err := strconv.ParseInt(frac, 10, 64); err != nil {
				return "", fmt.Errorf("failed to parse value as time or duration: %q", value)
			}
		}
		return value, nil
	}
	return fmt.Sprintf("%d.%09d", t.Unix(), int64(t.Nanosecond())), nil
}

// dockerLogConfig is the LogConfig of a HostConfig: --log-driver and
// --log-opt of docker run and create.
type dockerLogConfig struct {
	Type   string            `json:"Type"`
	Config map[string]string `json:"Config"`
}

// logConfigOf returns the LogConfig of the options, or nil without any.
func logConfigOf(o dockerRunOptions) *dockerLogConfig {
	if o.logDriver == "" && len(o.logOpts) == 0 {
		return nil
	}
	c := &dockerLogConfig{Type: o.logDriver, Config: map[string]string{}}
	for _, kv := range o.logOpts {
		k, v, _ := strings.Cut(kv, "=")
		c.Config[k] = v
	}
	return c
}
