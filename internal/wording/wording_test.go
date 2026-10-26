package wording

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedWordingLoads(t *testing.T) {
	w, err := Load(embedded)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range PhraseKeys {
		if strings.TrimSpace(w.phrases[k].Root.String()) == "" {
			t.Errorf("%s is empty", k)
		}
	}
	if got := w.Instructions("http://h:1", "team", false); !strings.Contains(got, "--namespace team --url 'http://h:1'") {
		t.Fatalf("instructions recipe: %s", got)
	}
	on := w.Instructions("http://h:1", "team", true)
	if !strings.Contains(on, "smoot attach <conversation> <file> --agent <name> --namespace team --url 'http://h:1'") {
		t.Fatalf("attachments on: %s", on)
	}
	if got := w.Instructions("http://h:1", "team", false); strings.Contains(got, "attach") {
		t.Fatalf("attachments off: %s", got)
	}
	if got := w.Instructions("", "team", false); strings.Contains(got, "--url") {
		t.Fatalf("instructions with no URL mention --url: %s", got)
	}
}

func TestPhrasesRender(t *testing.T) {
	w := Default()
	if got := w.Phrase(ReadMore, map[string]any{"LastSeq": 12}); !strings.Contains(got, "last_seq is 12") {
		t.Fatalf("read_more = %q", got)
	}
	defer func() {
		if recover() == nil {
			t.Fatal("missing field did not panic")
		}
	}()
	w.Phrase(ReadMore, map[string]any{})
}

func files(instructions, tools, phrases string) fstest.MapFS {
	return fstest.MapFS{
		"text/instructions.md": {Data: []byte(instructions)},
		"text/tools.md":        {Data: []byte(tools)},
		"text/phrases.md":      {Data: []byte(phrases)},
	}
}

func allPhrases(skip string, extra string) string {
	var b strings.Builder
	for _, k := range PhraseKeys {
		if k != skip {
			b.WriteString("## " + k + "\ntext {{.X}}\n\n")
		}
	}
	return b.String() + extra
}

func TestLoadRejectsBadFiles(t *testing.T) {
	good := "## say\nPost.\n\n- body: the message\n"
	tests := []struct {
		name string
		fs   fstest.MapFS
		want string
	}{
		{"missing phrase", files("hi", good, allPhrases(ReadMore, "")), "missing read_more"},
		{"unknown phrase", files("hi", good, allPhrases("", "## surprise\nboo\n")), `unknown key "surprise"`},
		{"duplicate section", files("hi", good+"## say\nAgain.\n", allPhrases("", "")), "duplicate section"},
		{"bad key", files("hi", "## Say\nPost.\n", allPhrases("", "")), "bad section key"},
		{"no description", files("hi", "## say\n- body: x\n", allPhrases("", "")), "no description"},
		{"text after params", files("hi", "## say\nPost.\n- body: x\nmore\n", allPhrases("", "")), "text after the parameter list"},
		{"duplicate param", files("hi", "## say\nPost.\n- body: x\n- body: y\n", allPhrases("", "")), "duplicate parameter"},
		{"bad template", files("{{.Oops", good, allPhrases("", "")), "instructions.md"},
		{"missing file", fstest.MapFS{}, "instructions.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.fs)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
