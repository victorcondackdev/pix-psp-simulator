// Package fsm is a tiny table-driven finite state machine. Each flow in the
// simulator (DICT claims, MED infractions) declares its transitions as data,
// which keeps the rules reviewable in one place and lets the README and tests
// enumerate them.
package fsm

import (
	"fmt"
	"sort"
)

// Transition moves from one state to another on an event.
type Transition[S, E comparable] struct {
	From  S
	Event E
	To    S
}

// Machine is an immutable set of transitions.
type Machine[S, E comparable] struct {
	table    map[S]map[E]S
	terminal map[S]bool
}

// New builds a machine. States listed in terminal accept no events.
// Duplicate (From, Event) pairs panic, as they are programming errors.
func New[S, E comparable](transitions []Transition[S, E], terminal ...S) *Machine[S, E] {
	m := &Machine[S, E]{table: map[S]map[E]S{}, terminal: map[S]bool{}}
	for _, s := range terminal {
		m.terminal[s] = true
	}
	for _, t := range transitions {
		if m.terminal[t.From] {
			panic(fmt.Sprintf("fsm: transition out of terminal state %v", t.From))
		}
		if m.table[t.From] == nil {
			m.table[t.From] = map[E]S{}
		}
		if _, dup := m.table[t.From][t.Event]; dup {
			panic(fmt.Sprintf("fsm: duplicate transition %v --%v-->", t.From, t.Event))
		}
		m.table[t.From][t.Event] = t.To
	}
	return m
}

// ErrInvalidTransition is returned when an event is not allowed in a state.
type ErrInvalidTransition[S, E comparable] struct {
	From    S
	Event   E
	Allowed []E
}

func (e *ErrInvalidTransition[S, E]) Error() string {
	return fmt.Sprintf("event %v is not allowed in state %v (allowed: %v)", e.Event, e.From, e.Allowed)
}

// Next returns the state reached from s on event e.
func (m *Machine[S, E]) Next(s S, e E) (S, error) {
	if to, ok := m.table[s][e]; ok {
		return to, nil
	}
	return s, &ErrInvalidTransition[S, E]{From: s, Event: e, Allowed: m.Events(s)}
}

// Events lists the events accepted in state s, sorted by their string form.
func (m *Machine[S, E]) Events(s S) []E {
	out := make([]E, 0, len(m.table[s]))
	for e := range m.table[s] {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return fmt.Sprint(out[i]) < fmt.Sprint(out[j]) })
	return out
}

// Terminal reports whether s is a terminal state.
func (m *Machine[S, E]) Terminal(s S) bool { return m.terminal[s] }
