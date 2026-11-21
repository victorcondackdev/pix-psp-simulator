package chaos

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
)

func dur(d time.Duration) *Duration { x := Duration(d); return &x }

func TestDurationJSON(t *testing.T) {
	var v struct{ D Duration }
	for in, want := range map[string]time.Duration{
		`{"D":"1.5s"}`: 1500 * time.Millisecond,
		`{"D":250}`:    250 * time.Millisecond,
	} {
		if err := json.Unmarshal([]byte(in), &v); err != nil || time.Duration(v.D) != want {
			t.Errorf("%s: %v %v", in, time.Duration(v.D), err)
		}
	}
	for _, bad := range []string{`{"D":"xx"}`, `{"D":true}`} {
		if err := json.Unmarshal([]byte(bad), &v); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
	b, _ := json.Marshal(Duration(2 * time.Second))
	if string(b) != `"2s"` {
		t.Errorf("marshal = %s", b)
	}
}

func TestRuleValidate(t *testing.T) {
	tests := []struct {
		name string
		r    Rule
		ok   bool
	}{
		{"empty", Rule{}, true},
		{"fail", Rule{Fail: iso.AM04}, true},
		{"bad code", Rule{Fail: "ZZ99"}, false},
		{"bad prob", Rule{Probability: 1.5}, false},
		{"bad op", Rule{Match: Match{Operation: "withdraw"}}, false},
		{"negative latency", Rule{Latency: dur(-time.Second)}, false},
		{"negative reorder", Rule{ReorderWebhooks: -1}, false},
		{"too many dups", Rule{DuplicateWebhooks: 11}, false},
	}
	for _, tt := range tests {
		if err := tt.r.Validate(); (err == nil) != tt.ok {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
}

func TestEngineEvaluate(t *testing.T) {
	e := New(1)
	mustAdd := func(r Rule) Rule {
		t.Helper()
		r, err := e.Add(r)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	big := mustAdd(Rule{Name: "big transfers fail", Match: Match{Operation: OpTransfer, MinAmount: 100_000}, Fail: iso.AM02})
	mustAdd(Rule{Match: Match{FromAccount: "slow"}, Latency: dur(5 * time.Second)})
	mustAdd(Rule{Match: Match{ToAccount: "flaky", MaxAmount: 500}, Timeout: true, DuplicateWebhooks: 2, ReorderWebhooks: Duration(time.Second)})
	if _, err := e.Add(Rule{ID: big.ID}); err == nil {
		t.Fatal("duplicate id accepted")
	}
	if _, err := e.Add(Rule{Fail: "nope"}); err == nil {
		t.Fatal("invalid rule accepted")
	}

	five := 5 * time.Second
	three := 3 * time.Second
	tests := []struct {
		name string
		op   Op
		ov   *Overrides
		want Decision
	}{
		{"nothing", Op{Operation: OpTransfer, Amount: 10}, nil, Decision{}},
		{"big", Op{Operation: OpTransfer, Amount: 100_000}, nil, Decision{Fail: iso.AM02, Rules: []string{"rule_1"}}},
		{"big refund not matched", Op{Operation: OpRefund, Amount: 100_000}, nil, Decision{}},
		{"slow", Op{Operation: OpTransfer, FromAccount: "slow", Amount: 1}, nil, Decision{Latency: &five, Rules: []string{"rule_2"}}},
		{"flaky", Op{Operation: OpTransfer, ToAccount: "flaky", Amount: 1}, nil, Decision{Fail: iso.AB03, Duplicates: 2, Reorder: time.Second, Rules: []string{"rule_3"}}},
		{"flaky too big", Op{Operation: OpTransfer, ToAccount: "flaky", Amount: 501}, nil, Decision{}},
		{"header wins", Op{Operation: OpTransfer, FromAccount: "slow", Amount: 1},
			&Overrides{Fail: iso.AC03, Latency: &three, Duplicates: 1, Reorder: 2 * time.Second},
			Decision{Fail: iso.AC03, Latency: &three, Duplicates: 1, Reorder: 2 * time.Second, Rules: []string{"rule_2", "header"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := e.Evaluate(tt.op, tt.ov)
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(tt.want)
			if string(a) != string(b) {
				t.Fatalf("got %s want %s", a, b)
			}
		})
	}
	if len(e.Rules()) != 3 || !e.Remove(big.ID) || e.Remove(big.ID) || len(e.Rules()) != 2 {
		t.Fatal("Remove")
	}
	e.Clear()
	if len(e.Rules()) != 0 {
		t.Fatal("Clear")
	}
}

func TestProbability(t *testing.T) {
	e := New(42)
	if _, err := e.Add(Rule{Probability: 0.3, Fail: iso.AB03}); err != nil {
		t.Fatal(err)
	}
	hits := 0
	for i := 0; i < 10_000; i++ {
		if e.Evaluate(Op{}, nil).Fail != "" {
			hits++
		}
	}
	if hits < 2700 || hits > 3300 {
		t.Fatalf("hits = %d, want about 3000", hits)
	}
	e.Clear()
	hits = 0
	for i := 0; i < 1000; i++ {
		if e.Evaluate(Op{}, &Overrides{Fail: iso.DS04, FailProbability: 0.5}).Fail != "" {
			hits++
		}
	}
	if hits < 400 || hits > 600 {
		t.Fatalf("header probability hits = %d", hits)
	}
	if f := e.Float(); f < 0 || f >= 1 {
		t.Fatal(f)
	}
	if e.Int63n(0) != 0 || e.Int63n(10) >= 10 {
		t.Fatal("Int63n")
	}
}

func TestFromHeaders(t *testing.T) {
	two := 2 * time.Second
	tests := []struct {
		name    string
		h       map[string]string
		want    *Overrides
		wantErr bool
	}{
		{"none", nil, nil, false},
		{"fail lowercase", map[string]string{HeaderFail: "am04"}, &Overrides{Fail: "AM04"}, false},
		{"bad fail", map[string]string{HeaderFail: "XX01"}, nil, true},
		{"timeout", map[string]string{HeaderTimeout: "true"}, &Overrides{Fail: iso.AB03}, false},
		{"timeout false", map[string]string{HeaderTimeout: "false"}, nil, false},
		{"bad timeout", map[string]string{HeaderTimeout: "maybe"}, nil, true},
		{"prob", map[string]string{HeaderFail: "DS04", HeaderFailProbability: "0.25"}, &Overrides{Fail: "DS04", FailProbability: 0.25}, false},
		{"bad prob", map[string]string{HeaderFailProbability: "2"}, nil, true},
		{"latency", map[string]string{HeaderLatency: "2s"}, &Overrides{Latency: &two}, false},
		{"bad latency", map[string]string{HeaderLatency: "soon"}, nil, true},
		{"dups", map[string]string{HeaderWebhookDuplicate: "3"}, &Overrides{Duplicates: 3}, false},
		{"bad dups", map[string]string{HeaderWebhookDuplicate: "99"}, nil, true},
		{"reorder", map[string]string{HeaderWebhookReorder: "2s"}, &Overrides{Reorder: two}, false},
		{"bad reorder", map[string]string{HeaderWebhookReorder: "-1s"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.h {
				h.Set(k, v)
			}
			got, err := FromHeaders(h)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			a, _ := json.Marshal(got)
			b, _ := json.Marshal(tt.want)
			if string(a) != string(b) {
				t.Fatalf("got %s want %s", a, b)
			}
		})
	}
}
