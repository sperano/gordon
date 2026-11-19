package route

import (
	"path"
	"strings"
)

// globstar is the pattern segment that matches any number of directories.
const globstar = "**"

// anyMatch reports whether any file matches any pattern.
func anyMatch(patterns, files []string) bool {
	for _, f := range files {
		for _, p := range patterns {
			if match(p, f) {
				return true
			}
		}
	}
	return false
}

// match applies a Kelos file pattern: `*` and `?` stay within one path
// segment and `**` spans zero or more directories. Kelos's `{a,b}`
// alternation is not supported; no configured pattern uses it.
func match(pattern, file string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(file, "/"))
}

func matchSegments(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == globstar {
		for i := 0; i <= len(name); i++ {
			if matchSegments(pat[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], name[0])
	return err == nil && ok && matchSegments(pat[1:], name[1:])
}
