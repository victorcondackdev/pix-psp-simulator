package api

import (
	"testing"
	"time"
)

func TestMED2OverHTTP(t *testing.T) {
	h := newHarness(t)
	h.must("POST", "/accounts", `{"id":"mule","owner_name":"Mula","document":"11144477735","ispb":"12345678"}`, 201)
	scam := h.must("POST", "/pix/transfers", `{"from_account_id":"alice","to_account_id":"bob","amount_cents":3000}`, 202)
	h.clock.Advance(time.Second)
	h.must("POST", "/pix/transfers", `{"from_account_id":"bob","to_account_id":"mule","amount_cents":8000}`, 202)
	h.clock.Advance(time.Second)
	inf := h.must("POST", "/med/infractions", `{"end_to_end_id":"`+scam["end_to_end_id"].(string)+`"}`, 201)
	if inf["med_version"] != "2.0" {
		t.Fatal(inf)
	}
	inf = h.must("POST", "/med/infractions/"+inf["id"].(string)+"/block", "", 200)
	trace := inf["trace"].([]any)
	if inf["held_cents"].(float64) != 3000 || len(trace) != 2 || trace[1].(map[string]any)["account_id"] != "mule" || inf["block_expires_at"] == nil {
		t.Fatal(inf)
	}
	h.clock.Advance(72 * time.Hour)
	inf = h.must("GET", "/med/infractions/"+inf["id"].(string), "", 200)
	if inf["status"] != "EXPIRED" || inf["held_cents"].(float64) != 0 {
		t.Fatal(inf)
	}
	if b := h.must("GET", "/accounts/mule", "", 200); b["balance_cents"].(float64) != 8000 {
		t.Fatal(b)
	}
	h.run(t, []tc{{name: "bad version", method: "POST", path: "/med/infractions",
		body: `{"end_to_end_id":"` + scam["end_to_end_id"].(string) + `","med_version":"9"}`, want: 422, code: "invalid_med_version"}})
}
