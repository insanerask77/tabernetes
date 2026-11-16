package main

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestEmptyDirMounts(t *testing.T) {
	var ignored []string
	got := emptyDirMounts("u1", []corev1.VolumeMount{
		{Name: "cache", MountPath: "/cache"},
		{Name: "cache", MountPath: "/ro", ReadOnly: true},
		{Name: "cache", MountPath: "/sub", SubPath: "x"},
		{Name: "config", MountPath: "/etc/app"},
	}, map[string]bool{"cache": true}, &ignored)
	want := []tabMount{{volume: "pod-u1-cache", path: "/cache"}, {volume: "pod-u1-cache", path: "/ro", readOnly: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mounts = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(ignored, []string{"the subPath of volumeMount cache"}) {
		t.Errorf("ignored = %q", ignored)
	}
	tp := &tabPod{emptyDirs: got}
	specs := tp.mountSpecs()
	if len(specs) != 2 || specs[0].(map[string]any)["copy"] != false || specs[1].(map[string]any)["readonly"] != true {
		t.Errorf("specs = %v", specs)
	}
}
