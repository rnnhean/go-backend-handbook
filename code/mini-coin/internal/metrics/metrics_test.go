package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCounterExposition(t *testing.T) {
	r := New()
	r.Inc("mini_coin_recharge_total")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "mini_coin_recharge_total 1") {
		t.Fatalf("unexpected metrics: %s", rec.Body.String())
	}
}
