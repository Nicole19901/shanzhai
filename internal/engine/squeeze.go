package engine

import (
	"math"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/yourorg/eth-perp-system/internal/config"
	"github.com/yourorg/eth-perp-system/internal/datafeed"
)

type SqueezeEngine struct {
	mu  sync.RWMutex
	cfg config.SqueezeEngineConfig
}

func NewSqueezeEngine(cfg config.SqueezeEngineConfig) *SqueezeEngine {
	return &SqueezeEngine{cfg: cfg}
}

func (e *SqueezeEngine) SetConfig(enabled bool, confidence, basisZScore float64) {
	e.mu.Lock()
	e.cfg.Enabled = enabled
	e.cfg.ConfidenceThreshold = confidence
	e.cfg.BasisZScoreThreshold = basisZScore
	e.mu.Unlock()
}

func (e *SqueezeEngine) Name() EngineType { return EngineSqueeze }

func (e *SqueezeEngine) Evaluate(ctx *datafeed.MarketContext) *Signal {
	e.mu.RLock()
	cfg := e.cfg
	e.mu.RUnlock()

	if !cfg.Enabled {
		return nil
	}

	// funding 结算前后 60 秒屏蔽
	nowMs := time.Now().UnixMilli()
	if ctx.NextFundingMs > 0 {
		distMs := ctx.NextFundingMs - nowMs
		if distMs < 60_000 || distMs > 28_800_000-60_000 {
			return nil
		}
	}

	// BTC 剧烈波动时屏蔽
	btcVol, _ := ctx.RealizedVol1m.Float64()
	if btcVol > 0.003 {
		return nil
	}

	// 1. |basis_zscore| > 2.5
	zscore, _ := ctx.BasisZScore.Float64()
	if math.Abs(zscore) < cfg.BasisZScoreThreshold {
		return nil
	}

	// 2. basis_velocity 方向明确 (>0.5 bps/sec)
	vel, _ := ctx.BasisVelocity.Float64()
	if math.Abs(vel) < 0.5 {
		return nil
	}

	// 3. orderbook survival decay > 30%
	decay, _ := ctx.OrderbookSurvivalDecay.Float64()
	if decay < 0.30 {
		return nil
	}

	// 4. OI acceleration 反向 OR OI_delta_5s < -0.3%
	oiAccel, _ := ctx.OIAccel.Float64()
	oiDelta5s, _ := ctx.OIDelta5s.Float64()
	oiCondition := false
	if !ctx.OIBaseline.IsZero() {
		baseline, _ := ctx.OIBaseline.Float64()
		if baseline > 0 {
			oiDelta5sPct := oiDelta5s / baseline
			if oiDelta5sPct < -0.003 {
				oiCondition = true
			}
		}
	}
	if !oiCondition && oiAccel >= 0 {
		return nil
	}

	// 5. spread widening > baseline × 2
	spreadRate, _ := ctx.SpreadWideningRate.Float64()
	if spreadRate < 1.0 { // 即 > baseline × 2
		return nil
	}

	// Direction: basis > 0 and vel > 0 → 多头被挤 → SHORT
	basisBps, _ := ctx.BasisBps.Float64()
	var dir datafeed.Direction
	if basisBps > 0 && vel > 0 {
		dir = datafeed.DirectionShort
	} else if basisBps < 0 && vel < 0 {
		dir = datafeed.DirectionLong
	} else {
		return nil
	}

	confidence := 0.35*normalize(math.Abs(zscore), 2.5, 5.0) +
		0.25*normalize(decay, 0.3, 1.0) +
		0.20*normalize(math.Abs(oiAccel), 0, 10) +
		0.20*normalize(spreadRate, 1.0, 3.0)

	if confidence < cfg.ConfidenceThreshold {
		return nil
	}

	sig := &Signal{
		Engine:      EngineSqueeze,
		Direction:   dir,
		Confidence:  confidence,
		EntryPrice:  ctx.ETHMarkPrice,
		Reasoning:   "zscore=" + ctx.BasisZScore.StringFixed(2) + " decay=" + ctx.OrderbookSurvivalDecay.StringFixed(2),
		GeneratedAt: nowMs,
		ExpiresAt:   nowMs + 1000, // squeeze 信号 1s 过期
	}
	log.Info().
		Str("engine", "SQUEEZE").
		Str("dir", dirStr(dir)).
		Float64("confidence", confidence).
		Msg("signal generated")
	return sig
}
