package fsm

import (
	"errors"
	"testing"
)

func door() *Machine[string, string] {
	return New([]Transition[string, string]{
		{"closed", "open", "opened"},
		{"opened", "close", "closed"},
		{"closed", "lock", "locked"},
		{"locked", "destroy", "gone"},
	}, "gone")
}

func TestNext(t *testing.T) {
	m := door()
	tests := []struct {
		from, ev, want string
		ok             bool
	}{
		{"closed", "open", "opened", true},
		{"opened", "close", "closed", true},
		{"closed", "lock", "locked", true},
		{"opened", "lock", "opened", false},
		{"gone", "open", "gone", false},
	}
	for _, tt := range tests {
		got, err := m.Next(tt.from, tt.ev)
		if got != tt.want || (err == nil) != tt.ok {
			t.Errorf("Next(%q,%q) = %q,%v", tt.from, tt.ev, got, err)
		}
		if err != nil {
			var it *ErrInvalidTransition[string, string]
			if !errors.As(err, &it) || it.From != tt.from {
				t.Errorf("error type %T", err)
			}
			if err.Error() == "" {
				t.Error("empty message")
			}
		}
	}
	if ev := m.Events("closed"); len(ev) != 2 || ev[0] != "lock" || ev[1] != "open" {
		t.Errorf("Events = %v", ev)
	}
	if !m.Terminal("gone") || m.Terminal("closed") {
		t.Error("Terminal wrong")
	}
}

func TestNewPanics(t *testing.T) {
	for name, f := range map[string]func(){
		"duplicate": func() {
			New([]Transition[int, int]{{1, 1, 2}, {1, 1, 3}})
		},
		"terminal": func() {
			New([]Transition[int, int]{{1, 1, 2}}, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			f()
		})
	}
}
