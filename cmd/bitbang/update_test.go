package main

import (
	"strings"
	"testing"
)

func TestIsNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
		why             string
	}{
		{"0.4.8", "0.4.7", true, "patch bump"},
		{"0.5.0", "0.4.7", true, "minor bump"},
		{"1.0.0", "0.9.9", true, "major bump"},
		{"0.4.7", "0.4.7", false, "same version"},
		{"0.4.6", "0.4.7", false, "server behind us"},
		{"0.4.10", "0.4.9", true, "numeric, not lexical -- 10 > 9"},
		{"0.10.0", "0.9.0", true, "numeric minor"},
		{"v0.4.8", "0.4.7", true, "leading v tolerated"},
		{"0.4.8", "v0.4.7", true, "leading v on ours too"},
		{"0.5", "0.4.7", true, "missing patch reads as zero"},

		// The case that decides whether every dev build nags forever.
		{"0.5.0", "0.5.0-dev", true, "released beats our pre-release of it"},
		{"0.4.7", "0.5.0-dev", false, "our dev build is ahead of the last release"},
		{"0.5.0-rc1", "0.5.0-dev", false, "no notice between two pre-releases"},

		{"", "0.4.7", false, "nothing reported"},
		{"garbage", "0.4.7", false, "unparseable remote stays quiet"},
		{"0.4.8", "garbage", false, "unparseable local stays quiet"},
		{"0.4.8.1", "0.4.7", false, "four components is not a version we know"},
	}
	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			if got := isNewer(c.latest, c.current); got != c.want {
				t.Errorf("isNewer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
			}
		})
	}
}

func TestUpdateNotice(t *testing.T) {
	t.Run("names both versions", func(t *testing.T) {
		got := updateNotice(map[string]string{"cli": "0.4.8"}, "0.4.7")
		if got == "" {
			t.Fatal("no notice for a newer release")
		}
		for _, want := range []string{"0.4.8", "0.4.7"} {
			if !strings.Contains(got, want) {
				t.Errorf("notice %q omits %q", got, want)
			}
		}
	})

	t.Run("silent when current", func(t *testing.T) {
		if got := updateNotice(map[string]string{"cli": "0.4.7"}, "0.4.7"); got != "" {
			t.Errorf("got %q", got)
		}
	})

	// A server that tracks other products but not this one, or tracks
	// nothing at all, must not produce a notice.
	t.Run("no row for us", func(t *testing.T) {
		if got := updateNotice(map[string]string{"octoprint": "9.9.9"}, "0.4.7"); got != "" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("nil table", func(t *testing.T) {
		if got := updateNotice(nil, "0.4.7"); got != "" {
			t.Errorf("got %q", got)
		}
	})
}

func TestReportUpdate(t *testing.T) {
	t.Run("names the server it talked to", func(t *testing.T) {
		var out strings.Builder
		reportUpdate(&out, map[string]string{"cli": "99.0.0"}, "signal.example.com")
		got := out.String()
		if !strings.Contains(got, "99.0.0") {
			t.Errorf("output %q omits the release", got)
		}
		// A self-hoster's install endpoint ships the binary they built,
		// so the notice must not point at bitba.ng.
		if !strings.Contains(got, "https://signal.example.com/install") {
			t.Errorf("output %q does not point at the server we registered with", got)
		}
	})

	// Nothing at all, not a blank line: `bitbang cp <url>:/file -` writes
	// the file to stdout and the caller may be watching stderr.
	t.Run("writes nothing when there is nothing to say", func(t *testing.T) {
		var out strings.Builder
		reportUpdate(&out, map[string]string{"cli": "0.0.1"}, "bitba.ng")
		reportUpdate(&out, nil, "bitba.ng")
		if got := out.String(); got != "" {
			t.Errorf("got %q, want no output", got)
		}
	})
}
