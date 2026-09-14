package bindings

// validGlobalName reports whether name can be referenced from a script as
// a global identifier by both bundled runtimes.
//
// The accepted shape is a conservative ASCII identifier — a letter or
// underscore followed by letters, digits or underscores — that is not a
// keyword of either JavaScript or Lua. Anything else (a dotted path, a
// leading digit, a language keyword, "$name") would be installed as a
// property the script cannot name, so [Assemble] rejects it instead of
// shipping a global that is silently unreachable.
func validGlobalName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	_, reserved := reservedGlobalNames[name]
	return !reserved
}

// reservedGlobalNames is the union of the JavaScript (including strict
// mode and await) and Lua keywords and literals. A binding named after
// one of them is unreachable as an identifier expression in the language
// that reserves it, and the engine supports both runtimes.
var reservedGlobalNames = map[string]struct{}{
	// JavaScript.
	"await": {}, "break": {}, "case": {}, "catch": {}, "class": {},
	"const": {}, "continue": {}, "debugger": {}, "default": {}, "delete": {},
	"do": {}, "else": {}, "enum": {}, "export": {}, "extends": {},
	"false": {}, "finally": {}, "for": {}, "function": {}, "if": {},
	"implements": {}, "import": {}, "in": {}, "instanceof": {},
	"interface": {}, "let": {}, "new": {}, "null": {}, "package": {},
	"private": {}, "protected": {}, "public": {}, "return": {},
	"static": {}, "super": {}, "switch": {}, "this": {}, "throw": {},
	"true": {}, "try": {}, "typeof": {}, "var": {}, "void": {},
	"while": {}, "with": {}, "yield": {},

	// Lua.
	"and": {}, "elseif": {}, "end": {}, "goto": {}, "local": {},
	"nil": {}, "not": {}, "or": {}, "repeat": {}, "then": {},
	"until": {},
}
