package signaling

import "strings"

// ParseFragment splits the access code and comma-separated flags used by the
// browser URL grammar. An optional embedded device URL begins at the first
// slash after the flag opener.
func ParseFragment(fragment string) (code string, flags []string) {
	i := 0
	for i < len(fragment) && isCodeByte(fragment[i]) {
		i++
	}
	code = fragment[:i]
	if i >= len(fragment) || fragment[i] != '!' {
		return code, nil
	}

	end := i + 1
	for end < len(fragment) && fragment[end] != '/' {
		end++
	}
	for _, flag := range strings.Split(fragment[i+1:end], ",") {
		if flag != "" {
			flags = append(flags, flag)
		}
	}
	return code, flags
}

func isCodeByte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' ||
		b >= '0' && b <= '9' || b == '_' || b == '-'
}

// HasFlag reports whether want is present, ignoring an optional value.
func HasFlag(flags []string, want string) bool {
	for _, flag := range flags {
		if name, _, _ := strings.Cut(flag, "="); name == want {
			return true
		}
	}
	return false
}
