package sim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/victorcondackdev/brpay/pix"

	"github.com/victorcondackdev/pix-psp-simulator/internal/chaos"
	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
	"github.com/victorcondackdev/pix-psp-simulator/internal/iso"
	"github.com/victorcondackdev/pix-psp-simulator/internal/ledger"
	"github.com/victorcondackdev/pix-psp-simulator/internal/notify"
	"github.com/victorcondackdev/pix-psp-simulator/webhook"
)

var t0 = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

type hooks struct {
	mu     sync.Mutex
	events []webhook.Event
}

func (h *hooks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var ev webhook.Event
	_ = json.Unmarshal(b, &ev)
	h.mu.Lock()
	h.events = append(h.events, ev)
	h.mu.Unlock()
}

func (h *hooks) types(account string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, e := range h.events {
		if account == "" || e.AccountID == account {
			out = append(out, e.Type)
		}
	}
	return out
}

type env struct {
	*Sim
	clock *clock.Fake
	hooks *hooks
	srv   *httptest.Server
}

func newEnv(t *testing.T, cfg Config) *env {
	t.Helper()
	c := clock.NewFake(t0)
	ch := chaos.New(1)
	h := &hooks{}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	n := notify.New(c, srv.Client(), notify.Config{SignTime: c.Now}, ch.Int63n, nil)
	t.Cleanup(n.Close)
	if cfg.SettlementLatency == 0 {
		cfg.SettlementLatency = time.Second
	}
	s := New(cfg, Deps{Clock: c, Chaos: ch, Notify: n, Rand: rand.New(rand.NewSource(7))})
	e := &env{Sim: s, clock: c, hooks: h, srv: srv}
	sc, err := os.ReadFile(filepath.Join("..", "..", "scenarios", "basic.json"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseScenario(sc)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LoadScenario(parsed); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice", "bob", "padaria", "closed"} {
		if _, err := n.SetEndpoint(id, srv.URL+"/"+id, "s"); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *env) balance(t *testing.T, id string) int64 {
	t.Helper()
	a, err := e.Ledger.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return a.Balance
}

func (e *env) settle() { e.clock.Advance(time.Minute) }

func kindOf(err error) Kind {
	var se *Error
	if errors.As(err, &se) {
		return se.Kind
	}
	return 0
}

func TestCreateAccountValidation(t *testing.T) {
	e := newEnv(t, Config{})
	tests := []struct {
		name string
		r    AccountRequest
		kind Kind
	}{
		{"ok", AccountRequest{OwnerName: "Carol", Document: "11222333000181", BalanceCents: 10}, 0},
		{"no name", AccountRequest{Document: "11222333000181"}, KindInvalid},
		{"bad doc", AccountRequest{OwnerName: "X", Document: "123"}, KindInvalid},
		{"sys id", AccountRequest{ID: "sys.x", OwnerName: "X", Document: "11222333000181"}, KindInvalid},
		{"bad ispb", AccountRequest{OwnerName: "X", Document: "11222333000181", ISPB: "12"}, KindInvalid},
		{"bad branch", AccountRequest{OwnerName: "X", Document: "11222333000181", Branch: "1"}, KindInvalid},
		{"bad type", AccountRequest{OwnerName: "X", Document: "11222333000181", Type: "XXXX"}, KindInvalid},
		{"negative", AccountRequest{OwnerName: "X", Document: "11222333000181", BalanceCents: -1}, KindInvalid},
		{"dup", AccountRequest{ID: "alice", OwnerName: "X", Document: "11222333000181"}, KindConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := e.CreateAccount(tt.r)
			if kindOf(err) != tt.kind {
				t.Fatalf("err = %v", err)
			}
			if err == nil && (a.ISPB != "99999999" || a.Balance != 10 || !strings.HasPrefix(a.ID, "acc_")) {
				t.Fatalf("account = %+v", a)
			}
		})
	}
	if _, err := e.Deposit("alice", 0); kindOf(err) != KindInvalid {
		t.Fatal(err)
	}
	if _, err := e.Deposit("nobody", 1); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	if a, err := e.Deposit("bob", 5); err != nil || a.Balance != 10005 {
		t.Fatal(a, err)
	}
	if _, err := e.SetAccountStatus("bob", "WEIRD"); kindOf(err) != KindInvalid {
		t.Fatal(err)
	}
	if _, err := e.SetAccountStatus("nobody", ledger.Blocked); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	if _, err := e.Account(ledger.Funding); kindOf(err) != KindNotFound {
		t.Fatal("system account exposed")
	}
	if _, err := e.RegisterKey("nobody", "x@y.com", false); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	if _, err := e.OpenClaim("nobody", "OWNERSHIP", "x@y.com"); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	if e.Config().ISPB != "99999999" {
		t.Fatal("config")
	}
}

func TestTransferOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(e *env)
		req        TransferRequest
		ov         *chaos.Overrides
		wantErr    Kind
		wantStatus string
		wantReason string
		payerDelta int64
		payeeID    string
		payeeDelta int64
		payerHooks []string
	}{
		{name: "by key settles", req: TransferRequest{FromAccountID: "alice", Key: "39053344705", AmountCents: 1500},
			wantStatus: iso.StatusSettled, payerDelta: -1500, payeeID: "bob", payeeDelta: 1500, payerHooks: []string{webhook.EventPixSettled}},
		{name: "by account id", req: TransferRequest{FromAccountID: "alice", ToAccountID: "padaria", AmountCents: 1},
			wantStatus: iso.StatusSettled, payerDelta: -1, payeeID: "padaria", payeeDelta: 1},
		{name: "by account details", req: TransferRequest{FromAccountID: "alice", ToAccount: &AccountRef{ISPB: "12345678", Branch: "0042", Number: "778899"}, AmountCents: 2},
			wantStatus: iso.StatusSettled, payerDelta: -2, payeeID: "padaria", payeeDelta: 2},
		{name: "unknown account details AC03", req: TransferRequest{FromAccountID: "alice", ToAccount: &AccountRef{ISPB: "12345678", Branch: "0042", Number: "1"}, AmountCents: 2},
			wantStatus: iso.StatusRejected, wantReason: iso.AC03, payerHooks: []string{webhook.EventPixRejected}},
		{name: "insufficient AM04", req: TransferRequest{FromAccountID: "bob", Key: "alice@example.com", AmountCents: 10001},
			wantStatus: iso.StatusRejected, wantReason: iso.AM04, payeeID: "alice"},
		{name: "closed payee AC04", req: TransferRequest{FromAccountID: "alice", Key: "closed@example.com", AmountCents: 10},
			wantStatus: iso.StatusRejected, wantReason: iso.AC04},
		{name: "blocked payee AC06", setup: func(e *env) { _, _ = e.SetAccountStatus("bob", ledger.Blocked) },
			req: TransferRequest{FromAccountID: "alice", ToAccountID: "bob", AmountCents: 10}, wantStatus: iso.StatusRejected, wantReason: iso.AC06},
		{name: "header fail", req: TransferRequest{FromAccountID: "alice", ToAccountID: "bob", AmountCents: 10}, ov: &chaos.Overrides{Fail: iso.DS04},
			wantStatus: iso.StatusRejected, wantReason: iso.DS04},
		{name: "rule fail", setup: func(e *env) { _, _ = e.Chaos.Add(chaos.Rule{Match: chaos.Match{FromAccount: "alice"}, Timeout: true}) },
			req: TransferRequest{FromAccountID: "alice", ToAccountID: "bob", AmountCents: 10}, wantStatus: iso.StatusRejected, wantReason: iso.AB03},
		{name: "unknown key", req: TransferRequest{FromAccountID: "alice", Key: "nobody@example.com", AmountCents: 10}, wantErr: KindNotFound},
		{name: "invalid key", req: TransferRequest{FromAccountID: "alice", Key: "nope", AmountCents: 10}, wantErr: KindInvalid},
		{name: "two destinations", req: TransferRequest{FromAccountID: "alice", Key: "39053344705", ToAccountID: "bob", AmountCents: 10}, wantErr: KindInvalid},
		{name: "no destination", req: TransferRequest{FromAccountID: "alice", AmountCents: 10}, wantErr: KindInvalid},
		{name: "bad account ref", req: TransferRequest{FromAccountID: "alice", ToAccount: &AccountRef{ISPB: "1"}, AmountCents: 10}, wantErr: KindInvalid},
		{name: "unknown to_account_id", req: TransferRequest{FromAccountID: "alice", ToAccountID: "ghost", AmountCents: 10}, wantErr: KindNotFound},
		{name: "self", req: TransferRequest{FromAccountID: "alice", Key: "alice@example.com", AmountCents: 10}, wantErr: KindInvalid},
		{name: "zero", req: TransferRequest{FromAccountID: "alice", ToAccountID: "bob"}, wantErr: KindInvalid},
		{name: "long description", req: TransferRequest{FromAccountID: "alice", ToAccountID: "bob", AmountCents: 1, Description: strings.Repeat("x", 141)}, wantErr: KindInvalid},
		{name: "unknown payer", req: TransferRequest{FromAccountID: "ghost", ToAccountID: "bob", AmountCents: 1}, wantErr: KindNotFound},
		{name: "closed payer", req: TransferRequest{FromAccountID: "closed", ToAccountID: "bob", AmountCents: 1}, wantErr: KindInvalid},
		{name: "bad e2e", req: TransferRequest{FromAccountID: "alice", ToAccountID: "bob", AmountCents: 1, EndToEndID: "E123"}, wantErr: KindInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, Config{})
			if tt.setup != nil {
				tt.setup(e)
			}
			payer0 := e.balance(t, "alice")
			if tt.req.FromAccountID == "bob" {
				payer0 = e.balance(t, "bob")
			}
			var payee0 int64
			if tt.payeeID != "" {
				payee0 = e.balance(t, tt.payeeID)
			}
			tr, err := e.CreateTransfer(tt.req, tt.ov)
			if kindOf(err) != tt.wantErr {
				t.Fatalf("err = %v, want kind %d", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if tr.Status != iso.StatusPending || len(tr.EndToEndID) != pix.EndToEndIDLength {
				t.Fatalf("initial = %+v", tr)
			}
			if !tr.SettleAt.Equal(t0.Add(time.Second)) {
				t.Fatalf("settle at %v", tr.SettleAt)
			}
			e.settle()
			got, _ := e.Transfer(tr.EndToEndID)
			if got.Status != tt.wantStatus || got.Reason != tt.wantReason {
				t.Fatalf("final = %s %s, want %s %s", got.Status, got.Reason, tt.wantStatus, tt.wantReason)
			}
			if got.SettledAt == nil {
				t.Fatal("settled_at missing")
			}
			payer1 := e.balance(t, tt.req.FromAccountID)
			if payer1-payer0 != tt.payerDelta {
				t.Fatalf("payer delta %d, want %d", payer1-payer0, tt.payerDelta)
			}
			if tt.payeeID != "" && e.balance(t, tt.payeeID)-payee0 != tt.payeeDelta {
				t.Fatalf("payee delta %d", e.balance(t, tt.payeeID)-payee0)
			}
			if tt.payerHooks != nil {
				if got := e.hooks.types(tt.req.FromAccountID); fmt.Sprint(got) != fmt.Sprint(tt.payerHooks) {
					t.Fatalf("payer hooks = %v", got)
				}
			}
			if e.Ledger.Total() != 0 || e.balance(t, ledger.Settlement) != 0 {
				t.Fatalf("ledger not balanced: total=%d settlement=%d", e.Ledger.Total(), e.balance(t, ledger.Settlement))
			}
		})
	}
}

func TestTransferMisc(t *testing.T) {
	e := newEnv(t, Config{MaxTransferCents: 100})
	e2e, _ := pix.NewEndToEndID("99999999", t0, nil)
	tr, err := e.CreateTransfer(TransferRequest{FromAccountID: "alice", ToAccountID: "bob", AmountCents: 101, EndToEndID: e2e}, nil)
	if err != nil || tr.EndToEndID != e2e {
		t.Fatal(tr, err)
	}
	if _, err := e.CreateTransfer(TransferRequest{FromAccountID: "alice", ToAccountID: "bob", AmountCents: 1, EndToEndID: e2e}, nil); kindOf(err) != KindConflict {
		t.Fatalf("duplicate e2e: %v", err)
	}
	lat := 10 * time.Second
	slow, _ := e.CreateTransfer(TransferRequest{FromAccountID: "alice", ToAccountID: "bob", AmountCents: 1}, &chaos.Overrides{Latency: &lat})
	e.clock.Advance(5 * time.Second)
	if got, _ := e.Transfer(slow.EndToEndID); got.Status != iso.StatusPending {
		t.Fatalf("settled before latency: %s", got.Status)
	}
	e.clock.Advance(5 * time.Second)
	if got, _ := e.Transfer(slow.EndToEndID); got.Status != iso.StatusSettled {
		t.Fatalf("not settled: %s", got.Status)
	}
	if got, _ := e.Transfer(e2e); got.Reason != iso.AM02 {
		t.Fatalf("max transfer: %+v", got)
	}
	if _, err := e.Transfer("nope"); kindOf(err) != KindNotFound {
		t.Fatal(err)
	}
	if n := len(e.Transfers("", 0)); n != 2 {
		t.Fatalf("transfers = %d", n)
	}
	if n := len(e.Transfers("alice", 1)); n != 1 {
		t.Fatalf("limit = %d", n)
	}
	if n := len(e.Transfers("padaria", 0)); n != 0 {
		t.Fatalf("filter = %d", n)
	}
	st := e.Stats()
	if st.Transfers != 2 || st.Accounts != 4 || st.LedgerTotal != 0 || st.Keys != 6 {
		t.Fatalf("stats = %+v", st)
	}
	e.Wait()
}

func TestWebhookChaosDuplicates(t *testing.T) {
	e := newEnv(t, Config{})
	_, err := e.CreateTransfer(TransferRequest{FromAccountID: "alice", Key: "12ABC34501DE35", AmountCents: 100}, &chaos.Overrides{Duplicates: 2})
	if err != nil {
		t.Fatal(err)
	}
	e.settle()
	if got := e.hooks.types("padaria"); len(got) != 3 || got[0] != webhook.EventPixReceived {
		t.Fatalf("payee hooks = %v", got)
	}
	ids := map[string]bool{}
	for _, ev := range e.hooks.events {
		if ev.AccountID == "padaria" {
			ids[ev.ID] = true
		}
	}
	if len(ids) != 1 {
		t.Fatalf("duplicates must share the event id: %v", ids)
	}
}

// Many concurrent transfers in all directions: no negative balance and the
// ledger stays balanced, including money in flight.
func TestConcurrentTransfersConserveMoney(t *testing.T) {
	c := clock.NewVirtual()
	ch := chaos.New(3)
	n := notify.New(c, nil, notify.Config{}, ch.Int63n, nil)
	defer n.Close()
	s := New(Config{SettlementLatency: time.Millisecond}, Deps{Clock: c, Chaos: ch, Notify: n})
	docs := []string{"52998224725", "39053344705", "11144477735", "11222333000181", "12ABC34501DE35"}
	var ids []string
	var start int64
	for i, d := range docs {
		a, err := s.CreateAccount(AccountRequest{OwnerName: fmt.Sprintf("u%d", i), Document: d, BalanceCents: 50_000})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
		start += a.Balance
	}
	if _, err := ch.Add(chaos.Rule{Probability: 0.1, Fail: iso.DS04}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for w := 0; w < 20; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < 100; i++ {
				from, to := ids[r.Intn(len(ids))], ids[r.Intn(len(ids))]
				if from == to {
					continue
				}
				_, _ = s.CreateTransfer(TransferRequest{FromAccountID: from, ToAccountID: to, AmountCents: int64(r.Intn(20_000) + 1)}, nil)
				if t.Failed() {
					return
				}
			}
		}(int64(w))
	}
	// Check invariants while transfers are in flight.
	for i := 0; i < 50; i++ {
		if s.Ledger.Total() != 0 {
			t.Fatal("ledger unbalanced mid-flight")
		}
		time.Sleep(time.Millisecond)
	}
	wg.Wait()
	s.Wait()
	var end int64
	for _, a := range s.Ledger.List(false) {
		if a.Balance < 0 {
			t.Fatalf("negative balance: %+v", a)
		}
		end += a.Balance
	}
	if end != start || s.Ledger.Total() != 0 {
		t.Fatalf("money not conserved: start=%d end=%d total=%d", start, end, s.Ledger.Total())
	}
	if st, _ := s.Ledger.Get(ledger.Settlement); st.Balance != 0 {
		t.Fatalf("settlement account not drained: %d", st.Balance)
	}
	for _, tr := range s.Transfers("", 0) {
		if tr.Status == iso.StatusPending {
			t.Fatalf("transfer still pending: %+v", tr)
		}
	}
}

func TestScenarioErrors(t *testing.T) {
	e := newEnv(t, Config{})
	tests := []struct {
		name string
		sc   Scenario
	}{
		{"no id", Scenario{Accounts: []ScenarioAccount{{AccountRequest: AccountRequest{OwnerName: "x", Document: "11222333000181"}}}}},
		{"bad account", Scenario{Accounts: []ScenarioAccount{{AccountRequest: AccountRequest{ID: "x", OwnerName: "x", Document: "1"}}}}},
		{"bad status", Scenario{Accounts: []ScenarioAccount{{AccountRequest: AccountRequest{ID: "y", OwnerName: "y", Document: "11222333000181"}, Status: "WEIRD"}}}},
		{"bad webhook", Scenario{Accounts: []ScenarioAccount{{AccountRequest: AccountRequest{ID: "z", OwnerName: "z", Document: "11222333000181"}, Webhook: &ScenarioWebhook{URL: "nope"}}}}},
		{"bad key", Scenario{Keys: []ScenarioKey{{AccountID: "alice", Key: "nope"}}}},
		{"bad rule", Scenario{ChaosRules: []chaos.Rule{{Fail: "XXXX"}}}},
	}
	for _, tt := range tests {
		if err := e.LoadScenario(tt.sc); err == nil {
			t.Errorf("%s: expected error", tt.name)
		}
	}
	if _, err := ParseScenario([]byte(`{"name":"x","unknown":1}`)); err == nil {
		t.Error("unknown field accepted")
	}
}

func TestLoadScenarioFiles(t *testing.T) {
	c := clock.NewFake(t0)
	s := New(Config{}, Deps{Clock: c})
	names, err := s.LoadScenarioFiles(filepath.Join("..", "..", "scenarios", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(names) != "[basic chaos]" {
		t.Fatalf("names = %v", names)
	}
	if len(s.Chaos.Rules()) != 3 || s.DICT.Limiter.Config().Capacity != 100 {
		t.Fatal("scenario not applied")
	}
	if _, err := s.LoadScenarioFiles("does-not-exist.json"); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := s.LoadScenarioFiles("[bad"); err == nil {
		t.Fatal("bad glob accepted")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`{`), 0o600)
	if _, err := s.LoadScenarioFiles(bad); err == nil {
		t.Fatal("bad json accepted")
	}
	dup := filepath.Join(dir, "dup.json")
	_ = os.WriteFile(dup, []byte(`{"name":"dup","accounts":[{"id":"alice","owner_name":"A","document":"52998224725"}]}`), 0o600)
	if _, err := s.LoadScenarioFiles(dup); err == nil {
		t.Fatal("duplicate account accepted")
	}
}
