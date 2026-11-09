package repository

import (
	"context"
	"errors"
	"sync"

	"minicoin/internal/model"
)

var ErrNotFound = errors.New("not found")

// MemoryRepository 是练习用实现。真实项目换成 MySQL 实现，接口不变。
type MemoryRepository struct {
	mu       sync.Mutex
	accounts map[string]model.Account
	orders   map[string]model.RechargeOrder
}

func NewMemory() *MemoryRepository {
	return &MemoryRepository{
		accounts: map[string]model.Account{
			"u-1001": {UID: "u-1001", Balance: 100, Status: model.AccountNormal},
		},
		orders: make(map[string]model.RechargeOrder),
	}
}

func (r *MemoryRepository) GetAccount(_ context.Context, uid string) (model.Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	acc, ok := r.accounts[uid]
	if !ok {
		return model.Account{}, model.ErrAccountNotFound
	}
	return acc, nil
}

// Recharge 在同一个临界区里完成幂等判断、账户校验、加余额和记录订单。
// 真实 MySQL 实现应使用同一个数据库事务，并使用 req_id 唯一键兜底。
func (r *MemoryRepository) Recharge(_ context.Context, order model.RechargeOrder) (model.RechargeOutcome, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if existing, ok := r.orders[order.ReqID]; ok {
		if existing.UID != order.UID || existing.Amount != order.Amount || existing.Channel != order.Channel {
			return model.RechargeOutcome{}, model.ErrReqIDConflict
		}
		switch existing.Status {
		case "succeeded":
			acc, ok := r.accounts[existing.UID]
			if !ok {
				return model.RechargeOutcome{}, model.ErrAccountNotFound
			}
			return model.RechargeOutcome{
				Order:      existing,
				Account:    acc,
				Duplicated: true,
			}, nil
		case "failed":
			return model.RechargeOutcome{}, model.ErrPreviouslyFailed
		default:
			return model.RechargeOutcome{}, model.ErrProcessing
		}
	}

	acc, ok := r.accounts[order.UID]
	if !ok {
		return model.RechargeOutcome{}, model.ErrAccountNotFound
	}
	if acc.Status != model.AccountNormal {
		return model.RechargeOutcome{}, model.ErrAccountFrozen
	}
	if acc.Balance+order.Amount < 0 {
		return model.RechargeOutcome{}, errors.New("insufficient balance")
	}

	acc.Balance += order.Amount
	acc.Version++
	r.accounts[order.UID] = acc

	order.Status = "succeeded"
	order.BalanceAfter = acc.Balance
	r.orders[order.ReqID] = order
	return model.RechargeOutcome{Order: order, Account: acc}, nil
}
