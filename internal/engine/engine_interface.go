package engine

import (
	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/datafeed"
)

type EngineType string

const (
	EngineTrend       EngineType = "TREND"
	EngineSqueeze     EngineType = "SQUEEZE"
	EngineTransition  EngineType = "TRANSITION"
	EngineLiquidation EngineType = "LIQUIDATION"
)

type Signal struct {
	Engine      EngineType
	Direction   datafeed.Direction
	Confidence  float64
	EntryPrice  decimal.Decimal
	Reasoning   string
	GeneratedAt int64 // ms
	ExpiresAt   int64 // ms
}

func (s *Signal) IsExpired(nowMs int64) bool {
	return nowMs > s.ExpiresAt
}

// Engine 引擎接口
type Engine interface {
	Evaluate(ctx *datafeed.MarketContext) *Signal
	Name() EngineType
}

// normalize 将 val 归一化到 [0,1]，min/max 为预期范围
func normalize(val, min, max float64) float64 {
	if max <= min {
		return 0
	}
	if val <= min {
		return 0
	}
	if val >= max {
		return 1
	}
	return (val - min) / (max - min)
}
