package main

import (
	"fmt"
	"regexp"
	"strings"
)

var eventNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]+)+$`)

// verbWhitelist is §3.3 in docs/event-sourcing-plan.md plus a few compound-friendly tokens.
var verbWhitelist = map[string]struct{}{
	"submitted": {}, "claimed": {}, "completed": {}, "failed": {}, "cancelled": {}, "timed_out": {},
	"created": {}, "changed": {}, "deleted": {}, "purged": {}, "disabled": {}, "enabled": {},
	"received": {}, "queued": {}, "sent": {}, "delivered": {}, "skipped": {}, "dropped": {},
	"fired": {}, "triggered": {}, "started": {}, "stopped": {}, "paused": {}, "resumed": {},
	"invoked": {}, "returned": {}, "aborted": {}, "expired": {}, "edited": {}, "acked": {},
	"performed": {}, "delta": {},
	// extensions used by §3.8 event names
	"attempt_failed": {}, "exhausted": {}, "scheduled": {}, "dismissed": {},
	"added": {}, "removed": {},
}

var imperativeBan = map[string]struct{}{
	"submit": {}, "create": {}, "update": {}, "delete": {}, "claim": {}, "complete": {},
	"start": {}, "fire": {}, "invoke": {}, "send": {}, "receive": {}, "queue": {},
	"deliver": {}, "perform": {}, "change": {}, "fail": {}, "run": {},
}

func lintSpec(spec *Spec) []error {
	var errs []error
	partNames := make(map[string]struct{})
	for _, p := range spec.Partitions {
		partNames[p.Name] = struct{}{}
	}
	for _, ev := range spec.Events {
		if !eventNameRe.MatchString(ev.Name) {
			errs = append(errs, fmt.Errorf("event %q: name must match %s", ev.Name, eventNameRe.String()))
		}
		prefix, _, ok := strings.Cut(ev.Name, ".")
		if !ok || prefix != ev.Domain {
			errs = append(errs, fmt.Errorf("event %q: prefix must equal domain %q", ev.Name, ev.Domain))
		}
		last := ev.Name[strings.LastIndex(ev.Name, ".")+1:]
		if strings.HasSuffix(last, "ing") {
			errs = append(errs, fmt.Errorf("event %q: progressive -ing suffix not allowed", ev.Name))
		}
		if _, bad := imperativeBan[last]; bad {
			errs = append(errs, fmt.Errorf("event %q: last segment %q uses imperative style", ev.Name, last))
		}
		if spec.Lint.EnforceVerbWhitelist {
			if _, ok := verbWhitelist[last]; !ok {
				errs = append(errs, fmt.Errorf("event %q: last segment %q not in verb whitelist", ev.Name, last))
			}
		}
		if spec.Lint.EnforcePartitionMatch {
			if _, ok := partNames[ev.Partition]; !ok {
				errs = append(errs, fmt.Errorf("event %q: partition %q not registered in manifest", ev.Name, ev.Partition))
			}
		}
		if spec.Lint.EnforceCategoryInCategories {
			if _, ok := spec.Categories[ev.Category]; !ok {
				errs = append(errs, fmt.Errorf("event %q: category %q not in manifest categories", ev.Name, ev.Category))
			}
		}
		if spec.Lint.EnforceAuditSummaryWhenRequired && ev.AuditRequired {
			if strings.TrimSpace(ev.AuditSummary) == "" {
				errs = append(errs, fmt.Errorf("event %q: audit_required but audit_summary empty", ev.Name))
			}
			if !strings.Contains(ev.AuditSummary, "{{payload.") {
				errs = append(errs, fmt.Errorf("event %q: audit_summary should reference {{payload.*}}", ev.Name))
			}
		}
		if ev.Version < 1 {
			errs = append(errs, fmt.Errorf("event %q: version must be >= 1", ev.Name))
		}
		if strings.TrimSpace(ev.Doc) == "" {
			errs = append(errs, fmt.Errorf("event %q: doc required", ev.Name))
		}
		if len(ev.Producers) == 0 {
			errs = append(errs, fmt.Errorf("event %q: producers required", ev.Name))
		}
		if len(ev.Consumers) == 0 {
			errs = append(errs, fmt.Errorf("event %q: consumers required", ev.Name))
		}
	}
	for pname, pdef := range spec.Payloads {
		for fname, f := range pdef.Fields {
			if err := lintField(fname, f); err != nil {
				errs = append(errs, fmt.Errorf("payload %s field %s: %w", pname, fname, err))
			}
		}
	}
	return errs
}

func lintField(name string, f FieldDef) error {
	switch f.Type {
	case "string", "int64", "int32", "bool", "float64", "timestamp", "bytes", "any":
		return nil
	case "array":
		if f.ItemType == "" {
			return fmt.Errorf("array requires item_type")
		}
		return nil
	case "map":
		if f.ValueType == "" {
			return fmt.Errorf("map requires value_type")
		}
		return nil
	default:
		return fmt.Errorf("unknown type %q", f.Type)
	}
}
