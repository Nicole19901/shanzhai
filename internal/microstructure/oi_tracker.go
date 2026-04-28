package microstructure

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/datafeed"
)

type oiSample struct {
	oi decimal.Decimal
	ts int64
}

// OITracker REST 轮询 OI，保留 30 分钟历史
type OITracker struct {
	mu      sync.RWMutex
	history []oiSample // 按时间升序

	rest   *datafeed.RESTClient
	symbol string
	pollMs int64

	// 派生指标
	delta5s  decimal.Decimal
	delta30s decimal.Decimal
	velocity decimal.Decimal
	accel    decimal.Decimal
	baseline decimal.Decimal // 30min 平均 OI 作为 baseline

	consecutiveMiss int
}

func NewOITracker(rest *datafeed.RESTClient, symbol string, pollMs int64) *OITracker {
	return &OITracker{
		rest:   rest,
		symbol: symbol,
		pollMs: pollMs,
	}
}

func (t *OITracker) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(t.pollMs) * time.Millisecond)
	defer ticker.Stop()
	consecutiveMiss := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			oi, err := t.rest.OpenInterest(ctx, t.symbol)
			if err != nil {
				consecutiveMiss++
				t.mu.Lock()
				t.consecutiveMiss = consecutiveMiss
				t.mu.Unlock()
				log.Error().Err(err).Int("consecutive", consecutiveMiss).Msg("OI fetch failed")
				if consecutiveMiss >= 3 {
					log.Error().Msg("OI: 连续3次缺失，触发 DEGRADATION")
				}
				continue
			}
			consecutiveMiss = 0
			t.mu.Lock()
			t.consecutiveMiss = 0
			t.addSample(oiSample{oi: oi, ts: time.Now().UnixMilli()})
			t.recompute()
			t.mu.Unlock()
		}
	}
}

func (t *OITracker) addSample(s oiSample) {
	t.history = append(t.history, s)
	// 保留最近 30min
	cutoff := s.ts - 1_800_000
	for len(t.history) > 0 && t.history[0].ts < cutoff {
		t.history = t.history[1:]
	}
}

func (t *OITracker) recompute() {
	n := len(t.history)
	if n == 0 {
		return
	}
	cur := t.history[n-1]

	t.delta5s = t.deltaFrom(cur, 5_000)
	t.delta30s = t.deltaFrom(cur, 30_000)

	// velocity = delta30s / 30s
	t.velocity = t.delta30s.Div(decimal.NewFromInt(30))

	// acceleration = (velocity_now - velocity_5s_ago) / 5
	velPrev := t.velocityAt(cur.ts - 5_000)
	t.accel = t.velocity.Sub(velPrev).Div(decimal.NewFromInt(5))

	// baseline: 30min 平均
	var sum decimal.Decimal
	for _, s := range t.history {
		sum = sum.Add(s.oi)
	}
	t.baseline = sum.Div(decimal.NewFromInt(int64(n)))
}

func (t *OITracker) deltaFrom(cur oiSample, windowMs int64) decimal.Decimal {
	target := cur.ts - windowMs
	// 找最接近 target 的样本
	var ref *oiSample
	for i := range t.history {
		s := &t.history[i]
		if s.ts <= target {
			ref = s
		}
	}
	if ref == nil {
		return decimal.Zero
	}
	return cur.oi.Sub(ref.oi)
}

func (t *OITracker) velocityAt(targetTs int64) decimal.Decimal {
	// 找 targetTs 附近 30s 的 delta
	cutoff := targetTs - 30_000
	var ref *oiSample
	var cur *oiSample
	for i := range t.history {
		s := &t.history[i]
		if s.ts <= targetTs {
			cur = s
		}
		if s.ts <= cutoff {
			ref = s
		}
	}
	if ref == nil || cur == nil {
		return decimal.Zero
	}
	return cur.oi.Sub(ref.oi).Div(decimal.NewFromInt(30))
}

type OISnapshot struct {
	Delta5s         decimal.Decimal
	Delta30s        decimal.Decimal
	Velocity        decimal.Decimal
	Accel           decimal.Decimal
	Baseline        decimal.Decimal
	ConsecutiveMiss int
}

func (t *OITracker) Snapshot() OISnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return OISnapshot{
		Delta5s:         t.delta5s,
		Delta30s:        t.delta30s,
		Velocity:        t.velocity,
		Accel:           t.accel,
		Baseline:        t.baseline,
		ConsecutiveMiss: t.consecutiveMiss,
	}
}
