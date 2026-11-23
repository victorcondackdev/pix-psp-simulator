// Package sta simulates the receiving side of the Banco Central's Sistema de
// Transmissão de Arquivos (STA) for two accounting documents of the Catálogo
// de Documentos (CADOC):
//
//   - Documento 4111, Saldos Contábeis Diários: one file per business day,
//     due by the 3rd business day after the data-base, accepted only in
//     sequence, with the cumulative monthly volume of payment transactions in
//     the 3.0.9.70 accounts;
//   - Documento 4010, Balancete Patrimonial Analítico: one file per month,
//     whose balances must match the 4111 of the month's last business day.
//
// Each submission gets a protocol, is validated and is ACEITO or REJEITADO
// with a list of errors, like the STA's processing report. When a participant
// is registered with a mapping from Cosif accounts to simulator accounts, the
// reported balances and the payment volume are reconciled against the
// simulator's own books, which is the check a supervisor would run.
//
// The header fields, the 10-digit account codes, the amount format and the
// deadline, sequence and replacement rules follow the published filling
// instructions of the 4111. The XML element names are the simulator's
// (<documento> with <conta> children), not the official XSD; validate the
// real file against the BCB schema before sending it to the real STA.
package sta

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/victorcondackdev/pix-psp-simulator/internal/bizday"
)

// Document codes.
const (
	Doc4111 = "4111" // Saldos Contábeis Diários
	Doc4010 = "4010" // Balancete Patrimonial Analítico
)

// Submission types.
const (
	Initial     = "I" // first submission for a data-base
	Replacement = "S" // replaces an accepted document
)

// Statuses of a protocol.
const (
	Accepted = "ACEITO"
	Rejected = "REJEITADO"
)

// VolumePrefix is the Cosif group of the payment transaction volume accounts
// (3.0.9.70.XX.00-X) in the 4111.
const VolumePrefix = "30970"

var (
	cnpjRE   = regexp.MustCompile(`^[0-9A-Z]{8}$`)
	contaRE  = regexp.MustCompile(`^[0-9]{10}$`)
	amountRE = regexp.MustCompile(`^-?[0-9]{1,16}\.[0-9]{2}$`)
)

// Books is what the STA reads from the simulator to reconcile a document.
type Books interface {
	// BalanceAt returns an account's balance at instant t.
	BalanceAt(accountID string, t time.Time) (int64, error)
	// Volume returns the sum of the absolute amounts of the payment
	// transactions settled in [from, to] in which an account of ispb was
	// payer or payee.
	Volume(ispb string, from, to time.Time) int64
	// Now is the simulator clock.
	Now() time.Time
}

// Mapping reconciles one Cosif account with the sum of simulator accounts.
type Mapping struct {
	Conta      string   `json:"conta"`
	AccountIDs []string `json:"account_ids"`
}

// Participant is an institution that reports to the STA.
type Participant struct {
	CNPJ string `json:"cnpj"` // CNPJ base, 8 characters
	ISPB string `json:"ispb,omitempty"`
	Name string `json:"name,omitempty"`
	// Reconcile maps Cosif accounts to simulator accounts whose balances
	// must equal the reported saldoDia at the end of the data-base.
	Reconcile []Mapping `json:"reconcile,omitempty"`
	// ReconcileVolume checks the 3.0.9.70 accounts against the Pix volume of
	// the participant's ISPB.
	ReconcileVolume bool `json:"reconcile_volume,omitempty"`
	// Enforce makes the simulator reject the participant's outgoing Pix
	// with RR04 while it is desenquadrado (a 4010 past its deadline).
	Enforce bool `json:"enforce,omitempty"`
	// Since is the first month for which a 4010 is owed (AAAA-MM).
	Since     string    `json:"since,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Line is one account line of a document.
type Line struct {
	Conta string `json:"conta"`
	Saldo string `json:"saldo"`
	cents int64
}

// Error is one validation failure, in the spirit of the STA's error report.
type Error struct {
	Code    string `json:"codigo"`
	Field   string `json:"campo,omitempty"`
	Message string `json:"mensagem"`
}

// Protocol is the receipt and processing result of one submission.
type Protocol struct {
	ID          string    `json:"protocolo"`
	Document    string    `json:"codigo_documento"`
	CNPJ        string    `json:"cnpj"`
	DataBase    string    `json:"data_base"`
	Type        string    `json:"tipo_remessa"`
	Status      string    `json:"status"`
	Errors      []Error   `json:"erros"`
	Lines       int       `json:"linhas"`
	ReceivedAt  time.Time `json:"recebido_em"`
	Deadline    string    `json:"prazo,omitempty"`
	Late        bool      `json:"fora_do_prazo"`
	Replaces    string    `json:"substitui,omitempty"`
	ReplacedBy  string    `json:"substituido_por,omitempty"`
	SHA256      string    `json:"sha256"`
	lines       []Line
	dataBaseDay time.Time
}

func (p *Protocol) snapshot() Protocol {
	c := *p
	c.Errors = append([]Error{}, p.Errors...)
	c.lines = nil
	return c
}

// Config tunes the deadlines.
type Config struct {
	// Deadline4111 is the number of business days after the data-base by
	// which a 4111 is due (3 in the filling instructions).
	Deadline4111 int
	// Deadline4010 returns the due date of the 4010 of a month. The
	// default, the last business day of the following month, is a
	// simulator assumption: check the current regulation.
	Deadline4010 func(cal bizday.Calendar, month time.Time) time.Time
}

func (c Config) withDefaults() Config {
	if c.Deadline4111 <= 0 {
		c.Deadline4111 = 3
	}
	if c.Deadline4010 == nil {
		c.Deadline4010 = func(cal bizday.Calendar, month time.Time) time.Time {
			return cal.LastOfMonth(month.AddDate(0, 1, 0))
		}
	}
	return c
}

// Service is the STA. It is safe for concurrent use.
type Service struct {
	cfg   Config
	cal   bizday.Calendar
	books Books

	mu           sync.Mutex
	participants map[string]*Participant
	protocols    map[string]*Protocol
	order        []string
	seq          int
	onSubmit     func(doc, status string)
}

// New returns an STA reading from books.
func New(books Books, cal bizday.Calendar, cfg Config) *Service {
	return &Service{
		cfg: cfg.withDefaults(), cal: cal, books: books,
		participants: map[string]*Participant{}, protocols: map[string]*Protocol{},
	}
}

// OnSubmit registers a hook called after every submission (metrics).
func (s *Service) OnSubmit(f func(doc, status string)) { s.onSubmit = f }

// Calendar returns the business-day calendar.
func (s *Service) Calendar() bizday.Calendar { return s.cal }

// ErrNotFound is returned for unknown participants and protocols.
var ErrNotFound = errors.New("not found")

// InvalidError rejects a participant registration.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// PutParticipant registers or replaces a participant.
func (s *Service) PutParticipant(p Participant) (Participant, error) {
	p.CNPJ = strings.ToUpper(strings.TrimSpace(p.CNPJ))
	if !cnpjRE.MatchString(p.CNPJ) {
		return Participant{}, &InvalidError{"cnpj must be the 8-character CNPJ base"}
	}
	if p.ISPB != "" && !regexp.MustCompile(`^[0-9]{8}$`).MatchString(p.ISPB) {
		return Participant{}, &InvalidError{"ispb must have 8 digits"}
	}
	for _, m := range p.Reconcile {
		if !contaRE.MatchString(m.Conta) || len(m.AccountIDs) == 0 {
			return Participant{}, &InvalidError{"each reconcile entry needs a 10-digit conta and at least one account id"}
		}
		for _, id := range m.AccountIDs {
			if _, err := s.books.BalanceAt(id, s.books.Now()); err != nil {
				return Participant{}, &InvalidError{"unknown account " + id}
			}
		}
	}
	if p.ReconcileVolume && p.ISPB == "" {
		return Participant{}, &InvalidError{"reconcile_volume needs an ispb"}
	}
	if p.Enforce && p.ISPB == "" {
		return Participant{}, &InvalidError{"enforce needs an ispb"}
	}
	if p.Since == "" {
		p.Since = bizday.Date(s.books.Now()).Format("2006-01")
	}
	if _, err := time.ParseInLocation("2006-01", p.Since, bizday.Brasilia); err != nil {
		return Participant{}, &InvalidError{"since must be AAAA-MM"}
	}
	p.CreatedAt = s.books.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.participants[p.CNPJ]; ok {
		p.CreatedAt = old.CreatedAt
	}
	s.participants[p.CNPJ] = &p
	return p, nil
}

// Participant returns a registered participant.
func (s *Service) Participant(cnpj string) (Participant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.participants[strings.ToUpper(cnpj)]
	if !ok {
		return Participant{}, fmt.Errorf("participant %s: %w", cnpj, ErrNotFound)
	}
	return *p, nil
}

// Participants lists participants by CNPJ.
func (s *Service) Participants() []Participant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Participant, 0, len(s.participants))
	for _, p := range s.participants {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CNPJ < out[j].CNPJ })
	return out
}

// Protocol returns one protocol.
func (s *Service) Protocol(id string) (Protocol, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.protocols[id]
	if !ok {
		return Protocol{}, fmt.Errorf("protocol %s: %w", id, ErrNotFound)
	}
	return p.snapshot(), nil
}

// Protocols lists protocols newest first, optionally filtered.
func (s *Service) Protocols(cnpj, doc string) []Protocol {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Protocol{}
	for i := len(s.order) - 1; i >= 0; i-- {
		p := s.protocols[s.order[i]]
		if (cnpj == "" || p.CNPJ == strings.ToUpper(cnpj)) && (doc == "" || p.Document == doc) {
			out = append(out, p.snapshot())
		}
	}
	return out
}

// xmlDocument is the simulator's XML shape.
type xmlDocument struct {
	XMLName  xml.Name `xml:"documento"`
	Codigo   string   `xml:"codigoDocumento,attr"`
	CNPJ     string   `xml:"cnpj,attr"`
	DataBase string   `xml:"dataBase,attr"`
	Tipo     string   `xml:"tipoRemessa,attr"`
	Contas   []struct {
		Codigo   string `xml:"codigoConta,attr"`
		SaldoDia string `xml:"saldoDia,attr"`
		Saldo    string `xml:"saldo,attr"`
	} `xml:"contas>conta"`
}

// parseCents converts "1234.56" or "-0.10" to cents.
func parseCents(v string) (int64, bool) {
	if !amountRE.MatchString(v) {
		return 0, false
	}
	neg := strings.HasPrefix(v, "-")
	v = strings.TrimPrefix(v, "-")
	n, err := strconv.ParseInt(strings.Replace(v, ".", "", 1), 10, 64)
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// FormatCents renders cents in the document amount format.
func FormatCents(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

func (s *Service) nextID(now time.Time) string {
	s.seq++
	return fmt.Sprintf("STA%s%06d", bizday.Date(now).Format("20060102"), s.seq)
}

// Submit receives a file. It always returns a protocol (ACEITO or
// REJEITADO); the error is only for files that are not XML at all.
func (s *Service) Submit(raw []byte) (Protocol, error) {
	now := s.books.Now()
	var doc xmlDocument
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.CharsetReader = func(label string, in io.Reader) (io.Reader, error) {
		switch strings.ToLower(label) {
		case "utf-8", "us-ascii":
			return in, nil
		}
		return nil, fmt.Errorf("encoding %s not supported by the simulator; use UTF-8", label)
	}
	if err := dec.Decode(&doc); err != nil {
		return Protocol{}, fmt.Errorf("arquivo não é um XML de documento válido: %w", err)
	}
	p := &Protocol{
		Document: doc.Codigo, CNPJ: strings.ToUpper(doc.CNPJ), DataBase: doc.DataBase, Type: doc.Tipo,
		ReceivedAt: now, Errors: []Error{}, Lines: len(doc.Contas), SHA256: sha(raw),
	}
	add := func(code, field, format string, a ...any) {
		p.Errors = append(p.Errors, Error{Code: code, Field: field, Message: fmt.Sprintf(format, a...)})
	}

	switch doc.Codigo {
	case Doc4111, Doc4010:
	default:
		add("E001", "codigoDocumento", "documento %q não é recebido por este STA (aceitos: 4111, 4010)", doc.Codigo)
	}
	if !cnpjRE.MatchString(p.CNPJ) {
		add("E002", "cnpj", "cnpj deve ter 8 caracteres alfanuméricos (raiz do CNPJ)")
	}
	if doc.Tipo != Initial && doc.Tipo != Replacement {
		add("E003", "tipoRemessa", "tipoRemessa deve ser I (inclusão) ou S (substituição)")
	}
	seen := map[string]bool{}
	for i, c := range doc.Contas {
		field := fmt.Sprintf("contas[%d]", i)
		v := c.SaldoDia
		if doc.Codigo == Doc4010 {
			v = c.Saldo
		}
		cents, ok := parseCents(v)
		switch {
		case !contaRE.MatchString(c.Codigo):
			add("E010", field, "conta %q deve ter 10 dígitos, sem pontos ou traços", c.Codigo)
		case seen[c.Codigo]:
			add("E011", field, "conta %s informada mais de uma vez", c.Codigo)
		case !ok:
			add("E012", field, "saldo %q da conta %s fora do formato (até 18 dígitos, 2 decimais separados por ponto)", v, c.Codigo)
		}
		seen[c.Codigo] = true
		p.lines = append(p.lines, Line{Conta: c.Codigo, Saldo: v, cents: cents})
	}

	s.mu.Lock()
	p.ID = s.nextID(now)
	part := s.participants[p.CNPJ]
	if len(p.Errors) == 0 {
		switch doc.Codigo {
		case Doc4111:
			s.check4111Locked(p, part, add)
		case Doc4010:
			s.check4010Locked(p, part, add)
		}
	}
	if len(p.Errors) == 0 {
		p.Status = Accepted
		if prev := s.acceptedLocked(p.Document, p.CNPJ, p.DataBase); prev != nil {
			prev.ReplacedBy, p.Replaces = p.ID, prev.ID
		}
	} else {
		p.Status = Rejected
	}
	s.protocols[p.ID] = p
	s.order = append(s.order, p.ID)
	out := p.snapshot()
	hook := s.onSubmit
	s.mu.Unlock()
	if hook != nil {
		hook(out.Document, out.Status)
	}
	return out, nil
}

// acceptedLocked returns the current accepted document for a data-base.
func (s *Service) acceptedLocked(doc, cnpj, dataBase string) *Protocol {
	for i := len(s.order) - 1; i >= 0; i-- {
		p := s.protocols[s.order[i]]
		if p.Document == doc && p.CNPJ == cnpj && p.DataBase == dataBase && p.Status == Accepted && p.ReplacedBy == "" {
			return p
		}
	}
	return nil
}

func (s *Service) checkType(p *Protocol, add func(code, field, format string, a ...any)) {
	prev := s.acceptedLocked(p.Document, p.CNPJ, p.DataBase)
	switch {
	case p.Type == Initial && prev != nil:
		add("E020", "tipoRemessa", "já existe documento aceito para %s (protocolo %s): use tipoRemessa S", p.DataBase, prev.ID)
	case p.Type == Replacement && prev == nil:
		add("E021", "tipoRemessa", "não há documento aceito para %s a substituir: use tipoRemessa I", p.DataBase)
	}
}

func (s *Service) check4111Locked(p *Protocol, part *Participant, add func(code, field, format string, a ...any)) {
	d, err := time.ParseInLocation("2006-01-02", p.DataBase, bizday.Brasilia)
	if err != nil {
		add("E030", "dataBase", "dataBase deve estar no formato AAAA-MM-DD")
		return
	}
	p.dataBaseDay = d
	today := bizday.Date(p.ReceivedAt)
	switch {
	case !d.Before(today):
		add("E031", "dataBase", "dataBase %s ainda não encerrou", p.DataBase)
		return
	case !s.cal.IsBusinessDay(d):
		why := "fim de semana"
		if n, ok := s.cal.Holiday(d); ok {
			why = "feriado nacional (" + n + ")"
		}
		add("E032", "dataBase", "%s não é dia útil (%s): o volume do dia entra no dia útil seguinte", p.DataBase, why)
		return
	}
	s.checkType(p, add)
	prev := s.cal.Previous(d).Format("2006-01-02")
	if !s.isFirst4111Locked(p.CNPJ, d) && s.acceptedLocked(Doc4111, p.CNPJ, prev) == nil {
		add("E033", "dataBase", "o documento de %s (dia útil anterior) precisa ser aceito antes", prev)
	}
	deadline := s.cal.Add(d, s.cfg.Deadline4111)
	p.Deadline = deadline.Format("2006-01-02")
	p.Late = today.After(deadline)

	if part == nil {
		return
	}
	end := bizday.EndOfDay(d)
	byConta := map[string]Line{}
	for _, l := range p.lines {
		byConta[l.Conta] = l
	}
	for _, m := range part.Reconcile {
		var want int64
		for _, id := range m.AccountIDs {
			b, _ := s.books.BalanceAt(id, end)
			want += b
		}
		got := byConta[m.Conta].cents
		if got != want {
			add("E040", "conta "+m.Conta, "saldoDia %s diverge do saldo apurado pelo BC no fim do dia: %s", FormatCents(got), FormatCents(want))
		}
	}
	if part.ReconcileVolume {
		first := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, bizday.Brasilia)
		want := s.books.Volume(part.ISPB, first, end)
		var got int64
		for _, l := range p.lines {
			if strings.HasPrefix(l.Conta, VolumePrefix) {
				got += l.cents
			}
		}
		if got != want {
			add("E041", "contas 3.0.9.70", "volume acumulado no mês %s diverge do volume Pix liquidado no SPI: %s", FormatCents(got), FormatCents(want))
		}
	}
	if d.Equal(s.cal.LastOfMonth(d)) {
		if b := s.acceptedLocked(Doc4010, p.CNPJ, d.Format("2006-01")); b != nil {
			crossCheck(p.lines, b.lines, "4010 "+b.ID, add)
		}
	}
}

// isFirst4111Locked is true when d is not after every accepted 4111, i.e. it
// does not need a predecessor (resending an old date or the first file).
func (s *Service) isFirst4111Locked(cnpj string, d time.Time) bool {
	for _, p := range s.protocols {
		if p.Document == Doc4111 && p.CNPJ == cnpj && p.Status == Accepted && p.dataBaseDay.Before(d) {
			return false
		}
	}
	return true
}

func crossCheck(a, b []Line, other string, add func(code, field, format string, a ...any)) {
	other4 := map[string]int64{}
	for _, l := range b {
		other4[l.Conta] = l.cents
	}
	for _, l := range a {
		if v, ok := other4[l.Conta]; ok && v != l.cents {
			add("E042", "conta "+l.Conta, "saldo %s diverge do informado no %s: %s", FormatCents(l.cents), other, FormatCents(v))
		}
	}
}

func (s *Service) check4010Locked(p *Protocol, part *Participant, add func(code, field, format string, a ...any)) {
	m, err := time.ParseInLocation("2006-01", p.DataBase, bizday.Brasilia)
	if err != nil {
		add("E030", "dataBase", "dataBase do 4010 deve estar no formato AAAA-MM")
		return
	}
	p.dataBaseDay = m
	today := bizday.Date(p.ReceivedAt)
	if today.Before(m.AddDate(0, 1, 0)) {
		add("E031", "dataBase", "o mês %s ainda não encerrou", p.DataBase)
		return
	}
	s.checkType(p, add)
	deadline := s.cfg.Deadline4010(s.cal, m)
	p.Deadline = deadline.Format("2006-01-02")
	p.Late = today.After(deadline)
	last := s.cal.LastOfMonth(m).Format("2006-01-02")
	if d := s.acceptedLocked(Doc4111, p.CNPJ, last); d != nil {
		crossCheck(p.lines, d.lines, "4111 de "+last+" ("+d.ID+")", add)
	}
}

// Status is a participant's standing with the supervisor.
type Status struct {
	Participant   Participant `json:"participant"`
	Desenquadrado bool        `json:"desenquadrado"`
	Missing4010   []string    `json:"missing_4010"`
	Missing4111   []string    `json:"missing_4111"`
	Next4111Due   string      `json:"next_4111_due,omitempty"`
	LastAccepted  string      `json:"last_accepted_4111,omitempty"`
}

// Status computes the participant's pending documents and whether it is
// desenquadrado: a 4010 not accepted after its deadline.
func (s *Service) Status(cnpj string) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	part, ok := s.participants[strings.ToUpper(cnpj)]
	if !ok {
		return Status{}, fmt.Errorf("participant %s: %w", cnpj, ErrNotFound)
	}
	return s.statusLocked(part), nil
}

func (s *Service) statusLocked(part *Participant) Status {
	st := Status{Participant: *part, Missing4010: []string{}, Missing4111: []string{}}
	today := bizday.Date(s.books.Now())
	since, _ := time.ParseInLocation("2006-01", part.Since, bizday.Brasilia)
	for m := since; m.AddDate(0, 1, 0).Before(today) || m.AddDate(0, 1, 0).Equal(today); m = m.AddDate(0, 1, 0) {
		key := m.Format("2006-01")
		if s.acceptedLocked(Doc4010, part.CNPJ, key) != nil {
			continue
		}
		st.Missing4010 = append(st.Missing4010, key)
		if today.After(s.cfg.Deadline4010(s.cal, m)) {
			st.Desenquadrado = true
		}
	}
	// 4111: every business day from the first accepted one up to yesterday.
	var first time.Time
	for _, p := range s.protocols {
		if p.Document == Doc4111 && p.CNPJ == part.CNPJ && p.Status == Accepted && (first.IsZero() || p.dataBaseDay.Before(first)) {
			first = p.dataBaseDay
		}
	}
	if !first.IsZero() {
		for d := first; d.Before(today); d = d.AddDate(0, 0, 1) {
			if !s.cal.IsBusinessDay(d) {
				continue
			}
			key := d.Format("2006-01-02")
			if s.acceptedLocked(Doc4111, part.CNPJ, key) == nil {
				st.Missing4111 = append(st.Missing4111, key)
			} else {
				st.LastAccepted = key
			}
		}
	}
	next := s.cal.Previous(today)
	if st.LastAccepted != "" {
		if d, _ := time.ParseInLocation("2006-01-02", st.LastAccepted, bizday.Brasilia); !d.Before(next) {
			next = s.cal.Add(d, 1)
		}
	}
	if len(st.Missing4111) > 0 {
		next, _ = time.ParseInLocation("2006-01-02", st.Missing4111[0], bizday.Brasilia)
	}
	st.Next4111Due = s.cal.Add(next, s.cfg.Deadline4111).Format("2006-01-02")
	return st
}

// Blocked reports whether outgoing Pix of ispb must be rejected for a
// regulatory reason (RR04): an enforcing participant that is desenquadrado.
func (s *Service) Blocked(ispb string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.participants {
		if p.Enforce && p.ISPB == ispb && s.statusLocked(p).Desenquadrado {
			return true
		}
	}
	return false
}

// Draft4111 builds the 4111 the simulator expects for a data-base from the
// participant's reconcile mapping and Pix volume: a reference file the PSP
// can diff against its own.
func (s *Service) Draft4111(cnpj, dataBase string) ([]byte, error) {
	part, err := s.Participant(cnpj)
	if err != nil {
		return nil, err
	}
	d, err := time.ParseInLocation("2006-01-02", dataBase, bizday.Brasilia)
	if err != nil {
		return nil, &InvalidError{"data_base must be AAAA-MM-DD"}
	}
	end := bizday.EndOfDay(d)
	var b strings.Builder
	b.WriteString(xml.Header)
	fmt.Fprintf(&b, "<documento codigoDocumento=%q cnpj=%q dataBase=%q tipoRemessa=\"I\">\n  <contas>\n", Doc4111, part.CNPJ, dataBase)
	for _, m := range part.Reconcile {
		var c int64
		for _, id := range m.AccountIDs {
			v, _ := s.books.BalanceAt(id, end)
			c += v
		}
		fmt.Fprintf(&b, "    <conta codigoConta=%q saldoDia=%q/>\n", m.Conta, FormatCents(c))
	}
	if part.ReconcileVolume {
		first := time.Date(d.Year(), d.Month(), 1, 0, 0, 0, 0, bizday.Brasilia)
		fmt.Fprintf(&b, "    <conta codigoConta=%q saldoDia=%q/>\n", "3097010000", FormatCents(s.books.Volume(part.ISPB, first, end)))
	}
	b.WriteString("  </contas>\n</documento>\n")
	return []byte(b.String()), nil
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
