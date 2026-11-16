package main

import (
	"strings"
	"syscall/js"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestTabVMs: the pods of a tab node and its Docker containers share the 8
// places of the tab. docker run fails when they are taken, and the node
// shows the places of the containers as allocatable pods less.
func TestTabVMs(t *testing.T) {
	host := js.Global().Get("Object").New()
	vm := js.Global().Get("Object").New()
	host.Set("vm", vm)
	n := &tabNode{name: "tab-9", host: host, pods: map[types.UID]*tabPod{}, kick: make(chan struct{}, 1)}
	s := &session{name: "tab-9", host: host, node: n}
	sessionsMu.Lock()
	sessions[s.name] = s
	sessionsMu.Unlock()
	t.Cleanup(func() {
		sessionsMu.Lock()
		delete(sessions, s.name)
		sessionsMu.Unlock()
	})

	// 5 pods, one of them done: 4 places
	for i, phase := range []corev1.PodPhase{corev1.PodRunning, corev1.PodRunning, corev1.PodPending, corev1.PodRunning, corev1.PodSucceeded} {
		n.pods[types.UID(rune('a'+i))] = &tabPod{phase: phase}
	}
	var releases []func()
	for range 4 {
		release, err := admitContainer(vm)
		if err != nil {
			t.Fatalf("container %d: %v", len(releases)+1, err)
		}
		releases = append(releases, release)
	}
	if _, err := admitContainer(vm); err == nil || !strings.Contains(err.Error(), "Node didn't have enough resource: pods, requested: 1, used: 8, capacity: 8") {
		t.Fatalf("a 9th VM: %v; want the kubelet's message", err)
	}
	if got := n.allocatablePods(); got != 4 {
		t.Errorf("allocatable pods %d; want 4 (8 less 4 containers)", got)
	}
	if got := n.node().Status.Allocatable[corev1.ResourcePods]; got.Value() != 4 {
		t.Errorf("node allocatable pods %s; want 4", got.String())
	}
	if got := n.node().Status.Capacity[corev1.ResourcePods]; got.Value() != 8 {
		t.Errorf("node capacity pods %s; want 8", got.String())
	}
	select {
	case <-n.kick:
	default:
		t.Error("no status update for the new allocatable")
	}
	releases[0]()
	if got := n.allocatablePods(); got != 5 {
		t.Errorf("allocatable pods after a container ended %d; want 5", got)
	}
	if _, err := admitContainer(vm); err != nil {
		t.Errorf("a container after one ended: %v", err)
	}

	// a closed tab runs nothing
	other := js.Global().Get("Object").New()
	if _, err := admitContainer(other); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Errorf("the vm of a closed tab: %v; want an error", err)
	}
}
