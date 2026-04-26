package microstructure

import (
	"math"
	"sync"

	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/datafeed"
)

// BasisTracker 跟踪 basis 和 basis zscore（Welford 在线算法）
type BasisTracker struct {
	mu sync.RWMutex

	// Welford
	count    int64
	mean     float64
	m2       float64 // sum of squared deviations

	lastBasisBps    float64
	prev500msBps    float64
	prev500msTime   int64

	currentBasisBps    decimal.Decimal
	currentBasisVel    decimal.Decimal
	currentBasisZScore decimal.Decimal
}

func NewBasisTracker() *BasisTracker {
	return &BasisTracker{}
}

func (b *BasisTracker) Update(mp *datafeed.MarkPrice) {
	if mp.IndexPrice.IsZero() {
		return
	}
	// basis_bps = (markPrice - indexPrice) / indexPrice * 10000
	diff := mp.MarkPrice.Sub(mp.IndexPrice)
	bpsD := diff.Div(mp.IndexPrice).Mul(decimal.NewFromInt(10000))
	bps, _ := bpsD.Float64()

	b.mu.Lock()
	defer b.mu.Unlock()

	// Welford online mean/variance
	b.count++
	delta := bps - b.mean
	b.mean += delta / float64(b.count)
	delta2 := bps - b.mean
	b.m2 += delta * delta2

	// velocity: bps/sec 相对 500ms 前
	var vel float64
	if b.prev500msTime > 0 && mp.EventTime-b.prev500msTime >= 400 {
		dtSec := float64(mp.EventTime-b.prev500msTime) / 1000.0
		vel = (bps - b.prev500msBps) / dtSec
		b.prev500msBps = bps
		b.prev500msTime = mp.EventTime
	} else if b.prev500msTime == 0 {
		b.prev500msBps = bps
		b.prev500msTime = mp.EventTime
	}

	// zscore
	var zscore float64
	if b.count >= 2 {
		variance := b.m2 / float64(b.count-1)
		std := math.Sqrt(variance)
		if std > 0 {
			zscore = (bps - b.mean) / std
		}
	}

	b.lastBasisBps = bps
	b.currentBasisBps = bpsD
	b.currentBasisVel = decimal.NewFromFloat(vel)
	b.currentBasisZScore = decimal.NewFromFloat(zscore)
}

type BasisSnapshot struct {
	BasisBps    decimal.Decimal
	Velocity    decimal.Decimal
	ZScore      decimal.Decimal
}

func (b *BasisTracker) Snapshot() BasisSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return BasisSnapshot{
		BasisBps: b.currentBasisBps,
		Velocity: b.currentBasisVel,
		ZScore:   b.currentBasisZScore,
	}
}
