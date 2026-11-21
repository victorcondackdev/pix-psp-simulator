package iso

import "testing"

func TestCodes(t *testing.T) {
	if !ValidReason(AM04) || ValidReason("XXXX") {
		t.Fatal("ValidReason")
	}
	if !ValidReturnReason(ReturnFraud) || ValidReturnReason(AM04) {
		t.Fatal("ValidReturnReason")
	}
	c := Codes(Reasons)
	if len(c) != len(Reasons) || c[0] != AB03 {
		t.Fatalf("Codes = %v", c)
	}
}
