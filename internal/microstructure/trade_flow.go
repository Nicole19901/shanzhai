package microstructure

import (
	"math"
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/datafeed"
)

// ringEntry 使用 float64 避免 decimal 的堆分配，RCVD 是方向性指标，float64 精度够用
type ringEntry struct {
	signedVol float64
	ts        int64
}

type rcvdWindow struct {
	buf      []ringEntry
	head     int
	size     int
	capacity int
	sum      float64
	windowMs int64
}

func newRcvdWindow(windowMs int64, capacity int) *rcvdWindow {
	return &rcvdWindow{
		buf:      make([]ringEntry, capacity),
		capacity: capacity,
		windowMs: windowMs,
	}
}

func (w *rcvdWindow) Add(signedVol float64, ts int64) {
	cutoff := ts - w.windowMs
	for w.size > 0 {
		oldest := w.buf[(w.head-w.size+w.capacity)%w.capacity]
		if oldest.ts >= cutoff {
			break
		}
		w.sum -= oldest.signedVol
		w.size--
	}
	idx := w.head % w.capacity
	w.buf[idx] = ringEntry{signedVol: signedVol, ts: ts}
	w.head = (w.head + 1) % w.capacity
	if w.size < w.capacity {
		w.size++
	}
	w.sum += signedVol
}

func (w *rcvdWindow) Value() float64 {
	return w.sum
}

// TradeFlowTracker 维护 RCVD 5s / 30s / 5m 三个滑动窗口
// 内部使用 float64，消除 decimal 热路径分配，支持高频 aggTrade 流
type TradeFlowTracker struct {
	mu   sync.Mutex // 只需普通 Mutex，Add 是唯一写路径
	w5s  *rcvdWindow
	w30s *rcvdWindow
	w5m  *rcvdWindow
}

func NewTradeFlowTracker() *TradeFlowTracker {
	return &TradeFlowTracker{
		w5s:  newRcvdWindow(5_000, 4096),
		w30s: newRcvdWindow(30_000, 16384),
		w5m:  newRcvdWindow(300_000, 65536),
	}
}

func (t *TradeFlowTracker) Add(trade *datafeed.AggTrade) {
	qty := trade.Quantity
	var sv float64
	if !trade.IsBuyerMaker {
		sv = qty
	} else {
		sv = -qty
	}
	ts := trade.EventTime
	t.mu.Lock()
	t.w5s.Add(sv, ts)
	t.w30s.Add(sv, ts)
	t.w5m.Add(sv, ts)
	t.mu.Unlock()
}

type RCVDSnapshot struct {
	RCVD5s  decimal.Decimal
	RCVD30s decimal.Decimal
	RCVD5m  decimal.Decimal
}

func (t *TradeFlowTracker) Snapshot() RCVDSnapshot {
	t.mu.Lock()
	s5 := t.w5s.Value()
	s30 := t.w30s.Value()
	s5m := t.w5m.Value()
	t.mu.Unlock()
	return RCVDSnapshot{
		RCVD5s:  decimal.NewFromFloat(s5),
		RCVD30s: decimal.NewFromFloat(s30),
		RCVD5m:  decimal.NewFromFloat(s5m),
	}
}

// VolatilityTracker 在线计算 realized volatility
type VolatilityTracker struct {
	mu      sync.RWMutex
	returns []volEntry
}

type volEntry struct {
	ret decimal.Decimal
	ts  int64
}

func NewVolatilityTracker() *VolatilityTracker {
	return &VolatilityTracker{returns: make([]volEntry, 0, 4096)}
}

func (v *VolatilityTracker) AddReturn(ret decimal.Decimal) {
	now := time.Now().UnixMilli()
	v.mu.Lock()
	v.returns = append(v.returns, volEntry{ret: ret, ts: now})
	cutoff := now - 3_600_000
	for len(v.returns) > 0 && v.returns[0].ts < cutoff {
		v.returns = v.returns[1:]
	}
	v.mu.Unlock()
}

func (v *VolatilityTracker) StdDev(windowMs int64) decimal.Decimal {
	v.mu.RLock()
	defer v.mu.RUnlock()
	cutoff := time.Now().UnixMilli() - windowMs
	var vals []decimal.Decimal
	for _, e := range v.returns {
		if e.ts >= cutoff {
			vals = append(vals, e.ret)
		}
	}
	return stdDev(vals)
}

func stdDev(vals []decimal.Decimal) decimal.Decimal {
	n := len(vals)
	if n < 2 {
		return decimal.Zero
	}
	nd := decimal.NewFromInt(int64(n))
	var sum decimal.Decimal
	for _, v := range vals {
		sum = sum.Add(v)
	}
	mean := sum.Div(nd)
	var sq decimal.Decimal
	for _, v := range vals {
		diff := v.Sub(mean)
		sq = sq.Add(diff.Mul(diff))
	}
	variance := sq.Div(nd)
	f, _ := variance.Float64()
	return decimal.NewFromFloat(math.Sqrt(f))
}
