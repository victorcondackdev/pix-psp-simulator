package sim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/victorcondackdev/pix-psp-simulator/internal/chaos"
	"github.com/victorcondackdev/pix-psp-simulator/internal/dict"
	"github.com/victorcondackdev/pix-psp-simulator/internal/ledger"
)

// Scenario seeds the simulator: accounts (with balances and webhooks),
// DICT keys, chaos rules and the DICT rate limit.
type Scenario struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Accounts    []ScenarioAccount `json:"accounts"`
	Keys        []ScenarioKey     `json:"keys,omitempty"`
	ChaosRules  []chaos.Rule      `json:"chaos_rules,omitempty"`
	RateLimit   *dict.RateLimit   `json:"dict_rate_limit,omitempty"`
}

// ScenarioAccount is an account to create.
type ScenarioAccount struct {
	AccountRequest
	Status  ledger.Status    `json:"status,omitempty"`
	Webhook *ScenarioWebhook `json:"webhook,omitempty"`
}

// ScenarioWebhook registers a webhook for an account.
type ScenarioWebhook struct {
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}

// ScenarioKey registers a DICT key; set EVP with an empty key for a random one.
type ScenarioKey struct {
	AccountID string `json:"account_id"`
	Key       string `json:"key,omitempty"`
	EVP       bool   `json:"evp,omitempty"`
}

// ParseScenario decodes a scenario, rejecting unknown fields.
func ParseScenario(b []byte) (Scenario, error) {
	var sc Scenario
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil {
		return Scenario{}, err
	}
	return sc, nil
}

// LoadScenario applies a scenario. It stops at the first error; entities
// created before it remain.
func (s *Sim) LoadScenario(sc Scenario) error {
	for i, a := range sc.Accounts {
		if a.ID == "" {
			return fmt.Errorf("accounts[%d]: id is required in scenarios", i)
		}
		if _, err := s.CreateAccount(a.AccountRequest); err != nil {
			return fmt.Errorf("accounts[%d] (%s): %w", i, a.ID, err)
		}
		if a.Status != "" && a.Status != ledger.Active {
			if _, err := s.SetAccountStatus(a.ID, a.Status); err != nil {
				return fmt.Errorf("accounts[%d] (%s): %w", i, a.ID, err)
			}
		}
		if a.Webhook != nil {
			if _, err := s.Notify.SetEndpoint(a.ID, a.Webhook.URL, a.Webhook.Secret); err != nil {
				return fmt.Errorf("accounts[%d] (%s) webhook: %w", i, a.ID, err)
			}
		}
	}
	for i, k := range sc.Keys {
		if _, err := s.RegisterKey(k.AccountID, k.Key, k.EVP); err != nil {
			return fmt.Errorf("keys[%d] (%s): %w", i, k.Key, err)
		}
	}
	for i, r := range sc.ChaosRules {
		if _, err := s.Chaos.Add(r); err != nil {
			return fmt.Errorf("chaos_rules[%d]: %w", i, err)
		}
	}
	if sc.RateLimit != nil {
		s.DICT.Limiter.SetConfig(*sc.RateLimit)
	}
	return nil
}

// LoadScenarioFiles loads every path, expanding globs, in sorted order.
func (s *Sim) LoadScenarioFiles(patterns ...string) ([]string, error) {
	var files []string
	for _, p := range patterns {
		m, err := filepath.Glob(p)
		if err != nil {
			return nil, err
		}
		if len(m) == 0 {
			return nil, fmt.Errorf("scenario %s: no such file", p)
		}
		files = append(files, m...)
	}
	sort.Strings(files)
	var names []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return names, err
		}
		sc, err := ParseScenario(b)
		if err != nil {
			return names, fmt.Errorf("%s: %w", f, err)
		}
		if err := s.LoadScenario(sc); err != nil {
			return names, fmt.Errorf("%s: %w", f, err)
		}
		names = append(names, sc.Name)
	}
	return names, nil
}
