package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/config"
	"github.com/yourorg/eth-perp-system/internal/datafeed"
	"github.com/yourorg/eth-perp-system/internal/engine"
	"github.com/yourorg/eth-perp-system/internal/microstructure"
	"github.com/yourorg/eth-perp-system/internal/webui"
)

// engSigEntry is a named type for engine signal snapshots stored in atomic.Value.
type engSigEntry struct {
	Name       string  `json:"name"`
	Dir        string  `json:"dir"`
	Confidence float64 `json:"confidence"`
	Fired      bool    `json:"fired"`
	Threshold  float64 `json:"threshold"`
	AgeMs      int64   `json:"age_ms"`
}

// engSigSlice wraps []engSigEntry so it can be stored in atomic.Value.
type engSigSlice struct {
	sigs []engSigEntry
}

// MarkPriceHook is called whenever a mark price update arrives for a symbol.
type MarkPriceHook func(sym string, price decimal.Decimal)

// SignalHook is called when an engine fires a valid signal for a symbol.
// Only the first valid signal per evaluation tick is delivered (engines are tried in order).
type SignalHook func(sym string, sig *engine.Signal, mctx *datafeed.MarketContext)

// symState holds per-symbol monitoring state.
type symState struct {
	symbol string
	chs    *datafeed.Channels

	tradeFlow    *microstructure.TradeFlowTracker
	volTracker   *microstructure.VolatilityTracker
	orderBook    *microstructure.OrderBook
	basisTracker *microstructure.BasisTracker
	oiTracker    *microstructure.OITracker

	markPrice  atomic.Value // decimal.Decimal
	indexPrice atomic.Value // decimal.Decimal
	funding    atomic.Value // decimal.Decimal
	nextFund   atomic.Int64
	prevClose  atomic.Value // decimal.Decimal
	priceMom   atomic.Value // decimal.Decimal

	msgCount atomic.Int64

	latestMarket  atomic.Value // map[string]interface{}
	latestEngSigs atomic.Value // engSigSlice

	cancel context.CancelFunc
}

func newSymState(sym string, rest *datafeed.RESTClient, cfg *config.Config) *symState {
	s := &symState{
		symbol:       sym,
		chs:          datafeed.NewChannels(),
		tradeFlow:    microstructure.NewTradeFlowTracker(),
		volTracker:   microstructure.NewVolatilityTracker(),
		orderBook:    microstructure.NewOrderBook(),
		basisTracker: microstructure.NewBasisTracker(),
		oiTracker:    microstructure.NewOITracker(rest, sym, cfg.Sampling.OIPollIntervalMs),
		cancel:       func() {},
	}
	s.markPrice.Store(decimal.Zero)
	s.indexPrice.Store(decimal.Zero)
	s.funding.Store(decimal.Zero)
	s.prevClose.Store(decimal.Zero)
	s.priceMom.Store(decimal.Zero)
	s.latestMarket.Store(map[string]interface{}{"ready": false})
	s.latestEngSigs.Store(engSigSlice{sigs: []engSigEntry{}})
	return s
}

// SymbolWatcher manages per-symbol monitoring for multiple symbols.
type SymbolWatcher struct {
	mu     sync.RWMutex
	states map[string]*symState

	ws         *datafeed.WSClient
	rest       *datafeed.RESTClient
	liveParams *webui.LiveParams
	cfg        *config.Config
	parentCtx  context.Context

	hookMu        sync.RWMutex
	markPriceHook MarkPriceHook
	signalHook    SignalHook
}

// NewSymbolWatcher creates a new SymbolWatcher.
func NewSymbolWatcher(ws *datafeed.WSClient, rest *datafeed.RESTClient, liveParams *webui.LiveParams, cfg *config.Config) *SymbolWatcher {
	return &SymbolWatcher{
		states:     make(map[string]*symState),
		ws:         ws,
		rest:       rest,
		liveParams: liveParams,
		cfg:        cfg,
	}
}

// SetParentContext sets the parent context used when spawning goroutines.
// Must be called before Add.
func (w *SymbolWatcher) SetParentContext(ctx context.Context) {
	w.parentCtx = ctx
}

// SetMarkPriceHook registers a callback fired on every mark price update.
func (w *SymbolWatcher) SetMarkPriceHook(fn MarkPriceHook) {
	w.hookMu.Lock()
	defer w.hookMu.Unlock()
	w.markPriceHook = fn
}

// SetSignalHook registers a callback fired when the first valid engine signal fires
// per evaluation tick.
func (w *SymbolWatcher) SetSignalHook(fn SignalHook) {
	w.hookMu.Lock()
	defer w.hookMu.Unlock()
	w.signalHook = fn
}

// GetMsgCount returns the number of WS messages received for a symbol so far.
func (w *SymbolWatcher) GetMsgCount(sym string) int64 {
	w.mu.RLock()
	state, ok := w.states[sym]
	w.mu.RUnlock()
	if !ok {
		return 0
	}
	return state.msgCount.Load()
}

// Add validates and adds a symbol to the watcher, establishing its WS connection.
// Returns nil immediately if the symbol is already watched.
func (w *SymbolWatcher) Add(ctx context.Context, sym string) error {
	w.mu.Lock()
	if _, exists := w.states[sym]; exists {
		w.mu.Unlock()
		return nil
	}
	state := newSymState(sym, w.rest, w.cfg)
	// 在锁内派生 cancel，确保 Remove() 拿到真实 cancel 而不是 no-op 占位符
	symCtx, cancel := context.WithCancel(ctx)
	state.cancel = cancel
	w.states[sym] = state
	w.mu.Unlock()

	log.Info().Str("symbol", sym).Msg("watcher: adding symbol, starting goroutines")
	w.startStateGoroutines(symCtx, state)
	w.rebuildWS()
	log.Info().Str("symbol", sym).Msg("watcher: symbol added, WS streams rebuilt")
	return nil
}

// Remove cancels all goroutines for a symbol and removes its WS streams.
func (w *SymbolWatcher) Remove(sym string) {
	w.mu.Lock()
	state, ok := w.states[sym]
	if ok {
		delete(w.states, sym)
	}
	w.mu.Unlock()

	if ok {
		state.cancel()
		w.rebuildWS()
		log.Info().Str("symbol", sym).Msg("watcher: symbol removed, WS streams rebuilt")
	}
}

// Has reports whether sym is currently watched.
func (w *SymbolWatcher) Has(sym string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	_, ok := w.states[sym]
	return ok
}

// Symbols returns all currently watched symbols.
func (w *SymbolWatcher) Symbols() []string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out := make([]string, 0, len(w.states))
	for sym := range w.states {
		out = append(out, sym)
	}
	return out
}

// GetSnapshot returns the latest market snapshot + engine signals for one symbol.
func (w *SymbolWatcher) GetSnapshot(sym string) (map[string]interface{}, bool) {
	w.mu.RLock()
	state, ok := w.states[sym]
	w.mu.RUnlock()
	if !ok {
		return nil, false
	}
	m := state.latestMarket.Load().(map[string]interface{})
	sigs := state.latestEngSigs.Load().(engSigSlice)

	out := make(map[string]interface{}, len(m)+2)
	for k, v := range m {
		out[k] = v
	}
	engList := make([]map[string]interface{}, len(sigs.sigs))
	for i, s := range sigs.sigs {
		engList[i] = map[string]interface{}{
			"name":       s.Name,
			"dir":        s.Dir,
			"confidence": s.Confidence,
			"fired":      s.Fired,
			"threshold":  s.Threshold,
			"age_ms":     s.AgeMs,
		}
	}
	out["engines"] = engList
	return out, true
}

// AllSnapshots returns snapshots for all watched symbols.
func (w *SymbolWatcher) AllSnapshots() map[string]interface{} {
	w.mu.RLock()
	syms := make([]string, 0, len(w.states))
	for sym := range w.states {
		syms = append(syms, sym)
	}
	w.mu.RUnlock()

	result := make(map[string]interface{}, len(syms))
	for _, sym := range syms {
		if snap, ok := w.GetSnapshot(sym); ok {
			result[sym] = snap
		}
	}
	return result
}

// rebuildWS rebuilds WS streams for all currently watched symbols.
// Called after Add or Remove.
func (w *SymbolWatcher) rebuildWS() {
	w.mu.RLock()
	syms := make([]string, 0, len(w.states))
	for sym := range w.states {
		syms = append(syms, sym)
	}
	w.mu.RUnlock()

	streams := []string{}
	handlers := map[string]datafeed.StreamHandler{}
	for _, sym := range syms {
		w.mu.RLock()
		state, ok := w.states[sym]
		w.mu.RUnlock()
		if !ok {
			continue
		}
		for _, s := range symbolStreams(sym) {
			streams = append(streams, s)
		}
		for k, h := range symbolHandlers(sym, state.chs) {
			handlers[k] = h
		}
	}
	if err := w.ws.SetStreams(streams, handlers); err != nil {
		log.Error().Err(err).Msg("watcher: SetStreams failed")
	}
}

// startStateGoroutines launches all goroutines for a symState.
// ctx must already be a child context derived from the parent; cancel is stored on state before this call.
func (w *SymbolWatcher) startStateGoroutines(ctx context.Context, state *symState) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Str("symbol", state.symbol).Msg("watcher aggTrade goroutine panic")
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case t, ok := <-state.chs.ETHAggTrade:
				if !ok {
					return
				}
				state.msgCount.Add(1)
				state.tradeFlow.Add(t)
			}
		}
	}()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Str("symbol", state.symbol).Msg("watcher markPrice goroutine panic")
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case mp, ok := <-state.chs.ETHMarkPrice:
				if !ok {
					return
				}
				state.msgCount.Add(1)
				state.basisTracker.Update(mp)
				state.markPrice.Store(mp.MarkPrice)
				state.indexPrice.Store(mp.IndexPrice)
				state.funding.Store(mp.FundingRate)
				state.nextFund.Store(mp.NextFundingTime)
				// Notify execution layer
				w.hookMu.RLock()
				hook := w.markPriceHook
				w.hookMu.RUnlock()
				if hook != nil {
					hook(state.symbol, mp.MarkPrice)
				}
			}
		}
	}()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Str("symbol", state.symbol).Msg("watcher kline goroutine panic")
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case k, ok := <-state.chs.ETHKline1m:
				if !ok {
					return
				}
				if !k.IsClosed {
					continue
				}
				prev := state.prevClose.Load().(decimal.Decimal)
				if !prev.IsZero() {
					ret := k.Close.Sub(prev).Div(prev)
					state.volTracker.AddReturn(ret)
					state.priceMom.Store(ret)
				}
				state.prevClose.Store(k.Close)
			}
		}
	}()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Str("symbol", state.symbol).Msg("watcher depth goroutine panic")
			}
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case d, ok := <-state.chs.ETHDepth:
				if !ok {
					return
				}
				lp := w.liveParams.Get()
				state.orderBook.Update(d, lp.DepthLevels)
				// Keep a global spread baseline (best-effort for multi-symbol)
				microstructure.UpdateSpreadBaseline(state.orderBook.SpreadBps())
			}
		}
	}()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Str("symbol", state.symbol).Msg("watcher OI goroutine panic")
			}
		}()
		state.oiTracker.Run(ctx)
	}()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error().Interface("panic", r).Str("symbol", state.symbol).Msg("watcher engine evaluator panic")
			}
		}()
		w.runEngineEvaluator(ctx, state)
	}()
}

// runEngineEvaluator runs the per-symbol engine evaluation loop.
// It builds MarketContext, updates the visual snapshot, and fires the SignalHook
// for the first valid signal per tick.
func (w *SymbolWatcher) runEngineEvaluator(ctx context.Context, state *symState) {
	trendEng := engine.NewTrendEngine(w.cfg.Engines.Trend)
	squeezeEng := engine.NewSqueezeEngine(w.cfg.Engines.Squeeze)
	transEng := engine.NewTransitionEngine(w.cfg.Engines.Transition)
	engines := []engine.Engine{trendEng, squeezeEng, transEng}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	f64 := func(d decimal.Decimal) float64 { v, _ := d.Float64(); return v }

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Must have mark price and enough messages before running engines
		ethMark := state.markPrice.Load().(decimal.Decimal)
		if ethMark.IsZero() {
			continue
		}
		if state.msgCount.Load() < 100 {
			continue
		}

		lp := w.liveParams.Get()

		rcvd := state.tradeFlow.Snapshot()
		oi := state.oiTracker.Snapshot()
		basis := state.basisTracker.Snapshot()
		spreadBaseline := microstructure.GetSpreadBaseline()

		mctx := &datafeed.MarketContext{
			ETHMarkPrice:  ethMark,
			ETHIndexPrice: state.indexPrice.Load().(decimal.Decimal),
			FundingRate:   state.funding.Load().(decimal.Decimal),
			NextFundingMs: state.nextFund.Load(),

			RCVD5s:  rcvd.RCVD5s,
			RCVD30s: rcvd.RCVD30s,
			RCVD5m:  rcvd.RCVD5m,

			OIDelta5s:  oi.Delta5s,
			OIDelta30s: oi.Delta30s,
			OIDelta5m:  oi.Delta5m,
			OIVelocity: oi.Velocity,
			OIAccel:    oi.Accel,
			OIBaseline: oi.Baseline,

			BasisBps:      basis.BasisBps,
			BasisVelocity: basis.Velocity,
			BasisZScore:   basis.ZScore,

			OrderbookSurvivalDecay: state.orderBook.SurvivalDecayRate(),
			SpreadWideningRate:     state.orderBook.SpreadWideningRate(spreadBaseline),
			SpreadBps:              state.orderBook.SpreadBps(),
			TotalBidDepth:          state.orderBook.TotalDepth(true),
			TotalAskDepth:          state.orderBook.TotalDepth(false),

			RealizedVol1m: state.volTracker.StdDev(60_000),
			RealizedVol5m: state.volTracker.StdDev(300_000),
			VolBaseline1h: state.volTracker.StdDev(3_600_000),

			PriceMomentum1m: state.priceMom.Load().(decimal.Decimal),

			SnapshotTime: datafeed.NowMs(),
		}

		// Update visual snapshot
		state.latestMarket.Store(map[string]interface{}{
			"ready":           true,
			"symbol":          state.symbol,
			"snapshot_age_ms": datafeed.NowMs() - mctx.SnapshotTime,
			"mark_price":      mctx.ETHMarkPrice.StringFixed(2),
			"index_price":     mctx.ETHIndexPrice.StringFixed(2),
			"basis_bps":       f64(mctx.BasisBps),
			"basis_velocity":  f64(mctx.BasisVelocity),
			"basis_zscore":    f64(mctx.BasisZScore),
			"funding_rate":    f64(mctx.FundingRate),
			"next_funding_ms": mctx.NextFundingMs,
			"rcvd_5s":         f64(mctx.RCVD5s),
			"rcvd_30s":        f64(mctx.RCVD30s),
			"rcvd_5m":         f64(mctx.RCVD5m),
			"oi_delta_5s":     f64(mctx.OIDelta5s),
			"oi_delta_30s":    f64(mctx.OIDelta30s),
			"oi_delta_5m":     f64(mctx.OIDelta5m),
			"oi_velocity":     f64(mctx.OIVelocity),
			"oi_accel":        f64(mctx.OIAccel),
			"oi_baseline":     f64(mctx.OIBaseline),
			"vol_1m":          f64(mctx.RealizedVol1m),
			"vol_5m":          f64(mctx.RealizedVol5m),
			"vol_baseline_1h": f64(mctx.VolBaseline1h),
			"price_momentum":  f64(mctx.PriceMomentum1m),
			"survival_decay":  f64(mctx.OrderbookSurvivalDecay),
			"spread_widening": f64(mctx.SpreadWideningRate),
			"spread_bps":      f64(mctx.SpreadBps),
			"msg_count":       state.msgCount.Load(),
		})

		// Configure engines from live params（使用多空中较低阈值让引擎评估，方向过滤在下方逐信号处理）
		trendMin := lp.TrendLongConfidence
		if lp.TrendShortConfidence < trendMin { trendMin = lp.TrendShortConfidence }
		squeezeMin := lp.SqueezeLongConfidence
		if lp.SqueezeShortConfidence < squeezeMin { squeezeMin = lp.SqueezeShortConfidence }
		transMin := lp.TransitionLongConfidence
		if lp.TransitionShortConfidence < transMin { transMin = lp.TransitionShortConfidence }
		trendEng.SetConfig(lp.TrendEnabled != nil && *lp.TrendEnabled, trendMin, lp.OIDeltaThreshold)
		squeezeEng.SetConfig(lp.SqueezeEnabled != nil && *lp.SqueezeEnabled, squeezeMin, lp.BasisZScoreThreshold)
		transEng.SetConfig(lp.TransitionEnabled != nil && *lp.TransitionEnabled, transMin, lp.VolCompressionRatio)

		engThresholds := map[engine.EngineType]float64{
			engine.EngineTrend:      trendMin,
			engine.EngineSqueeze:    squeezeMin,
			engine.EngineTransition: transMin,
		}

		// Evaluate engines: build visual snapshots and fire signal hook on first valid signal.
		w.hookMu.RLock()
		sigHook := w.signalHook
		w.hookMu.RUnlock()

		sigSnaps := make([]engSigEntry, 0, len(engines))
		hookFired := false
		for _, eng := range engines {
			rawSig := eng.Evaluate(mctx)
			// 方向独立置信度过滤
			if rawSig != nil {
				var dirThresh float64
				switch eng.Name() {
				case engine.EngineTrend:
					if rawSig.Direction == datafeed.DirectionLong { dirThresh = lp.TrendLongConfidence } else { dirThresh = lp.TrendShortConfidence }
				case engine.EngineSqueeze:
					if rawSig.Direction == datafeed.DirectionLong { dirThresh = lp.SqueezeLongConfidence } else { dirThresh = lp.SqueezeShortConfidence }
				case engine.EngineTransition:
					if rawSig.Direction == datafeed.DirectionLong { dirThresh = lp.TransitionLongConfidence } else { dirThresh = lp.TransitionShortConfidence }
				}
				if rawSig.Confidence < dirThresh {
					rawSig = nil
				}
			}
			snap := engSigEntry{
				Name:      string(eng.Name()),
				Dir:       "FLAT",
				Threshold: engThresholds[eng.Name()],
				AgeMs:     datafeed.NowMs() - mctx.SnapshotTime,
			}
			if rawSig != nil {
				snap.Dir = watcherDirLabel(rawSig.Direction)
				snap.Confidence = rawSig.Confidence
				snap.Fired = !rawSig.IsExpired(datafeed.NowMs())
				// Fire signal hook for first valid signal (execution layer decides what to do)
				if !hookFired && snap.Fired && sigHook != nil {
					sigHook(state.symbol, rawSig, mctx)
					hookFired = true
				}
			}
			sigSnaps = append(sigSnaps, snap)
		}
		state.latestEngSigs.Store(engSigSlice{sigs: sigSnaps})
	}
}

func watcherDirLabel(d datafeed.Direction) string {
	switch d {
	case datafeed.DirectionLong:
		return "LONG"
	case datafeed.DirectionShort:
		return "SHORT"
	default:
		return "FLAT"
	}
}
