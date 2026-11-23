package sta

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/bizday"
)

type books struct {
	now      time.Time
	balances map[string]int64
	volume   int64
}

func (b *books) BalanceAt(id string, _ time.Time) (int64, error) {
	v, ok := b.balances[id]
	if !ok {
		return 0, fmt.Errorf("unknown %s", id)
	}
	return v, nil
}
func (b *books) Volume(string, time.Time, time.Time) int64 { return b.volume }
func (b *books) Now() time.Time                            { return b.now }

func at(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, bizday.Brasilia)
}

func doc4111(dataBase, tipo string, lines ...string) []byte {
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<documento codigoDocumento="4111" cnpj="12345678" dataBase="` + dataBase + `" tipoRemessa="` + tipo + `"><contas>` +
		strings.Join(lines, "") + `</contas></documento>`)
}

func line(conta, saldo string) string {
	return `<conta codigoConta="` + conta + `" saldoDia="` + saldo + `"/>`
}

func newSTA(t *testing.T) (*Service, *books) {
	t.Helper()
	b := &books{now: at(2026, 11, 23, 10), balances: map[string]int64{"psp": 150000}, volume: 98765}
	s := New(b, bizday.Calendar{}, Config{})
	if _, err := s.PutParticipant(Participant{
		CNPJ: "12345678", ISPB: "99999999", Reconcile: []Mapping{{Conta: "1120000006", AccountIDs: []string{"psp"}}},
		ReconcileVolume: true, Since: "2026-10",
	}); err != nil {
		t.Fatal(err)
	}
	return s, b
}

func codes(p Protocol) []string {
	var out []string
	for _, e := range p.Errors {
		out = append(out, e.Code)
	}
	return out
}

func submit(t *testing.T, s *Service, raw []byte, want string, wantCodes ...string) Protocol {
	t.Helper()
	p, err := s.Submit(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != want || fmt.Sprint(codes(p)) != fmt.Sprint(wantCodes) && len(wantCodes) > 0 {
		t.Fatalf("status %s errors %v, want %s %v", p.Status, p.Errors, want, wantCodes)
	}
	return p
}

func TestSubmit4111(t *testing.T) {
	s, b := newSTA(t)
	var hooked []string
	s.OnSubmit(func(doc, st string) { hooked = append(hooked, doc+":"+st) })
	good := []string{line("1120000006", "1500.00"), line("3097010000", "987.65")}

	// Thursday 19/11 (Friday 20/11 is a national holiday); received Monday 23/11.
	p := submit(t, s, doc4111("2026-11-19", "I", good...), Accepted)
	if p.Deadline != "2026-11-25" || p.Late || !strings.HasPrefix(p.ID, "STA20261123") || len(p.SHA256) != 64 || p.Lines != 2 {
		t.Fatalf("%+v", p)
	}
	submit(t, s, doc4111("2026-11-19", "I", good...), Rejected, "E020")
	r := submit(t, s, doc4111("2026-11-19", "S", good...), Accepted)
	if r.Replaces != p.ID {
		t.Error(r.Replaces)
	}
	if old, _ := s.Protocol(p.ID); old.ReplacedBy != r.ID {
		t.Error(old.ReplacedBy)
	}
	submit(t, s, doc4111("2026-11-18", "S", good...), Rejected, "E021")
	submit(t, s, doc4111("2026-11-20", "I", good...), Rejected, "E032")
	submit(t, s, doc4111("2026-11-21", "I", good...), Rejected, "E032")
	submit(t, s, doc4111("2026-11-23", "I", good...), Rejected, "E031")
	submit(t, s, doc4111("23/11/2026", "I", good...), Rejected, "E030")

	// Divergent balance and volume.
	submit(t, s, doc4111("2026-11-18", "I", line("1120000006", "1499.99"), line("3097010000", "1.00")), Rejected, "E040", "E041")

	// Sequence: an older date (18/11) may still be sent, but a later date
	// needs the previous business day accepted first.
	submit(t, s, doc4111("2026-11-18", "I", good...), Accepted)
	b.now = at(2026, 11, 27, 9)
	submit(t, s, doc4111("2026-11-25", "I", good...), Rejected, "E033")
	submit(t, s, doc4111("2026-11-23", "I", good...), Accepted)
	l := submit(t, s, doc4111("2026-11-24", "I", good...), Accepted)
	if l.Deadline != "2026-11-27" || l.Late {
		t.Errorf("%+v", l)
	}
	b.now = at(2026, 12, 1, 9)
	if late := submit(t, s, doc4111("2026-11-25", "I", good...), Accepted); !late.Late {
		t.Error("late flag")
	}
	if len(hooked) == 0 || hooked[0] != "4111:ACEITO" {
		t.Error(hooked)
	}
	if got := s.Protocols("12345678", Doc4111); len(got) != 14 || got[0].DataBase != "2026-11-25" {
		t.Error(len(got))
	}
	if len(s.Protocols("", Doc4010)) != 0 {
		t.Error("filter")
	}
}

func TestSubmitLayoutErrors(t *testing.T) {
	s, _ := newSTA(t)
	raw := `<documento codigoDocumento="9999" cnpj="123" dataBase="2026-11-19" tipoRemessa="X"><contas>` +
		line("1.1.2.00", "1") + line("1120000006", "1,00") + line("1120000006", "1.00") + `</contas></documento>`
	submit(t, s, []byte(raw), Rejected, "E001", "E002", "E003", "E010", "E012", "E011")
	if _, err := s.Submit([]byte("not xml")); err == nil {
		t.Error("not xml")
	}
	if _, err := s.Submit([]byte(`<?xml version="1.0" encoding="ISO-8859-1"?><documento/>`)); err == nil {
		t.Error("charset")
	}
	if _, err := s.Protocol("nope"); err == nil {
		t.Error("protocol")
	}
}

func TestParseCents(t *testing.T) {
	for in, want := range map[string]int64{"0.00": 0, "1234.56": 123456, "-0.10": -10, "9999999999999999.99": 999999999999999999} {
		if got, ok := parseCents(in); !ok || got != want {
			t.Errorf("%s: %d %v", in, got, ok)
		}
	}
	for _, bad := range []string{"1", "1.0", "1,00", "", "12345678901234567.00", "+1.00"} {
		if _, ok := parseCents(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if FormatCents(-5) != "-0.05" || FormatCents(123456) != "1234.56" {
		t.Error("format")
	}
}

func doc4010(month, tipo string, lines ...string) []byte {
	return []byte(`<documento codigoDocumento="4010" cnpj="12345678" dataBase="` + month + `" tipoRemessa="` + tipo + `"><contas>` +
		strings.ReplaceAll(strings.Join(lines, ""), "saldoDia", "saldo") + `</contas></documento>`)
}

func TestSubmit4010AndEnquadramento(t *testing.T) {
	s, b := newSTA(t)
	// Since 2026-10; on 23/11 the October 4010 is owed but not yet late
	// (due on the last business day of November, 30/11).
	st, _ := s.Status("12345678")
	if st.Desenquadrado || fmt.Sprint(st.Missing4010) != "[2026-10]" {
		t.Fatalf("%+v", st)
	}
	submit(t, s, doc4010("2026-11", "I", line("1120000006", "1.00")), Rejected, "E031")
	submit(t, s, doc4010("10/2026", "I"), Rejected, "E030")

	b.now = at(2026, 12, 1, 9)
	st, _ = s.Status("12345678")
	if !st.Desenquadrado {
		t.Fatal("October 4010 is past due")
	}
	if s.Blocked("99999999") {
		t.Fatal("enforce is off")
	}
	p, _ := s.Participant("12345678")
	p.Enforce = true
	if _, err := s.PutParticipant(p); err != nil {
		t.Fatal(err)
	}
	if !s.Blocked("99999999") {
		t.Fatal("enforced and desenquadrado")
	}

	// The 4111 of 30/10 (last business day of October) and the 4010 must agree.
	b.now = at(2026, 11, 3, 9)
	submit(t, s, doc4111("2026-10-30", "I", line("1120000006", "1500.00"), line("3097010000", "987.65")), Accepted)
	b.now = at(2026, 12, 1, 9)
	submit(t, s, doc4010("2026-10", "I", line("1120000006", "1400.00")), Rejected, "E042")
	late := submit(t, s, doc4010("2026-10", "I", line("1120000006", "1500.00"), line("4111000006", "-1500.00")), Accepted)
	if late.Deadline != "2026-11-30" || !late.Late {
		t.Errorf("%+v", late)
	}
	if s.Blocked("99999999") {
		t.Fatal("4010 accepted: enquadrado again")
	}
	st, _ = s.Status("12345678")
	if st.LastAccepted != "2026-10-30" || len(st.Missing4111) == 0 || st.Missing4111[0] != "2026-11-03" {
		t.Errorf("%+v", st)
	}
	// A replacement 4111 for 30/10 is checked against the accepted 4010.
	submit(t, s, doc4111("2026-10-30", "S", line("1120000006", "1400.00"), line("3097010000", "987.65")), Rejected, "E040", "E042")
}

func TestParticipants(t *testing.T) {
	s, _ := newSTA(t)
	for _, bad := range []Participant{
		{CNPJ: "1"}, {CNPJ: "12345678", ISPB: "x"},
		{CNPJ: "12345678", Reconcile: []Mapping{{Conta: "1"}}},
		{CNPJ: "12345678", Reconcile: []Mapping{{Conta: "1120000006", AccountIDs: []string{"ghost"}}}},
		{CNPJ: "12345678", ReconcileVolume: true}, {CNPJ: "12345678", Enforce: true},
		{CNPJ: "12345678", Since: "2026"},
	} {
		if _, err := s.PutParticipant(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	p, err := s.PutParticipant(Participant{CNPJ: "abcd1234"})
	if err != nil || p.CNPJ != "ABCD1234" || p.Since != "2026-11" {
		t.Fatal(p, err)
	}
	if len(s.Participants()) != 2 {
		t.Error("list")
	}
	if _, err := s.Participant("zz"); err == nil {
		t.Error("unknown")
	}
	if _, err := s.Status("zz"); err == nil {
		t.Error("unknown status")
	}
}

func TestDraft4111(t *testing.T) {
	s, b := newSTA(t)
	raw, err := s.Draft4111("12345678", "2026-11-19")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `codigoConta="1120000006" saldoDia="1500.00"`) || !strings.Contains(string(raw), `saldoDia="987.65"`) {
		t.Fatal(string(raw))
	}
	// The draft is accepted as is.
	submit(t, s, raw, Accepted)
	_ = b
	if _, err := s.Draft4111("12345678", "19/11"); err == nil {
		t.Error("bad date")
	}
	if _, err := s.Draft4111("nope", "2026-11-19"); err == nil {
		t.Error("unknown")
	}
	if s.Calendar().IsBusinessDay(at(2026, 11, 20, 0)) {
		t.Error("calendar")
	}
}
