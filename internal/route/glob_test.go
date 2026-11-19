package route

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		pattern, file string
		want          bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "internal/route/glob.go", true},
		{"**/*.go", "internal/route/glob.go.txt", false},
		{"**/go.mod", "go.mod", true},
		{"**/tsconfig*.json", "web/tsconfig.app.json", true},
		{"project.yml", "project.yml", true},
		{"project.yml", "ios/project.yml", false},
		{"*.go", "a/b.go", false},
		{"a/**/b", "a/b", true},
		{"a/**/b", "a/x/y/b", true},
		{"[", "[", false},
	}
	for _, c := range cases {
		if got := match(c.pattern, c.file); got != c.want {
			t.Errorf("match(%q, %q) = %v, want %v", c.pattern, c.file, got, c.want)
		}
	}
	if anyMatch([]string{"**/*.rs"}, []string{"a.go", "b.md"}) || !anyMatch([]string{"**/*.rs"}, []string{"a.go", "src/b.rs"}) {
		t.Error("anyMatch")
	}
}
