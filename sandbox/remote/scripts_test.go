package remote_test

import (
	goruntime "runtime"

	"github.com/GizClaw/flowcraft/core/sandbox/sandboxtest"
)

// The wire suite drives a real backend, and the backends start real shells. The
// scripts below are written once per platform: the transport does not care which
// shell runs the command, so the test should not either.

// script renders a script for this platform's shell.
func script(body string) []string {
	return sandboxtest.ForPlatform(goruntime.GOOS).Argv(body)
}

// longRunning is a command that stays alive until it is terminated.
func longRunning() []string {
	shell := sandboxtest.ForPlatform(goruntime.GOOS)
	return shell.Argv(shell.LongRunning())
}

// delayedEcho prints body once a short wait has passed: the suite needs a session
// that is silent at first, and "wait" is spelled differently in every shell.
func delayedEcho(body string) []string {
	if goruntime.GOOS == "windows" {
		// ping is the portable cmd.exe sleep: timeout(1) needs a console.
		return script(`ping -n 2 127.0.0.1 >nul & echo ` + body)
	}
	return script("sleep 0.4; echo " + body)
}

// echoAfterDelay prints first, waits, then prints second, so output arrives in two
// moments on either platform.
func echoAfterDelay(first, second string) []string {
	if goruntime.GOOS == "windows" {
		return script(`echo ` + first + ` & ping -n 2 127.0.0.1 >nul & echo ` + second)
	}
	return script("echo " + first + "; sleep 0.4; echo " + second)
}

// echoThenHold prints first and then keeps the process alive.
func echoThenHold(first string) []string {
	shell := sandboxtest.ForPlatform(goruntime.GOOS)
	if goruntime.GOOS == "windows" {
		return script(`echo ` + first + ` & ` + shell.LongRunning())
	}
	return script("echo " + first + "; sleep 30")
}
