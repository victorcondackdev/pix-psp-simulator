package api

import (
	"strings"
	"testing"
	"time"
)

// t0 is 2026-07-01 09:00 in Brasília (12:00 UTC).

func TestPixAutomaticoOverHTTP(t *testing.T) {
	h := newHarness(t)
	rec := h.must("POST", "/pix/automatico/recurrences", `{"receiver_account_id":"shop","payer_account_id":"alice","frequency":"MONTHLY","start_date":"2026-07-01","max_amount_cents":9000}`, 201)
	id := rec["id"].(string)
	h.must("POST", "/pix/automatico/recurrences/"+id+"/approve", `{"payer_max_amount_cents":5000}`, 200)
	c := h.must("POST", "/pix/automatico/recurrences/"+id+"/charges", `{"amount_cents":4990,"due_date":"2026-07-03"}`, 201)
	h.clock.Advance(45*time.Hour + time.Minute) // 03/07 06:01 in Brasília
	h.clock.Advance(time.Second)
	c = h.must("GET", "/pix/automatico/charges/"+c["id"].(string), "", 200)
	if c["status"] != "SETTLED" {
		t.Fatal(c)
	}
	if l := h.must("GET", "/pix/automatico/recurrences?account_id=shop", "", 200); len(l["recurrences"].([]any)) != 1 {
		t.Fatal(l)
	}
	h.must("GET", "/pix/automatico/recurrences/"+id, "", 200)
	h.clock.Advance(25 * 24 * time.Hour) // 28/07
	c2 := h.must("POST", "/pix/automatico/recurrences/"+id+"/charges", `{"amount_cents":100,"due_date":"2026-08-04"}`, 201)
	h.must("POST", "/pix/automatico/charges/"+c2["id"].(string)+"/cancel", "", 200)
	h.run(t, []tc{
		{name: "above payer limit", method: "POST", path: "/pix/automatico/recurrences/" + id + "/charges", body: `{"amount_cents":6000,"due_date":"2026-08-05"}`, want: 422, code: "amount_above_payer_limit"},
		{name: "bad transition", method: "POST", path: "/pix/automatico/recurrences/" + id + "/approve", want: 409, code: "invalid_transition"},
		{name: "unknown recurrence", method: "GET", path: "/pix/automatico/recurrences/nope", want: 404, code: "recurrence_not_found"},
		{name: "unknown charge", method: "GET", path: "/pix/automatico/charges/nope", want: 404, code: "charge_not_found"},
		{name: "cancel twice", method: "POST", path: "/pix/automatico/charges/" + c2["id"].(string) + "/cancel", want: 409, code: "charge_not_cancellable"},
		{name: "bad recurrence", method: "POST", path: "/pix/automatico/recurrences", body: `{"receiver_account_id":"shop","payer_account_id":"alice","frequency":"DAILY","start_date":"2026-07-01"}`, want: 422, code: "invalid_frequency"},
		{name: "bad body", method: "POST", path: "/pix/automatico/recurrences/" + id + "/cancel", body: `{"x":1}`, want: 400},
		{name: "bad charge body", method: "POST", path: "/pix/automatico/recurrences/" + id + "/charges", body: `[]`, want: 400},
		{name: "bad recurrence body", method: "POST", path: "/pix/automatico/recurrences", body: `[]`, want: 400},
	})
}

func TestProximityLimitsAndStatement(t *testing.T) {
	h := newHarness(t)
	ch := h.must("POST", "/cob", `{"type":"static","receiver_account_id":"shop","key":"12ABC34501DE35"}`, 201)
	h.must("PUT", "/accounts/alice/limits", `{"proximity_per_transaction_cents":1000}`, 200)
	if l := h.must("GET", "/accounts/alice/limits", "", 200); l["proximity_per_transaction_cents"].(float64) != 1000 {
		t.Fatal(l)
	}
	body := `{"from_account_id":"alice","brcode":"` + ch["brcode"].(string) + `","amount_cents":%s,"device_id":"iphone-16-xyz"}`
	tr := h.must("POST", "/pix/proximity-payments", strings.Replace(body, "%s", "900", 1), 202)
	if tr["proximity"].(map[string]any)["device_id"] != "iphone-16-xyz" {
		t.Fatal(tr)
	}
	h.clock.Advance(time.Second)
	h.run(t, []tc{
		{name: "over limit", method: "POST", path: "/pix/proximity-payments", body: strings.Replace(body, "%s", "1001", 1), want: 422, code: "proximity_limit_exceeded"},
		{name: "bad limits", method: "PUT", path: "/accounts/alice/limits", body: `{"proximity_daily_cents":-1}`, want: 422, code: "invalid_amount"},
		{name: "limits unknown account", method: "GET", path: "/accounts/nope/limits", want: 404, code: "account_not_found"},
		{name: "bad limits body", method: "PUT", path: "/accounts/alice/limits", body: `[]`, want: 400},
		{name: "bad proximity body", method: "POST", path: "/pix/proximity-payments", body: `[]`, want: 400},
		{name: "bad chaos", method: "POST", path: "/pix/proximity-payments", body: strings.Replace(body, "%s", "1", 1), headers: []string{"X-Sim-Latency", "soon"}, want: 400, code: "invalid_chaos_header"},
		{name: "statement bad date", method: "GET", path: "/iso/camt.053?account_id=alice&date=x", want: 422, code: "invalid_date"},
		{name: "statement unknown", method: "GET", path: "/iso/camt.053?account_id=nope", want: 404, code: "account_not_found"},
	})

	r := h.do("GET", "/iso/camt.053?account_id=shop&date=2026-07-01", "")
	x := string(r.body)
	if r.status != 200 || !strings.Contains(x, `xmlns="urn:iso:std:iso:20022:tech:xsd:camt.053.001.08"`) ||
		!strings.Contains(x, "<Cd>OPBD</Cd>") || !strings.Contains(x, `<Amt Ccy="BRL">9.00</Amt>`) ||
		!strings.Contains(x, "<NtryRef>"+tr["end_to_end_id"].(string)+"</NtryRef>") || !strings.Contains(x, "<CdtDbtInd>CRDT</CdtDbtInd>") {
		t.Fatal(x)
	}
	// Alice's statement shows the reservation (debit) of the payment.
	if x := string(h.do("GET", "/iso/camt.053?account_id=alice", "").body); !strings.Contains(x, "<CdtDbtInd>DBIT</CdtDbtInd>") || !strings.Contains(x, "<NbOfNtries>2</NbOfNtries>") {
		t.Fatal(x)
	}

	// The DICT lookup carries the fraud indicator.
	l := h.must("GET", "/dict/keys/bob@example.com", "", 200)
	fr := l["fraud_risk"].(map[string]any)
	if fr["level"] != "MEDIUM" || fr["score"].(float64) != 35 || l["owner_name"] == nil {
		t.Fatal(l)
	}
}
