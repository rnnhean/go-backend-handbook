package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"minicoin/internal/cache"
	"minicoin/internal/model"
	"minicoin/internal/mq"
	"minicoin/internal/repository"
	"minicoin/internal/service"
)

const demoToken = "Bearer demo-token"

func newHandler(t *testing.T) http.Handler {
	t.Helper()
	svc := service.NewRecharge(repository.NewMemory(), cache.NewMemory(), mq.NewMemoryProducer())
	return New(svc).Routes()
}

func doJSON(h http.Handler, method, path, token, requestID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	if requestID != "" {
		req.Header.Set("X-Request-Id", requestID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRechargeHTTPContract(t *testing.T) {
	h := newHandler(t)
	body := `{"req_id":"http-1","amount_cent":10,"channel":"manual"}`

	first := doJSON(h, http.MethodPost, "/v1/recharges", demoToken, "trace-1", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s want=201", first.Code, first.Body.String())
	}
	firstBody := decodeMap(t, first)
	assertEnvelope(t, firstBody, "trace-1", "OK")
	data := firstBody["data"].(map[string]any)
	if data["uid"] != "u-1001" || data["balance"].(float64) != 110 || data["duplicated"] != false {
		t.Fatalf("unexpected first data: %#v", data)
	}

	second := doJSON(h, http.MethodPost, "/v1/recharges", demoToken, "trace-2", body)
	if second.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s want=200", second.Code, second.Body.String())
	}
	secondBody := decodeMap(t, second)
	assertEnvelope(t, secondBody, "trace-2", "OK")
	data = secondBody["data"].(map[string]any)
	if data["balance"].(float64) != 110 || data["duplicated"] != true {
		t.Fatalf("unexpected duplicate data: %#v", data)
	}
}

func TestRechargeHTTPRejectsInvalidRequests(t *testing.T) {
	h := newHandler(t)
	tests := []struct {
		name       string
		body       string
		token      string
		wantStatus int
		wantCode   string
	}{
		{"missing auth", `{"req_id":"r-auth","amount_cent":10,"channel":"manual"}`, "", http.StatusUnauthorized, "UNAUTHORIZED"},
		{"invalid json", `not-json`, demoToken, http.StatusBadRequest, "INVALID_JSON"},
		{"unknown field", `{"req_id":"r-unknown","uid":"u-1001","amount_cent":10,"channel":"manual"}`, demoToken, http.StatusBadRequest, "INVALID_JSON"},
		{"zero amount", `{"req_id":"r-zero","amount_cent":0,"channel":"manual"}`, demoToken, http.StatusBadRequest, "INVALID_AMOUNT"},
		{"amount above max", `{"req_id":"r-max","amount_cent":100000001,"channel":"manual"}`, demoToken, http.StatusBadRequest, "INVALID_AMOUNT"},
		{"missing req id", `{"amount_cent":10,"channel":"manual"}`, demoToken, http.StatusBadRequest, "MISSING_REQ_ID"},
		{"req id too long", `{"req_id":"rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr","amount_cent":10,"channel":"manual"}`, demoToken, http.StatusBadRequest, "INVALID_REQ_ID"},
		{"missing channel", `{"req_id":"r-channel","amount_cent":10}`, demoToken, http.StatusBadRequest, "MISSING_CHANNEL"},
		{"invalid channel", `{"req_id":"r-bad-channel","amount_cent":10,"channel":"unknown"}`, demoToken, http.StatusBadRequest, "INVALID_CHANNEL"},
		{"wrong method", ``, demoToken, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := http.MethodPost
			if tt.name == "wrong method" {
				method = http.MethodGet
			}
			rec := doJSON(h, method, "/v1/recharges", tt.token, "trace-err", tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status=%d body=%s want=%d", rec.Code, rec.Body.String(), tt.wantStatus)
			}
			body := decodeMap(t, rec)
			assertEnvelope(t, body, "trace-err", tt.wantCode)
			if _, leaked := body["error"]; leaked {
				t.Fatalf("response leaked raw error: %#v", body)
			}
		})
	}
}

func TestRechargeHTTPConflict(t *testing.T) {
	h := newHandler(t)
	first := `{"req_id":"http-conflict","amount_cent":10,"channel":"manual"}`
	if rec := doJSON(h, http.MethodPost, "/v1/recharges", demoToken, "trace-a", first); rec.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", rec.Code, rec.Body.String())
	}
	second := `{"req_id":"http-conflict","amount_cent":20,"channel":"manual"}`
	rec := doJSON(h, http.MethodPost, "/v1/recharges", demoToken, "trace-b", second)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s want=409", rec.Code, rec.Body.String())
	}
	assertEnvelope(t, decodeMap(t, rec), "trace-b", "REQ_ID_CONFLICT")
}

func TestRequestIDIsGeneratedAndMatchesBody(t *testing.T) {
	h := newHandler(t)
	rec := doJSON(h, http.MethodPost, "/v1/recharges", demoToken, "", `{"req_id":"generated-id","amount_cent":1,"channel":"manual"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	headerID := rec.Header().Get("X-Request-Id")
	body := decodeMap(t, rec)
	if headerID == "" || body["request_id"] != headerID {
		t.Fatalf("header=%q body request_id=%#v", headerID, body["request_id"])
	}
}

type failingRepo struct{ err error }

func (f failingRepo) GetAccount(context.Context, string) (model.Account, error) {
	return model.Account{}, f.err
}

func (f failingRepo) Recharge(context.Context, model.RechargeOrder) (model.RechargeOutcome, error) {
	return model.RechargeOutcome{}, f.err
}

func TestInternalErrorDoesNotLeakDetails(t *testing.T) {
	secret := `SQL INSERT INTO account failed: password=do-not-return`
	svc := service.NewRecharge(failingRepo{err: errors.New(secret)}, cache.NewMemory(), mq.NewMemoryProducer())
	h := New(svc).Routes()
	rec := doJSON(h, http.MethodPost, "/v1/recharges", demoToken, "trace-500", `{"req_id":"internal","amount_cent":10,"channel":"manual"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s want=500", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("response leaked internal error: %s", rec.Body.String())
	}
	assertEnvelope(t, decodeMap(t, rec), "trace-500", "INTERNAL_ERROR")
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return body
}

func assertEnvelope(t *testing.T, body map[string]any, requestID, code string) {
	t.Helper()
	if body["request_id"] != requestID {
		t.Fatalf("request_id=%#v want=%q body=%#v", body["request_id"], requestID, body)
	}
	if body["code"] != code {
		t.Fatalf("code=%#v want=%q body=%#v", body["code"], code, body)
	}
	if _, ok := body["message"].(string); !ok {
		t.Fatalf("message missing or not a string: %#v", body)
	}
	if _, ok := body["msg"]; ok {
		t.Fatalf("legacy msg field present: %#v", body)
	}
}
