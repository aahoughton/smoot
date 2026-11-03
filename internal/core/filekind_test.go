package core

import "testing"

func TestFileKind(t *testing.T) {
	cases := []struct{ name, want string }{
		{"fix.patch", "patch"},
		{"CHANGES.DIFF", "patch"},
		{"notes.md", "markdown"},
		{"README.markdown", "markdown"},
		{"shot.png", "image"},
		{"photo.JPEG", "image"},
		{"bench.csv", "file"},
		{"Makefile", "file"},
		{".bashrc", "file"},
		{"archive.tar.gz", "file"},
		{"", "file"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FileKind(c.name); got != c.want {
				t.Fatalf("FileKind(%q) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}
