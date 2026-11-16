// Published ports of Docker containers: docker run -p and docker port.
//
// The Docker API (dockerapi) keeps the ports of a container in a label and
// opens them while its task runs: v86.Ports listens on 127.0.0.1:<host port>
// of the in-process network, like docker-proxy, and copies each connection
// to the container port of the VM. This file gives it what only the glue
// has: the dial into a VM (dialVM, v86.go) and the links under the terminal
// (forwards.go), so a published port opens like one of kubectl
// port-forward: proxy 8080, wget localhost:8080 or a click on the link.
//
// The CLI part (the -p syntax and docker port) speaks only HTTP, like
// dockercli.go.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/containerd/v2/core/runtime/v2/v86"
	"github.com/containerd/containerd/v2/core/runtime/v2/v86/jsruntime"
	"mvdan.cc/sh/v3/interp"
)

// dockerPorts is the PortPublisher of the Docker API (startDocker).
func dockerPorts(rt *jsruntime.Runtime) *v86.Ports {
	return &v86.Ports{
		Dial: func(_ context.Context, id string, port int) (net.Conn, error) {
			h, ok := rt.VM(id)
			if !ok {
				return nil, fmt.Errorf("container %s is not running", shortID(id))
			}
			return dialVM(h, "container "+shortID(id), int32(port))
		},
		Changed: dockerLinks,
	}
}

// dockerForwards are the entries of forwards.list for the published ports,
// by container id. forwards guards it.
var dockerForwards = map[string][]*forward{}

// dockerLinks shows the open ports of container id under the terminal, as
// "<name>:<container port>", like those of kubectl port-forward.
func dockerLinks(id, name string, ports []v86.PortBinding) {
	forwards.Lock()
	old := dockerForwards[id]
	delete(dockerForwards, id)
	forwards.list = slices.DeleteFunc(forwards.list, func(f *forward) bool { return slices.Contains(old, f) })
	var mine []*forward
	for _, p := range ports {
		port, err := strconv.ParseUint(p.HostPort, 10, 16)
		if err != nil {
			continue
		}
		mine = append(mine, &forward{uint16(port), dockerName(id, name) + ":" + p.ContainerPort})
	}
	if len(mine) > 0 {
		dockerForwards[id] = mine
		forwards.list = append(forwards.list, mine...)
	}
	forwards.Unlock()
	publishForwards()
}

// dockerName is the container as the user knows it: its name (--name, or
// the one the API made up), else the short id.
func dockerName(id, name string) string {
	if name != "" {
		return name
	}
	return shortID(id)
}

// The CLI side.

// dockerPortBinding is the host side of a port in the create request.
type dockerPortBinding struct {
	HostIp   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// dockerHostConfig is the part of Docker's HostConfig the CLI sends.
type dockerHostConfig struct {
	PortBindings map[string][]dockerPortBinding `json:"PortBindings,omitempty"`
	// AutoRemove: the daemon removes the container when it exits (--rm),
	// also when nobody waits for it (-d).
	AutoRemove bool `json:"AutoRemove,omitempty"`
	// -P, -m, --restart and --init (dockerrunflags.go)
	PublishAllPorts bool                 `json:"PublishAllPorts,omitempty"`
	Memory          int64                `json:"Memory,omitempty"`
	RestartPolicy   *dockerRestartPolicy `json:"RestartPolicy,omitempty"`
	Init            *bool                `json:"Init,omitempty"`
	// --log-driver and --log-opt (dockerlogs.go)
	LogConfig *dockerLogConfig `json:"LogConfig,omitempty"`
	// Binds and Mounts: -v and --mount (dockervolumes.go)
	dockerMountConfig
}

// setHostConfig puts the options of docker run and create that belong to
// HostConfig (-p, -P, --rm, -m, --restart, --init) and the exposed ports
// into the create request.
// parseDockerRun checked the -p specs already.
func setHostConfig(req *dockerCreateRequest, o dockerRunOptions) {
	hcfg := &dockerHostConfig{AutoRemove: o.rm, PublishAllPorts: o.publishAll, Memory: o.memory}
	if exposed, bindings, _ := dockerPublish(o.publish); len(bindings) > 0 {
		req.ExposedPorts, hcfg.PortBindings = exposed, bindings
	}
	if o.restart.Name != "" {
		hcfg.RestartPolicy = &o.restart
	}
	if o.init {
		hcfg.Init = &o.init
	}
	hcfg.LogConfig = logConfigOf(o)
	req.HostConfig = hcfg
}

// dockerPublish turns the -p options into ExposedPorts and PortBindings of
// the create request, as the docker CLI does (nat.ParsePortSpecs).
func dockerPublish(specs []string) (map[string]struct{}, map[string][]dockerPortBinding, error) {
	if len(specs) == 0 {
		return nil, nil, nil
	}
	exposed, bindings := map[string]struct{}{}, map[string][]dockerPortBinding{}
	for _, s := range specs {
		ports, err := parsePortSpec(s)
		if err != nil {
			return nil, nil, err
		}
		for _, p := range ports {
			exposed[p.port] = struct{}{}
			bindings[p.port] = append(bindings[p.port], p.binding)
		}
	}
	return exposed, bindings, nil
}

type portMapping struct {
	port    string // "80/tcp"
	binding dockerPortBinding
}

// parsePortSpec reads one -p value: [[IP:][HOST_PORT]:]CONTAINER_PORT[/PROTO],
// where the ports may be ranges. It follows ParsePortSpec of
// github.com/docker/go-connections/nat, with its messages.
func parsePortSpec(raw string) ([]portMapping, error) {
	var ip, hostPort, containerPort string
	parts := strings.Split(raw, ":")
	switch n := len(parts); n {
	case 1:
		containerPort = parts[0]
	case 2:
		hostPort, containerPort = parts[0], parts[1]
	default:
		ip, hostPort, containerPort = strings.Join(parts[:n-2], ":"), parts[n-2], parts[n-1]
	}
	proto := "tcp"
	if p, pr, ok := strings.Cut(containerPort, "/"); ok {
		containerPort = p
		proto = cmp.Or(strings.ToLower(pr), "tcp")
	}
	if strings.HasPrefix(ip, "[") {
		rawIP, _, err := net.SplitHostPort(ip + ":")
		if err != nil {
			return nil, fmt.Errorf("invalid IP address %v: %w", ip, err)
		}
		ip = rawIP
	}
	if ip != "" && net.ParseIP(ip) == nil {
		return nil, errors.New("invalid IP address: " + ip)
	}
	if containerPort == "" {
		return nil, fmt.Errorf("no port specified: %s<empty>", raw)
	}
	start, end, err := parsePortRange(containerPort)
	if err != nil {
		return nil, errors.New("invalid containerPort: " + containerPort)
	}
	var hostStart, hostEnd uint64
	if hostPort != "" {
		if hostStart, hostEnd, err = parsePortRange(hostPort); err != nil {
			return nil, errors.New("invalid hostPort: " + hostPort)
		}
		// a host range for one container port is the range to pick from
		if end-start != hostEnd-hostStart && end != start {
			return nil, fmt.Errorf("invalid ranges specified for container and host Ports: %s and %s", containerPort, hostPort)
		}
	}
	switch proto {
	case "tcp", "udp", "sctp":
	default:
		return nil, errors.New("invalid proto: " + proto)
	}
	count := end - start + 1
	var out []portMapping
	for i := range count {
		hp := ""
		if hostPort != "" {
			hp = strconv.FormatUint(hostStart+i, 10)
			if count == 1 && hostStart != hostEnd {
				hp += "-" + strconv.FormatUint(hostEnd, 10)
			}
		}
		out = append(out, portMapping{
			port:    strconv.FormatUint(start+i, 10) + "/" + proto,
			binding: dockerPortBinding{HostIp: ip, HostPort: hp},
		})
	}
	return out, nil
}

// parsePortRange reads "80" or "8000-8010".
func parsePortRange(s string) (uint64, uint64, error) {
	if s == "" {
		return 0, 0, errors.New("empty string specified for ports")
	}
	a, b, isRange := strings.Cut(s, "-")
	start, err := strconv.ParseUint(a, 10, 16)
	if err != nil {
		return 0, 0, err
	}
	if !isRange {
		return start, start, nil
	}
	end, err := strconv.ParseUint(b, 10, 16)
	if err != nil {
		return 0, 0, err
	}
	if end < start {
		return 0, 0, errors.New("invalid range specified for port: " + s)
	}
	return start, end, nil
}

// dockerPortCmd lists the published ports of a container: `docker port
// CONTAINER [PRIVATE_PORT[/PROTO]]`, with the output and messages of the
// docker CLI. It reads them from docker inspect (NetworkSettings.Ports).
func dockerPortCmd(ctx context.Context, hc interp.HandlerContext, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fail(hc, 1, "docker: \"docker port\" requires at least 1 and at most 2 arguments.\nUsage:  docker port CONTAINER [PRIVATE_PORT[/PROTO]]")
	}
	body, status, err := dockerCall(ctx, http.MethodGet, "/containers/"+args[0]+"/json", nil)
	if err != nil {
		return fail(hc, 1, "docker: %v", err)
	}
	if status != http.StatusOK {
		return fail(hc, 1, "Error response from daemon: %s", dockerErr(body, status))
	}
	var c struct {
		NetworkSettings struct {
			Ports map[string][]dockerPortBinding
		}
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return fail(hc, 1, "docker: %v", err)
	}
	lines, err := dockerPortLines(c.NetworkSettings.Ports, args[0], args[1:])
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	for _, l := range lines {
		fmt.Fprintln(hc.Stdout, l)
	}
	return nil
}

// dockerPortLines formats the ports as docker port does: "80/tcp ->
// 0.0.0.0:8080" for all, or "0.0.0.0:8080" for one private port.
func dockerPortLines(ports map[string][]dockerPortBinding, container string, private []string) ([]string, error) {
	type line struct {
		port  uint64
		proto string
		text  string
	}
	var out []line
	if len(private) == 1 {
		port, proto, _ := strings.Cut(private[0], "/")
		proto = cmp.Or(proto, "tcp")
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return nil, fmt.Errorf("Error: invalid port (%s): %w", port, err)
		}
		hosts := ports[port+"/"+proto]
		if len(hosts) == 0 {
			return nil, fmt.Errorf("Error: No public port '%s' published for %s", private[0], container)
		}
		for _, h := range hosts {
			out = append(out, line{text: net.JoinHostPort(h.HostIp, h.HostPort)})
		}
	} else {
		for key, hosts := range ports {
			p, proto, _ := strings.Cut(key, "/")
			n, _ := strconv.ParseUint(p, 10, 16)
			for _, h := range hosts {
				out = append(out, line{n, proto, key + " -> " + net.JoinHostPort(h.HostIp, h.HostPort)})
			}
		}
	}
	slices.SortStableFunc(out, func(a, b line) int {
		return cmp.Or(cmp.Compare(a.port, b.port), cmp.Compare(a.proto, b.proto), cmp.Compare(a.text, b.text))
	})
	texts := make([]string, len(out))
	for i, l := range out {
		texts[i] = l.text
	}
	return texts, nil
}
