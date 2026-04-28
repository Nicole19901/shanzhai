package risk

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// Guardrails 日亏损限制 + 连亏限制
type Guardrails struct {
	mu sync.RWMutex

	dailyLossLimitPct    float64
	consecutiveLossLimit int

	// 当日累计 PnL（相对初始资本的百分比）
	dailyPnLPct float64
	dayResetAt  time.Time

	consecutiveLosses int
	cooldownUntil     time.Time
}

func NewGuardrails(dailyLossLimitPct float64, consecutiveLossLimit int) *Guardrails {
	return &Guardrails{
		dailyLossLimitPct:    dailyLossLimitPct,
		consecutiveLossLimit: consecutiveLossLimit,
		dayResetAt:           midnight(),
	}
}

// RecordTrade records one closed trade result as a fractional return.
// Example: -0.004 means -0.4%.
func (g *Guardrails) RecordTrade(pnlPct float64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// 日切换重置
	if time.Now().After(g.dayResetAt) {
		g.dailyPnLPct = 0
		g.dayResetAt = midnight()
	}

	g.dailyPnLPct += pnlPct

	if pnlPct < 0 {
		g.consecutiveLosses++
		if g.consecutiveLosses >= g.consecutiveLossLimit {
			g.cooldownUntil = time.Now().Add(time.Hour)
			log.Error().
				Int("consecutive_losses", g.consecutiveLosses).
				Msg("guardrails: consecutive loss limit hit, 1h cooldown")
		}
	} else {
		g.consecutiveLosses = 0
	}

	if g.dailyPnLPct <= -g.dailyLossLimitPct {
		log.Error().
			Float64("daily_pnl_pct", g.dailyPnLPct).
			Msg("guardrails: daily loss limit hit, system halt")
	}
}

// CanTrade 是否允许开仓
func (g *Guardrails) CanTrade() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if time.Now().Before(g.cooldownUntil) {
		return false
	}
	if time.Now().After(g.dayResetAt) {
		return true // 日切换后重置
	}
	return g.dailyPnLPct > -g.dailyLossLimitPct
}

// ComputeUnrealizedPnLPct calculates fractional PnL from mark price.
// Example: 0.008 means +0.8%.
// direction: +1 LONG, -1 SHORT
func ComputeUnrealizedPnLPct(markPrice, entryPrice decimal.Decimal, direction int) decimal.Decimal {
	if entryPrice.IsZero() {
		return decimal.Zero
	}
	diff := markPrice.Sub(entryPrice).Div(entryPrice)
	return diff.Mul(decimal.NewFromInt(int64(direction)))
}

func midnight() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
}
