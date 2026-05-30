package streamtype

import (
	"bytes"
	"strings"
	"testing"
)

// The test sink is *bytes.Buffer — not *os.File — so newLineMirror's
// auto-TTY detection leaves color off and the emitted output is the
// portable prefix+text form. Tests for the TTY/color branch live
// separately below.
const pre = "│ " // mirrorPrefix; kept in test for readability

func TestLineMirror_StripsANSIAndBuffersLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain line", in: "hello world\n", want: pre + "hello world\n"},
		{name: "color sequence stripped",
			in: "\x1b[31mred\x1b[0m text\n", want: pre + "red text\n"},
		{name: "cursor positioning stripped",
			in: "before\x1b[2;5Hafter\n", want: pre + "beforeafter\n"},
		{name: "OSC title sequence stripped",
			in: "\x1b]0;some title\x07prompt$ \n", want: pre + "prompt$ \n"},
		{name: "OSC terminated by ST",
			in: "\x1b]0;title\x1b\\done\n", want: pre + "done\n"},
		{name: "CRLF emits once per line",
			in: "first\r\nsecond\r\n", want: pre + "first\n" + pre + "second\n"},
		{name: "less status redraw deduped",
			in: "(END)\r(END)\r(END)\r(END)\r", want: pre + "(END)\n"},
		{name: "progress bar emits each step",
			in: "1%\r2%\r3%\r", want: pre + "1%\n" + pre + "2%\n" + pre + "3%\n"},
		{name: "vim tilde markers not deduped (use newlines)",
			in: "~\n~\n~\n", want: pre + "~\n" + pre + "~\n" + pre + "~\n"},
		{name: "newline breaks redraw dedup run",
			in: "(END)\rmore\n(END)\r", want: pre + "(END)\n" + pre + "more\n" + pre + "(END)\n"},
		{name: "bell + backspace dropped",
			in: "ding\x07dong\x08\n", want: pre + "dingdong\n"},
		{name: "tab kept",
			in: "col1\tcol2\n", want: pre + "col1\tcol2\n"},
		{name: "pure-escape line skipped (clear screen)",
			in: "\x1b[2J\x1b[H\n", want: ""},
		{name: "incomplete line not emitted",
			in: "no newline here yet", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sink bytes.Buffer
			m := newLineMirror(&sink)
			n, err := m.Write([]byte(tc.in))
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if n != len(tc.in) {
				t.Errorf("n = %d, want %d", n, len(tc.in))
			}
			if got := sink.String(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLineMirror_HoldsPartialLineUntilNewline(t *testing.T) {
	var sink bytes.Buffer
	m := newLineMirror(&sink)
	_, _ = m.Write([]byte("user@host:~$ "))
	if sink.Len() != 0 {
		t.Errorf("prompt emitted before newline: %q", sink.String())
	}
	_, _ = m.Write([]byte("ls /tmp\n"))
	if got, want := sink.String(), pre+"user@host:~$ ls /tmp\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLineMirror_LongAccumulation(t *testing.T) {
	var sink bytes.Buffer
	m := newLineMirror(&sink)
	for _, chunk := range []string{"\x1b[1mhe", "ll", "o\x1b[0m wo", "rld\n"} {
		_, _ = m.Write([]byte(chunk))
	}
	if !strings.HasSuffix(sink.String(), pre+"hello world\n") {
		t.Errorf("got %q, want suffix %q", sink.String(), pre+"hello world\n")
	}
}

// TestLineMirror_ColorModeWrapsDim verifies the TTY-output branch:
// each emitted line is wrapped with SGR-dim + reset. We force-flip
// the color flag instead of opening a real TTY (which is awkward in
// unit tests).
func TestLineMirror_ColorModeWrapsDim(t *testing.T) {
	var sink bytes.Buffer
	m := newLineMirror(&sink)
	m.color = true
	_, _ = m.Write([]byte("hi\n"))
	want := ansiDim + pre + "hi" + ansiReset + "\n"
	if got := sink.String(); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
