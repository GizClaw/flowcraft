package session

// ManagerBinder receives the Runtime's session manager when a deployment
// generation is built or rebuilt. Consumers such as the delegation
// service implement it to upgrade subagent execution from the legacy
// bare-run path to session lifecycle. Binding is set-once: a second bind
// must return an error so the runtime can roll back a reload.
type ManagerBinder interface {
	BindSessionManager(manager *Manager) error
}
