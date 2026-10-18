package main

// The samples the pathsafe contract is computed from: core's own test table,
// plus the edges a port can get wrong without a table noticing -- trailing
// separators, a mixed absolute/relative pair, a reference that is absolute but
// gets appended anyway.

type withinSample struct{ root, path string }

var pathsafeWithinSamples = []withinSample{
	// core's within_test.go table.
	{"/w/root", "/w/root"},
	{"/w/root", "/w/root/a"},
	{"/w/root", "/w/root/a/b/c"},
	{"/w/root", "/w"},
	{"/w/root", "/w/rooted"},
	{"/w/root", "/w/root/../other"},
	{"/w/root", ""},
	// core's within_fold table, which exercises the same function.
	{"/w/root", "/W/ROOT"},
	{"/w/root", "/W/root/sub"},
	{"/w/root", "/W/ROOT/a/b"},
	{"/w/root", "/W/rooted"},
	{"/w/root", "/W"},
	// Edges: trailing separators, a root that is a prefix string but not a
	// path prefix, a relative pair, and the empty root's answer.
	{"/w/root/", "/w/root/a"},
	{"/w/root", "/w/root/a/"},
	{"/w/roo", "/w/root"},
	{"/", "/w/root"},
	{"/", "/"},
	{"", "/w/root"},
	{"relative", "relative/sub"},
	{"relative", "relativeish"},
	{"relative", "other/sub"},
	{"a/b", "a/b/c"},
	{"a/b", "a/b/../c"},
	{"a/b", "."},
	{".", "a"},
	{"\u212a", "\u212a"},
}

type relSample struct{ root, target string }

var pathsafeRelSamples = []relSample{
	// core's Rel test table.
	{"/w/root", "/w/root"},
	{"/w/root", "/w/root/a"},
	{"/w/root", "/w/root/a/b"},
	{"/w/root", "/w/other"},
	// The cleaning step Rel performs before it walks.
	{"/w/root", "/w/root/a/../b"},
	{"/w/root", "/w/root/./a"},
	{"/w/root/", "/w/root/a"},
	{"/w/root", "/w/root/a/"},
	// Up, then back down: the direction filepath.Rel spends most of its code
	// on, and the one a naive prefix strip gets wrong.
	{"/a/b/c", "/a"},
	{"/a/b/c", "/a/x"},
	{"/a/b/c", "/a/b"},
	{"/a", "/a/b/c/d"},
	{"/a/b", "/a/b"},
	{"/", "/a/b"},
	{"/a/b", "/"},
	// Relative pairs, and the pair Rel refuses.
	{"a/b", "a/c"},
	{"a/b", "../a/b/c"},
	{".", "a"},
	{"a/b", "."},
	{"/w/root", "relative/path"},
	{"relative/path", "/w/root"},
	// Empty arguments: Rel answers "." for two empties and an error for one.
	{"", ""},
	{"", "/w"},
	{"/w", ""},
}

type resolveSample struct{ root, reference string }

var pathsafeResolveSamples = []resolveSample{
	// core's ResolveUnder test table.
	{"/w/root", "sub/file.txt"},
	{"/w/root", "."},
	{"/w/root", "a/b/c"},
	{"/w/root", "../escape"},
	{"/w/root", "sub/../../escape"},
	{"/w/root", ".."},
	// An absolute reference is appended rather than replacing the root
	// (filepath.Join), and then refused.
	{"/w/root", "/absolute"},
	// The root's own edges: cleaned first, and refused when empty.
	{"/w/root/", "a/../b"},
	{"/", "a"},
	{"/", ".."},
	{"/w/root/", "../root/a"},
	{"", "x"},
	{"/w/root", ""},
	{"/w/root", "./"},
	{"/w/root", "a//b/"},
}

var pathsafeRelRefSamples = []string{
	// core's RelRef test table.
	"bin/plugin",
	"./bin/plugin",
	"a/../b",
	"..",
	"../escape",
	"a/../../escape",
	"/absolute",
	// The empty reference cleans to "." and is allowed.
	"",
	// A backslash is an ordinary character on Unix, so this is a plain name --
	// the rooted check that refuses it is Windows' business (plan §12).
	`\x`,
	".",
	"./",
	"a/..",
	"a/../..",
	"../a/../b",
	"/",
	"//host/share",
	"a\\..\\b",
}

var pathsafeCleanSamples = []string{
	// filepath.Clean's documented behaviour, one rule at a time.
	"/w/root/",
	"/w//root/./a",
	"/w/root/../other",
	"/w/root/a/..",
	"/..",
	"/../..",
	"",
	".",
	"..",
	"a/../../b",
	"../../a",
	"a/b/./c/",
	"a//b",
	"//",
	"/",
	"a/..",
	"a/../..",
	"./a",
	"a/./",
	"a/b/../../../c",
}
