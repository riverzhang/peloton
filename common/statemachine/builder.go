package statemachine

import "github.com/pkg/errors"

// Builder is the state machine builder
type Builder struct {
	statemachine       StateMachine
	rules              map[State]*Rule
	current            State
	name               string
	transitionCallback Callback
}

// NewBuilder creates new state machine builder
func NewBuilder() *Builder {
	return &Builder{
		statemachine: &statemachine{},
		rules:        make(map[State]*Rule),
	}
}

// WithName adds the name to state machine
func (b *Builder) WithName(name string) *Builder {
	b.name = name
	return b
}

// WithCurrentState adds the current state
func (b *Builder) WithCurrentState(current State) *Builder {
	b.current = current
	return b
}

// AddRule adds the rule for state machine
func (b *Builder) AddRule(rule *Rule) *Builder {
	b.rules[rule.from] = rule
	return b
}

// WithTransitionCallback adds the transition call back
func (b *Builder) WithTransitionCallback(callback Callback) *Builder {
	b.transitionCallback = callback
	return b
}

// Build builds the state machine
func (b *Builder) Build() (StateMachine, error) {
	var err error
	b.statemachine, err = NewStateMachine(
		b.name,
		b.current,
		b.rules,
		b.transitionCallback,
	)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return b.statemachine, nil
}