package api

// ReadinessAdapter wraps any concrete ProjectorManager (with the right shape)
// into the ReadinessProbe interface the api package consumes. It exists so
// projection.ProjectorStatus and api.ProjectorStatusView can stay in their
// own packages without an import cycle.
type ReadinessAdapter struct {
	IsAllReadyFunc func() bool
	StatusFunc     func() []ProjectorStatusView
}

// IsAllReady delegates to the configured probe function.
func (a *ReadinessAdapter) IsAllReady() bool {
	if a == nil || a.IsAllReadyFunc == nil {
		return true
	}
	return a.IsAllReadyFunc()
}

// Status delegates to the configured probe function.
func (a *ReadinessAdapter) Status() []ProjectorStatusView {
	if a == nil || a.StatusFunc == nil {
		return nil
	}
	return a.StatusFunc()
}
