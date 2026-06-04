package ingest

import (
	"fmt"
	"strconv"
	"strings"
)

type normalizedParameterValue struct {
	value   string
	display string
	kind    string
	unit    string
	trace   map[string]any
}

const (
	clearedParameterValue       = "__cleared__"
	clearedParameterDisplay     = "cleared"
	clearedParameterValueKind   = "clear"
	clearedParameterTraceReason = "operation_clear"
)

func normalizeParameterValue(raw, hint string) normalizedParameterValue {
	raw = strings.TrimSpace(raw)
	hint = strings.TrimSpace(hint)
	value := raw
	unit := ""
	value = normalizeRangeValue(value)
	value, unit = splitTrailingUnit(value)
	kind := inferParameterValueKind(value)
	display := value
	if unit != "" && !strings.HasSuffix(strings.ToLower(display), strings.ToLower(unit)) {
		display += unit
	}
	return normalizedParameterValue{
		value:   value,
		display: display,
		kind:    kind,
		unit:    unit,
		trace: map[string]any{
			"raw_value":        raw,
			"normalized_value": value,
			"normalized_hint":  hint,
			"unit":             unit,
			"normalizer":       "deterministic_parameter_v1",
		},
	}
}

func normalizeParameterValueForOperation(raw, hint, operation string) normalizedParameterValue {
	normalized := normalizeParameterValue(raw, hint)
	if strings.TrimSpace(operation) == "clear" && strings.TrimSpace(normalized.value) == "" {
		normalized.value = clearedParameterValue
		normalized.display = clearedParameterDisplay
		normalized.kind = clearedParameterValueKind
		if normalized.trace == nil {
			normalized.trace = map[string]any{}
		}
		normalized.trace["sentinel"] = clearedParameterTraceReason
		normalized.trace["normalized_value"] = clearedParameterValue
	}
	return normalized
}

func inferParameterValueKind(value string) string {
	lower := strings.ToLower(strings.TrimSpace(value))
	switch lower {
	case "true", "false":
		return "boolean"
	}
	if _, ok := parseNumberLike(lower); ok {
		return "number"
	}
	if isRangeValue(lower) {
		return "range"
	}
	if strings.Contains(value, ",") {
		return "list"
	}
	if isEnumLikeValue(lower) {
		return "enum"
	}
	return "string"
}

func normalizeRangeValue(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	for _, sep := range []string{"..", "–", "—"} {
		if strings.Contains(lower, sep) {
			parts := strings.SplitN(lower, sep, 2)
			if len(parts) == 2 {
				left := strings.TrimSpace(parts[0])
				right := strings.TrimSpace(parts[1])
				if _, ok := parseNumberLike(left); ok {
					if _, ok := parseNumberLike(right); ok {
						return left + ".." + right
					}
				}
			}
		}
	}
	return strings.TrimSpace(raw)
}

func isRangeValue(value string) bool {
	parts := strings.Split(value, "..")
	if len(parts) != 2 {
		return false
	}
	_, leftOK := parseNumberLike(parts[0])
	_, rightOK := parseNumberLike(parts[1])
	return leftOK && rightOK
}

func isEnumLikeValue(value string) bool {
	if value == "" || strings.ContainsAny(value, " \t\r\n,") {
		return false
	}
	if _, ok := parseNumberLike(value); ok {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func parseNumberLike(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func splitTrailingUnit(value string) (string, string) {
	lower := strings.ToLower(value)
	for _, suffix := range []string{"ms", "s", "min", "mb", "gb", "kb", "%"} {
		if strings.HasSuffix(lower, suffix) && len(value) > len(suffix) {
			prefix := strings.TrimSpace(value[:len(value)-len(suffix)])
			if _, ok := parseNumberLike(prefix); ok {
				return prefix, suffix
			}
		}
	}
	return value, ""
}

func canonicalParameterOwner(owner string) string {
	owner = canonicalSpace(owner)
	if owner == "" {
		return "default"
	}
	return owner
}

func canonicalParameterName(namespaceHint, surface string) (string, string) {
	s := strings.TrimSpace(surface)
	s = strings.Trim(s, "`\"'“”‘’")
	s = strings.ReplaceAll(s, "-", "_")
	canonical := strings.ToLower(strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == ':'
	}), "_"))
	namespace := strings.ToLower(strings.Trim(strings.ReplaceAll(namespaceHint, " ", "_"), "."))
	if dot := strings.LastIndex(canonical, "."); dot > 0 {
		namespace = strings.Trim(canonical[:dot], ".")
		canonical = canonical[dot+1:]
	}
	if canonical == "" {
		canonical = "unknown"
	}
	return namespace, canonical
}

type parameterOperationResolution struct {
	Operation string
	Explicit  bool
	Ambiguous bool
}

func resolveParameterOperation(surfaces ...string) parameterOperationResolution {
	for _, raw := range surfaces {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		operation := normalizeParameterOperationSurface(raw)
		if operation != "" {
			return parameterOperationResolution{Operation: operation, Explicit: true}
		}
		return parameterOperationResolution{Ambiguous: true}
	}
	return parameterOperationResolution{Operation: "set"}
}

func normalizeParameterOperation(surfaces ...string) string {
	res := resolveParameterOperation(surfaces...)
	if res.Operation == "" {
		return "set"
	}
	return res.Operation
}

func normalizeParameterOperationSurface(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	switch lower {
	case "set", "update", "clear", "confirm", "compare", "constrain":
		return lower
	}
	if lower == "" {
		return ""
	}
	if canonicalConstraintOperator(lower) != "" {
		return "constrain"
	}
	return ""
}

func canonicalConstraintOperator(raw string) string {
	raw = canonicalOperatorSurface(raw)
	switch raw {
	case "<", "<=", ">", ">=", "=", "==", "!=":
		if raw == "==" {
			return "equals"
		}
		if raw == "=" {
			return "equals"
		}
		if raw == "!=" {
			return "not_equals"
		}
		return raw
	}
	return ""
}

func canonicalOperatorSurface(raw string) string {
	raw = strings.ToLower(canonicalSpace(raw))
	raw = strings.Trim(raw, " \t\r\n:：,，.;；")
	return raw
}

func renderParameterContent(owner, namespace, name, operation, value, unit, operator, condition string) string {
	fullName := name
	if namespace != "" {
		fullName = namespace + "." + name
	}
	display := value
	if unit != "" && !strings.HasSuffix(strings.ToLower(display), strings.ToLower(unit)) {
		display += unit
	}
	if operator != "" {
		content := fmt.Sprintf("%s has parameter constraint %s: %s %s.", owner, fullName, operator, display)
		if strings.TrimSpace(condition) != "" {
			content = strings.TrimSuffix(content, ".") + " when " + strings.TrimSpace(condition) + "."
		}
		return content
	}
	content := fmt.Sprintf("%s has parameter %s %s to %s.", owner, fullName, operation, display)
	if strings.TrimSpace(condition) != "" {
		content = strings.TrimSuffix(content, ".") + " when " + strings.TrimSpace(condition) + "."
	}
	return content
}

func parameterEntities(values ...string) []string {
	return canonicalSet(values)
}

func metadataStringFromMap(meta map[string]any, key string) string {
	if len(meta) == 0 {
		return ""
	}
	if v, ok := meta[key]; ok {
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
