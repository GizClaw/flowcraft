package agent

// The lifecycle is stored in an atomic uint32 rather than behind a mutex, because
// every Status event reads it. This file is the encoding and nothing else: adding
// a TaskState means adding it here and in agent.TaskState, and the two switches
// keep each other honest.
//
// stateCode and stateFromCode pack the lifecycle into the atomic. A switch is
// cheaper than an interface box per Status event.
func stateCode(state TaskState) uint32 {
	switch state {
	case StateWorking:
		return 2
	case StateInputRequired:
		return 3
	case StateCompleted:
		return 4
	case StateFailed:
		return 5
	case StateCanceled:
		return 6
	case StateRejected:
		return 7
	case StateAuthRequired:
		return 8
	default:
		return 1 // submitted
	}
}

func stateFromCode(code uint32) TaskState {
	switch code {
	case 2:
		return StateWorking
	case 3:
		return StateInputRequired
	case 4:
		return StateCompleted
	case 5:
		return StateFailed
	case 6:
		return StateCanceled
	case 7:
		return StateRejected
	case 8:
		return StateAuthRequired
	default:
		return StateSubmitted
	}
}
