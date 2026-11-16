// docker events: what happens to containers (create, start, kill, die,
// stop, destroy), as the Docker API publishes it (dockerapi/events.go),
// in the format of the docker CLI, until Ctrl-C or --until.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
)

func init() {
	c := &dockerCommand{
		name: "events", use: "events [OPTIONS]", short: "Get real time events from the server",
		aliases: []string{"docker system events", "docker events"},
		flags: []flagSpec{
			{'f', "filter", "filter", "Filter output based on conditions provided"},
			{0, "format", "string", formatHelp},
			{0, "since", "string", "Show all events created since timestamp"},
			{0, "until", "string", "Stream events until this timestamp"},
		},
		maxArgs: 0, run: dockerEvents,
	}
	dockerCommands[c.name] = c
}

// dockerEvent is an event of the API (events.Message), with the fields of
// the templates of the docker CLI.
type dockerEvent struct {
	Status string `json:"status,omitempty"`
	ID     string `json:"id,omitempty"`
	From   string `json:"from,omitempty"`
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	Scope    string `json:"scope"`
	Time     int64  `json:"time"`
	TimeNano int64  `json:"timeNano"`
}

// prettyEvent is the line of docker events (prettyPrintEvent of the CLI).
func prettyEvent(e dockerEvent) string {
	var b strings.Builder
	if e.TimeNano != 0 {
		b.WriteString(time.Unix(0, e.TimeNano).Format("2006-01-02T15:04:05.000000000Z07:00") + " ")
	} else if e.Time != 0 {
		b.WriteString(time.Unix(e.Time, 0).Format("2006-01-02T15:04:05.000000000Z07:00") + " ")
	}
	fmt.Fprintf(&b, "%s %s %s", e.Type, e.Action, e.Actor.ID)
	if len(e.Actor.Attributes) > 0 {
		keys := make([]string, 0, len(e.Actor.Attributes))
		for k := range e.Actor.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		attrs := make([]string, len(keys))
		for i, k := range keys {
			attrs[i] = k + "=" + e.Actor.Attributes[k]
		}
		fmt.Fprintf(&b, " (%s)", strings.Join(attrs, ", "))
	}
	return b.String() + "\n"
}

// eventsQuery is the query of GET /events for the flags.
func eventsQuery(f dockerFlagValues, now time.Time) (url.Values, error) {
	q := url.Values{}
	for _, name := range []string{"since", "until"} {
		if v := f[name]; v != "" {
			ts, err := getTimestamp(v, now)
			if err != nil {
				return nil, err
			}
			q.Set(name, ts)
		}
	}
	if v := f["filter"]; v != "" {
		k, val, ok := strings.Cut(v, "=")
		if !ok {
			return nil, fmt.Errorf("invalid argument %q for \"-f, --filter\" flag: bad format of filter (expected name=value)", v)
		}
		b, _ := json.Marshal(map[string]map[string]bool{strings.ToLower(k): {val: true}})
		q.Set("filters", string(b))
	}
	return q, nil
}

// dockerEvents is `docker events`.
func dockerEvents(ctx context.Context, hc interp.HandlerContext, f dockerFlagValues, _ []string) error {
	q, err := eventsQuery(f, time.Now())
	if err != nil {
		return fail(hc, 125, "%v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dockerAPIBase+"/events?"+q.Encode(), nil)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	resp, err := localClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fail(hc, 1, "%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fail(hc, 1, "%s", daemonError(b, resp.StatusCode))
	}
	format := f["format"]
	dec := json.NewDecoder(resp.Body)
	for {
		var e dockerEvent
		if err := dec.Decode(&e); err != nil {
			return nil
		}
		if format == "" {
			io.WriteString(hc.Stdout, prettyEvent(e))
			continue
		}
		if err := writeFormattedTo(hc.Stdout, format, nil, []any{e}); err != nil {
			return fail(hc, 64, "%v", err)
		}
	}
}
