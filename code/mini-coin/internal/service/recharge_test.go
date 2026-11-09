package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"minicoin/internal/cache"
	"minicoin/internal/mq"
	"minicoin/internal/repository"
)

func newTestService() *RechargeService {
	return NewRecharge(repository.NewMemory(), cache.NewMemory(), mq.NewMemoryProducer())
}

func TestRechargeIdempotent(t *testing.T) {
	svc := newTestService()
	req := RechargeRequest{ReqID: "req-1", UID: "u-1001", Amount: 10, Channel: "manual"}

	first, err := svc.Recharge(context.Background(), req)
	if err != nil {
		t.Fatalf("first recharge: %v", err)
	}
	second, err := svc.Recharge(context.Background(), req)
	if err != nil {
		t.Fatalf("second recharge: %v", err)
	}
	if first.Balance != 110 || second.Balance != 110 {
		t.Fatalf("idempotency broken: first=%d second=%d", first.Balance, second.Balance)
	}
	if first.Duplicated || !second.Duplicated {
		t.Fatalf("duplicate flag wrong: first=%v second=%v", first.Duplicated, second.Duplicated)
	}
}

func TestRechargeRejectsSameReqIDWithDifferentParameters(t *testing.T) {
	svc := newTestService()
	first := RechargeRequest{ReqID: "req-conflict", UID: "u-1001", Amount: 10, Channel: "manual"}
	if _, err := svc.Recharge(context.Background(), first); err != nil {
		t.Fatalf("first recharge: %v", err)
	}

	cases := []RechargeRequest{
		{ReqID: first.ReqID, UID: first.UID, Amount: 11, Channel: first.Channel},
		{ReqID: first.ReqID, UID: "u-other", Amount: first.Amount, Channel: first.Channel},
		{ReqID: first.ReqID, UID: first.UID, Amount: first.Amount, Channel: "coupon"},
	}
	for _, req := range cases {
		if _, err := svc.Recharge(context.Background(), req); !errors.Is(err, ErrReqIDConflict) {
			t.Fatalf("want ErrReqIDConflict for %+v, got %v", req, err)
		}
	}
}

func TestRechargeConflictDoesNotChangeBalance(t *testing.T) {
	repo := repository.NewMemory()
	svc := NewRecharge(repo, cache.NewMemory(), mq.NewMemoryProducer())
	req := RechargeRequest{ReqID: "req-conflict", UID: "u-1001", Amount: 10, Channel: "manual"}
	if _, err := svc.Recharge(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	req.Amount = 999
	if _, err := svc.Recharge(context.Background(), req); !errors.Is(err, ErrReqIDConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	acc, err := repo.GetAccount(context.Background(), "u-1001")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Balance != 110 {
		t.Fatalf("conflicting retry changed balance: got %d want 110", acc.Balance)
	}
}

func TestRechargeConcurrentSameRequestCreditsOnce(t *testing.T) {
	repo := repository.NewMemory()
	svc := NewRecharge(repo, cache.NewMemory(), mq.NewMemoryProducer())
	req := RechargeRequest{ReqID: "req-race", UID: "u-1001", Amount: 10, Channel: "manual"}

	const goroutines = 50
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	results := make(chan RechargeResult, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := svc.Recharge(context.Background(), req)
			errs <- err
			results <- result
		}()
	}
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent recharge: %v", err)
		}
	}
	for result := range results {
		if result.Balance != 110 {
			t.Fatalf("concurrent call saw unexpected balance: %d", result.Balance)
		}
	}

	acc, err := repo.GetAccount(context.Background(), "u-1001")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Balance != 110 {
		t.Fatalf("credited more than once: got %d want 110", acc.Balance)
	}
}

func TestRechargeRejectsInvalidInput(t *testing.T) {
	svc := newTestService()
	tests := []struct {
		name string
		req  RechargeRequest
		want error
	}{
		{"zero amount", RechargeRequest{ReqID: "r", UID: "u-1001", Amount: 0, Channel: "manual"}, ErrInvalidAmount},
		{"negative amount", RechargeRequest{ReqID: "r", UID: "u-1001", Amount: -1, Channel: "manual"}, ErrInvalidAmount},
		{"amount above max", RechargeRequest{ReqID: "r", UID: "u-1001", Amount: maxRechargeAmount + 1, Channel: "manual"}, ErrInvalidAmount},
		{"missing req id", RechargeRequest{UID: "u-1001", Amount: 1, Channel: "manual"}, ErrMissingReqID},
		{"req id too long", RechargeRequest{ReqID: strings.Repeat("r", 65), UID: "u-1001", Amount: 1, Channel: "manual"}, ErrInvalidReqID},
		{"missing uid", RechargeRequest{ReqID: "r", Amount: 1, Channel: "manual"}, ErrMissingUID},
		{"missing channel", RechargeRequest{ReqID: "r", UID: "u-1001", Amount: 1}, ErrMissingChannel},
		{"invalid channel", RechargeRequest{ReqID: "r", UID: "u-1001", Amount: 1, Channel: "unknown"}, ErrInvalidChannel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := svc.Recharge(context.Background(), tt.req); !errors.Is(err, tt.want) {
				t.Fatalf("want %v, got %v", tt.want, err)
			}
		})
	}
}

func TestRechargeDifferentRequestsAllApply(t *testing.T) {
	repo := repository.NewMemory()
	svc := NewRecharge(repo, cache.NewMemory(), mq.NewMemoryProducer())
	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Recharge(context.Background(), RechargeRequest{
				ReqID: fmt.Sprintf("req-%d", i), UID: "u-1001", Amount: 1, Channel: "manual",
			})
			if err != nil {
				t.Errorf("recharge %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	acc, err := repo.GetAccount(context.Background(), "u-1001")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Balance != 120 {
		t.Fatalf("got %d want 120", acc.Balance)
	}
}
