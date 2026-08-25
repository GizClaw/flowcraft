package sandbox_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

func TestClassifySafeReadOnly(t *testing.T) {
	tests := []struct {
		name string
		req  sandbox.ExecRequest
		want bool
	}{
		// Base read-only commands.
		{name: "ls", req: sandbox.ExecRequest{Command: "ls", Args: []string{"-la"}}, want: true},
		{name: "cat", req: sandbox.ExecRequest{Command: "cat", Args: []string{"file"}}, want: true},
		{name: "pwd", req: sandbox.ExecRequest{Command: "pwd"}, want: true},
		{name: "grep", req: sandbox.ExecRequest{Command: "grep", Args: []string{"-n", "x", "."}}, want: true},
		{name: "head", req: sandbox.ExecRequest{Command: "/usr/bin/head", Args: []string{"-5", "f"}}, want: true},
		{name: "wc", req: sandbox.ExecRequest{Command: "wc", Args: []string{"-l", "f"}}, want: true},
		{name: "echo", req: sandbox.ExecRequest{Command: "echo", Args: []string{"hi"}}, want: true},

		// find: read-only forms safe, write/exec actions unsafe.
		{name: "find print", req: sandbox.ExecRequest{Command: "find", Args: []string{".", "-name", "*.go", "-print"}}, want: true},
		{name: "find delete", req: sandbox.ExecRequest{Command: "find", Args: []string{".", "-delete"}}, want: false},
		{name: "find exec", req: sandbox.ExecRequest{Command: "find", Args: []string{".", "-exec", "rm", "{}", ";"}}, want: false},
		{name: "find execdir", req: sandbox.ExecRequest{Command: "find", Args: []string{".", "-execdir", "echo", "{}", "+"}}, want: false},
		{name: "find ok", req: sandbox.ExecRequest{Command: "find", Args: []string{".", "-ok", "rm", "{}", ";"}}, want: false},
		{name: "find fprintf", req: sandbox.ExecRequest{Command: "find", Args: []string{".", "-fprintf", "out", "%p"}}, want: false},

		// rg: -z / --pre and friends unsafe.
		{name: "rg plain", req: sandbox.ExecRequest{Command: "rg", Args: []string{"pattern", "."}}, want: true},
		{name: "rg -l", req: sandbox.ExecRequest{Command: "rg", Args: []string{"-l", "pattern"}}, want: true},
		{name: "rg -z", req: sandbox.ExecRequest{Command: "rg", Args: []string{"-z", "pattern"}}, want: false},
		{name: "rg null-data", req: sandbox.ExecRequest{Command: "rg", Args: []string{"--null-data", "pattern"}}, want: false},
		{name: "rg pre", req: sandbox.ExecRequest{Command: "rg", Args: []string{"--pre", "cat", "pattern"}}, want: false},
		{name: "rg pre-glob", req: sandbox.ExecRequest{Command: "rg", Args: []string{"--pre-glob", "*.md", "pattern"}}, want: false},
		{name: "rg combined short", req: sandbox.ExecRequest{Command: "rg", Args: []string{"-lz", "pattern"}}, want: false},

		// git: subcommand whitelist.
		{name: "git bare", req: sandbox.ExecRequest{Command: "git"}, want: true},
		{name: "git --version", req: sandbox.ExecRequest{Command: "git", Args: []string{"--version"}}, want: true},
		{name: "git status", req: sandbox.ExecRequest{Command: "git", Args: []string{"status", "--porcelain"}}, want: true},
		{name: "git diff", req: sandbox.ExecRequest{Command: "git", Args: []string{"diff", "--cached"}}, want: true},
		{name: "git log", req: sandbox.ExecRequest{Command: "git", Args: []string{"log", "--oneline", "-5"}}, want: true},
		{name: "git -C", req: sandbox.ExecRequest{Command: "git", Args: []string{"-C", "/tmp", "status"}}, want: true},
		{name: "git --git-dir", req: sandbox.ExecRequest{Command: "git", Args: []string{"--git-dir", "/tmp/.git", "status"}}, want: true},
		{name: "git show", req: sandbox.ExecRequest{Command: "git", Args: []string{"show", "HEAD"}}, want: true},
		{name: "git add", req: sandbox.ExecRequest{Command: "git", Args: []string{"add", "."}}, want: false},
		{name: "git branch -d", req: sandbox.ExecRequest{Command: "git", Args: []string{"branch", "-d", "x"}}, want: false},
		{name: "git remote add", req: sandbox.ExecRequest{Command: "git", Args: []string{"remote", "add", "origin", "u"}}, want: false},
		{name: "git push", req: sandbox.ExecRequest{Command: "git", Args: []string{"push"}}, want: false},
		{name: "git checkout", req: sandbox.ExecRequest{Command: "git", Args: []string{"checkout", "x"}}, want: false},

		// sed: -i and script write commands unsafe.
		{name: "sed -n", req: sandbox.ExecRequest{Command: "sed", Args: []string{"-n", "1,5p", "f"}}, want: true},
		{name: "sed plain", req: sandbox.ExecRequest{Command: "sed", Args: []string{"s/a/b/", "f"}}, want: true},
		{name: "sed -i", req: sandbox.ExecRequest{Command: "sed", Args: []string{"-i", "s/a/b/", "f"}}, want: false},
		{name: "sed in-place long", req: sandbox.ExecRequest{Command: "sed", Args: []string{"--in-place", "s/a/b/", "f"}}, want: false},
		{name: "sed script write", req: sandbox.ExecRequest{Command: "sed", Args: []string{"s/a/b/w /tmp/out", "f"}}, want: false},

		// Shell unwrapping: simple scripts safe, composites unsafe.
		{name: "sh -c ls", req: sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "ls"}}, want: true},
		{name: "bash -lc pwd", req: sandbox.ExecRequest{Command: "bash", Args: []string{"-lc", "pwd"}}, want: true},
		{name: "sh -c composite", req: sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "ls; rm x"}}, want: false},
		{name: "sh -c pipe", req: sandbox.ExecRequest{Command: "sh", Args: []string{"-c", "ls | grep x"}}, want: false},

		// Unknown / dangerous commands: conservative false.
		{name: "rm", req: sandbox.ExecRequest{Command: "rm", Args: []string{"-rf", "x"}}, want: false},
		{name: "xargs", req: sandbox.ExecRequest{Command: "xargs", Args: []string{"rm"}}, want: false},
		{name: "env exec", req: sandbox.ExecRequest{Command: "env", Args: []string{"FOO=1", "rm", "x"}}, want: false},
		{name: "tar extract", req: sandbox.ExecRequest{Command: "tar", Args: []string{"xf", "x.tar"}}, want: false},
		{name: "python", req: sandbox.ExecRequest{Command: "python", Args: []string{"x.py"}}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sandbox.ClassifySafeReadOnly(tt.req); got != tt.want {
				t.Fatalf("ClassifySafeReadOnly(%v) = %v, want %v",
					tt.req, got, tt.want)
			}
		})
	}
}
