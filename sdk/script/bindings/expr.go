package bindings

import (
	"container/list"
	"sync"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

type lruEntry struct {
	key  string
	prog *vm.Program
}

// exprProgramCache bounds compiled expr programs with LRU eviction.
// Front of the list = most recently used; back = least recently used.
type exprProgramCache struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[string]*list.Element // expression -> list element
}

const maxExprPrograms = 512

var globalExprCache = &exprProgramCache{
	max:   maxExprPrograms,
	ll:    list.New(),
	items: make(map[string]*list.Element),
}

func (c *exprProgramCache) getOrCompile(expression string) (*vm.Program, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[expression]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*lruEntry).prog, nil
	}

	program, err := expr.Compile(expression, expr.AllowUndefinedVariables())
	if err != nil {
		return nil, err
	}

	for c.ll.Len() >= c.max {
		back := c.ll.Back()
		if back == nil {
			break
		}
		ent := back.Value.(*lruEntry)
		delete(c.items, ent.key)
		c.ll.Remove(back)
	}

	ent := &lruEntry{key: expression, prog: program}
	el := c.ll.PushFront(ent)
	c.items[expression] = el
	return program, nil
}

func evalExpr(expression string, env map[string]any) (any, error) {
	program, err := globalExprCache.getOrCompile(expression)
	if err != nil {
		return nil, err
	}
	return expr.Run(program, env)
}
