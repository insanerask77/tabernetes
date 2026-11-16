package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/runtime/v2/v86"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
)

// workload is what runs in place of a container on the tab node.
type workload interface {
	// start loads the workload and returns once it runs. It reports the
	// download to pull, and writes to log (kubectl logs) once that is done.
	start(ctx context.Context, log io.Writer, pull *imagePull) error
	// exec runs a command of the workload (kubectl exec).
	exec(ctx context.Context, cmd []string, in io.Reader, out, errOut io.Writer) error
	// dial connects to a port of the pod (kubectl port-forward).
	dial(port int32) (net.Conn, error)
	stop()
	// imageID tells kubectl describe what runs.
	imageID() string
	// runtime names what runs it, for the container ID.
	runtime() string
}

// exiter is a workload that ends by itself, as a container process does.
type exiter interface {
	// wait returns the exit code once the workload has ended.
	wait() <-chan int
}

// signaler is a workload whose main process takes signals, so that it can
// stop gracefully.
type signaler interface {
	// signal sends sig to the main process; it returns once it is sent.
	signal(ctx context.Context, sig int) error
	// stopSignal is the STOPSIGNAL of the image, or "".
	stopSignal() string
}

// startError is a container that did not start, as runc reports it.
type startError struct {
	code int
	msg  string
}

func (e *startError) Error() string { return e.msg }

// newWorkload picks the workload for the image of c.
func newWorkload(tp *tabPod, c *corev1.Container) (workload, error) {
	if model, ok := webllmModel(c.Image); ok {
		return newWebLLM(tp, model)
	}
	if tp.n.hasVM() {
		return newV86(tp, c)
	}
	return nil, fmt.Errorf("the tab node runs no containers, only images that run in the browser: "+webllmRepo+"<model> (not %q)", c.Image)
}

const (
	backoffStart = 10 * time.Second
	backoffMax   = 5 * time.Minute
)

// tabPod is a pod on the tab node. It has one container.
type tabPod struct {
	n               *tabNode
	uid             types.UID
	namespace, name string
	container       corev1.Container
	restartPolicy   corev1.RestartPolicy
	ref             *corev1.ObjectReference
	ip              string
	ignored         []string // what the pod asks for and the tab node does not have
	logs            *logBuffer
	ctx             context.Context
	cancel          context.CancelFunc

	emptyDirs []tabMount // the emptyDir volumes that the container mounts (tabvolumes.go)

	mu      sync.Mutex
	w       workload
	status  corev1.ContainerStatus
	phase   corev1.PodPhase
	reason  string // of the pod, when the tab cannot run it at all
	message string
	started metav1.Time
	push    sync.Mutex // one status update at a time

	terminating bool          // the pod is deleted: no restart (under mu)
	termOnce    sync.Once     // one termination
	runDone     chan struct{} // closed when run returns
}

func (n *tabNode) newPod(pod *corev1.Pod) *tabPod {
	ctx, cancel := context.WithCancel(context.Background())
	tp := &tabPod{
		n:             n,
		uid:           pod.UID,
		namespace:     pod.Namespace,
		name:          pod.Name,
		restartPolicy: pod.Spec.RestartPolicy,
		ref:           &corev1.ObjectReference{Kind: "Pod", APIVersion: "v1", Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID},
		ip:            n.allocateIP(),
		logs:          newLogBuffer(),
		ctx:           ctx,
		cancel:        cancel,
		phase:         corev1.PodPending,
		started:       metav1.Now(),
		runDone:       make(chan struct{}),
	}
	if pod.Spec.HostNetwork {
		tp.ip = n.ip
	}
	if len(pod.Spec.Containers) > 0 {
		tp.container = pod.Spec.Containers[0]
	}
	tp.status = corev1.ContainerStatus{
		Name:  tp.container.Name,
		Image: tp.container.Image,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
	}
	if len(pod.Spec.Containers) != 1 || len(pod.Spec.InitContainers) > 0 {
		tp.phase, tp.reason, tp.message = corev1.PodFailed, "UnsupportedPod", "the tab node runs pods with exactly one container and no init containers"
	}
	emptyDir := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.EmptyDir != nil:
			emptyDir[v.Name] = true
		// every pod has the one of its service account token
		case !strings.HasPrefix(v.Name, "kube-api-access-"):
			tp.ignored = append(tp.ignored, "volume "+v.Name)
		}
	}
	tp.emptyDirs = emptyDirMounts(pod.UID, tp.container.VolumeMounts, emptyDir, &tp.ignored)
	if len(tp.container.EnvFrom) > 0 {
		tp.ignored = append(tp.ignored, "envFrom")
	}
	for _, e := range tp.container.Env {
		if _, ok := tp.envValue(e); !ok {
			tp.ignored = append(tp.ignored, "env "+e.Name)
		}
	}
	return tp
}

// envValue returns the value of an environment variable of the container.
// Of the downward API the tab node knows the fields of the pod itself, and
// it does not read ConfigMaps or Secrets: ok is false for these.
func (tp *tabPod) envValue(e corev1.EnvVar) (value string, ok bool) {
	if e.ValueFrom == nil {
		return e.Value, true
	}
	if f := e.ValueFrom.FieldRef; f != nil {
		switch f.FieldPath {
		case "metadata.name":
			return tp.name, true
		case "metadata.namespace":
			return tp.namespace, true
		case "metadata.uid":
			return string(tp.uid), true
		case "spec.nodeName":
			return tp.n.name, true
		case "status.podIP", "status.podIPs":
			return tp.ip, true
		case "status.hostIP", "status.hostIPs":
			return tp.n.ip, true
		}
	}
	return "", false
}

func (tp *tabPod) event(typ, reason, msg string) {
	tp.n.event(tp.ref, typ, reason, msg)
}

// run starts the workload and restarts it after a failure, with the same
// back-off as the kubelet.
func (tp *tabPod) run() {
	defer close(tp.runDone)
	ctx := tp.ctx
	tp.update()
	if tp.phase == corev1.PodFailed {
		// the kubelet's event of a rejected pod has the message without
		// the prefix of the status
		tp.event(corev1.EventTypeWarning, tp.reason, strings.TrimPrefix(tp.message, "Pod was rejected: "))
		return
	}
	if len(tp.ignored) > 0 {
		tp.event(corev1.EventTypeWarning, "Unsupported", "the tab node has only emptyDir volumes, and of valueFrom only the fields of the pod; the container starts without: "+strings.Join(tp.ignored, ", "))
	}
	backoff := backoffStart
	for ctx.Err() == nil {
		tp.mu.Lock()
		select {
		case <-tp.logs.done:
			tp.logs = newLogBuffer()
		default:
		}
		logs := tp.logs
		tp.mu.Unlock()
		w, err := newWorkload(tp, &tp.container)
		if err != nil {
			tp.event(corev1.EventTypeWarning, "Failed", err.Error())
			tp.setWaiting("ErrImagePull", err.Error())
			logs.finish()
			return
		}
		pull := &imagePull{tp: tp, t0: time.Now()}
		err = w.start(ctx, logs, pull)
		if ctx.Err() != nil {
			w.stop()
			logs.finish()
			return
		}
		if err != nil {
			w.stop()
			if !pull.wasPulled() {
				logs.finish()
				if _, ok := errors.AsType[errNeverPull](err); ok {
					// what the kubelet does: no pull, the pod waits and
					// tries again
					tp.event(corev1.EventTypeWarning, "ErrImageNeverPull", err.Error())
					tp.setWaiting("ErrImageNeverPull", err.Error())
					select {
					case <-ctx.Done():
						return
					case <-time.After(backoff):
					}
					backoff = min(backoff*2, backoffMax)
					continue
				}
				// the download failed: the kubelet calls that a failed pull
				image := tp.container.Image
				tp.event(corev1.EventTypeWarning, "Failed", fmt.Sprintf("Failed to pull image %q: %v", image, err))
				tp.event(corev1.EventTypeWarning, "Failed", "Error: ErrImagePull")
				tp.setWaiting("ErrImagePull", err.Error())
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				tp.event(corev1.EventTypeNormal, "BackOff", fmt.Sprintf("Back-off pulling image %q", image))
				tp.setWaiting("ImagePullBackOff", fmt.Sprintf("Back-off pulling image %q: ErrImagePull: %v", image, err))
				backoff = min(backoff*2, backoffMax)
				continue
			}
			code, reason := 1, "Error"
			if se, ok := errors.AsType[*startError](err); ok {
				// the kubelet tells the error of runc only in the events
				code, reason = se.code, "StartError"
				tp.event(corev1.EventTypeWarning, "Failed", "Error: "+err.Error())
			} else {
				fmt.Fprintln(logs, "error:", err)
				tp.event(corev1.EventTypeWarning, "Failed", err.Error())
			}
			restart := tp.restarts(code)
			tp.setTerminated(code, reason, err.Error(), restart)
			if !restart || !tp.backOff(&backoff) {
				return
			}
			continue
		}
		pull.done(0)
		tp.event(corev1.EventTypeNormal, "Started", "Started container "+tp.container.Name)
		tp.setRunning(w)
		e, ok := w.(exiter)
		if !ok {
			return
		}
		started := time.Now()
		var code int
		select {
		case <-ctx.Done():
			// stop took the workload, unless it came before setRunning
			tp.mu.Lock()
			mine := tp.w == w
			if mine {
				tp.w = nil
			}
			tp.mu.Unlock()
			if mine {
				w.stop()
			}
			return
		case code = <-e.wait():
		}
		tp.mu.Lock()
		if tp.w == w {
			tp.w = nil
		}
		tp.mu.Unlock()
		w.stop()
		reason := "Completed"
		if code != 0 {
			reason = "Error"
		}
		restart := tp.restarts(code)
		tp.setTerminated(code, reason, "", restart)
		if !restart {
			return
		}
		// the kubelet forgets the back-off of a container that ran for 10 minutes
		if time.Since(started) > 10*time.Minute {
			backoff = backoffStart
		}
		if !tp.backOff(&backoff) {
			return
		}
	}
}

// restarts tells whether the restart policy starts the container again
// after it exited with code.
func (tp *tabPod) restarts(code int) bool {
	tp.mu.Lock()
	terminating := tp.terminating
	tp.mu.Unlock()
	if terminating {
		return false
	}
	switch tp.restartPolicy {
	case corev1.RestartPolicyNever:
		return false
	case corev1.RestartPolicyOnFailure:
		return code != 0
	}
	return true
}

// backOff waits before the container starts again, as CrashLoopBackOff. It
// returns false if the pod is gone meanwhile.
func (tp *tabPod) backOff(backoff *time.Duration) bool {
	msg := fmt.Sprintf("back-off %s restarting failed container=%s pod=%s_%s(%s)", *backoff, tp.container.Name, tp.name, tp.namespace, tp.uid)
	tp.event(corev1.EventTypeWarning, "BackOff", "Back-off restarting failed container "+tp.container.Name+" in pod "+tp.name+"_"+tp.namespace+"("+string(tp.uid)+")")
	tp.setWaiting("CrashLoopBackOff", msg)
	select {
	case <-tp.ctx.Done():
		return false
	case <-time.After(*backoff):
	}
	*backoff = min(*backoff*2, backoffMax)
	return true
}

// errNeverPull is the error of a container whose image is not present, with
// the pull policy Never.
type errNeverPull struct{ error }

// imagePull tells the events of a pod what the kubelet would say about an
// image pull, for a workload that downloads what it runs: the model of
// webllm comes from Hugging Face, or from the browser cache.
type imagePull struct {
	tp      *tabPod
	t0      time.Time
	mu      sync.Mutex
	pulling bool // the download started
	pulled  bool // the download is done, the container is created
}

// fetching says that the workload downloads, not that it reads its cache.
func (p *imagePull) fetching() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pulling || p.pulled {
		return
	}
	p.pulling = true
	p.tp.event(corev1.EventTypeNormal, "Pulling", fmt.Sprintf("Pulling image %q", p.tp.container.Image))
}

// done says that the download is complete, with its size if known. Without
// a download, the image was already there.
func (p *imagePull) done(size int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pulled {
		return
	}
	p.pulled = true
	image := p.tp.container.Image
	if p.pulling {
		d := time.Since(p.t0).Truncate(time.Millisecond)
		p.tp.event(corev1.EventTypeNormal, "Pulled", fmt.Sprintf("Successfully pulled image %q in %v (%v including waiting). Image size: %d bytes.", image, d, d, size))
	} else {
		p.tp.event(corev1.EventTypeNormal, "Pulled", fmt.Sprintf("Container image %q already present on machine and can be accessed by the pod", image))
	}
	p.tp.event(corev1.EventTypeNormal, "Created", "Created container: "+p.tp.container.Name)
}

// wasPulled reports whether the download was done.
func (p *imagePull) wasPulled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pulled
}

// minimumGracePeriod is the shortest time that the kubelet gives a
// container to stop (minimumGracePeriodInSeconds in pkg/kubelet/kuberuntime).
const minimumGracePeriod = 2 * time.Second

// killWait is how long terminate waits for the exit after SIGKILL before it
// ends the VM.
var killWait = 5 * time.Second

// terminate stops the container of a deleted pod as the kubelet does
// (killContainer in pkg/kubelet/kuberuntime, StopContainer of containerd's
// CRI): the stop signal, up to grace for the exit, then SIGKILL. The exit
// code is the one of the process, so a pod that handles SIGTERM ends with
// 0, one that does not with 143. It runs once; it reports whether this call
// did the work.
func (tp *tabPod) terminate(grace time.Duration) (first bool) {
	tp.termOnce.Do(func() {
		first = true
		tp.kill(max(grace, minimumGracePeriod))
		// the emptyDir volumes go with the pod, as on a kubelet
		go tp.removeEmptyDirs()
	})
	return first
}

func (tp *tabPod) kill(grace time.Duration) {
	tp.mu.Lock()
	tp.terminating = true
	w := tp.w
	tp.mu.Unlock()
	if w == nil {
		tp.stop("", "")
		return
	}
	tp.event(corev1.EventTypeNormal, "Killing", "Stopping container "+tp.container.Name)
	if s, ok := w.(signaler); ok {
		ctx := context.Background()
		exited := func(d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-tp.runDone: // run saw the exit and recorded it
				return true
			case <-t.C:
				return false
			}
		}
		if err := s.signal(ctx, tp.stopSignal(s)); err == nil && exited(grace) {
			tp.stop("", "")
			return
		}
		if err := s.signal(ctx, v86.SIGKILL); err == nil {
			exited(killWait)
		}
	}
	tp.stop("", "")
}

// stopSignal is the signal that stops the container: lifecycle.stopSignal
// of the container, else the STOPSIGNAL of the image, else SIGTERM, as
// containerd's CRI picks it.
func (tp *tabPod) stopSignal(s signaler) int {
	name := s.stopSignal()
	if l := tp.container.Lifecycle; l != nil && l.StopSignal != nil {
		name = string(*l.StopSignal)
	}
	if name != "" {
		if n, err := v86.ParseSignal(name); err == nil {
			return int(n)
		}
		fmt.Fprintf(os.Stderr, "tab node: pod %s/%s: invalid stop signal %q, using SIGTERM\n", tp.namespace, tp.name, name)
	}
	return v86.SIGTERM
}

// stop ends the workload. With a reason, it tells the pod's events.
func (tp *tabPod) stop(reason, msg string) {
	tp.cancel()
	tp.mu.Lock()
	w := tp.w
	logs := tp.logs
	tp.w = nil
	tp.mu.Unlock()
	if w != nil {
		if reason != "" {
			tp.event(corev1.EventTypeNormal, reason, msg)
		}
		w.stop()
	}
	logs.finish()
}

func (tp *tabPod) running() workload {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return tp.w
}

func (tp *tabPod) setWaiting(reason, msg string) {
	tp.mu.Lock()
	tp.status.State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: msg}}
	tp.status.Ready, tp.status.Started = false, ptr.To(false)
	tp.mu.Unlock()
	tp.update()
}

// setTerminated records that the container ended. Unless it restarts, the
// pod is done.
func (tp *tabPod) setTerminated(code int, reason, msg string, restart bool) {
	tp.mu.Lock()
	logs := tp.logs
	now := metav1.Now()
	t := &corev1.ContainerStateTerminated{ExitCode: int32(code), Reason: reason, Message: msg, StartedAt: now, FinishedAt: now}
	if r := tp.status.State.Running; r != nil {
		t.StartedAt = r.StartedAt
	}
	tp.status.State = corev1.ContainerState{Terminated: t}
	tp.status.Ready, tp.status.Started = false, ptr.To(false)
	switch {
	case restart:
		tp.status.LastTerminationState = corev1.ContainerState{Terminated: t}
		tp.status.RestartCount++
	case code == 0:
		tp.phase = corev1.PodSucceeded
	default:
		tp.phase = corev1.PodFailed
	}
	tp.mu.Unlock()
	logs.finish()
	tp.update()
}

func (tp *tabPod) setRunning(w workload) {
	tp.mu.Lock()
	tp.w = w
	tp.phase = corev1.PodRunning
	tp.status.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}
	tp.status.Ready, tp.status.Started = true, ptr.To(true)
	tp.status.ImageID = w.imageID()
	tp.status.ContainerID = w.runtime() + "://" + strings.ReplaceAll(string(tp.uid), "-", "")
	tp.mu.Unlock()
	tp.update()
}

// update writes the status of the pod.
func (tp *tabPod) update() {
	tp.push.Lock()
	defer tp.push.Unlock()
	pods := tp.n.cs.CoreV1().Pods(tp.namespace)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pod, err := pods.Get(context.Background(), tp.name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if pod.UID != tp.uid {
			return nil
		}
		tp.mu.Lock()
		cs := *tp.status.DeepCopy()
		phase, reason, message := tp.phase, tp.reason, tp.message
		tp.mu.Unlock()

		s := &pod.Status
		s.Phase, s.Reason, s.Message = phase, reason, message
		s.HostIP, s.HostIPs = tp.n.ip, []corev1.HostIP{{IP: tp.n.ip}}
		s.StartTime = &tp.started
		// a pod the tab rejected or cannot run never got a network
		if phase != corev1.PodFailed || reason == "" {
			s.PodIP, s.PodIPs = tp.ip, []corev1.PodIP{{IP: tp.ip}}
			s.ContainerStatuses = []corev1.ContainerStatus{cs}
		}
		ready := corev1.ConditionFalse
		if cs.Ready {
			ready = corev1.ConditionTrue
		}
		for _, c := range []struct {
			t corev1.PodConditionType
			s corev1.ConditionStatus
		}{
			{corev1.PodReadyToStartContainers, corev1.ConditionTrue},
			{corev1.PodInitialized, corev1.ConditionTrue},
			{corev1.ContainersReady, ready},
			{corev1.PodReady, ready},
			{corev1.PodScheduled, corev1.ConditionTrue},
		} {
			setPodCondition(s, c.t, c.s)
		}
		_, err = pods.UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
		return err
	})
	if err != nil && !apierrors.IsNotFound(err) {
		fmt.Fprintln(os.Stderr, "tab node: pod status:", err)
	}
}

func setPodCondition(s *corev1.PodStatus, t corev1.PodConditionType, status corev1.ConditionStatus) {
	for i := range s.Conditions {
		if s.Conditions[i].Type == t {
			if s.Conditions[i].Status != status {
				s.Conditions[i].Status = status
				s.Conditions[i].LastTransitionTime = metav1.Now()
			}
			return
		}
	}
	s.Conditions = append(s.Conditions, corev1.PodCondition{Type: t, Status: status, LastTransitionTime: metav1.Now()})
}
