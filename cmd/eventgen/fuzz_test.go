package main

import (
	"strings"
	"testing"
)

func FuzzEventNameLint(f *testing.F) {
	f.Add("task.submitted")
	f.Add("bad name")
	f.Add("Task.Submitted")
	f.Fuzz(func(t *testing.T, name string) {
		if !eventNameRe.MatchString(name) {
			return
		}
		if strings.Contains(name, "..") {
			return
		}
		last := name[strings.LastIndex(name, ".")+1:]
		if strings.HasSuffix(last, "ing") {
			return
		}
		if _, bad := imperativeBan[last]; bad {
			return
		}
		if _, ok := verbWhitelist[last]; !ok {
			return
		}
	})
}
