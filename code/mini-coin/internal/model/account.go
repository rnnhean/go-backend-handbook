package model

import (
	"errors"
	"time"
)

// Account 是迷你充值系统的账户。
type Account struct {
	UID       string
	Balance   int64
	Frozen    int64
	Version   int64
	Status    int8 // 1=正常, 2=冻结
	UpdatedAt time.Time
}

const (
	AccountNormal int8 = 1
	AccountFrozen int8 = 2
)

// RechargeOrder 记录一次充值请求。ReqID 是幂等键。
type RechargeOrder struct {
	ReqID        string
	UID          string
	Amount       int64
	Channel      string
	Status       string // succeeded / failed
	BalanceAfter int64
	CreatedAt    time.Time
}

// RechargeOutcome 是仓库层一次原子充值操作的结果。
type RechargeOutcome struct {
	Order      RechargeOrder
	Account    Account
	Duplicated bool
}

var (
	ErrAccountNotFound  = errors.New("account not found")
	ErrAccountFrozen    = errors.New("account frozen")
	ErrReqIDConflict    = errors.New("req_id was used with different parameters")
	ErrProcessing       = errors.New("recharge is still processing")
	ErrPreviouslyFailed = errors.New("recharge previously failed")
)
