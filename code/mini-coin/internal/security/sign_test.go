package security

import "testing"

func TestSignAndVerify(t *testing.T) {
	params := map[string]string{"uid": "u-1", "amount_cent": "10", "req_id": "r-1"}
	sig := Sign(params, "secret")
	if !Verify(params, "secret", sig) {
		t.Fatal("valid signature rejected")
	}
	params["amount_cent"] = "999"
	if Verify(params, "secret", sig) {
		t.Fatal("tampered request accepted")
	}
}
