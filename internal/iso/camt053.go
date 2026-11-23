package iso

import (
	"encoding/xml"
	"fmt"
	"time"
)

// Camt053Namespace is the camt.053 (BankToCustomerStatement) version the
// simulator writes.
const Camt053Namespace = "urn:iso:std:iso:20022:tech:xsd:camt.053.001.08"

// StatementEntry is one booked movement of a statement.
type StatementEntry struct {
	Ref         string // the movement's business reference (EndToEndId, return id, ...)
	ServicerRef string // the servicer's unique reference (journal sequence)
	AmountCents int64  // positive
	Credit      bool
	BookedAt    time.Time
	Info        string
}

// Statement is the input of Camt053.
type Statement struct {
	ID          string
	CreatedAt   time.Time
	AccountID   string
	ISPB        string
	Branch      string
	Number      string
	OwnerName   string
	From, To    time.Time
	OpeningCnts int64
	ClosingCnts int64
	Entries     []StatementEntry
}

type camtAmt struct {
	Ccy   string `xml:"Ccy,attr"`
	Value string `xml:",chardata"`
}

type camtBal struct {
	Tp struct {
		CdOrPrtry struct {
			Cd string `xml:"Cd"`
		} `xml:"CdOrPrtry"`
	} `xml:"Tp"`
	Amt       camtAmt `xml:"Amt"`
	CdtDbtInd string  `xml:"CdtDbtInd"`
	Dt        struct {
		Dt string `xml:"Dt"`
	} `xml:"Dt"`
}

type camtNtry struct {
	NtryRef   string  `xml:"NtryRef"`
	Amt       camtAmt `xml:"Amt"`
	CdtDbtInd string  `xml:"CdtDbtInd"`
	Sts       struct {
		Cd string `xml:"Cd"`
	} `xml:"Sts"`
	BookgDt struct {
		DtTm string `xml:"DtTm"`
	} `xml:"BookgDt"`
	AcctSvcrRef string `xml:"AcctSvcrRef"`
	BkTxCd      struct {
		Prtry struct {
			Cd string `xml:"Cd"`
		} `xml:"Prtry"`
	} `xml:"BkTxCd"`
	AddtlNtryInf string `xml:"AddtlNtryInf,omitempty"`
}

type camtDoc struct {
	XMLName xml.Name `xml:"Document"`
	Xmlns   string   `xml:"xmlns,attr"`
	Stmt    struct {
		GrpHdr struct {
			MsgID   string `xml:"MsgId"`
			CreDtTm string `xml:"CreDtTm"`
		} `xml:"GrpHdr"`
		Stmt struct {
			ID      string `xml:"Id"`
			CreDtTm string `xml:"CreDtTm"`
			FrToDt  struct {
				FrDtTm string `xml:"FrDtTm"`
				ToDtTm string `xml:"ToDtTm"`
			} `xml:"FrToDt"`
			Acct struct {
				ID struct {
					Othr struct {
						ID string `xml:"Id"`
					} `xml:"Othr"`
				} `xml:"Id"`
				Ccy  string `xml:"Ccy"`
				Ownr struct {
					Nm string `xml:"Nm"`
				} `xml:"Ownr"`
				Svcr struct {
					FinInstnID struct {
						ClrSysMmbID struct {
							MmbID string `xml:"MmbId"`
						} `xml:"ClrSysMmbId"`
					} `xml:"FinInstnId"`
				} `xml:"Svcr"`
			} `xml:"Acct"`
			Bal       []camtBal `xml:"Bal"`
			TxsSummry struct {
				TtlNtries struct {
					NbOfNtries string `xml:"NbOfNtries"`
				} `xml:"TtlNtries"`
				TtlCdtNtries struct {
					NbOfNtries string `xml:"NbOfNtries"`
					Sum        string `xml:"Sum"`
				} `xml:"TtlCdtNtries"`
				TtlDbtNtries struct {
					NbOfNtries string `xml:"NbOfNtries"`
					Sum        string `xml:"Sum"`
				} `xml:"TtlDbtNtries"`
			} `xml:"TxsSummry"`
			Ntry []camtNtry `xml:"Ntry"`
		} `xml:"Stmt"`
	} `xml:"BkToCstmrStmt"`
}

func decimal(c int64) string {
	if c < 0 {
		c = -c
	}
	return fmt.Sprintf("%d.%02d", c/100, c%100)
}

func ind(credit bool) string {
	if credit {
		return "CRDT"
	}
	return "DBIT"
}

// Camt053 renders a statement as a camt.053.001.08 document: opening
// (OPBD) and closing (CLBD) booked balances, a transaction summary and one
// booked entry per movement. Only the elements a reconciliation needs are
// written.
func Camt053(s Statement) ([]byte, error) {
	var d camtDoc
	d.Xmlns = Camt053Namespace
	d.Stmt.GrpHdr.MsgID = s.ID
	d.Stmt.GrpHdr.CreDtTm = s.CreatedAt.Format(time.RFC3339)
	st := &d.Stmt.Stmt
	st.ID, st.CreDtTm = s.ID, s.CreatedAt.Format(time.RFC3339)
	st.FrToDt.FrDtTm, st.FrToDt.ToDtTm = s.From.Format(time.RFC3339), s.To.Format(time.RFC3339)
	st.Acct.ID.Othr.ID = s.Branch + "-" + s.Number
	st.Acct.Ccy = "BRL"
	st.Acct.Ownr.Nm = s.OwnerName
	st.Acct.Svcr.FinInstnID.ClrSysMmbID.MmbID = s.ISPB
	bal := func(code string, cents int64, day time.Time) camtBal {
		var b camtBal
		b.Tp.CdOrPrtry.Cd = code
		b.Amt = camtAmt{Ccy: "BRL", Value: decimal(cents)}
		b.CdtDbtInd = ind(cents >= 0)
		b.Dt.Dt = day.Format("2006-01-02")
		return b
	}
	st.Bal = []camtBal{bal("OPBD", s.OpeningCnts, s.From), bal("CLBD", s.ClosingCnts, s.To)}
	var nc, nd int
	var sc, sd int64
	for _, e := range s.Entries {
		var n camtNtry
		n.NtryRef = e.Ref
		n.Amt = camtAmt{Ccy: "BRL", Value: decimal(e.AmountCents)}
		n.CdtDbtInd = ind(e.Credit)
		n.Sts.Cd = "BOOK"
		n.BookgDt.DtTm = e.BookedAt.Format(time.RFC3339Nano)
		n.AcctSvcrRef = e.ServicerRef
		n.BkTxCd.Prtry.Cd = "PIX"
		n.AddtlNtryInf = e.Info
		st.Ntry = append(st.Ntry, n)
		if e.Credit {
			nc, sc = nc+1, sc+e.AmountCents
		} else {
			nd, sd = nd+1, sd+e.AmountCents
		}
	}
	st.TxsSummry.TtlNtries.NbOfNtries = fmt.Sprint(len(s.Entries))
	st.TxsSummry.TtlCdtNtries.NbOfNtries, st.TxsSummry.TtlCdtNtries.Sum = fmt.Sprint(nc), decimal(sc)
	st.TxsSummry.TtlDbtNtries.NbOfNtries, st.TxsSummry.TtlDbtNtries.Sum = fmt.Sprint(nd), decimal(sd)
	out, err := xml.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(out, '\n')...), nil
}
