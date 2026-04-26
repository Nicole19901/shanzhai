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

type TransitionEngine struct {
	mu  sync.RWMutex
	cfg config.TransitionEngineConfig
}

func NewTransitionEngine(cfg config.TransitionEngineConfig) *TransitionEngine {
	return &TransitionEngine{cfg: cfg}
}

func (e *TransitionEngine) SetThresholds(confidence, volCompressionRatio float64) {
	e.mu.Lock()
	e.cfg.ConfidenceThreshold = confidence
	e.cfg.VolCompressionRatio = volCompressionRatio
	e.mu.Unlock()
}

func (e *TransitionEngine) Name() EngineType { return EngineTransition }

func (e *TransitionEngine) Evaluate(ctx *datafeed.MarketContext) *Signal {
	e.mu.RLock()
	cfg := e.cfg
	e.mu.RUnlock()

	if !cfg.Enabled {
		return nil
	}

	// 1. realized_volatility_5m < baseline_1h × 0.6
	vol5m, _ := ctx.RealizedVol5m.Float64()
	volBase1h, _ := ctx.VolBaseline1h.Float64()
	if volBase1h == 0 || vol5m >= volBase1h*cfg.VolCompressionRatio {
		return nil
	}
	volCompression := 1 - vol5m/volBase1h

	// 2. OI_delta_5min 显著 |delta| > 1%（使用 OIDelta30s * 10 近似 5min）
	oiDelta5m, _ := ctx.OIDelta30s.Mul(decimal.NewFromInt(10)).Float64()
	if ctx.OIBaseline.IsZero() {
		return nil
	}
	baseline, _ := ctx.OIBaseline.Float64()
	oiDeltaPct := math.Abs(oiDelta5m) / baseline
	if oiDeltaPct < 0.01 {
		return nil
	}

	// 3. 无 squeeze（basis 正常）
	basisAbs, _ := ctx.BasisBps.Abs().Float64()
	if basisAbs > 5 {
		return nil
	}

	// 4. orderbook 流动性健康（decay 较低）
	decay, _ := ctx.OrderbookSurvivalDecay.Float64()
	if decay > 0.20 {
		return nil
	}

	// Direction: OI 方向 + RCVD30s + price momentum 三者一致
	oiDir := ctx.OIDelta30s.IsPositive()
	rcvdDir := ctx.RCVD30s.IsPositive()
	pmDir := ctx.PriceMomentum1m.IsPositive()

	var dir datafeed.Direction
	if oiDir && rcvdDir && pmDir {
		dir = datafeed.DirectionLong
	} else if !oiDir && !rcvdDir && !pmDir {
		dir = datafeed.DirectionShort
	} else {
		return nil // 三者不一致
	}

	oiLean := normalize(oiDeltaPct, 0.01, 0.05)
	rcvdLean, _ := ctx.RCVD30s.Abs().Float64()

	confidence := 0.40*normalize(volCompression, 0, 0.5) +
		0.30*normalize(oiLean, 0, 1) +
		0.30*normalize(rcvdLean, 0, 100)

	if confidence < cfg.ConfidenceThreshold {
		return nil
	}

	now := time.Now().UnixMilli()
	sig := &Signal{
		Engine:      EngineTransition,
		Direction:   dir,
		Confidence:  confidence,
		EntryPrice:  ctx.ETHMarkPrice,
		Reasoning:   "vol_compression=" + ctx.RealizedVol5m.StringFixed(4),
		GeneratedAt: now,
		ExpiresAt:   now + 2000,
	}
	log.Info().
		Str("engine", "TRANSITION").
		Str("dir", dirStr(dir)).
		Float64("confidence", confidence).
		Msg("signal generated")
	return sig
}
