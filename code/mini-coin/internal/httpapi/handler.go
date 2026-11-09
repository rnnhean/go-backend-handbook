package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"minicoin/internal/service"
)

const maxRechargeBody = 64 << 10

var errUnauthorized = errors.New("missing or invalid bearer token")

type Handler struct {
	recharge *service.RechargeService
}

func New(recharge *service.RechargeService) *Handler { return &Handler{recharge: recharge} }

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/recharges", h.rechargeHTTP)
	return withRequestID(mux)
}

type rechargeBody struct {
	ReqID      string `json:"req_id"`
	AmountCent int64  `json:"amount_cent"`
	Channel    string `json:"channel"`
}

type requestIDKey struct{}

func (h *Handler) rechargeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}

	uid, err := authenticate(r)
	if err != nil {
		writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRechargeBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var body rechargeBody
	if err := dec.Decode(&body); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	if err := ensureSingleJSONValue(dec); err != nil {
		writeError(w, r, http.StatusBadRequest, "INVALID_JSON")
		return
	}

	result, err := h.recharge.Recharge(r.Context(), service.RechargeRequest{
		ReqID: body.ReqID, UID: uid, Amount: body.AmountCent, Channel: body.Channel,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidAmount):
			writeError(w, r, http.StatusBadRequest, "INVALID_AMOUNT")
		case errors.Is(err, service.ErrInvalidReqID):
			writeError(w, r, http.StatusBadRequest, "INVALID_REQ_ID")
		case errors.Is(err, service.ErrInvalidChannel):
			writeError(w, r, http.StatusBadRequest, "INVALID_CHANNEL")
		case errors.Is(err, service.ErrMissingReqID):
			writeError(w, r, http.StatusBadRequest, "MISSING_REQ_ID")
		case errors.Is(err, service.ErrMissingUID):
			writeError(w, r, http.StatusUnauthorized, "UNAUTHORIZED")
		case errors.Is(err, service.ErrMissingChannel):
			writeError(w, r, http.StatusBadRequest, "MISSING_CHANNEL")
		case errors.Is(err, service.ErrReqIDConflict):
			writeError(w, r, http.StatusConflict, "REQ_ID_CONFLICT")
		case errors.Is(err, service.ErrProcessing):
			writeError(w, r, http.StatusAccepted, "PROCESSING")
		case errors.Is(err, service.ErrPreviouslyFailed):
			writeError(w, r, http.StatusConflict, "PREVIOUSLY_FAILED")
		case errors.Is(err, service.ErrAccountNotFound):
			writeError(w, r, http.StatusNotFound, "ACCOUNT_NOT_FOUND")
		case errors.Is(err, service.ErrFrozen):
			writeError(w, r, http.StatusConflict, "ACCOUNT_FROZEN")
		default:
			log.Printf("recharge failed request_id=%s err=%v", RequestIDFrom(r.Context()), err)
			writeError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR")
		}
		return
	}

	status := http.StatusCreated
	if result.Duplicated {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{
		"code":       "OK",
		"message":    "success",
		"request_id": RequestIDFrom(r.Context()),
		"data": map[string]any{
			"uid": result.UID, "balance": result.Balance, "duplicated": result.Duplicated,
		},
	})
}

func authenticate(r *http.Request) (string, error) {
	const prefix = "Bearer "
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(value, prefix) || strings.TrimSpace(strings.TrimPrefix(value, prefix)) == "" {
		return "", errUnauthorized
	}
	// 教学实现只演示身份来源；真实 JWT 必须校验签名、过期时间、issuer 和 audience。
	return "u-1001", nil
}

func ensureSingleJSONValue(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if reqID == "" || len(reqID) > 64 {
			reqID = newRequestID()
		}
		w.Header().Set("X-Request-Id", reqID)
		ctx := context.WithValue(r.Context(), requestIDKey{}, reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-generated"
	}
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code string) {
	writeJSON(w, status, map[string]any{
		"code":       code,
		"message":    publicMessage(code),
		"request_id": RequestIDFrom(r.Context()),
	})
}

func publicMessage(code string) string {
	switch code {
	case "UNAUTHORIZED":
		return "authentication is required"
	case "INVALID_JSON":
		return "request body must be one valid JSON object without unknown fields"
	case "INVALID_AMOUNT":
		return "amount_cent must be between 1 and 100000000"
	case "INVALID_REQ_ID":
		return "req_id must be 1 to 64 characters"
	case "INVALID_CHANNEL":
		return "channel must be manual or coupon"
	case "MISSING_REQ_ID":
		return "req_id is required"
	case "MISSING_CHANNEL":
		return "channel is required"
	case "REQ_ID_CONFLICT":
		return "req_id was reused with different parameters"
	case "PROCESSING":
		return "request is still processing"
	case "PREVIOUSLY_FAILED":
		return "request previously failed and cannot be retried automatically"
	case "ACCOUNT_NOT_FOUND":
		return "account not found"
	case "ACCOUNT_FROZEN":
		return "account is frozen"
	case "METHOD_NOT_ALLOWED":
		return "method is not allowed"
	default:
		return "internal server error"
	}
}

// RequestIDFrom 从 context 读取中间件写入的 request id。
func RequestIDFrom(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return requestID
}
