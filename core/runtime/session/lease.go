package session

import "sync"

// Lease keeps a shared Session retained by its Manager.
type Lease struct {
	manager *Manager
	key     Key
	session *Session

	closeOnce sync.Once
	closeErr  error
}

// Session returns the Session retained by this Lease.
func (l *Lease) Session() *Session {
	if l == nil {
		return nil
	}
	return l.session
}

// Close releases this Lease. It is safe to call repeatedly or concurrently.
func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		if l.manager != nil {
			l.closeErr = l.manager.release(l.key, l.session)
		}
	})
	return l.closeErr
}
