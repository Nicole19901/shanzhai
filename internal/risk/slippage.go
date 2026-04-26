package risk

import (
	"sync"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// SlippageEstimator 估算滑点并决定是否拒绝信号
type SlippageEstimator struct {
	mu sync.RWMutex

	// 历史实际滑点样本（bps）
	actuals []float64
}

func NewSlippageEstimator() *SlippageEstimator {
	return &SlippageEstimator{}
}

// Estimate 估算滑点（bps）
// spreadBps: 当前买卖价差
// orderQty: 下单数量（ETH）
// depthQty: 同侧5档总量
// volBps: realized_vol_1m 转 bps
func (s *SlippageEstimator) Estimate(spreadBps, orderQty, depthQty, volBps float64) float64 {
	var impact float64
	if depthQty > 0 {
		impact = (orderQty / depthQty) * 50
	}
	premium := volBps * 100
	return spreadBps + impact + premium
}

// ShouldReject 期望收益 < 成本×1.5 则拒绝
func (s *SlippageEstimator) ShouldReject(
	takeProfitPct float64,
	confidence float64,
	estimatedSlippageBps float64,
	feesBps float64,
) bool {
	expectedPnLBps := takeProfitPct * 10000 * confidence
	totalCostBps := estimatedSlippageBps*2 + feesBps*2
	if expectedPnLBps < totalCostBps*1.5 {
		log.Info().
			Float64("expected_pnl_bps", expectedPnLBps).
			Float64("total_cost_bps", totalCostBps).
			Msg("slippage check: signal rejected")
		return true
	}
	return false
}

// RecordActual 记录实际成交滑点
func (s *SlippageEstimator) RecordActual(actualBps, estimatedBps float64) {
	s.mu.Lock()
	s.actuals = append(s.actuals, actualBps)
	if len(s.actuals) > 200 {
		s.actuals = s.actuals[1:]
	}
	s.mu.Unlock()

	if estimatedBps > 0 && actualBps > estimatedBps*3 {
		log.Error().
			Float64("actual_bps", actualBps).
			Float64("estimated_bps", estimatedBps).
			Msg("ALERT: actual slippage > 3x estimate, model may be invalid")
	}
}

// EntrySlippageBps 计算入场滑点（实际成交均价 vs markPrice）
func EntrySlippageBps(fillAvgPrice, markPrice decimal.Decimal) float64 {
	if markPrice.IsZero() {
		return 0
	}
	diff := fillAvgPrice.Sub(markPrice).Abs()
	bps, _ := diff.Div(markPrice).Mul(decimal.NewFromInt(10000)).Float64()
	return bps
}
