package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/config"
	"github.com/yourorg/eth-perp-system/internal/datafeed"
	"github.com/yourorg/eth-perp-system/internal/engine"
	"github.com/yourorg/eth-perp-system/internal/execution"
	"github.com/yourorg/eth-perp-system/internal/microstructure"
	"github.com/yourorg/eth-perp-system/internal/risk"
	"github.com/yourorg/eth-perp-system/internal/statemachine"
	"github.com/yourorg/eth-perp-system/internal/telemetry"
	"github.com/yourorg/eth-perp-system/internal/webui"
)

func main() {
	log.Logger = zerolog.New(os.Stdout).With().Timestamp().Logger()

	defer func() {
		if r := recover(); r != nil {
			log.Fatal().Interface("panic", r).Msg("unhandled panic in main")
		}
	}()

	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
	}
	log.Info().Str("symbol", cfg.Trading.Symbol).Int("leverage", cfg.Trading.Leverage).Msg("config loaded")
	var activeSymbol atomic.Value
	activeSymbol.Store(strings.ToUpper(cfg.Trading.Symbol))
	currentSymbol := func() string { return activeSymbol.Load().(string) }

	rest := datafeed.NewRESTClient(cfg.Binance.RESTEndpoint, cfg.Binance.APIKey, cfg.Binance.APISecret)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if rest.HasCredentials() {
		bootCheck(ctx, rest, cfg)
	} else {
		log.Warn().Msg("boot check skipped: missing Binance API credentials; webui and market-data mode only")
	}

	metrics := telemetry.NewMetrics()
	metrics.ServeHTTP(":9090")

	liveParams := webui.NewLiveParams(cfg)
	eventLog := webui.NewEventLog(300)

	admin := webui.NewServer(liveParams, eventLog, rest, cfg.WebUI.ServiceName, cfg.Trading.Symbol)
	admin.Listen(cfg.WebUI.Addr)

	pm := execution.NewPositionManager(cfg.Trading.Symbol)

	sm := statemachine.New(func(from, to statemachine.State, reason string) {
		metrics.StateTransition(from.String(), to.String())
		metrics.SetState(float64(to))
	})

	guard := risk.NewGuardrails(cfg.Risk.DailyLossLimitPct, cfg.Risk.ConsecutiveLossLimit)
	slippageEst := risk.NewSlippageEstimator()

	omgr := execution.NewOrderManager(rest, cfg, metrics)
	omgr.SetSymbolProvider(currentSymbol)
	tradeHandler := execution.NewTradeHandler(pm, omgr, sm, guard, cfg, liveParams, eventLog, metrics)

	admin.SetManualTrader(tradeHandler)

	recon := execution.NewReconciliationLoop(
		rest, pm, omgr, sm, cfg.Trading.Symbol,
		cfg.Execution.ReconciliationIntervalSec,
	)
	recon.SetSymbolProvider(currentSymbol)

	tradeFlow := microstructure.NewTradeFlowTracker()
	volTracker := microstructure.NewVolatilityTracker()
	orderBook := microstructure.NewOrderBook()
	basisTracker := microstructure.NewBasisTracker()
	oiTracker := microstructure.NewOITracker(rest, cfg.Trading.Symbol, cfg.Sampling.OIPollIntervalMs)
	oiTracker.SetSymbolProvider(currentSymbol)

	trendEng := engine.NewTrendEngine(cfg.Engines.Trend)
	squeezeEng := engine.NewSqueezeEngine(cfg.Engines.Squeeze)
	transEng := engine.NewTransitionEngine(cfg.Engines.Transition)
	engines := []engine.Engine{trendEng, squeezeEng, transEng}

	chs := datafeed.NewChannels()

	ethHandlers := symbolHandlers(currentSymbol(), chs)

	ethWS, err := datafeed.NewWSClient(cfg.Binance.WSEndpoint,
		symbolStreams(currentSymbol()),
		ethHandlers)
	if err != nil {
		log.Fatal().Err(err).Msg("eth ws init failed")
	}
	var symbolMsgCount int64
	warmupDone := make(chan struct{})
	go func() {
		// 宽限期：前 10 次（5 分钟）只写 debug 日志，不刷 eventLog
		// 超过宽限期后每 10 次（5 分钟）写一次 eventLog，避免刷屏
		const graceTicks = 10
		var tick int
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			<-ticker.C
			tick++
			if !rest.HasCredentials() {
				log.Debug().Msg("data stream warmup: waiting for API credentials")
				continue
			}
			symbolMsgs := atomic.LoadInt64(&symbolMsgCount)
			if symbolMsgs < 100 {
				log.Warn().Str("symbol", currentSymbol()).Int64("symbol_msgs", symbolMsgs).
					Int("tick", tick).
					Msg("data stream warmup: insufficient messages (WS may not be connected)")
				// 宽限期内不刷 eventLog；超过后每 5 分钟记录一次
				if tick > graceTicks && tick%10 == 1 {
					eventLog.AddSystem("DATA_WARMUP_WAIT", fmt.Sprintf("数据流预热等待中：已收到 %d/100 条消息，请检查网络是否能访问 Binance WS", symbolMsgs), map[string]interface{}{
						"symbol":      currentSymbol(),
						"symbol_msgs": symbolMsgs,
						"wait_min":    tick / 2,
					})
				}
				continue
			}
			log.Info().Str("symbol", currentSymbol()).Int64("symbol_msgs", symbolMsgs).Msg("data stream stable")
			eventLog.AddSystem("DATA_READY", "数据流预热完成，开始计时引擎启动", map[string]interface{}{
				"symbol":      currentSymbol(),
				"symbol_msgs": symbolMsgs,
			})
			close(warmupDone)
			return
		}
	}()

	var (
		latestETHMark  atomic.Value // decimal.Decimal
		latestETHIndex atomic.Value
		latestFunding  atomic.Value
		latestNextFund atomic.Int64
		latestPriceMom atomic.Value // decimal.Decimal

		prevETHClose atomic.Value
	)
	latestETHMark.Store(decimal.Zero)
	latestETHIndex.Store(decimal.Zero)
	latestFunding.Store(decimal.Zero)
	latestPriceMom.Store(decimal.Zero)
	prevETHClose.Store(decimal.Zero)

	tradeHandler.SetMarkPriceProvider(func() decimal.Decimal {
		return latestETHMark.Load().(decimal.Decimal)
	})
	admin.SetSymbolSwitcher(func(ctx context.Context, sym string) error {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" {
			return fmt.Errorf("symbol is required")
		}
		if !pm.SetSymbol(sym) {
			return fmt.Errorf("cannot switch symbol while local position is open")
		}
		oldSymbol := currentSymbol()
		if rest.HasCredentials() {
			if err := rest.CancelAllOrders(ctx, oldSymbol); err != nil {
				log.Warn().Err(err).Str("symbol", oldSymbol).Msg("cancel current symbol orders during symbol switch failed")
			}
			if err := rest.SetLeverage(ctx, sym, liveParams.Get().Leverage); err != nil {
				log.Warn().Err(err).Str("symbol", sym).Int("leverage", liveParams.Get().Leverage).Msg("set leverage during symbol switch failed")
			}
			if err := rest.SetMarginType(ctx, sym, cfg.Trading.MarginType); err != nil {
				log.Warn().Err(err).Str("symbol", sym).Msg("set margin type during symbol switch failed")
			}
		}
		activeSymbol.Store(sym)
		admin.SetSymbol(sym)
		latestETHMark.Store(decimal.Zero)
		latestETHIndex.Store(decimal.Zero)
		latestFunding.Store(decimal.Zero)
		latestPriceMom.Store(decimal.Zero)
		prevETHClose.Store(decimal.Zero)
		atomic.StoreInt64(&symbolMsgCount, 0)
		return ethWS.SetStreams(symbolStreams(sym), symbolHandlers(sym, chs))
	})

	var wg sync.WaitGroup
	start := func(name string, fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Error().Interface("panic", r).Str("goroutine", name).Msg("goroutine panic")
				}
			}()
			fn()
		}()
	}

	start("eth_ws", func() { ethWS.Run(ctx) })
	start("state_machine", func() { sm.Run(ctx) })
	start("oi_tracker", func() { oiTracker.Run(ctx) })
	start("reconciliation", func() { recon.Run(ctx) })

	start("eth_aggtrade_handler", func() {
		for {
			select {
			case <-ctx.Done():
				return
			case t, ok := <-chs.ETHAggTrade:
				if !ok {
					return
				}
				atomic.AddInt64(&symbolMsgCount, 1)
				if !datafeed.ValidateLatency(t.EventTime, t.LocalTime, latencyThresholdMs(sm)) {
					metrics.DataError("eth_aggtrade", "stale")
					continue
				}
				tradeFlow.Add(t)
			}
		}
	})

	start("eth_depth_handler", func() {
		for {
			select {
			case <-ctx.Done():
				return
			case d, ok := <-chs.ETHDepth:
				if !ok {
					return
				}
				if !datafeed.ValidateLatency(d.EventTime, d.LocalTime, latencyThresholdMs(sm)) {
					metrics.DataError("eth_depth", "stale")
					continue
				}
				orderBook.Update(d, liveParams.Get().DepthLevels)
				microstructure.UpdateSpreadBaseline(orderBook.SpreadBps())
			}
		}
	})

	start("eth_markprice_handler", func() {
		for {
			select {
			case <-ctx.Done():
				return
			case mp, ok := <-chs.ETHMarkPrice:
				if !ok {
					return
				}
				atomic.AddInt64(&symbolMsgCount, 1)
				if !datafeed.ValidateLatency(mp.EventTime, mp.LocalTime, latencyThresholdMs(sm)) {
					metrics.DataError("eth_markprice", "stale")
					continue
				}
				basisTracker.Update(mp)
				latestETHMark.Store(mp.MarkPrice)
				latestETHIndex.Store(mp.IndexPrice)
				latestFunding.Store(mp.FundingRate)
				latestNextFund.Store(mp.NextFundingTime)

				tradeHandler.CheckAndExit(ctx, mp.MarkPrice)
			}
		}
	})

	start("eth_kline_handler", func() {
		for {
			select {
			case <-ctx.Done():
				return
			case k, ok := <-chs.ETHKline1m:
				if !ok {
					return
				}
				if !k.IsClosed {
					continue
				}
				prev := prevETHClose.Load().(decimal.Decimal)
				if !prev.IsZero() {
					ret := k.Close.Sub(prev).Div(prev)
					volTracker.AddReturn(ret)
					latestPriceMom.Store(ret)
				}
				prevETHClose.Store(k.Close)
			}
		}
	})

	start("engine_evaluator", func() {
		select {
		case <-warmupDone:
		case <-ctx.Done():
			return
		}
		// 预热完成后额外等待 2 分钟，让微结构指标积累足够样本
		const engineStartDelay = 2 * time.Minute
		log.Info().Dur("delay", engineStartDelay).Msg("data stream ready, waiting for microstructure warmup before engine start")
		eventLog.AddSystem("ENGINE_WARMUP", fmt.Sprintf("数据流就绪，引擎将在 %.0f 分钟后启动（等待微结构指标积累）", engineStartDelay.Minutes()), nil)
		select {
		case <-time.After(engineStartDelay):
		case <-ctx.Done():
			return
		}
		log.Info().Msg("engine evaluator started")
		eventLog.AddSystem("ENGINE_START", "引擎评估器已启动，开始监控信号", nil)

		ticker := time.NewTicker(time.Duration(cfg.Sampling.FastIntervalMs) * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				lp := liveParams.Get()

				if !guard.CanTrade() {
					continue
				}
				if !omgr.HasCredentials() {
					continue
				}

				ethMark := latestETHMark.Load().(decimal.Decimal)
				if ethMark.IsZero() {
					continue
				}

				rcvd := tradeFlow.Snapshot()
				oi := oiTracker.Snapshot()
				if oi.ConsecutiveMiss >= 3 {
					eventLog.AddSystem("DATA_ERROR", "OI fetch missed 3 consecutive samples", map[string]interface{}{
						"source": "open_interest",
						"count":  oi.ConsecutiveMiss,
					})
					sm.RequestTransition(statemachine.StateDegradation, "OI fetch missed 3 consecutive samples")
					continue
				}
				basis := basisTracker.Snapshot()
				spreadBaseline := microstructure.GetSpreadBaseline()

				mctx := &datafeed.MarketContext{
					ETHMarkPrice:  ethMark,
					ETHIndexPrice: latestETHIndex.Load().(decimal.Decimal),
					FundingRate:   latestFunding.Load().(decimal.Decimal),
					NextFundingMs: latestNextFund.Load(),

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

					OrderbookSurvivalDecay: orderBook.SurvivalDecayRate(),
					SpreadWideningRate:     orderBook.SpreadWideningRate(spreadBaseline),

					RealizedVol1m: volTracker.StdDev(60_000),
					RealizedVol5m: volTracker.StdDev(300_000),
					VolBaseline1h: volTracker.StdDev(3_600_000),

					PriceMomentum1m: latestPriceMom.Load().(decimal.Decimal),

					SnapshotTime: datafeed.NowMs(),
				}

				basisZf, _ := basis.ZScore.Float64()
				metrics.SetBasisZScore(basisZf)

				trendEng.SetConfig(lp.TrendEnabled, lp.TrendConfidence, lp.OIDeltaThreshold)
				squeezeEng.SetConfig(lp.SqueezeEnabled, lp.SqueezeConfidence, lp.BasisZScoreThreshold)
				transEng.SetConfig(lp.TransitionEnabled, lp.TransitionConfidence, lp.VolCompressionRatio)

				for _, eng := range engines {
					if eng.Name() == engine.EngineTrend && !sm.CanOpenTrend() {
						continue
					}
					if eng.Name() == engine.EngineTransition && !sm.CanOpen() {
						continue
					}
					if eng.Name() == engine.EngineSqueeze && !sm.CanOpen() {
						continue
					}

					sig := eng.Evaluate(mctx)
					if sig == nil || sig.IsExpired(datafeed.NowMs()) {
						continue
					}

					metrics.SignalGenerated(string(eng.Name()), dirLabel(sig.Direction))

					spreadBps, _ := orderBook.SpreadBps().Float64()
					lotDecimal, err := decimal.NewFromString(lp.MarginUSDT)
					if err == nil && lotDecimal.IsPositive() {
						lotDecimal = lotDecimal.Mul(decimal.NewFromInt(int64(lp.Leverage))).Div(ethMark).Truncate(6)
					}
					if err != nil || lotDecimal.IsZero() || lotDecimal.IsNegative() {
						log.Error().Str("margin_usdt", lp.MarginUSDT).Msg("invalid live margin, signal rejected")
						eventLog.AddReject("SIGNAL_REJECTED", "invalid live lot size", map[string]interface{}{
							"engine":      string(sig.Engine),
							"dir":         dirLabel(sig.Direction),
							"margin_usdt": lp.MarginUSDT,
						})
						continue
					}
					lot, _ := lotDecimal.Float64()
					depth, _ := orderBook.TotalDepth(sig.Direction == datafeed.DirectionLong).Float64()
					vol1m, _ := volTracker.StdDev(60_000).Float64()
					estSlip := slippageEst.Estimate(spreadBps, lot, depth, vol1m*10000)
					if estSlip > lp.MaxSlippageBps {
						log.Debug().Float64("est_slip_bps", estSlip).Msg("slippage too high, signal rejected")
						eventLog.AddReject("SIGNAL_REJECTED", "estimated slippage exceeds max", map[string]interface{}{
							"engine":       string(sig.Engine),
							"dir":          dirLabel(sig.Direction),
							"est_slip_bps": estSlip,
							"max_bps":      lp.MaxSlippageBps,
						})
						continue
					}
					if slippageEst.ShouldReject(lp.TakeProfitPct, sig.Confidence, estSlip, 0.04) {
						eventLog.AddReject("SIGNAL_REJECTED", "expected pnl does not cover trading cost", map[string]interface{}{
							"engine":       string(sig.Engine),
							"dir":          dirLabel(sig.Direction),
							"confidence":   sig.Confidence,
							"est_slip_bps": estSlip,
						})
						continue
					}

					tradeHandler.TryReversal(ctx, sig.Direction, mctx)
					tradeHandler.TryOpen(ctx, sig)
					break
				}
			}
		}
	})

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	log.Info().Str("signal", sig.String()).Msg("shutting down gracefully...")
	cancel()
	wg.Wait()
	log.Info().Msg("shutdown complete")
}

func bootCheck(ctx context.Context, rest *datafeed.RESTClient, cfg *config.Config) {
	if !rest.HasCredentials() {
		log.Warn().Msg("boot: missing Binance API credentials, skipping signed account checks")
		return
	}
	risks, err := rest.PositionRisk(ctx, cfg.Trading.Symbol)
	if err != nil {
		log.Warn().Err(err).Msg("boot: cannot fetch position risk, skipping (credentials can be set via webui)")
		return
	}
	for _, r := range risks {
		if r.Symbol == cfg.Trading.Symbol && !r.PositionAmt.IsZero() {
			log.Warn().Str("amt", r.PositionAmt.String()).
				Msg("boot: existing position detected, please close manually before trading")
		}
	}
	if err := rest.CancelAllOrders(ctx, cfg.Trading.Symbol); err != nil {
		log.Warn().Err(err).Msg("boot: cancel all orders (may be empty)")
	}
	if err := rest.SetLeverage(ctx, cfg.Trading.Symbol, cfg.Trading.Leverage); err != nil {
		log.Warn().Err(err).Msg("boot: set leverage failed, please check via webui")
	}
	if err := rest.SetMarginType(ctx, cfg.Trading.Symbol, cfg.Trading.MarginType); err != nil {
		log.Warn().Err(err).Msg("boot: set margin type (may already be set)")
	}
	log.Info().Msg("boot check passed")
}

func dirLabel(d datafeed.Direction) string {
	switch d {
	case datafeed.DirectionLong:
		return "LONG"
	case datafeed.DirectionShort:
		return "SHORT"
	default:
		return "FLAT"
	}
}

func symbolStreams(symbol string) []string {
	base := strings.ToLower(symbol)
	return []string{
		base + "@aggTrade",
		base + "@depth@100ms",
		base + "@kline_1m",
		base + "@markPrice@1s",
	}
}

func symbolHandlers(symbol string, chs *datafeed.Channels) map[string]datafeed.StreamHandler {
	base := strings.ToLower(symbol)
	return map[string]datafeed.StreamHandler{
		base + "@aggTrade":     datafeed.MakeAggTradeHandler(chs.ETHAggTrade),
		base + "@depth@100ms":  datafeed.MakeDepthHandler(chs.ETHDepth),
		base + "@kline_1m":     datafeed.MakeKlineHandler(chs.ETHKline1m),
		base + "@markPrice@1s": datafeed.MakeMarkPriceHandler(chs.ETHMarkPrice),
	}
}

func latencyThresholdMs(sm *statemachine.StateMachine) int64 {
	switch sm.Current() {
	case statemachine.StateStress, statemachine.StateDegradation:
		return 1000
	default:
		return 300
	}
}
