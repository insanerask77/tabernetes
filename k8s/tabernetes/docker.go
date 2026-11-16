// Docker in the tab (docs/docker-in-wasm.md): containerd runs in-process next
// to the Kubernetes control plane, the v86 shim runs its tasks in VMs, and a
// thin Docker Engine API gives the terminal a real `docker` command.
//
// This file is the glue: it starts containerd (daemon.Start, vendored with
// the containerd-js patch by patch.go) and the Docker API on the first
// `docker` command, not at boot, so a cluster that never runs docker pays
// nothing. The `docker` command itself, a pure HTTP client, lives in
// dockercli.go.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"syscall/js"
	"time"

	"github.com/containerd/containerd/v2/core/runtime/v2/v86"
	"github.com/containerd/containerd/v2/core/runtime/v2/v86/daemon"
	"github.com/containerd/containerd/v2/core/runtime/v2/v86/dockerapi"
	"github.com/containerd/containerd/v2/core/runtime/v2/v86/jsruntime"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
	utilexec "k8s.io/utils/exec"
)

// Where containerd keeps its data, and the consoles of the containers
// (v86.LogDir). The file system of the page keeps all three across a reload
// (keptPaths in web/src/fs.js; the lists must match), the state too: after a
// reload containerd loads the shims of the old tasks from it, and they
// report the tasks as stopped, with their exit codes (v86.NewBundleService).
// Like `docker ps -a` after a restart of the Docker daemon, the containers
// are still there, as Exited, with their logs. Their VMs are gone: they ran
// in the tab that reloaded.
const (
	containerdRoot  = "/var/lib/containerd"
	containerdState = "/run/containerd"
	containerLogs   = "/var/log/v86"
)

var docker struct {
	mu      sync.Mutex
	started bool
	err     error

	// vm is the vm object of the tab whose `docker` command came last: with
	// several tabs, the containers run in the tab that asked for them.
	vmMu sync.Mutex
	vm   js.Value
	rt   *jsruntime.Runtime // once containerd runs
}

// dockerVM returns the vm object for the next container (jsruntime).
func dockerVM() js.Value {
	docker.vmMu.Lock()
	defer docker.vmMu.Unlock()
	return docker.vm
}

// maxTabVMs is the number of pods of a tab node (its capacity), and of VMs
// that a tab runs: every pod and every Docker container is a VM, a worker of
// the tab with an emulator in it. AGENTS.md keeps the limit on purpose.
const maxTabVMs = 8

// sessionOfVM returns the open session whose tab gives vm, or nil.
func sessionOfVM(vm js.Value) *session {
	if vm.Type() != js.TypeObject {
		return nil
	}
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	for _, s := range sessions {
		if s.host.Get("vm").Equal(vm) {
			return s
		}
	}
	return nil
}

// dockerVMsOf is the number of Docker containers that run in the tab name.
func dockerVMsOf(name string) int {
	sessionsMu.Lock()
	s := sessions[name]
	sessionsMu.Unlock()
	if s == nil {
		return 0
	}
	s.vmMu.Lock()
	defer s.vmMu.Unlock()
	return s.dockerVMs
}

// admitContainer takes a place for a container VM in the tab of vm
// (jsruntime.Runtime.Admit). The pods of the tab node and the containers
// share the 8 places, like the pods of a kubelet: docker run fails with the
// message of the kubelet's admission (pkg/kubelet/lifecycle/predicate.go)
// when all are taken. The node shows the places of the containers as
// allocatable pods less (tabNode.node), so the scheduler does not send a pod
// that the tab could not run. Lock order: tabNode.mu, then session.vmMu.
func admitContainer(vm js.Value) (func(), error) {
	s := sessionOfVM(vm)
	if s == nil {
		return nil, errors.New("the tab that ran docker was closed: run the command again")
	}
	s.nodeMu.Lock()
	n, closed := s.node, s.closed
	s.nodeMu.Unlock()
	if closed {
		return nil, errors.New("the tab that ran docker was closed: run the command again")
	}
	pods := 0
	if n != nil {
		n.mu.Lock()
		defer n.mu.Unlock()
		pods = n.activePods()
	}
	s.vmMu.Lock()
	used := pods + s.dockerVMs
	if used >= maxTabVMs {
		s.vmMu.Unlock()
		return nil, fmt.Errorf("Node didn't have enough resource: pods, requested: 1, used: %d, capacity: %d (%s runs at most %d pods and containers together)",
			used, maxTabVMs, s.name, maxTabVMs)
	}
	s.dockerVMs++
	s.vmMu.Unlock()
	if n != nil {
		n.allocatableChanged()
	}
	return func() {
		s.vmMu.Lock()
		s.dockerVMs--
		s.vmMu.Unlock()
		s.nodeMu.Lock()
		n := s.node
		s.nodeMu.Unlock()
		if n != nil {
			n.allocatableChanged()
		}
	}, nil
}

// dockerTabGone ends the containers of a tab that is gone (session.close):
// their VMs ended with the tab. The next container must not go to the vm
// object of that tab either.
func dockerTabGone(s *session) {
	vm := s.host.Get("vm")
	if vm.Type() != js.TypeObject {
		return
	}
	docker.vmMu.Lock()
	if docker.vm.Equal(vm) {
		docker.vm = js.Undefined()
	}
	rt := docker.rt
	docker.vmMu.Unlock()
	if rt != nil {
		rt.Gone(vm)
	}
}

// ensureDocker makes the tab of ctx the one that runs new containers and
// starts containerd and the Docker API, once. A failed start is not retried:
// containerd's plugins register process wide.
func ensureDocker(ctx context.Context) error {
	s := sessionFrom(ctx)
	if s == nil {
		return errors.New("docker needs a tab")
	}
	vm := s.host.Get("vm")
	if vm.Type() != js.TypeObject {
		return errors.New("this tab cannot run container images")
	}
	docker.vmMu.Lock()
	docker.vm = vm
	docker.vmMu.Unlock()

	docker.mu.Lock()
	defer docker.mu.Unlock()
	if !docker.started {
		docker.started = true
		docker.err = startDocker()
	}
	return docker.err
}

// startDocker starts containerd with the v86 runtime and serves the Docker
// Engine API on 127.0.0.1:2375 (dockerAPIBase). Both run until the control
// plane ends.
func startDocker() error {
	start := time.Now()
	// containerd lives as long as the control plane, not as the command line
	// that started it.
	ctx := context.Background()
	rt := jsruntime.NewRuntime(dockerVM)
	rt.Signal = signalContainer
	rt.Admit = admitContainer
	v86.LogDir = containerLogs
	docker.vmMu.Lock()
	docker.rt = rt
	docker.vmMu.Unlock()
	d, err := daemon.Start(ctx, containerdRoot, containerdState, "default", rt)
	if err != nil {
		return fmt.Errorf("start containerd: %w", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:2375") // the Docker API port
	if err != nil {
		d.Stop()
		return err
	}
	srv := dockerapi.NewServer(d.Client, dockerStreams{rt})
	srv.Ports = dockerPorts(rt)     // docker run -p (dockerports.go)
	srv.Images = dockerImageStore{} // dockerimages.go
	srv.Addresses = rt.Address
	go http.Serve(l, srv)
	fmt.Printf("tabernetes: containerd started in %s\n", time.Since(start).Round(time.Millisecond))
	return nil
}

// dockerStreams gives the Docker API attach and exec (v86.Streamer): the
// output of attach comes from the console of the VM (the log, then what
// follows), stdin, resize and exec go over the exec protocol of init, as
// kubectl attach and exec of a pod do (v86exec.go).
type dockerStreams struct{ rt *jsruntime.Runtime }

func (d dockerStreams) dialer(id string) (func(int32) (net.Conn, error), error) {
	h, ok := d.rt.VM(id)
	if !ok {
		return nil, fmt.Errorf("container %s is not running", id)
	}
	return func(port int32) (net.Conn, error) { return dialVM(h, "container "+shortID(id), port) }, nil
}

func (d dockerStreams) Attach(ctx context.Context, id string, stdin io.Reader, out io.Writer, resize <-chan v86.TerminalSize) error {
	done, stop, err := d.rt.Follow(id, out)
	if err != nil {
		return err
	}
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if stdin != nil {
		// Only stdin and the size go over the attach of init; its copy of
		// the output is dropped, the console has it already.
		if dial, err := d.dialer(id); err == nil {
			go streamVM(ctx, dial, true, nil, stdin, io.Discard, io.Discard, true, terminalSizes(ctx, resize))
		}
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
	return nil
}

func (d dockerStreams) Exec(ctx context.Context, id string, cmd []string, tty bool, stdin io.Reader, stdout, stderr io.Writer, resize <-chan v86.TerminalSize) (int, error) {
	dial, err := d.dialer(id)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	err = streamVM(ctx, dial, false, cmd, stdin, stdout, stderr, tty, terminalSizes(ctx, resize))
	if ce, ok := errors.AsType[utilexec.CodeExitError](err); ok {
		return ce.Code, nil
	}
	return 0, err
}

// signalContainer sends sig to the main process of a container, over the
// exec port of init (signalVM): docker stop and kill (jsruntime Kill).
func signalContainer(ctx context.Context, vm js.Value, id string, sig uint32) error {
	err := signalVM(ctx, func(port int32) (net.Conn, error) { return dialVM(vm, "container "+shortID(id), port) }, int(sig))
	if errors.Is(err, errNoSignal) {
		return v86.ErrNoSignals
	}
	return err
}

// StopSignal is the STOPSIGNAL of the image of a container, the default
// signal of docker stop (dockerapi).
func (d dockerStreams) StopSignal(id string) string { return d.rt.StopSignal(id) }

// terminalSizes turns the sizes of the Docker API into those of streamVM,
// until ctx ends.
func terminalSizes(ctx context.Context, in <-chan v86.TerminalSize) <-chan remotecommand.TerminalSize {
	if in == nil {
		return nil
	}
	out := make(chan remotecommand.TerminalSize, 1)
	go func() {
		defer close(out)
		for {
			select {
			case s := <-in:
				select {
				case out <- remotecommand.TerminalSize{Width: s.Cols, Height: s.Rows}:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// shortID is the id as docker ps shows it.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
