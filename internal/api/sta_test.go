package api

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSTAEndToEnd(t *testing.T) {
	h := newHarness(t) // 2026-07-01 12:00 UTC, a Wednesday
	h.must("PUT", "/sta/participants/99999999", `{"ispb":"99999999","name":"PSP Sim",
		"reconcile":[{"conta":"1120000006","account_ids":["alice","bob","shop"]}],"reconcile_volume":true,"since":"2026-07"}`, 200)
	h.must("POST", "/pix/transfers", `{"from_account_id":"alice","key":"bob@example.com","amount_cents":2500}`, 202)
	h.clock.Advance(24 * time.Hour)

	r := h.do("GET", "/sta/participants/99999999/drafts/4111", "")
	if r.status != 200 || !strings.HasPrefix(r.header.Get("Content-Type"), "application/xml") {
		t.Fatal(r.status, string(r.body))
	}
	draft := string(r.body)
	if !strings.Contains(draft, `dataBase="2026-07-01"`) || !strings.Contains(draft, `saldoDia="1050.00"`) || !strings.Contains(draft, `codigoConta="3097010000" saldoDia="25.00"`) {
		t.Fatal(draft)
	}
	p := h.must("POST", "/sta/documents", draft, http.StatusCreated, "Content-Type", "application/xml")
	if p["status"] != "ACEITO" || p["prazo"] != "2026-07-06" {
		t.Fatal(p)
	}
	id := p["protocolo"].(string)
	h.must("GET", "/sta/documents/"+id, "", 200)
	if l := h.must("GET", "/sta/documents?cnpj=99999999&codigo_documento=4111", "", 200); len(l["documents"].([]any)) != 1 {
		t.Fatal(l)
	}
	bad := strings.Replace(strings.Replace(draft, `saldoDia="1050.00"`, `saldoDia="1049.00"`, 1), `tipoRemessa="I"`, `tipoRemessa="S"`, 1)
	if p := h.must("POST", "/sta/documents", bad, 201); p["status"] != "REJEITADO" || !strings.Contains(p["erros"].([]any)[0].(map[string]any)["mensagem"].(string), "1050.00") {
		t.Fatal(p)
	}

	h.run(t, []tc{
		{name: "not xml", method: "POST", path: "/sta/documents", body: "{}", want: 400, code: "invalid_document_xml"},
		{name: "unknown protocol", method: "GET", path: "/sta/documents/STA1", want: 404, code: "not_found"},
		{name: "unknown participant", method: "GET", path: "/sta/participants/00000000", want: 404, code: "not_found"},
		{name: "bad participant", method: "PUT", path: "/sta/participants/x", body: `{}`, want: 422, code: "invalid_participant"},
		{name: "bad draft date", method: "GET", path: "/sta/participants/99999999/drafts/4111?data_base=1", want: 422, code: "invalid_participant"},
		{name: "bad year", method: "GET", path: "/admin/calendar?year=x", want: 400, code: "invalid_year"},
	})
	if c := h.must("GET", "/admin/calendar?year=2026", "", 200); len(c["holidays"].([]any)) != 13 || c["previous_business_day"] != "2026-07-01" {
		t.Fatal(c)
	}
	if l := h.must("GET", "/sta/participants", "", 200); len(l["participants"].([]any)) != 1 {
		t.Fatal(l)
	}

	// Owing the May 4010 since 30/06 makes the PSP desenquadrado; with
	// enforcement its outgoing Pix are rejected with RR04.
	h.must("PUT", "/sta/participants/99999999", `{"ispb":"99999999","since":"2026-05","enforce":true}`, 200)
	if st := h.must("GET", "/sta/participants/99999999", "", 200); st["desenquadrado"] != true {
		t.Fatal(st)
	}
	tr := h.must("POST", "/pix/transfers", `{"from_account_id":"alice","key":"bob@example.com","amount_cents":100}`, 202)
	h.clock.Advance(time.Second)
	if got := h.must("GET", "/pix/transfers/"+tr["end_to_end_id"].(string), "", 200); got["status"] != "RJCT" || got["reason"] != "RR04" {
		t.Fatal(got)
	}
	if m := string(h.do("GET", "/metrics", "").body); !strings.Contains(m, `pixsim_sta_submissions_total{document="4111",status="ACEITO"} 1`) ||
		!strings.Contains(m, `pixsim_sta_submissions_total{document="4111",status="REJEITADO"} 1`) {
		t.Fatal("sta metrics")
	}
}
