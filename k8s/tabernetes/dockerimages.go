package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"syscall/js"

	"github.com/containerd/containerd/v2/core/runtime/v2/v86/dockerapi"
)

// dockerImageStore gives the Docker API the images of the tab
// (dockerapi.ImageStore): the VM worker pulls them into the image store of
// the browser (web/src/layers.js), which the pods of the tab node use too.
// So docker images lists the images of the pods as well, as on a real node
// where Docker and the kubelet share containerd. vm.js runs each request in
// a worker of its own, without a VM.
type dockerImageStore struct{}

func (dockerImageStore) vm() (js.Value, error) {
	vm := dockerVM()
	if vm.Type() != js.TypeObject || vm.Get("images").Type() != js.TypeFunction {
		return js.Value{}, errors.New("this tab cannot run container images")
	}
	return vm, nil
}

func (s dockerImageStore) List(ctx context.Context) ([]dockerapi.Image, error) {
	vm, err := s.vm()
	if err != nil {
		return nil, err
	}
	v, err := await(ctx, vm.Call("images"))
	if err != nil {
		return nil, err
	}
	var imgs []dockerapi.Image
	if err := json.Unmarshal([]byte(v.String()), &imgs); err != nil {
		return nil, err
	}
	return imgs, nil
}

func (s dockerImageStore) Pull(ctx context.Context, ref string, progress func(dockerapi.PullProgress)) (dockerapi.Image, bool, error) {
	vm, err := s.vm()
	if err != nil {
		return dockerapi.Image{}, false, err
	}
	// The callback runs in the event loop of JavaScript: it must not wait
	// for the client, so the steps go through a channel.
	steps := make(chan dockerapi.PullProgress, 1024)
	var mu sync.Mutex
	closed := false
	done := make(chan struct{})
	go func() {
		defer close(done)
		for p := range steps {
			progress(p)
		}
	}()
	on := js.FuncOf(func(_ js.Value, a []js.Value) any {
		m := a[0]
		p := dockerapi.PullProgress{Layer: m.Get("layer").String(), Status: m.Get("status").String()}
		if c := m.Get("current"); c.Type() == js.TypeNumber {
			p.Current = int64(c.Float())
		}
		if t := m.Get("total"); t.Type() == js.TypeNumber {
			p.Total = int64(t.Float())
		}
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return nil
		}
		select {
		case steps <- p:
		default: // a slow client misses steps, never the end
		}
		return nil
	})
	v, err := await(ctx, vm.Call("pull", ref, on))
	mu.Lock()
	closed = true
	close(steps)
	mu.Unlock()
	<-done
	if ctx.Err() == nil {
		on.Release() // else the pull runs on and may still call it
	}
	if err != nil {
		return dockerapi.Image{}, false, err
	}
	var res struct {
		Image      dockerapi.Image `json:"image"`
		Downloaded bool            `json:"downloaded"`
	}
	if err := json.Unmarshal([]byte(v.String()), &res); err != nil {
		return dockerapi.Image{}, false, err
	}
	return res.Image, res.Downloaded, nil
}

func (s dockerImageStore) Remove(ctx context.Context, name string) ([]string, error) {
	vm, err := s.vm()
	if err != nil {
		return nil, err
	}
	v, err := await(ctx, vm.Call("removeImage", name))
	if err != nil {
		return nil, err
	}
	var deleted []string
	if err := json.Unmarshal([]byte(v.String()), &deleted); err != nil {
		return nil, err
	}
	return deleted, nil
}

// Tag gives the image of the reference source the name target too, for
// docker tag (dockerapi.ImageTagger).
func (s dockerImageStore) Tag(ctx context.Context, source, target string) error {
	vm, err := s.vm()
	if err != nil {
		return err
	}
	_, err = await(ctx, vm.Call("tagImage", source, target))
	return err
}

// PruneLayers deletes the layers that no reference uses, for docker image
// prune (dockerapi.LayerPruner).
func (s dockerImageStore) PruneLayers(ctx context.Context) ([]string, int64, error) {
	vm, err := s.vm()
	if err != nil {
		return nil, 0, err
	}
	v, err := await(ctx, vm.Call("pruneLayers"))
	if err != nil {
		return nil, 0, err
	}
	var res struct {
		Deleted   []string `json:"deleted"`
		Reclaimed int64    `json:"reclaimed"`
	}
	if err := json.Unmarshal([]byte(v.String()), &res); err != nil {
		return nil, 0, err
	}
	return res.Deleted, res.Reclaimed, nil
}
