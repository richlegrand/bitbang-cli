package main

import "testing"

func TestParseRemoteSpec(t *testing.T) {
	cases := []struct {
		in     string
		remote bool
		server string
		uid    string
		code   string
		path   string
	}{
		// Full URL with scheme, server, UID, code, path.
		{
			in:     "https://bitba.ng/abc123#XYZ:/foo/bar",
			remote: true, server: "bitba.ng", uid: "abc123", code: "XYZ", path: "/foo/bar",
		},
		// Same but no scheme — should default to https + same host.
		{
			in:     "bitba.ng/abc123#XYZ:/foo/bar",
			remote: true, server: "bitba.ng", uid: "abc123", code: "XYZ", path: "/foo/bar",
		},
		// Bare UID#CODE — should default to bitba.ng.
		{
			in:     "abc123#XYZ:/foo",
			remote: true, server: "bitba.ng", uid: "abc123", code: "XYZ", path: "/foo",
		},
		// No code — accepted by parser; auth will fail at dial time if
		// the listener has one.
		{
			in:     "https://bitba.ng/abc123:/foo",
			remote: true, server: "bitba.ng", uid: "abc123", code: "", path: "/foo",
		},
		// Non-default server, with port.
		{
			in:     "https://test.bitba.ng/abc#XYZ:/x",
			remote: true, server: "test.bitba.ng", uid: "abc", code: "XYZ", path: "/x",
		},
		// Local paths.
		{in: "./foo.txt", remote: false},
		{in: "/tmp/foo.txt", remote: false},
		{in: "foo.txt", remote: false},
		// Pipe sentinels — stdin/stdout shortcut.
		{in: "-", remote: false},
		// Looks like a remote spec but missing the path → not remote
		// (so cp doesn't silently misinterpret a local file with a
		// colon in the name).
		{in: "bitba.ng/abc#XYZ", remote: false},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseRemoteSpec(tc.in)
			if ok != tc.remote {
				t.Fatalf("remote=%v, want %v", ok, tc.remote)
			}
			if !ok {
				return
			}
			if got.Server != tc.server {
				t.Errorf("server=%q, want %q", got.Server, tc.server)
			}
			if got.UID != tc.uid {
				t.Errorf("uid=%q, want %q", got.UID, tc.uid)
			}
			if got.Code != tc.code {
				t.Errorf("code=%q, want %q", got.Code, tc.code)
			}
			if got.Path != tc.path {
				t.Errorf("path=%q, want %q", got.Path, tc.path)
			}
		})
	}
}
