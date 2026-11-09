package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"minicoin/internal/model"
)

const maxRechargeAmount = 100_000_000

var (
	ErrInvalidAmount  = errors.New("amount must be between 1 and 100000000 cents")
	ErrInvalidReqID   = errors.New("req_id must be 1 to 64 characters")
	ErrInvalidChannel = errors.New("channel must be manual or coupon")
	ErrMissingReqID   = errors.New("req_id is required")
	ErrMissingUID     = errors.New("uid is required")
	ErrMissingChannel = errors.New("channel is required")
	// 对外保留服务层错误名，仓库层返回的领域错误会被包裹，errors.Is 仍可判断。
	ErrAccountNotFound  = model.ErrAccountNotFound
	ErrFrozen           = model.ErrAccountFrozen
	ErrReqIDConflict    = model.ErrReqIDConflict
	ErrProcessing       = model.ErrProcessing
	ErrPreviouslyFailed = model.ErrPreviouslyFailed
)

type Repository interface {
	GetAccount(ctx context.Context, uid string) (model.Account, error)
	Recharge(ctx context.Context, order model.RechargeOrder) (model.RechargeOutcome, error)
}

type Cache interface {
	Delete(ctx context.Context, key string) error
}

type Publisher interface {
	Publish(ctx context.Context, topic string, payload any) error
}

type RechargeService struct {
	repo  Repository
	cache Cache
	pub   Publisher
	now   func() time.Time
}

func NewRecharge(repo Repository, cache Cache, pub Publisher) *RechargeService {
	return &RechargeService{repo: repo, cache: cache, pub: pub, now: time.Now}
}

type RechargeRequest struct {
	ReqID   string
	UID     string
	Amount  int64
	Channel string
}

type RechargeResult struct {
	UID        string
	Balance    int64
	Duplicated bool
}

func (s *RechargeService) Recharge(ctx context.Context, req RechargeRequest) (RechargeResult, error) {
	if req.Amount <= 0 || req.Amount > maxRechargeAmount {
		return RechargeResult{}, ErrInvalidAmount
	}
	if req.ReqID == "" {
		return RechargeResult{}, ErrMissingReqID
	}
	if len([]rune(req.ReqID)) > 64 {
		return RechargeResult{}, ErrInvalidReqID
	}
	if req.UID == "" {
		return RechargeResult{}, ErrMissingUID
	}
	if req.Channel == "" {
		return RechargeResult{}, ErrMissingChannel
	}
	if req.Channel != "manual" && req.Channel != "coupon" {
		return RechargeResult{}, ErrInvalidChannel
	}

	outcome, err := s.repo.Recharge(ctx, model.RechargeOrder{
		ReqID: req.ReqID, UID: req.UID, Amount: req.Amount, Channel: req.Channel,
		CreatedAt: s.now(),
	})
	if err != nil {
		return RechargeResult{}, fmt.Errorf("atomic recharge: %w", err)
	}

	// 缓存删除失败不能吞掉业务结果，但应记录日志并让 TTL 兜底。
	_ = s.cache.Delete(ctx, "account:"+req.UID)
	// 示例用尽力而为的消息发送。生产环境应在数据库事务中写 outbox，
	// 再由 outbox worker 重试投递，避免“充值已提交但消息永久丢失”。
	if !outcome.Duplicated {
		_ = s.pub.Publish(ctx, "coin.recharged", map[string]any{
			"req_id": req.ReqID, "uid": req.UID, "amount_cent": req.Amount, "channel": req.Channel,
		})
	}
	return RechargeResult{
		UID: outcome.Account.UID, Balance: outcome.Order.BalanceAfter, Duplicated: outcome.Duplicated,
	}, nil
}
