-- +goose Up

-- audit_entries are immutable. UPDATE is always rejected; DELETE is only
-- permitted when the corresponding event_log row has been pruned by the
-- 365d retention sweep (matching category=audit retention policy).

-- +goose StatementBegin
CREATE TRIGGER audit_entries_no_update
BEFORE UPDATE ON audit_entries
BEGIN
    SELECT RAISE(FAIL, 'audit_entries are immutable: UPDATE forbidden');
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER audit_entries_no_delete
BEFORE DELETE ON audit_entries
WHEN EXISTS (SELECT 1 FROM event_log WHERE seq = OLD.seq)
BEGIN
    SELECT RAISE(FAIL, 'audit_entries are immutable: DELETE only after event_log retention');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS audit_entries_no_update;
DROP TRIGGER IF EXISTS audit_entries_no_delete;
