package agent

import (
	"context"
	"errors"
	"testing"
)

type closableEngine struct {
	closed bool
	err    error
}

func (e *closableEngine) Execute(
	context.Context, Run, Host, *Board,
) (*Board, error) {
	return nil, nil
}

func (e *closableEngine) Close() error {
	e.closed = true
	return e.err
}

type closablePreparer struct {
	closed bool
	err    error
}

func (p *closablePreparer) Before(
	context.Context, Identity, *Request, *Board,
) (*Board, error) {
	return nil, nil
}

func (p *closablePreparer) Close() error {
	p.closed = true
	return p.err
}

type closableObserver struct {
	BaseObserver
	closed bool
	err    error
}

func (o *closableObserver) Close() error {
	o.closed = true
	return o.err
}

type closableReferee struct {
	closed bool
	err    error
}

func (r *closableReferee) After(
	context.Context, Identity, *Request, *Result,
) (Decision, error) {
	return Decision{}, nil
}

func (r *closableReferee) Close() error {
	r.closed = true
	return r.err
}

type closableCommitter struct {
	closed bool
	err    error
}

func (c *closableCommitter) Commit(
	context.Context, Identity, *Request, *Result,
) error {
	return nil
}

func (c *closableCommitter) Close() error {
	c.closed = true
	return c.err
}

func TestAgentCloseClosesComponents(t *testing.T) {
	engine := &closableEngine{}
	prepare := &closablePreparer{}
	observe := &closableObserver{}
	referee := &closableReferee{}
	commit := &closableCommitter{}

	a := &Agent{
		Engine:   engine,
		Prepare:  []Preparer{prepare},
		Observe:  []Observer{observe},
		Referees: []Referee{referee},
		Commit:   []Committer{commit},
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	for name, closed := range map[string]bool{
		"engine":  engine.closed,
		"prepare": prepare.closed,
		"observe": observe.closed,
		"referee": referee.closed,
		"commit":  commit.closed,
	} {
		if !closed {
			t.Errorf("%s was not closed", name)
		}
	}
}

func TestAgentCloseAggregatesErrors(t *testing.T) {
	engineErr := errors.New("engine boom")
	hookErr := errors.New("hook boom")
	a := &Agent{
		Engine: &closableEngine{err: engineErr},
		Observe: []Observer{
			&closableObserver{err: hookErr},
			&closableObserver{},
		},
	}
	err := a.Close()
	if err == nil {
		t.Fatal("Close() error = nil, want aggregated errors")
	}
	if !errors.Is(err, engineErr) || !errors.Is(err, hookErr) {
		t.Fatalf("Close() error = %v, want both component errors", err)
	}
}

func TestAgentCloseSkipsNilAndTypedNil(t *testing.T) {
	a := &Agent{
		Engine:  EngineFunc(nil),
		Prepare: []Preparer{nil},
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	var nilAgent *Agent
	if err := nilAgent.Close(); err != nil {
		t.Fatalf("nil receiver Close() error = %v, want nil", err)
	}
}
