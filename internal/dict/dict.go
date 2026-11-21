// Package dict is a mock of the Pix DICT (Diretório de Identificadores de
// Contas Transacionais): key registration, deletion, lookups with anti-scan
// rate limiting, and ownership/portability claims.
//
// It is a simplified model. Keys are validated with brpay's pix.NormalizeKey
// and lookups mask the owner's document the way PSP apps show it, but there is
// no synchronisation protocol, no PSP-to-PSP messaging and no CID/VSync.
package dict

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/victorcondackdev/brpay/document"
	"github.com/victorcondackdev/brpay/pix"

	"github.com/victorcondackdev/pix-psp-simulator/internal/clock"
	"github.com/victorcondackdev/pix-psp-simulator/internal/fsm"
)

// Per-account key limits (5 for natural persons, 20 for legal persons).
const (
	MaxKeysNaturalPerson = 5
	MaxKeysLegalPerson   = 20
)

// Owner describes the account a key points to.
type Owner struct {
	AccountID   string `json:"account_id"`
	ISPB        string `json:"ispb"`
	Branch      string `json:"branch"`
	Number      string `json:"number"`
	AccountType string `json:"account_type"`
	Name        string `json:"name"`
	Document    string `json:"document"`
}

// Entry is a registered key.
type Entry struct {
	Key       string    `json:"key"`
	KeyType   string    `json:"key_type"`
	Owner     Owner     `json:"owner"`
	CreatedAt time.Time `json:"created_at"`
}

// PersonType returns NATURAL_PERSON for a CPF owner, LEGAL_PERSON otherwise.
func (o Owner) PersonType() string {
	if document.Detect(o.Document) == document.CPF {
		return "NATURAL_PERSON"
	}
	return "LEGAL_PERSON"
}

// LookupResult is what a lookup returns: the owner document is masked.
type LookupResult struct {
	Key         string    `json:"key"`
	KeyType     string    `json:"key_type"`
	OwnerName   string    `json:"owner_name"`
	OwnerTaxID  string    `json:"owner_tax_id_masked"`
	OwnerType   string    `json:"owner_type"`
	ISPB        string    `json:"ispb"`
	Branch      string    `json:"branch"`
	Number      string    `json:"account_number"`
	AccountType string    `json:"account_type"`
	CreatedAt   time.Time `json:"created_at"`
	// AccountID is a simulator convenience, absent from the real DICT.
	AccountID string `json:"account_id"`
}

// Errors.
var (
	ErrKeyNotFound       = errors.New("key not found")
	ErrKeyExists         = errors.New("key already registered")
	ErrKeyLimit          = errors.New("account key limit reached")
	ErrDocumentMismatch  = errors.New("CPF/CNPJ key must match the account owner document")
	ErrNotOwner          = errors.New("key is not registered to this account")
	ErrRateLimited       = errors.New("lookup rate limit exceeded")
	ErrClaimNotFound     = errors.New("claim not found")
	ErrClaimInProgress   = errors.New("key has a claim in progress")
	ErrClaimNotAllowed   = errors.New("claim not allowed")
	ErrClaimForbidden    = errors.New("account is not a party allowed to perform this action")
	ErrClaimSameAccount  = errors.New("key is already registered to the claimer account")
	ErrInvalidClaimType  = errors.New("claim type must be OWNERSHIP or PORTABILITY")
	ErrInvalidTransition = errors.New("invalid claim transition")
)

// RateLimitError carries the retry delay of a rate-limited lookup.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string { return ErrRateLimited.Error() }

// Unwrap lets errors.Is match ErrRateLimited.
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// ClaimType is OWNERSHIP (another person proves possession of a phone or
// e-mail) or PORTABILITY (the same person moves a key to another account).
type ClaimType string

// Claim types.
const (
	Ownership   ClaimType = "OWNERSHIP"
	Portability ClaimType = "PORTABILITY"
)

// ClaimStatus is the state of a claim.
type ClaimStatus string

// Claim states.
const (
	ClaimOpen      ClaimStatus = "OPEN"
	ClaimConfirmed ClaimStatus = "CONFIRMED"
	ClaimCompleted ClaimStatus = "COMPLETED"
	ClaimCancelled ClaimStatus = "CANCELLED"
)

// ClaimEvent drives the claim state machine.
type ClaimEvent string

// Claim events.
const (
	EvConfirm  ClaimEvent = "confirm"  // donor accepts
	EvCancel   ClaimEvent = "cancel"   // donor or claimer gives up
	EvComplete ClaimEvent = "complete" // claimer takes the key
	EvTimeout  ClaimEvent = "timeout"  // resolution period elapsed
)

// ClaimMachine is the simplified claim lifecycle. On timeout an ownership
// claim is confirmed (the donor failed to defend the key) and a portability
// claim is cancelled (the donor never agreed); see Directory.refresh.
var ClaimMachine = fsm.New([]fsm.Transition[ClaimStatus, ClaimEvent]{
	{From: ClaimOpen, Event: EvConfirm, To: ClaimConfirmed},
	{From: ClaimOpen, Event: EvCancel, To: ClaimCancelled},
	{From: ClaimOpen, Event: EvTimeout, To: ClaimConfirmed},
	{From: ClaimConfirmed, Event: EvComplete, To: ClaimCompleted},
	{From: ClaimConfirmed, Event: EvCancel, To: ClaimCancelled},
}, ClaimCompleted, ClaimCancelled)

// Claim is a request to move a key to another account.
type Claim struct {
	ID                 string      `json:"id"`
	Type               ClaimType   `json:"type"`
	Key                string      `json:"key"`
	KeyType            string      `json:"key_type"`
	Status             ClaimStatus `json:"status"`
	Claimer            Owner       `json:"claimer"`
	DonorAccountID     string      `json:"donor_account_id"`
	ResolutionDeadline time.Time   `json:"resolution_deadline"`
	CreatedAt          time.Time   `json:"created_at"`
	UpdatedAt          time.Time   `json:"updated_at"`
}

// Directory is the DICT mock. It is safe for concurrent use.
type Directory struct {
	mu               sync.Mutex
	clock            clock.Clock
	rand             io.Reader
	keys             map[string]*Entry
	claims           map[string]*Claim
	claimSeq         int
	ResolutionPeriod time.Duration
	Limiter          *Limiter
}

// New returns an empty directory. The resolution period of claims defaults to
// 7 days; the rate limit to DefaultRateLimit.
func New(c clock.Clock) *Directory {
	return &Directory{
		clock:            c,
		rand:             rand.Reader,
		keys:             map[string]*Entry{},
		claims:           map[string]*Claim{},
		ResolutionPeriod: 7 * 24 * time.Hour,
		Limiter:          NewLimiter(c, DefaultRateLimit),
	}
}

// SetRand replaces the randomness source used for EVP keys (tests).
func (d *Directory) SetRand(r io.Reader) { d.rand = r }

// NewEVP returns a random lowercase version-4 UUID.
func NewEVP(r io.Reader) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Normalize validates a key with brpay and returns its canonical form.
func Normalize(key string) (string, pix.KeyType, error) { return pix.NormalizeKey(key) }

func maxKeys(o Owner) int {
	if document.Detect(o.Document) == document.CNPJ {
		return MaxKeysLegalPerson
	}
	return MaxKeysNaturalPerson
}

func (d *Directory) countKeys(accountID string) int {
	n := 0
	for _, e := range d.keys {
		if e.Owner.AccountID == accountID {
			n++
		}
	}
	return n
}

func (d *Directory) activeClaim(key string) *Claim {
	for _, c := range d.claims {
		d.refresh(c)
		if c.Key == key && !ClaimMachine.Terminal(c.Status) {
			return c
		}
	}
	return nil
}

// Register adds a key for owner. An empty key with evp=true generates a
// random key.
func (d *Directory) Register(key string, evp bool, owner Owner) (Entry, error) {
	if key == "" && evp {
		k, err := NewEVP(d.rand)
		if err != nil {
			return Entry{}, err
		}
		key = k
	}
	norm, typ, err := pix.NormalizeKey(key)
	if err != nil {
		return Entry{}, err
	}
	if (typ == pix.KeyCPF || typ == pix.KeyCNPJ) && norm != document.Clean(owner.Document) {
		return Entry{}, ErrDocumentMismatch
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.keys[norm]; ok {
		if e.Owner.AccountID == owner.AccountID {
			return Entry{}, fmt.Errorf("%w to this account", ErrKeyExists)
		}
		return Entry{}, fmt.Errorf("%w to another account; open a claim to move it", ErrKeyExists)
	}
	if d.countKeys(owner.AccountID) >= maxKeys(owner) {
		return Entry{}, fmt.Errorf("%w (%d)", ErrKeyLimit, maxKeys(owner))
	}
	e := &Entry{Key: norm, KeyType: typ.String(), Owner: owner, CreatedAt: d.clock.Now()}
	d.keys[norm] = e
	return *e, nil
}

// Delete removes a key registered to accountID; open claims are cancelled.
func (d *Directory) Delete(key, accountID string) error {
	norm, _, err := pix.NormalizeKey(key)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.keys[norm]
	if !ok {
		return ErrKeyNotFound
	}
	if accountID != "" && e.Owner.AccountID != accountID {
		return ErrNotOwner
	}
	delete(d.keys, norm)
	if c := d.activeClaim(norm); c != nil {
		c.Status = ClaimCancelled
		c.UpdatedAt = d.clock.Now()
	}
	return nil
}

// Resolve returns the entry for a key without rate limiting (internal use by
// the simulator when settling payments).
func (d *Directory) Resolve(key string) (Entry, error) {
	norm, _, err := pix.NormalizeKey(key)
	if err != nil {
		return Entry{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.keys[norm]
	if !ok {
		return Entry{}, ErrKeyNotFound
	}
	return *e, nil
}

// Lookup is the rate-limited public lookup made on behalf of payer.
func (d *Directory) Lookup(key, payer string) (LookupResult, error) {
	if payer == "" {
		payer = "anonymous"
	}
	ok, wait, _ := d.Limiter.Take(payer, 1)
	if !ok {
		return LookupResult{}, &RateLimitError{RetryAfter: wait}
	}
	e, err := d.Resolve(key)
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			d.Limiter.Charge(payer, d.Limiter.Config().NotFoundCost)
		}
		return LookupResult{}, err
	}
	return LookupResult{
		Key:         e.Key,
		KeyType:     e.KeyType,
		OwnerName:   e.Owner.Name,
		OwnerTaxID:  document.Mask(e.Owner.Document),
		OwnerType:   e.Owner.PersonType(),
		ISPB:        e.Owner.ISPB,
		Branch:      e.Owner.Branch,
		Number:      e.Owner.Number,
		AccountType: e.Owner.AccountType,
		CreatedAt:   e.CreatedAt,
		AccountID:   e.Owner.AccountID,
	}, nil
}

// Keys lists registered keys, optionally for one account, sorted.
func (d *Directory) Keys(accountID string) []Entry {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Entry
	for _, e := range d.keys {
		if accountID == "" || e.Owner.AccountID == accountID {
			out = append(out, *e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// OpenClaim starts a claim of key by claimer.
func (d *Directory) OpenClaim(typ ClaimType, key string, claimer Owner) (Claim, error) {
	if typ != Ownership && typ != Portability {
		return Claim{}, ErrInvalidClaimType
	}
	norm, kt, err := pix.NormalizeKey(key)
	if err != nil {
		return Claim{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.keys[norm]
	if !ok {
		return Claim{}, ErrKeyNotFound
	}
	if e.Owner.AccountID == claimer.AccountID {
		return Claim{}, ErrClaimSameAccount
	}
	if d.activeClaim(norm) != nil {
		return Claim{}, ErrClaimInProgress
	}
	sameOwner := document.Clean(e.Owner.Document) == document.Clean(claimer.Document)
	switch typ {
	case Ownership:
		if kt != pix.KeyPhone && kt != pix.KeyEmail {
			return Claim{}, fmt.Errorf("%w: ownership claims apply only to PHONE and EMAIL keys", ErrClaimNotAllowed)
		}
		if sameOwner {
			return Claim{}, fmt.Errorf("%w: same owner, use PORTABILITY", ErrClaimNotAllowed)
		}
	case Portability:
		if !sameOwner {
			return Claim{}, fmt.Errorf("%w: portability requires the same owner, use OWNERSHIP", ErrClaimNotAllowed)
		}
	}
	if (kt == pix.KeyCPF || kt == pix.KeyCNPJ) && norm != document.Clean(claimer.Document) {
		return Claim{}, ErrDocumentMismatch
	}
	d.claimSeq++
	now := d.clock.Now()
	c := &Claim{
		ID:                 fmt.Sprintf("clm_%06d", d.claimSeq),
		Type:               typ,
		Key:                norm,
		KeyType:            kt.String(),
		Status:             ClaimOpen,
		Claimer:            claimer,
		DonorAccountID:     e.Owner.AccountID,
		ResolutionDeadline: now.Add(d.ResolutionPeriod),
		CreatedAt:          now,
		UpdatedAt:          now,
	}
	d.claims[c.ID] = c
	return *c, nil
}

// refresh applies the resolution-period timeout lazily; caller holds d.mu.
func (d *Directory) refresh(c *Claim) {
	if c.Status != ClaimOpen || d.clock.Now().Before(c.ResolutionDeadline) {
		return
	}
	if c.Type == Ownership {
		c.Status, _ = ClaimMachine.Next(c.Status, EvTimeout)
	} else {
		c.Status, _ = ClaimMachine.Next(c.Status, EvCancel)
	}
	c.UpdatedAt = c.ResolutionDeadline
}

// GetClaim returns a claim, applying any elapsed timeout.
func (d *Directory) GetClaim(id string) (Claim, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.claims[id]
	if !ok {
		return Claim{}, ErrClaimNotFound
	}
	d.refresh(c)
	return *c, nil
}

// Claims lists every claim sorted by ID.
func (d *Directory) Claims() []Claim {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Claim, 0, len(d.claims))
	for _, c := range d.claims {
		d.refresh(c)
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ActOnClaim applies event on behalf of actor (an account ID). Confirm is
// for the donor, complete for the claimer, cancel for either.
func (d *Directory) ActOnClaim(id string, ev ClaimEvent, actor string) (Claim, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.claims[id]
	if !ok {
		return Claim{}, ErrClaimNotFound
	}
	d.refresh(c)
	switch ev {
	case EvConfirm:
		if actor != c.DonorAccountID {
			return *c, ErrClaimForbidden
		}
	case EvComplete:
		if actor != c.Claimer.AccountID {
			return *c, ErrClaimForbidden
		}
	case EvCancel:
		if actor != c.DonorAccountID && actor != c.Claimer.AccountID {
			return *c, ErrClaimForbidden
		}
	default:
		return *c, fmt.Errorf("%w: unknown event %q", ErrInvalidTransition, ev)
	}
	next, err := ClaimMachine.Next(c.Status, ev)
	if err != nil {
		return *c, fmt.Errorf("%w: %v", ErrInvalidTransition, err)
	}
	if ev == EvComplete {
		e, ok := d.keys[c.Key]
		if !ok {
			return *c, ErrKeyNotFound
		}
		if d.countKeys(c.Claimer.AccountID) >= maxKeys(c.Claimer) {
			return *c, fmt.Errorf("%w (%d)", ErrKeyLimit, maxKeys(c.Claimer))
		}
		e.Owner = c.Claimer
		e.CreatedAt = d.clock.Now()
	}
	c.Status = next
	c.UpdatedAt = d.clock.Now()
	return *c, nil
}
