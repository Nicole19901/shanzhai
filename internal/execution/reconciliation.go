package execution

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/datafeed"
	"github.com/yourorg/eth-perp-system/internal/statemachine"
)

// ReconciliationLoop 每 10s 对账一次
type ReconciliationLoop struct {
	rest     *datafeed.RESTClient
	pm       *PositionManager
	om       *OrderManager
	sm       *statemachine.StateMachine
	symbol   string
	symbolFn func() string
	interval time.Duration

	consecutiveFailures int
}

func (r *ReconciliationLoop) SetSymbolProvider(fn func() string) {
	if fn != nil {
		r.symbolFn = fn
	}
}

func (r *ReconciliationLoop) currentSymbol() string {
	if r.symbolFn != nil {
		return r.symbolFn()
	}
	return r.symbol
}

func NewReconciliationLoop(
	rest *datafeed.RESTClient,
	pm *PositionManager,
	om *OrderManager,
	sm *statemachine.StateMachine,
	symbol string,
	intervalSec int64,
) *ReconciliationLoop {
	return &ReconciliationLoop{
		rest:     rest,
		pm:       pm,
		om:       om,
		sm:       sm,
		symbol:   symbol,
		interval: time.Duration(intervalSec) * time.Second,
	}
}

func (r *ReconciliationLoop) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reconcile(ctx)
		}
	}
}

func (r *ReconciliationLoop) reconcile(ctx context.Context) {
	if !r.rest.HasCredentials() {
		return // 无凭证时静默跳过，属正常等待状态
	}
	symbol := r.currentSymbol()
	risks, err := r.rest.PositionRisk(ctx, symbol)
	if err != nil {
		r.consecutiveFailures++
		log.Error().Err(err).Int("consecutive", r.consecutiveFailures).Msg("reconciliation REST failed")
		if r.consecutiveFailures >= 3 {
			r.sm.RequestTransition(statemachine.StateFailure, "reconciliation 3 consecutive failures")
		}
		return
	}
	r.consecutiveFailures = 0

	local := r.pm.Snapshot()
	var exAmt decimal.Decimal
	for _, pos := range risks {
		if pos.Symbol == symbol {
			exAmt = pos.PositionAmt
			break
		}
	}

	localAmt := local.Quantity
	if local.Direction == datafeed.DirectionShort {
		localAmt = local.Quantity.Neg()
	}

	localFlat := local.IsFlat()
	exFlat := exAmt.IsZero()

	switch {
	case !localFlat && exFlat:
		// Case A: 本地有仓，交易所无仓（兜底订单已触发但 WS 未通知）
		log.Warn().Msg("reconciliation CASE A: exchange flat, local has position → reset local")
		r.pm.Close()
		r.sm.RequestTransition(statemachine.StateStress, "reconciliation case A")

	case localFlat && !exFlat:
		// Case B: 本地无仓，交易所有仓 → 通过 OrderManager 紧急平仓
		log.Error().Str("ex_amt", exAmt.String()).Msg("reconciliation CASE B: local flat, exchange has position → emergency close")
		dir := datafeed.DirectionLong
		if exAmt.IsNegative() {
			dir = datafeed.DirectionShort
		}
		if _, err := r.om.CloseMarket(ctx, dir, exAmt.Abs()); err != nil {
			log.Error().Err(err).Msg("reconciliation case B: emergency close failed")
		}
		r.sm.RequestTransition(statemachine.StateDegradation, "reconciliation case B")

	case !localFlat && !exFlat && !localAmt.Equal(exAmt):
		// Case C: 数量或方向不一致
		log.Error().
			Str("local", localAmt.String()).
			Str("exchange", exAmt.String()).
			Msg("reconciliation CASE C: position mismatch → FAILURE")
		r.sm.RequestTransition(statemachine.StateFailure, "reconciliation case C: position mismatch")
	}
}
