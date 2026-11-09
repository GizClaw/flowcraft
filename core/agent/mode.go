package agent

// Mode says what a submitted message wants to do with a running Task.
//
// It is part of the submission contract, not of an engine: a client that steers
// and a client that queues are making the same decision whoever runs the Task.
// An engine that cannot honour a mode reports the one it applied instead of
// pretending.
type Mode int

const (
	// ModeQueue delivers the message to the next Task. Default.
	ModeQueue Mode = iota
	// ModeSteer makes the message visible to the next model request.
	ModeSteer
	// ModeInterrupt cancels the running round, then delivers the message.
	ModeInterrupt
)

// String names the mode.
func (m Mode) String() string {
	switch m {
	case ModeQueue:
		return "queue"
	case ModeSteer:
		return "steer"
	case ModeInterrupt:
		return "interrupt"
	}
	return "unknown"
}
