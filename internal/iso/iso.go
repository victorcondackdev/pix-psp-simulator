// Package iso holds the ISO 20022 codes the simulator uses in pacs.002-like
// status reports and pacs.004-like returns. Only a small, well-known subset is
// modelled; the real SPI catalogue is larger.
package iso

import "sort"

// Transaction statuses (ExternalPaymentTransactionStatus1Code).
const (
	StatusPending  = "ACSP" // accepted, settlement in process
	StatusSettled  = "ACSC" // accepted, settlement completed
	StatusRejected = "RJCT" // rejected
)

// Rejection reasons (ExternalStatusReason1Code).
const (
	AC03 = "AC03" // invalid creditor account number
	AC04 = "AC04" // closed account number
	AC06 = "AC06" // blocked account
	AM02 = "AM02" // amount not allowed
	AM04 = "AM04" // insufficient funds
	AB03 = "AB03" // settlement aborted due to timeout
	BE01 = "BE01" // inconsistent with end customer
	DS04 = "DS04" // order rejected
	RR04 = "RR04" // regulatory reason
)

// Reasons maps every supported rejection reason to its description.
var Reasons = map[string]string{
	AC03: "Invalid creditor account number",
	AC04: "Closed account number",
	AC06: "Blocked account",
	AM02: "Amount not allowed",
	AM04: "Insufficient funds",
	AB03: "Settlement aborted due to timeout",
	BE01: "Inconsistent with end customer",
	DS04: "Order rejected",
	RR04: "Regulatory reason",
}

// Return reasons for Pix devoluções.
const (
	ReturnBankError = "BE08" // error of the PSP
	ReturnFraud     = "FR01" // fraud (used by MED)
	ReturnRequested = "MD06" // requested by the end customer
	ReturnWithdraw  = "SL02" // Pix Saque/Troco related
)

// ReturnReasons maps every supported return reason to its description.
var ReturnReasons = map[string]string{
	ReturnBankError: "Bank error",
	ReturnFraud:     "Fraud",
	ReturnRequested: "Refund requested by end customer",
	ReturnWithdraw:  "Pix Saque/Troco service error",
}

// ValidReason reports whether code is a supported rejection reason.
func ValidReason(code string) bool { _, ok := Reasons[code]; return ok }

// ValidReturnReason reports whether code is a supported return reason.
func ValidReturnReason(code string) bool { _, ok := ReturnReasons[code]; return ok }

// Codes returns the keys of m, sorted.
func Codes(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
