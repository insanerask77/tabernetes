package main

import (
	"strings"
	"testing"
)

// The modules.txt of a Kubernetes checkout: no blank lines between blocks, a
// "## workspace" line first and local replaces of the staging modules.
const testModules = `## workspace
# github.com/containerd/ttrpc v1.2.7
## explicit; go 1.19
github.com/containerd/ttrpc
# github.com/go-logr/logr v1.4.3
## explicit; go 1.18
github.com/go-logr/logr
github.com/go-logr/logr/funcr
# k8s.io/api v0.0.0 => ./staging/src/k8s.io/api
## explicit; go 1.26.0
k8s.io/api/core/v1
# mvdan.cc/sh/v3 v3.14.1
## explicit; go 1.23
mvdan.cc/sh/v3/syntax
`

func TestSetModuleReplacesOnlyItsBlock(t *testing.T) {
	got := setModule(testModules, vendorModuleEntry{
		path:      "github.com/containerd/ttrpc",
		version:   "v1.2.8",
		goVersion: "1.23",
		explicit:  true,
		pkgs:      []string{"github.com/containerd/ttrpc", "github.com/containerd/ttrpc/proto"},
	})
	want := strings.Replace(testModules,
		"# github.com/containerd/ttrpc v1.2.7\n## explicit; go 1.19\ngithub.com/containerd/ttrpc\n",
		"# github.com/containerd/ttrpc v1.2.8\n## explicit; go 1.23\ngithub.com/containerd/ttrpc\ngithub.com/containerd/ttrpc/proto\n", 1)
	if got != want {
		t.Errorf("setModule:\n%s\nwant:\n%s", got, want)
	}
}

func TestSetModuleInsertsSorted(t *testing.T) {
	for _, tc := range []struct{ path, before string }{
		{"github.com/containerd/containerd/v2", "# github.com/containerd/ttrpc "},
		{"github.com/opencontainers/image-spec", "# k8s.io/api "},
		{"sigs.k8s.io/yaml", ""}, // last
	} {
		got := setModule(testModules, vendorModuleEntry{path: tc.path, version: "v1.0.0", explicit: true, pkgs: []string{tc.path}})
		block := "# " + tc.path + " v1.0.0\n## explicit\n" + tc.path + "\n"
		if tc.before == "" {
			if !strings.HasSuffix(got, block) {
				t.Errorf("%s: not appended at the end:\n%s", tc.path, got)
			}
		} else if !strings.Contains(got, block+tc.before) {
			t.Errorf("%s: not inserted before %q:\n%s", tc.path, tc.before, got)
		}
		// Every other block is still there, unchanged.
		if strings.Replace(got, block, "", 1) != testModules {
			t.Errorf("%s: other blocks changed:\n%s", tc.path, got)
		}
	}
}

func TestParseModules(t *testing.T) {
	mods := parseModules(testModules)
	if len(mods) != 4 {
		t.Fatalf("got %d modules, want 4: %+v", len(mods), mods)
	}
	logr := mods[1]
	if logr.path != "github.com/go-logr/logr" || logr.version != "v1.4.3" || !logr.explicit || logr.goVersion != "1.18" || len(logr.pkgs) != 2 {
		t.Errorf("logr: %+v", logr)
	}
	api := mods[2]
	if api.path != "k8s.io/api" || api.version != "v0.0.0" || api.pkgs[0] != "k8s.io/api/core/v1" {
		t.Errorf("k8s.io/api: %+v", api)
	}
}
