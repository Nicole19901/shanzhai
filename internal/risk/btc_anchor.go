package risk

import (
	"math"
	"sync"

	"github.com/rs/zerolog/log"

	"github.com/yourorg/eth-perp-system/internal/config"
	"github.com/yourorg/eth-perp-system/internal/datafeed"
	"github.com/yourorg/eth-perp-system/internal/engine"
)

// BTCAnchor 计算 ETH-BTC Pearson 相关系数并过滤信号
type BTCAnchor struct {
	mu  sync.RWMutex
	cfg config.RiskConfig

	ethReturns []float64
	btcReturns []float64
	maxSamples int

	lastETHPrice float64
	lastBTCPrice float64

	corr1m float64
	corr5m float64
}

func NewBTCAnchor(cfg config.RiskConfig) *BTCAnchor {
	return &BTCAnchor{
		cfg:        cfg,
		maxSamples: 300, // 5min × 60 = 300 个 1s 样本
	}
}

func (a *BTCAnchor) AddETHPrice(price float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastETHPrice > 0 {
		ret := (price - a.lastETHPrice) / a.lastETHPrice
		a.ethReturns = append(a.ethReturns, ret)
		if len(a.ethReturns) > a.maxSamples {
			a.ethReturns = a.ethReturns[1:]
		}
	}
	a.lastETHPrice = price
}

func (a *BTCAnchor) AddBTCPrice(price float64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastBTCPrice > 0 {
		ret := (price - a.lastBTCPrice) / a.lastBTCPrice
		a.btcReturns = append(a.btcReturns, ret)
		if len(a.btcReturns) > a.maxSamples {
			a.btcReturns = a.btcReturns[1:]
		}
	}
	a.lastBTCPrice = price
	// 每次更新 BTC 价格时重算相关性
	a.recompute()
}

func (a *BTCAnchor) recompute() {
	n := min64(len(a.ethReturns), len(a.btcReturns))
	if n < 30 {
		return
	}
	a.corr1m = pearson(a.ethReturns[max64(0, n-60):n], a.btcReturns[max64(0, n-60):n])
	a.corr5m = pearson(a.ethReturns[max64(0, n-300):n], a.btcReturns[max64(0, n-300):n])
}

// Adjust 根据 BTC 相关性过滤/调整信号，BTC 数据不足时拒绝所有信号
func (a *BTCAnchor) Adjust(sig *engine.Signal, btcTrend1m, btcTrend5m datafeed.Direction) *engine.Signal {
	a.mu.RLock()
	defer a.mu.RUnlock()

	n := min64(len(a.ethReturns), len(a.btcReturns))
	if n < 30 {
		log.Warn().Msg("BTC anchor: insufficient data, rejecting signal")
		return nil
	}

	corr1m := a.corr1m
	corr5m := a.corr5m

	// 高相关期：与 BTC 反向则拒绝
	if corr1m > a.cfg.BTCCorrelationLockThreshold {
		if sig.Direction != btcTrend1m && btcTrend1m != datafeed.DirectionFlat {
			log.Info().Msg("BTC anchor: signal rejected (anti-BTC in high-corr regime)")
			return nil
		}
		sig.Confidence *= 0.85
	}

	// 5m 强相关且反向
	if corr5m > 0.85 && sig.Direction != btcTrend5m && btcTrend5m != datafeed.DirectionFlat {
		log.Info().Msg("BTC anchor: signal rejected (anti-BTC 5m trend)")
		return nil
	}

	return sig
}

func pearson(x, y []float64) float64 {
	n := min64(len(x), len(y))
	if n < 2 {
		return 0
	}
	var sumX, sumY, sumXY, sumX2, sumY2 float64
	fn := float64(n)
	for i := 0; i < n; i++ {
		sumX += x[i]
		sumY += y[i]
		sumXY += x[i] * y[i]
		sumX2 += x[i] * x[i]
		sumY2 += y[i] * y[i]
	}
	num := fn*sumXY - sumX*sumY
	den := math.Sqrt((fn*sumX2 - sumX*sumX) * (fn*sumY2 - sumY*sumY))
	if den == 0 {
		return 0
	}
	return num / den
}

func min64(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int) int {
	if a > b {
		return a
	}
	return b
}
