package cli

import (
	"flag"
	"strings"
)

// newRepeatedFlag installs a string-slice flag on the given
// FlagSet. Standard library `flag` does not ship a Var for "may
// be passed N times", so we bind a tiny implementation here. The
// usage form is `--config a --config b` (kubectl style).
func newRepeatedFlag(fs *flag.FlagSet, name, usage string) *[]string {
	var slice repeatedString
	fs.Var(&slice, name, usage)
	return (*[]string)(&slice)
}

// repeatedString is the flag.Value impl backing newRepeatedFlag.
type repeatedString []string

// String formats the accumulated values for `--help` output.
func (r *repeatedString) String() string {
	if r == nil {
		return ""
	}
	return strings.Join(*r, ",")
}

// Set is called once per flag occurrence by the flag package.
func (r *repeatedString) Set(v string) error {
	*r = append(*r, v)
	return nil
}
