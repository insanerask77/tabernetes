package main

import (
	"reflect"
	"testing"
)

func TestDockerFlags(t *testing.T) {
	for _, tc := range []struct {
		fs    dockerFlagSet
		args  []string
		flags map[string]string
		rest  []string
		err   string
	}{
		{fs: dockerStopFlags, args: []string{"a", "b"}, flags: map[string]string{}, rest: []string{"a", "b"}},
		{fs: dockerStopFlags, args: []string{"-t", "2", "a"}, flags: map[string]string{"timeout": "2"}, rest: []string{"a"}},
		{fs: dockerStopFlags, args: []string{"a", "-t2", "-s=HUP"}, flags: map[string]string{"timeout": "2", "signal": "HUP"}, rest: []string{"a"}},
		{fs: dockerStopFlags, args: []string{"--time=5", "--signal", "QUIT", "a"}, flags: map[string]string{"timeout": "5", "signal": "QUIT"}, rest: []string{"a"}},
		{fs: dockerStopFlags, args: []string{"--timeout", "-1", "--", "-a"}, flags: map[string]string{"timeout": "-1"}, rest: []string{"-a"}},
		{fs: dockerStopFlags, args: []string{"-t"}, err: "flag needs an argument: 't' in -t"},
		{fs: dockerStopFlags, args: []string{"--timeout"}, err: "flag needs an argument: --timeout"},
		{fs: dockerStopFlags, args: []string{"-f", "a"}, err: "unknown shorthand flag: 'f' in -f"},
		{fs: dockerKillFlags, args: []string{"-s", "HUP", "web"}, flags: map[string]string{"signal": "HUP"}, rest: []string{"web"}},
		{fs: dockerKillFlags, args: []string{"--force", "web"}, err: "unknown flag: --force"},
		{fs: dockerRMFlags, args: []string{"-fv", "a", "b"}, flags: map[string]string{"force": "true", "volumes": "true"}, rest: []string{"a", "b"}},
		{fs: dockerRMFlags, args: []string{"--force=false", "a"}, flags: map[string]string{"force": "false"}, rest: []string{"a"}},
		{fs: dockerRMFlags, args: []string{"--force=maybe", "a"}, err: `invalid argument "maybe" for "--force" flag`},
	} {
		flags, rest, err := tc.fs.parse(tc.args)
		if tc.err != "" {
			if err == nil || err.Error() != tc.err {
				t.Errorf("%q: error %v; want %q", tc.args, err, tc.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(flags, tc.flags) || !reflect.DeepEqual(rest, tc.rest) {
			t.Errorf("%q: %v %q %v; want %v %q", tc.args, flags, rest, err, tc.flags, tc.rest)
		}
	}
}
