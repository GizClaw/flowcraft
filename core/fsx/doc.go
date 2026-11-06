// Package fsx is where file I/O happens: reads go through this process
// unrestricted, writes go through an OS handle that cannot leave its root.
//
// # Why the split
//
// Reads are not restricted by this package, by decision. The boundary a read
// rule would draw is not the boundary a write rule draws: it would have to
// cover what a tool opens here and what every sandboxed command opens there,
// and a secret is better kept outside the tree than fenced inside it. So reads
// are unrestricted: any absolute path this process can open, fsx can read. That
// is a decision with a consequence — the model can read a secret and copy it
// into the workspace — and it is stated here rather than left to be discovered.
//
// Writes are the opposite. A write is the one file operation where a mistake
// becomes persistent, so each declared writable root is opened with
// [os.OpenRoot] and every write is performed relative to that handle. The
// kernel resolves the path inside the root (openat2 on Linux, a relative NT
// open on Windows), so nothing here has to prove a path is safe: a spelling
// this package does not understand cannot widen anything, because the handle
// decides at open time. There is also no resolve-then-open window to lose: the
// resolution happens as part of the open.
//
// # What a root has to be
//
// A writable root must exist when Open is called and must be a directory. An
// entry that cannot be opened cannot be enforced, and a writable set nothing
// enforces is a comment. Paths are resolved through symlinks at Open, so the
// handle and the selector that picks it compare the same spelling.
//
// # Absolute paths only
//
// There is no single base for a relative path when several roots are declared,
// so every path is absolute. Selecting the root for a target is a string
// comparison and only a selector: picking wrongly costs a denied operation,
// never a wider one, because the handle refuses the relative path it is handed.
//
// # What is not here
//
// Command confinement. Commands run under sandbox.Runner with the same
// permission.Writes projected onto the kernel; that projection and these
// handles are two expressions of one value, which is why the fs side and the
// shell side cannot confine to different roots.
package fsx
