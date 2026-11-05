package permission

import (
	"path/filepath"
	"testing"
)

func abs(parts ...string) string {
	return filepath.Join(append([]string{string(filepath.Separator) + "work"}, parts...)...)
}

func TestAllowsWrite(t *testing.T) {
	f := Files{Entries: []Entry{
		{Path: abs("out"), Access: Write},
		{Path: abs("tmp"), Access: Write},
	}}

	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"the entry itself", abs("out"), true},
		{"a child", abs("out", "a.txt"), true},
		{"a nested child", abs("out", "deep", "x", "y.txt"), true},
		{"a sibling", abs("other"), false},
		{"a prefix but not a parent", abs("outside"), false},
		{"outside the tree", "/etc/passwd", false},
		{"empty", "", false},
	} {
		if got := f.Allows(tc.path, Write); got != tc.want {
			t.Errorf("%s: Allows(%q, Write) = %v, want %v", tc.name, tc.path, got, tc.want)
		}
	}
}

func TestDenyWins(t *testing.T) {
	f := Files{Entries: []Entry{
		{Path: abs("out"), Access: Write},
		{Path: abs("out", "secret"), Access: Deny},
	}}
	if !f.Allows(abs("out", "ok.txt"), Write) {
		t.Error("a non-denied child must stay writable")
	}
	if f.Allows(abs("out", "secret", "key"), Write) {
		t.Error("deny must win over the enclosing write entry")
	}
	if f.Allows(abs("out", "secret"), Read) {
		t.Error("deny must also win for reads")
	}
}

func TestEmptyIsReadOnly(t *testing.T) {
	var f Files
	if !f.Allows("/etc/passwd", Read) {
		t.Error("no entries means reads are unrestricted")
	}
	if f.Allows(abs("out", "a.txt"), Write) {
		t.Error("no entries means writes are refused")
	}
	if f.HasReadRestrictions() {
		t.Error("an empty entry list constrains nothing")
	}
}

func TestReadEntriesRestrictReads(t *testing.T) {
	f := Files{Entries: []Entry{{Path: abs("docs"), Access: Read}}}
	if !f.Allows(abs("docs", "a.md"), Read) {
		t.Error("a read entry permits its subtree")
	}
	if f.Allows(abs("secrets", "x"), Read) {
		t.Error("a read entry restricts everything outside it")
	}
	if !f.HasReadRestrictions() {
		t.Error("a read entry is a read restriction")
	}
}

func TestWritable(t *testing.T) {
	f := Files{Entries: []Entry{
		{Path: abs("a"), Access: Write},
		{Path: abs("b"), Access: Read},
		{Path: abs("c"), Access: Write},
	}}
	got := f.Writable()
	want := []string{abs("a"), abs("c")}
	if len(got) != len(want) {
		t.Fatalf("Writable() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Writable()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestValidateRejectsWhatMountsCannotExpress(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry Entry
	}{
		{"relative path", Entry{Path: "work/out", Access: Write}},
		{"glob in path", Entry{Path: abs("*"), Access: Write}},
		{"glob segment", Entry{Path: abs("out", "**"), Access: Write}},
		{"unknown access", Entry{Path: abs("out"), Access: Access(9)}},
	} {
		f := Files{Entries: []Entry{tc.entry}}
		if err := f.validate(); err == nil {
			t.Errorf("%s: validate() = nil, want an error", tc.name)
		}
	}
	if err := (Files{Entries: []Entry{{Path: abs("out"), Access: Write}}}).validate(); err != nil {
		t.Errorf("absolute, glob-free entry rejected: %v", err)
	}
}
