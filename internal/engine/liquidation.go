package engine

import (
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/yourorg/eth-perp-system/internal/config"
	"github.com/yourorg/eth-perp-system/internal/datafeed"
)

// LiquidationEngine 踩踏/逼空引擎
//
// 捕捉由强制平仓驱动的单边行情：
//   - 多头踩踏（Long Liquidation）：OI 快速下降 + 持续卖压 + 负动量 → SHORT
//   - 空头逼空（Short Squeeze）  ：OI 快速下降 + 持续买压 + 正动量 → LONG
//
// 与 TREND 引擎的区别：
//   - 不检查资金费率（踩踏时 funding 必然极端，不能用于过滤）
//   - 不要求 OI 增长（强制平仓导致 OI 下降）
//   - 信号有效期缩短至 500ms（踩踏行情速度极快）
type LiquidationEngine struct {
	mu  sync.RWMutex
	cfg config.LiquidationEngineConfig
}

func NewLiquidationEngine(cfg config.LiquidationEngineConfig) *LiquidationEngine {
	return &LiquidationEngine{cfg: cfg}
}

func (e *LiquidationEngine) SetConfig(enabled bool, confidence, oiThreshold float64) {
	e.mu.Lock()
	e.cfg.Enabled = enabled
	e.cfg.ConfidenceThreshold = confidence
	e.cfg.OIThreshold = oiThreshold
	e.mu.Unlock()
}

func (e *LiquidationEngine) Name() EngineType { return EngineLiquidation }

func (e *LiquidationEngine) Evaluate(ctx *datafeed.MarketContext) *Signal {
	e.mu.RLock()
	cfg := e.cfg
	e.mu.RUnlock()

	if !cfg.Enabled {
		return nil
	}

	// 1. OI baseline 必须就绪
	if ctx.OIBaseline.IsZero() {
		return nil
	}
	baseline, _ := ctx.OIBaseline.Float64()

	// 2. OI 30s 变化率绝对值超过阈值，且必须为负（强制平仓→OI 下降）
	oiDelta30s, _ := ctx.OIDelta30s.Float64()
	if oiDelta30s >= 0 {
		return nil // OI 在增加，不是强制平仓场景
	}
	oiChangePct := math.Abs(oiDelta30s) / baseline
	if oiChangePct < cfg.OIThreshold {
		return nil
	}

	// 3. RCVD 5s 与 30s 方向必须一致（确认持续压力，非瞬时噪声）
	rcvd5s := ctx.RCVD5s
	rcvd30s := ctx.RCVD30s
	if rcvd5s.IsZero() || rcvd30s.IsZero() {
		return nil
	}
	if rcvd5s.Sign() != rcvd30s.Sign() {
		return nil // 方向不一致，跳过
	}

	// 4. 价格动量超过最低阈值（至少 0.3%）
	pm := ctx.PriceMomentum1m
	pmVal, _ := pm.Abs().Float64()
	if pmVal < 0.003 {
		return nil
	}

	// 5. 方向由 RCVD 决定，价格动量必须同向
	var dir datafeed.Direction
	if rcvd5s.IsNegative() {
		// 多头踩踏：卖压 + 下跌动量 → SHORT
		if pm.IsPositive() {
			return nil
		}
		dir = datafeed.DirectionShort
	} else {
		// 空头逼空：买压 + 上涨动量 → LONG
		if pm.IsNegative() {
			return nil
		}
		dir = datafeed.DirectionLong
	}

	// 6. OI 5s 与 RCVD 同向确认（可选加分，不硬性要求）
	oi5sBonus := 0.0
	oi5s, _ := ctx.OIDelta5s.Float64()
	if oi5s < 0 && dir == datafeed.DirectionShort {
		// 5s 内 OI 也在下降，踩踏正在加速
		oi5sBonus = normalize(math.Abs(oi5s)/baseline, 0, cfg.OIThreshold) * 0.10
	} else if oi5s < 0 && dir == datafeed.DirectionLong {
		// 逼空时空头也在快速减少，加分
		oi5sBonus = normalize(math.Abs(oi5s)/baseline, 0, cfg.OIThreshold) * 0.10
	}

	// Confidence 加权：OI 变化幅度 > RCVD 5s 强度 > RCVD 30s 持续性 > 动量幅度
	rcvd5sAbs, _ := rcvd5s.Abs().Float64()
	rcvd30sAbs, _ := rcvd30s.Abs().Float64()

	confidence := 0.35*normalize(oiChangePct, cfg.OIThreshold, cfg.OIThreshold*5) +
		0.25*normalize(rcvd5sAbs, 0, 50) +
		0.20*normalize(rcvd30sAbs, 0, 100) +
		0.15*normalize(pmVal, 0.003, 0.02) +
		0.05 + oi5sBonus

	if confidence < cfg.ConfidenceThreshold {
		return nil
	}

	now := time.Now().UnixMilli()
	sig := &Signal{
		Engine:    EngineLiquidation,
		Direction: dir,
		Confidence: confidence,
		EntryPrice: ctx.ETHMarkPrice,
		Reasoning: "liq_oi_chg=" + ctx.OIDelta30s.StringFixed(0) +
			" rcvd5s=" + ctx.RCVD5s.StringFixed(2) +
			" pm=" + ctx.PriceMomentum1m.StringFixed(4) +
			" conf=" + strconv.FormatFloat(confidence, 'f', 3, 64),
		GeneratedAt: now,
		ExpiresAt:   now + 500, // 踩踏行情速度极快，500ms 过期
	}

	log.Info().
		Str("engine", "LIQUIDATION").
		Str("dir", dirStr(dir)).
		Float64("confidence", confidence).
		Float64("oi_chg_pct", oiChangePct).
		Msg("liquidation signal generated")

	return sig
}
