package engine

import (
	"math"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/config"
	"github.com/yourorg/eth-perp-system/internal/datafeed"
)

type TrendEngine struct {
	mu  sync.RWMutex
	cfg config.TrendEngineConfig
}

func NewTrendEngine(cfg config.TrendEngineConfig) *TrendEngine {
	return &TrendEngine{cfg: cfg}
}

// SetConfig applies runtime WebUI updates.
func (e *TrendEngine) SetConfig(enabled bool, confidence, oiDelta float64) {
	e.mu.Lock()
	e.cfg.Enabled = enabled
	e.cfg.ConfidenceThreshold = confidence
	e.cfg.OIDeltaThreshold = oiDelta
	e.mu.Unlock()
}

func (e *TrendEngine) Name() EngineType { return EngineTrend }

func (e *TrendEngine) Evaluate(ctx *datafeed.MarketContext) *Signal {
	e.mu.RLock()
	cfg := e.cfg
	e.mu.RUnlock()

	if !cfg.Enabled {
		return nil
	}

	// 1. OI delta_30s / baseline > 0.5%
	if ctx.OIBaseline.IsZero() {
		return nil
	}
	oiStrength, _ := ctx.OIDelta30s.Div(ctx.OIBaseline).Float64()
	oiStrength = math.Abs(oiStrength)
	if oiStrength < cfg.OIDeltaThreshold {
		return nil
	}

	// 2. 方向判断：OI + RCVD + price momentum
	rcvd30s := ctx.RCVD30s
	oidelta30s := ctx.OIDelta30s

	var dir datafeed.Direction
	if oidelta30s.IsPositive() && rcvd30s.IsPositive() {
		// 做多：OI 增长 + 买压
		dir = datafeed.DirectionLong
	} else if rcvd30s.IsNegative() && (oidelta30s.IsPositive() || ctx.OIDelta5s.IsNegative()) {
		// 做空：卖压 + (OI 增长说明新空单入场 OR OI 快速下降说明多头清出)
		dir = datafeed.DirectionShort
	} else {
		return nil
	}

	// 3. |funding_rate| < 0.1%/8h（放宽阈值，原来 0.05% 过于保守）
	frAbs, _ := ctx.FundingRate.Abs().Float64()
	if frAbs >= 0.001 {
		return nil
	}

	// 4. price momentum 方向一致
	pm := ctx.PriceMomentum1m
	if dir == datafeed.DirectionLong && pm.IsNegative() {
		return nil
	}
	if dir == datafeed.DirectionShort && pm.IsPositive() {
		return nil
	}

	// Confidence：做空额外奖励 OI 双向确认
	rcvdStrength, _ := rcvd30s.Abs().Float64()
	pmVal, _ := pm.Abs().Float64()
	frPenalty := 1 - frAbs/0.001

	shortOIBonus := 0.0
	if dir == datafeed.DirectionShort && ctx.OIDelta5s.IsNegative() {
		oi5sAbs, _ := ctx.OIDelta5s.Abs().Float64()
		shortOIBonus = normalize(oi5sAbs, 0, 0.005) * 0.10
	}

	confidence := 0.30*normalize(oiStrength, 0, 0.02) +
		0.25*normalize(rcvdStrength, 0, 100) +
		0.20*normalize(pmVal, 0, 0.005) +
		0.15*normalize(frPenalty, 0, 1) +
		0.10 + shortOIBonus

	if confidence < cfg.ConfidenceThreshold {
		return nil
	}

	now := time.Now().UnixMilli()
	sig := &Signal{
		Engine:      EngineTrend,
		Direction:   dir,
		Confidence:  confidence,
		EntryPrice:  ctx.ETHMarkPrice,
		Reasoning:   buildTrendReasoning(oiStrength, rcvd30s, confidence),
		GeneratedAt: now,
		ExpiresAt:   now + 2000,
	}
	log.Info().
		Str("engine", "TREND").
		Str("dir", dirStr(dir)).
		Float64("confidence", confidence).
		Str("reasoning", sig.Reasoning).
		Msg("signal generated")
	return sig
}

func buildTrendReasoning(oiStr float64, rcvd decimal.Decimal, conf float64) string {
	return "OI_strength=" + decimal.NewFromFloat(oiStr).StringFixed(4) +
		" RCVD30s=" + rcvd.StringFixed(4) +
		" conf=" + decimal.NewFromFloat(conf).StringFixed(3)
}

func dirStr(d datafeed.Direction) string {
	switch d {
	case datafeed.DirectionLong:
		return "LONG"
	case datafeed.DirectionShort:
		return "SHORT"
	default:
		return "FLAT"
	}
}
