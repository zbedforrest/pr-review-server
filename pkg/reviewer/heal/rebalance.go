package heal

import "strings"

// Rebalance fixes mis-nested and missing brackets outside strings: a closer
// that does not match the innermost open bracket first closes the brackets
// opened inside it, a closer with nothing open is dropped, a "key": pair
// sitting directly in an array gets the opening brace it lacks, and at the end
// an open string is closed and every open bracket gets its closer. It never
// touches characters inside strings.
func Rebalance(s string) string {
	var out strings.Builder
	out.Grow(len(s) + 8)
	var stack []byte
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			out.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			// Inside an array, a "key": pair can only belong to an object whose
			// opening brace the model left out; supply it.
			if len(stack) > 0 && stack[len(stack)-1] == '[' && startsKey(s, i) {
				stack = append(stack, '{')
				out.WriteByte('{')
			}
			inString = true
			out.WriteByte(c)
		case '{', '[':
			stack = append(stack, c)
			out.WriteByte(c)
		case '}', ']':
			if len(stack) == 0 {
				continue
			}
			opener := byte('{')
			if c == ']' {
				opener = '['
			}
			if stack[len(stack)-1] != opener && contains(stack, opener) {
				for stack[len(stack)-1] != opener {
					out.WriteByte(closer(stack[len(stack)-1]))
					stack = stack[:len(stack)-1]
				}
			}
			out.WriteByte(closer(stack[len(stack)-1]))
			stack = stack[:len(stack)-1]
		default:
			out.WriteByte(c)
		}
	}
	if inString {
		out.WriteByte('"')
	}
	for i := len(stack) - 1; i >= 0; i-- {
		out.WriteByte(closer(stack[i]))
	}
	return out.String()
}

func closer(open byte) byte {
	if open == '[' {
		return ']'
	}
	return '}'
}

func contains(stack []byte, b byte) bool {
	for _, s := range stack {
		if s == b {
			return true
		}
	}
	return false
}

// startsKey reports whether the string opening at s[i] is followed by a colon,
// that is, whether it is an object key.
func startsKey(s string, i int) bool {
	escaped := false
	for j := i + 1; j < len(s); j++ {
		switch {
		case escaped:
			escaped = false
		case s[j] == '\\':
			escaped = true
		case s[j] == '"':
			for k := j + 1; k < len(s); k++ {
				switch s[k] {
				case ' ', '\t', '\n', '\r':
					continue
				case ':':
					return true
				}
				return false
			}
			return false
		}
	}
	return false
}
