package agent

// InterruptKind tells the engine how a caller interrupted a run.
//
// Only steer and inject carry a new instruction; cancel is a stop and must not
// be learned as a preference.
type InterruptKind string

const (
	// InterruptCancel stops the task without a new instruction.
	InterruptCancel InterruptKind = "cancel"
	// InterruptSteer redirects the task at the next step boundary.
	InterruptSteer InterruptKind = "steer"
	// InterruptInject corrects a premise at the next step boundary.
	InterruptInject InterruptKind = "inject"
)

// Interruption is what the engine applied, or accepted for the next boundary.
type Interruption struct {
	IntentKind       InterruptKind `json:"intent_kind"`
	BeforePlanHash   string        `json:"before_plan_hash,omitempty"`
	AfterInstruction string        `json:"after_instruction,omitempty"`
	BoundaryStep     int           `json:"boundary_step,omitempty"`
}
