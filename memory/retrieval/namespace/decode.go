package namespace

import (
	"strconv"
	"strings"
)

// DecodeScope reverses UserScope and GlobalScope.
//
// V2 user scopes are parsed first. Legacy V1 user scopes are accepted only as
// a compatibility bridge for migration tools.
func (p *Prefix) DecodeScope(ns string) (runtimeID, userID string, isUser, ok bool) {
	header := p.name + "_"
	if !strings.HasPrefix(ns, header) {
		return "", "", false, false
	}
	rest := ns[len(header):]
	if rt, user, ok := decodeUserScopeV2(rest); ok {
		return rt, user, true, true
	}
	if rt, ok := decodeGlobalScope(rest); ok {
		return rt, "", false, true
	}
	if rt, user, ok := decodeUserScopeV1(rest); ok {
		return rt, user, true, true
	}
	return "", "", false, false
}

func decodeUserScopeV2(rest string) (runtimeID, userID string, ok bool) {
	search := rest
	offset := 0
	for {
		i := strings.Index(search, userMarker)
		if i < 0 {
			return "", "", false
		}
		mark := offset + i
		digitsStart := mark + len(userMarker)
		j := digitsStart
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if j == digitsStart || j >= len(rest) || rest[j] != '_' {
			search = rest[digitsStart:]
			offset = digitsStart
			continue
		}
		n, err := strconv.Atoi(rest[digitsStart:j])
		if err != nil {
			search = rest[digitsStart:]
			offset = digitsStart
			continue
		}
		userStart := j + 1
		if len(rest)-userStart != n {
			search = rest[digitsStart:]
			offset = digitsStart
			continue
		}
		return rest[:mark], rest[userStart:], true
	}
}

func decodeGlobalScope(rest string) (runtimeID string, ok bool) {
	if !strings.HasSuffix(rest, globalToken) {
		return "", false
	}
	return rest[:len(rest)-len(globalToken)], true
}

func decodeUserScopeV1(rest string) (runtimeID, userID string, ok bool) {
	const infix = "__u_"
	i := strings.LastIndex(rest, infix)
	if i < 0 {
		return "", "", false
	}
	return rest[:i], rest[i+len(infix):], true
}
