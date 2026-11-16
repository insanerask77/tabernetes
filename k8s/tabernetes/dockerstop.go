// docker stop, kill and rm: the commands that end containers. Like the
// docker CLI they take several containers, run them in parallel, print each
// container that worked and the error of each that did not.
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

// dockerFlagSet describes the flags of a command: short letters to long
// names, and which long names take a value.
type dockerFlagSet struct {
	short  map[byte]string
	alias  map[string]string // other long names
	valued map[string]bool
}

// parse splits args into flags (by long name; "true" for a flag without a
// value) and arguments. Flags may come between the arguments, as with the
// docker CLI; "--" ends them.
func (fs dockerFlagSet) parse(args []string) (map[string]string, []string, error) {
	flags := map[string]string{}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return flags, append(rest, args[i+1:]...), nil
		case strings.HasPrefix(a, "--"):
			name, value, hasValue := strings.Cut(a[2:], "=")
			if long, ok := fs.alias[name]; ok {
				name = long
			}
			if _, ok := fs.valued[name]; !ok {
				return nil, nil, fmt.Errorf("unknown flag: --%s", name)
			}
			switch {
			case !fs.valued[name] && hasValue:
				if _, err := strconv.ParseBool(value); err != nil {
					return nil, nil, fmt.Errorf("invalid argument %q for \"--%s\" flag", value, name)
				}
			case !fs.valued[name]:
				value = "true"
			case !hasValue:
				if i+1 >= len(args) {
					return nil, nil, fmt.Errorf("flag needs an argument: --%s", name)
				}
				i++
				value = args[i]
			}
			flags[name] = value
		case strings.HasPrefix(a, "-") && a != "-":
			// -f, -fv, -t 5, -t5, -t=5
			for j := 1; j < len(a); j++ {
				name, ok := fs.short[a[j]]
				if !ok {
					return nil, nil, fmt.Errorf("unknown shorthand flag: '%c' in %s", a[j], a)
				}
				if !fs.valued[name] {
					flags[name] = "true"
					continue
				}
				value := strings.TrimPrefix(a[j+1:], "=")
				if j+1 == len(a) {
					if i+1 >= len(args) {
						return nil, nil, fmt.Errorf("flag needs an argument: '%c' in -%c", a[j], a[j])
					}
					i++
					value = args[i]
				}
				flags[name] = value
				break
			}
		default:
			rest = append(rest, a)
		}
	}
	return flags, rest, nil
}

var (
	dockerStopFlags = dockerFlagSet{
		short: map[byte]string{'s': "signal", 't': "timeout"},
		// --time is the old name of --timeout
		alias:  map[string]string{"time": "timeout"},
		valued: map[string]bool{"signal": true, "timeout": true},
	}
	dockerKillFlags = dockerFlagSet{
		short:  map[byte]string{'s': "signal"},
		valued: map[string]bool{"signal": true},
	}
	dockerRMFlags = dockerFlagSet{
		short:  map[byte]string{'f': "force", 'v': "volumes", 'l': "link"},
		valued: map[string]bool{"force": false, "volumes": false, "link": false},
	}
)

// dockerEach sends one request per container, all at once, and prints each
// container that worked in the order given, as the docker CLI does. ok
// tells the statuses that count as success.
func dockerEach(ctx context.Context, hc interp.HandlerContext, containers []string, method string, path func(string) string, ok ...int) error {
	errs := make([]string, len(containers))
	done := make(chan int)
	for i, c := range containers {
		go func() {
			defer func() { done <- i }()
			body, status, err := dockerCall(ctx, method, path(c), nil)
			switch {
			case err != nil:
				errs[i] = err.Error()
			case !slices.Contains(ok, status):
				errs[i] = "Error response from daemon: " + dockerErr(body, status)
			}
		}()
	}
	for range containers {
		<-done
	}
	failed := false
	for i, c := range containers {
		if errs[i] != "" {
			fmt.Fprintln(hc.Stderr, errs[i])
			failed = true
			continue
		}
		fmt.Fprintln(hc.Stdout, c)
	}
	if failed {
		return exitCode(1)
	}
	return nil
}

// dockerStop is `docker stop [-s SIGNAL] [-t SECONDS] CONTAINER...`: the
// stop signal (SIGTERM, or the one of the image), up to 10 seconds (or -t)
// for the exit, then SIGKILL. The API does the waiting.
func dockerStop(ctx context.Context, hc interp.HandlerContext, args []string) error {
	flags, containers, err := dockerStopFlags.parse(args)
	if err != nil {
		return fail(hc, 125, "docker: %v", err)
	}
	if len(containers) == 0 {
		return fail(hc, 1, "docker: \"stop\" requires at least 1 argument")
	}
	q := url.Values{}
	if t, ok := flags["timeout"]; ok {
		if _, err := strconv.Atoi(t); err != nil {
			return fail(hc, 125, "docker: invalid argument %q for \"-t, --timeout\" flag: strconv.ParseInt: parsing %q: invalid syntax", t, t)
		}
		q.Set("t", t)
	}
	if s, ok := flags["signal"]; ok {
		q.Set("signal", s)
	}
	query := ""
	if len(q) > 0 {
		query = "?" + q.Encode()
	}
	// 304: it was not running, which docker stop does not count as an error
	return dockerEach(ctx, hc, containers, http.MethodPost,
		func(c string) string { return "/containers/" + url.PathEscape(c) + "/stop" + query },
		http.StatusNoContent, http.StatusNotModified)
}

// dockerKill is `docker kill [-s SIGNAL] CONTAINER...`: SIGKILL, or the
// signal, to the main process. A signal that the process handles, such as
// SIGHUP for nginx, leaves the container running.
func dockerKill(ctx context.Context, hc interp.HandlerContext, args []string) error {
	flags, containers, err := dockerKillFlags.parse(args)
	if err != nil {
		return fail(hc, 125, "docker: %v", err)
	}
	if len(containers) == 0 {
		return fail(hc, 1, "docker: \"kill\" requires at least 1 argument")
	}
	query := ""
	if s, ok := flags["signal"]; ok {
		query = "?signal=" + url.QueryEscape(s)
	}
	return dockerEach(ctx, hc, containers, http.MethodPost,
		func(c string) string { return "/containers/" + url.PathEscape(c) + "/kill" + query },
		http.StatusNoContent)
}

// dockerRM is `docker rm [-f] CONTAINER...`. With -f a running container
// gets SIGKILL first. -v and -l are accepted: there are no volumes or links.
func dockerRM(ctx context.Context, hc interp.HandlerContext, args []string) error {
	flags, containers, err := dockerRMFlags.parse(args)
	if err != nil {
		return fail(hc, 125, "docker: %v", err)
	}
	if len(containers) == 0 {
		return fail(hc, 1, "docker: \"rm\" requires at least 1 argument")
	}
	query := ""
	if force, _ := strconv.ParseBool(flags["force"]); force {
		query = "?force=1"
	}
	return dockerEach(ctx, hc, containers, http.MethodDelete,
		func(c string) string { return "/containers/" + url.PathEscape(c) + query },
		http.StatusNoContent)
}
