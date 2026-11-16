package main

import (
	"context"
	"fmt"
	"os"
	"syscall/js"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// The emptyDir volumes of the pods of the tab node. Each one is a Docker
// volume of the browser (web/src/volumes.js) named after the pod and the
// volume: the VM worker mounts it like docker run -v, its files stay in
// OPFS while the pod lives, also across restarts of the container, and
// they go when the pod is deleted. A medium of Memory is kept the same way:
// a VM has no memory to spare for it. Other kinds of volumes (ConfigMap,
// Secret, hostPath, PVC, ...) stay unsupported.

// tabMount is a volume mount of the container.
type tabMount struct {
	volume, path string // the volume of the browser, the mount path
	readOnly     bool
}

// emptyDirVolume is the name of the volume of the browser for an emptyDir.
func emptyDirVolume(uid types.UID, name string) string {
	return fmt.Sprintf("pod-%s-%s", uid, name)
}

// emptyDirMounts returns the mounts of the emptyDir volumes of a container
// (emptyDir: the names of those volumes). A mount with a subPath is not
// supported: it goes to ignored.
func emptyDirMounts(uid types.UID, mounts []corev1.VolumeMount, emptyDir map[string]bool, ignored *[]string) []tabMount {
	var out []tabMount
	for _, m := range mounts {
		if !emptyDir[m.Name] {
			continue
		}
		if m.SubPath != "" || m.SubPathExpr != "" {
			*ignored = append(*ignored, "the subPath of volumeMount "+m.Name)
			continue
		}
		out = append(out, tabMount{volume: emptyDirVolume(uid, m.Name), path: m.MountPath, readOnly: m.ReadOnly})
	}
	return out
}

// mountSpecs is opts.mounts of the start of the VM (vm.js, volumes.js).
func (tp *tabPod) mountSpecs() []any {
	var out []any
	for _, m := range tp.emptyDirs {
		// an emptyDir starts empty: nothing of the image is copied
		out = append(out, map[string]any{"type": "volume", "name": m.volume, "destination": m.path, "readonly": m.readOnly, "copy": false})
	}
	return out
}

// removeEmptyDirs deletes the files of the emptyDir volumes of a pod that
// is gone. vm.js waits until no VM uses a volume.
func (tp *tabPod) removeEmptyDirs() {
	if len(tp.emptyDirs) == 0 {
		return
	}
	vm := tp.n.host.Get("vm")
	if vm.Type() != js.TypeObject || vm.Get("removeVolume").Type() != js.TypeFunction {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, m := range tp.emptyDirs {
		if _, err := await(ctx, vm.Call("removeVolume", m.volume)); err != nil {
			fmt.Fprintf(os.Stderr, "tab node: remove the emptyDir %s: %v\n", m.volume, err)
		}
	}
}
