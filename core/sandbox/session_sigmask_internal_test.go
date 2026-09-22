//go:build linux || darwin

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// workerSignal is the signal the cross-thread test blocks on a bystander
// thread. Nothing in this package ever sends it, so the only way it moves
// is a mask write that reached a thread it was not meant for.
const workerSignal = int(syscall.SIGUSR1)

// spawnSpecs lists the spawn paths StartSession owns, so each spawn-side
// test covers both instead of the pipe path alone: the pty branch carries
// its own closure and error plumbing.
func spawnSpecs() map[string]SessionSpec {
	return map[string]SessionSpec{
		"pipes": {},
		"tty":   {TTY: true, Rows: 24, Cols: 80},
	}
}

// describeMask names the two signals these tests move, so a failure says
// which bits changed rather than dumping raw mask words.
func describeMask(set sigset) string {
	return fmt.Sprintf("{SIGINT:%v SIGUSR1:%v}",
		sigsetBlocks(set, int(syscall.SIGINT)), sigsetBlocks(set, workerSignal))
}

// sleepCommand returns a child that would outlive the test, so an
// interrupt that goes missing shows up as a deadline instead of a fast,
// unremarkable exit.
func sleepCommand(t *testing.T) *exec.Cmd {
	t.Helper()
	bin, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("locate sleep: %v", err)
	}
	return exec.Command(bin, "30")
}

// TestSpawnLeavesOtherThreadsMasksAlone is the cross-thread half of the
// spawn contract: clearing the forking thread's mask must not reach any
// other thread, and putting the mask back must not broadcast the spawner's
// mask onto them.
//
// A process-wide mask write passes every assertion that only watches the
// child - the child really does start clean - so a bystander thread blocks
// a signal the spawner never blocks, and the spawner blocks one the
// bystander never does. A process-wide clear drops the bystander's block;
// a process-wide restore hands it the spawner's. Both blocks are planted
// with the platform primitive rather than replaceThreadSignalMask, so the
// test cannot inherit the behaviour it is checking.
func TestSpawnLeavesOtherThreadsMasksAlone(t *testing.T) {
	for name, spec := range spawnSpecs() {
		t.Run(name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			type report struct {
				mask sigset
				err  error
			}
			ready := make(chan struct{})
			release := make(chan struct{})
			before := make(chan report, 1)
			after := make(chan report, 1)

			go func() {
				runtime.LockOSThread()
				defer runtime.UnlockOSThread()
				if err := blockThreadSignal(workerSignal); err != nil {
					before <- report{err: err}
					after <- report{err: err}
					close(ready)
					return
				}
				mask, err := queryThreadSignalMask()
				before <- report{mask: mask, err: err}
				close(ready)
				<-release
				mask, err = queryThreadSignalMask()
				after <- report{mask: mask, err: err}
			}()

			<-ready
			if got := <-before; got.err != nil {
				t.Fatalf("block SIGUSR1 on a bystander thread: %v", got.err)
			} else if !sigsetBlocks(got.mask, workerSignal) {
				t.Fatalf("bystander mask before the spawn = %s, want SIGUSR1 blocked",
					describeMask(got.mask))
			}

			if err := blockThreadSignal(int(syscall.SIGINT)); err != nil {
				t.Fatalf("block SIGINT on the spawning thread: %v", err)
			}
			spawnerBefore, err := queryThreadSignalMask()
			if err != nil {
				t.Fatalf("read the spawner's mask: %v", err)
			} else if !sigsetBlocks(spawnerBefore, int(syscall.SIGINT)) {
				t.Fatalf("spawner mask before the spawn = %s, want SIGINT blocked",
					describeMask(spawnerBefore))
			}

			released := false
			releaseWorker := func() {
				if !released {
					released = true
					close(release)
				}
			}
			defer releaseWorker()

			spec.ID = "cross-thread-" + name
			sess, err := StartSession(context.Background(), spec, sleepCommand(t))
			if err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			defer func() { _ = sess.Close() }()

			releaseWorker()

			if got := <-after; got.err != nil {
				t.Fatalf("read the bystander's mask after the spawn: %v", got.err)
			} else {
				if !sigsetBlocks(got.mask, workerSignal) {
					t.Fatalf("the spawn cleared a bystander thread's mask: %s, want SIGUSR1 still blocked",
						describeMask(got.mask))
				}
				if sigsetBlocks(got.mask, int(syscall.SIGINT)) {
					t.Fatalf("the spawn handed a bystander thread the spawner's mask: %s",
						describeMask(got.mask))
				}
			}

			if spawnerAfter, err := queryThreadSignalMask(); err != nil {
				t.Fatalf("read the spawner's mask: %v", err)
			} else if spawnerAfter != spawnerBefore {
				t.Fatalf("the spawn rewrote the spawner's own mask: %s -> %s",
					describeMask(spawnerBefore), describeMask(spawnerAfter))
			}
		})
	}
}

// TestInterruptEndsAChildSpawnedFromABlockedMask pins the spawn-side
// contract behind Session.Signal(Interrupt): a child must not inherit the
// forking thread's signal mask.
//
// A mask survives exec while dispositions do not, so a child that starts
// with SIGINT blocked can never be interrupted - kill(2) to its group
// succeeds, the signal stays pending, and the process runs to its own end,
// which is how an interrupt turns into a silent no-op. Go reuses threads
// that entered the runtime from C, and those keep the mask their thread
// arrived with, so this blocks SIGINT on its own thread and spawns the
// session from it: with the block cleared for the fork, the interrupt ends
// the child like any other.
func TestInterruptEndsAChildSpawnedFromABlockedMask(t *testing.T) {
	for name, spec := range spawnSpecs() {
		t.Run(name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			previous, err := replaceThreadSignalMask(sigsetOnly(int(syscall.SIGINT)))
			if err != nil {
				t.Fatalf("block SIGINT on the spawning thread: %v", err)
			}
			defer func() {
				if _, err := replaceThreadSignalMask(previous); err != nil {
					t.Errorf("restore the spawning thread's mask: %v", err)
				}
			}()

			// Positive control: without a block actually in place the
			// spawn below could pass for the wrong reason.
			if blocked, err := queryThreadSignalMask(); err != nil {
				t.Fatalf("read the spawning thread's mask: %v", err)
			} else if !sigsetBlocks(blocked, int(syscall.SIGINT)) {
				t.Fatalf("spawning thread's mask = %s, want SIGINT blocked before the spawn",
					describeMask(blocked))
			}

			spec.ID = "dirty-mask-" + name
			sess, err := StartSession(context.Background(), spec, sleepCommand(t))
			if err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			defer func() { _ = sess.Close() }()

			// The seam owes the caller its mask back.
			if after, err := queryThreadSignalMask(); err != nil {
				t.Fatalf("read the spawning thread's mask: %v", err)
			} else if !sigsetBlocks(after, int(syscall.SIGINT)) {
				t.Fatalf("the spawn dropped the caller's SIGINT block: mask = %s", describeMask(after))
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := sess.Signal(ctx, SessionSignalInterrupt); err != nil {
				t.Fatalf("Signal: %v", err)
			}

			waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelWait()
			exit, err := sess.Wait(waitCtx)
			if err != nil {
				t.Fatalf("wait after interrupt: %v", err)
			}
			if exit.Code != -1 || exit.Signal != int(syscall.SIGINT) || exit.Reason != SessionSignaled {
				t.Fatalf("exit = %+v, want the child ended by SIGINT (code -1, signal %d, signalled)",
					exit, int(syscall.SIGINT))
			}
		})
	}
}

// TestStartWithCleanSignalMaskContract covers the seam itself: what the
// spawn callback sees, what the caller gets back, and what a spawn that
// fails while the mask is cleared leaves behind.
func TestStartWithCleanSignalMaskContract(t *testing.T) {
	blockSigint := func(t *testing.T) sigset {
		t.Helper()
		previous, err := replaceThreadSignalMask(sigsetOnly(int(syscall.SIGINT)))
		if err != nil {
			t.Fatalf("block SIGINT on the calling thread: %v", err)
		}
		return previous
	}
	assertSigintStillBlocked := func(t *testing.T, what string) {
		t.Helper()
		if after, err := queryThreadSignalMask(); err != nil {
			t.Fatalf("read the calling thread's mask: %v", err)
		} else if !sigsetBlocks(after, int(syscall.SIGINT)) {
			t.Fatalf("%s: mask = %s, want the caller's SIGINT block back", what, describeMask(after))
		}
	}

	t.Run("the callback sees an empty mask and the caller's mask comes back", func(t *testing.T) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		previous := blockSigint(t)
		defer func() {
			if _, err := replaceThreadSignalMask(previous); err != nil {
				t.Errorf("restore the calling thread's mask: %v", err)
			}
		}()

		var observed sigset
		if err := startWithCleanSignalMask(func() error {
			var err error
			observed, err = queryThreadSignalMask()
			return err
		}); err != nil {
			t.Fatalf("startWithCleanSignalMask: %v", err)
		}
		if sigsetBlocks(observed, int(syscall.SIGINT)) {
			t.Fatalf("the spawn callback ran with SIGINT still blocked: %s", describeMask(observed))
		}
		assertSigintStillBlocked(t, "after a successful spawn")
	})

	t.Run("a failed spawn still restores the caller's mask", func(t *testing.T) {
		sentinel := errors.New("spawn refused")
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		previous := blockSigint(t)
		defer func() {
			if _, err := replaceThreadSignalMask(previous); err != nil {
				t.Errorf("restore the calling thread's mask: %v", err)
			}
		}()

		if err := startWithCleanSignalMask(func() error { return sentinel }); !errors.Is(err, sentinel) {
			t.Fatalf("startWithCleanSignalMask = %v, want the spawn's own error", err)
		}
		assertSigintStillBlocked(t, "after a failed spawn")
	})

	t.Run("a caller that pinned itself stays on its thread", func(t *testing.T) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		before := currentThreadID()
		if before == 0 {
			t.Fatal("currentThreadID returned 0: thread identity is not wired up on this platform")
		}
		if err := startWithCleanSignalMask(func() error { return nil }); err != nil {
			t.Fatalf("startWithCleanSignalMask: %v", err)
		}
		// Give the scheduler room to move a goroutine that lost its pin.
		for i := 0; i < 100; i++ {
			runtime.Gosched()
		}
		if after := currentThreadID(); after != before {
			t.Fatalf("the seam moved a pinned goroutine from thread %d to %d", before, after)
		}
	})
}

// TestStartSessionClassifiesAMissingTTYBinary covers the pty branch's
// error plumbing: a spawn that fails while the mask is cleared must be
// classified and must not hand back a session.
func TestStartSessionClassifiesAMissingTTYBinary(t *testing.T) {
	cmd := exec.Command("flowcraft-no-such-binary")
	sess, err := StartSession(context.Background(), SessionSpec{TTY: true, Rows: 24, Cols: 80}, cmd)
	if sess != nil {
		_ = sess.Close()
		t.Fatal("StartSession returned a session for a binary that does not exist")
	}
	if !errdefs.IsNotFound(err) {
		t.Fatalf("StartSession error = %v, want a NotFound classification", err)
	}
}
