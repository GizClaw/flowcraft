package helpers

import (
	"os"
	"syscall"
)

// interruptSignal is the signal SignalQuit sends. SIGTERM is the
// "graceful shutdown" convention vesseld listens for; using SIGINT
// here would also work but SIGTERM matches the daemon's documented
// shutdown contract more precisely.
var interruptSignal os.Signal = syscall.SIGTERM
