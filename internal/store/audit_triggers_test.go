package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func newTempStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "audit.db")
	st, err := NewSQLiteStore(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestAuditTriggers_RejectUpdate(t *testing.T) {
	st := newTempStore(t)
	db := st.DB()

	if _, err := db.Exec(`INSERT INTO audit_entries(seq,type,actor_id,actor_kind,actor_realm_id,actor_json,ts,partition,trace_id,summary)
	                       VALUES (1,'audit.action.performed','u1','user',NULL,'{}','2026-04-22T00:00:00Z','audit:rt-1','t1','demo')`); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	_, err := db.Exec(`UPDATE audit_entries SET summary='tampered' WHERE seq=1`)
	if err == nil {
		t.Fatal("UPDATE should be rejected by trigger")
	}
	if !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAuditTriggers_RejectDeleteWhenEventLogPresent(t *testing.T) {
	st := newTempStore(t)
	db := st.DB()

	if _, err := db.Exec(`INSERT INTO event_log(seq,partition,type,version,category,ts,payload)
	                       VALUES (5,'audit:rt-1','audit.action.performed',1,'audit','2026-04-22T00:00:00Z',X'7B7D')`); err != nil {
		t.Fatalf("seed event_log: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO audit_entries(seq,type,actor_id,actor_kind,actor_realm_id,actor_json,ts,partition,trace_id,summary)
	                       VALUES (5,'audit.action.performed','u1','user',NULL,'{}','2026-04-22T00:00:00Z','audit:rt-1','t1','demo')`); err != nil {
		t.Fatalf("seed audit: %v", err)
	}

	_, err := db.Exec(`DELETE FROM audit_entries WHERE seq=5`)
	if err == nil {
		t.Fatal("DELETE should be rejected while event_log row exists")
	}
}

func TestAuditTriggers_AllowDeleteAfterEventLogPruned(t *testing.T) {
	st := newTempStore(t)
	db := st.DB()

	if _, err := db.Exec(`INSERT INTO audit_entries(seq,type,actor_id,actor_kind,actor_realm_id,actor_json,ts,partition,trace_id,summary)
	                       VALUES (10,'audit.action.performed','u1','user',NULL,'{}','2026-04-22T00:00:00Z','audit:rt-1','t1','demo')`); err != nil {
		t.Fatalf("seed audit: %v", err)
	}

	if _, err := db.Exec(`DELETE FROM audit_entries WHERE seq=10`); err != nil {
		t.Fatalf("DELETE should be allowed after retention sweep: %v", err)
	}
}
