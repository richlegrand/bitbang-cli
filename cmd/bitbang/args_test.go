package main

import (
	"flag"
	"reflect"
	"testing"
)

func TestReorderArgs(t *testing.T) {
	// Build a FlagSet with a representative mix of value-flags and
	// bool-flags so the reordering logic exercises both branches.
	mkFS := func() *flag.FlagSet {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fs.String("pin", "", "")
		fs.String("server", "", "")
		fs.Bool("v", false, "")
		fs.Bool("ephemeral", false, "")
		return fs
	}

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "flags first (unchanged)",
			in:   []string{"--pin", "1234", "URL"},
			want: []string{"--pin", "1234", "URL"},
		},
		{
			name: "value-flag after positional",
			in:   []string{"URL", "--pin", "1234"},
			want: []string{"--pin", "1234", "URL"},
		},
		{
			name: "bool-flag after positional",
			in:   []string{"URL", "-v"},
			want: []string{"-v", "URL"},
		},
		{
			name: "embedded = value",
			in:   []string{"URL", "--pin=1234"},
			want: []string{"--pin=1234", "URL"},
		},
		{
			name: "mixed interleaved",
			in:   []string{"src", "--pin", "1234", "dst", "-v"},
			want: []string{"--pin", "1234", "-v", "src", "dst"},
		},
		{
			name: "double-dash terminator preserved",
			in:   []string{"URL", "--pin", "1234", "--", "ls", "-l"},
			want: []string{"--pin", "1234", "URL", "--", "ls", "-l"},
		},
		{
			name: "bare dash is positional, not a flag",
			in:   []string{"-", "--pin", "1234"},
			want: []string{"--pin", "1234", "-"},
		},
		{
			name: "no positionals at all",
			in:   []string{"--pin", "1234", "-v"},
			want: []string{"--pin", "1234", "-v"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reorderArgs(mkFS(), tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("reorderArgs(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
