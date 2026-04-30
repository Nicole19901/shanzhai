package microstructure

import (
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/datafeed"
)

const defaultDepthLevels = 5

type levelState struct {
	qty       decimal.Decimal
	firstSeen int64 // ms
}

// OrderBook 维护 ±5 档盘口，跟踪 level 存活时间和消耗率
type OrderBook struct {
	mu   sync.RWMutex
	bids map[string]*levelState // key = price string
	asks map[string]*levelState

	// 历史 avg survival time（简单 EMA）
	avgSurvivalMs     float64
	survivalBaseline  float64
	baselineInitialized bool
}

func NewOrderBook() *OrderBook {
	return &OrderBook{
		bids: make(map[string]*levelState),
		asks: make(map[string]*levelState),
	}
}

func (ob *OrderBook) Update(update *datafeed.DepthUpdate, depthLevels int) {
	if depthLevels < 1 {
		depthLevels = defaultDepthLevels
	}
	now := update.LocalTime
	ob.mu.Lock()
	defer ob.mu.Unlock()

	ob.updateSide(ob.bids, topN(update.Bids, depthLevels, false), now)
	ob.updateSide(ob.asks, topN(update.Asks, depthLevels, true), now)
}

func (ob *OrderBook) updateSide(side map[string]*levelState, levels []datafeed.PriceLevel, now int64) {
	active := make(map[string]bool)
	for _, l := range levels {
		key := l.Price.String()
		active[key] = true
		existing, ok := side[key]
		if !ok {
			side[key] = &levelState{qty: l.Quantity, firstSeen: now}
		} else {
			if l.Quantity.IsZero() {
				// 档位被吃掉
				survival := now - existing.firstSeen
				ob.recordSurvival(float64(survival))
				delete(side, key)
			} else {
				existing.qty = l.Quantity
			}
		}
	}
	// 清理不再出现的档位（防止内存泄漏）
	for key := range side {
		if !active[key] {
			delete(side, key)
		}
	}
}

func (ob *OrderBook) recordSurvival(ms float64) {
	if !ob.baselineInitialized {
		ob.avgSurvivalMs = ms
		ob.survivalBaseline = ms
		ob.baselineInitialized = true
		return
	}
	// EMA α=0.1
	ob.avgSurvivalMs = 0.9*ob.avgSurvivalMs + 0.1*ms
}

// SurvivalDecayRate 返回当前平均存活时间相对基线的下降率 [0,1]
func (ob *OrderBook) SurvivalDecayRate() decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if !ob.baselineInitialized || ob.survivalBaseline == 0 {
		return decimal.Zero
	}
	decay := (ob.survivalBaseline - ob.avgSurvivalMs) / ob.survivalBaseline
	if decay < 0 {
		decay = 0
	}
	return decimal.NewFromFloat(decay)
}

// BestBid / BestAsk
func (ob *OrderBook) BestBid() decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	best := decimal.Zero
	for k := range ob.bids {
		p, _ := decimal.NewFromString(k)
		if best.IsZero() || p.GreaterThan(best) {
			best = p
		}
	}
	return best
}

func (ob *OrderBook) BestAsk() decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	best := decimal.Zero
	for k := range ob.asks {
		p, _ := decimal.NewFromString(k)
		if best.IsZero() || p.LessThan(best) {
			best = p
		}
	}
	return best
}

// Spread 返回当前买卖价差（bps）
func (ob *OrderBook) SpreadBps() decimal.Decimal {
	bid := ob.BestBid()
	ask := ob.BestAsk()
	if bid.IsZero() || ask.IsZero() {
		return decimal.Zero
	}
	mid := bid.Add(ask).Div(decimal.NewFromInt(2))
	spread := ask.Sub(bid)
	bps := spread.Div(mid).Mul(decimal.NewFromInt(10000))
	return bps
}

// TotalDepth 返回同侧 5 档总量
func (ob *OrderBook) TotalDepth(isBid bool) decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	side := ob.asks
	if isBid {
		side = ob.bids
	}
	var total decimal.Decimal
	for _, s := range side {
		total = total.Add(s.qty)
	}
	return total
}

// SpreadWideningRate 简单用当前 spread / baseline spread - 1
func (ob *OrderBook) SpreadWideningRate(baselineSpreadBps decimal.Decimal) decimal.Decimal {
	current := ob.SpreadBps()
	if baselineSpreadBps.IsZero() {
		return decimal.Zero
	}
	return current.Sub(baselineSpreadBps).Div(baselineSpreadBps)
}

// SpreadBaseline 用于外部记录基线（简单近似）
var spreadBaseline decimal.Decimal
var spreadBaselineMu sync.Mutex
var spreadBaselineUpdated int64

func UpdateSpreadBaseline(bps decimal.Decimal) {
	spreadBaselineMu.Lock()
	defer spreadBaselineMu.Unlock()
	now := time.Now().UnixMilli()
	if spreadBaseline.IsZero() || now-spreadBaselineUpdated > 300_000 {
		spreadBaseline = bps
		spreadBaselineUpdated = now
	} else {
		// EMA
		f1, _ := spreadBaseline.Float64()
		f2, _ := bps.Float64()
		spreadBaseline = decimal.NewFromFloat(0.99*f1 + 0.01*f2)
	}
}

func GetSpreadBaseline() decimal.Decimal {
	spreadBaselineMu.Lock()
	defer spreadBaselineMu.Unlock()
	return spreadBaseline
}

// topN 取前 N 档（bids 取最高，asks 取最低）
func topN(levels []datafeed.PriceLevel, n int, ascending bool) []datafeed.PriceLevel {
	if len(levels) <= n {
		return levels
	}
	return levels[:n]
}
