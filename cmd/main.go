package main

import (
	"context"
	"os"
	"os/signal"
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

	// ── 1. 加载配置 ──────────────────────────────────────────────
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
	}
	log.Info().Str("symbol", cfg.Trading.Symbol).Int("leverage", cfg.Trading.Leverage).Msg("config loaded")

	// ── 2. REST 客户端 ────────────────────────────────────────────
	rest := datafeed.NewRESTClient(cfg.Binance.RESTEndpoint, cfg.Binance.APIKey, cfg.Binance.APISecret)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ── 3. Boot checklist ─────────────────────────────────────────
	if rest.HasCredentials() {
		bootCheck(ctx, rest, cfg)
	} else {
		log.Warn().Msg("boot check skipped: missing Binance API credentials; webui and market-data mode only")
	}

	// ── 4. 初始化组件 ────────────────────────────────────────────
	metrics := telemetry.NewMetrics()
	metrics.ServeHTTP(":9090")

	// LiveParams：运行时可热更新的参数
	liveParams := webui.NewLiveParams(cfg)
	eventLog := webui.NewEventLog(300)

	// 管理后台（:8080）
	admin := webui.NewServer(liveParams, eventLog, rest, cfg.WebUI.ServiceName)
	admin.Listen(cfg.WebUI.Addr)

	pm := execution.NewPositionManager(cfg.Trading.Symbol)

	sm := statemachine.New(func(from, to statemachine.State, reason string) {
		metrics.StateTransition(from.String(), to.String())
		metrics.SetState(float64(to))
	})

	guard := risk.NewGuardrails(cfg.Risk.DailyLossLimitPct, cfg.Risk.ConsecutiveLossLimit)
	btcAnchor := risk.NewBTCAnchor(cfg.Risk)
	slippageEst := risk.NewSlippageEstimator()

	omgr := execution.NewOrderManager(rest, cfg, metrics)
	tradeHandler := execution.NewTradeHandler(pm, omgr, sm, guard, cfg, liveParams, eventLog, metrics)

	recon := execution.NewReconciliationLoop(
		rest, pm, omgr, sm, cfg.Trading.Symbol,
		cfg.Execution.ReconciliationIntervalSec,
	)

	// 微观结构
	tradeFlow := microstructure.NewTradeFlowTracker()
	volTracker := microstructure.NewVolatilityTracker()
	orderBook := microstructure.NewOrderBook()
	basisTracker := microstructure.NewBasisTracker()
	oiTracker := microstructure.NewOITracker(rest, cfg.Trading.Symbol, cfg.Sampling.OIPollIntervalMs)

	// 引擎（具体类型持有，支持运行时热更新）
	trendEng := engine.NewTrendEngine(cfg.Engines.Trend)
	squeezeEng := engine.NewSqueezeEngine(cfg.Engines.Squeeze)
	transEng := engine.NewTransitionEngine(cfg.Engines.Transition)
	engines := []engine.Engine{trendEng, squeezeEng, transEng}

	// ── 5. WebSocket 连接 ──────────────────────────────────────────
	chs := datafeed.NewChannels()

	ethHandlers := map[string]datafeed.StreamHandler{
		"ethusdt@aggTrade":     datafeed.MakeAggTradeHandler(chs.ETHAggTrade),
		"ethusdt@depth@100ms":  datafeed.MakeDepthHandler(chs.ETHDepth),
		"ethusdt@kline_1s":     datafeed.MakeKlineHandler(chs.ETHKline1s),
		"ethusdt@kline_1m":     datafeed.MakeKlineHandler(chs.ETHKline1m),
		"ethusdt@markPrice@1s": datafeed.MakeMarkPriceHandler(chs.ETHMarkPrice),
	}
	btcHandlers := map[string]datafeed.StreamHandler{
		"btcusdt@aggTrade":     datafeed.MakeAggTradeHandler(chs.BTCAggTrade),
		"btcusdt@markPrice@1s": datafeed.MakeMarkPriceHandler(chs.BTCMarkPrice),
	}

	ethWS, err := datafeed.NewWSClient(cfg.Binance.WSEndpoint,
		[]string{"ethusdt@aggTrade", "ethusdt@depth@100ms", "ethusdt@kline_1s", "ethusdt@kline_1m", "ethusdt@markPrice@1s"},
		ethHandlers)
	if err != nil {
		log.Fatal().Err(err).Msg("eth ws init failed")
	}
	btcWS, err := datafeed.NewWSClient(cfg.Binance.WSEndpoint,
		[]string{"btcusdt@aggTrade", "btcusdt@markPrice@1s"},
		btcHandlers)
	if err != nil {
		log.Fatal().Err(err).Msg("btc ws init failed")
	}

	// ── 6. 数据流预热计数（atomic 防数据竞争）────────────────────
	var ethMsgCount, btcMsgCount int64
	warmupDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			<-ticker.C
			eth := atomic.LoadInt64(&ethMsgCount)
			btc := atomic.LoadInt64(&btcMsgCount)
			if eth < 100 || btc < 100 {
				log.Error().Int64("eth_msgs", eth).Int64("btc_msgs", btc).
					Msg("data stream warmup waiting: insufficient messages")
				eventLog.AddSystem("DATA_WARMUP_WAIT", "data stream warmup waiting: insufficient messages", map[string]interface{}{
					"eth_msgs": eth,
					"btc_msgs": btc,
				})
				continue
			}
			log.Info().Int64("eth_msgs", eth).Int64("btc_msgs", btc).Msg("data streams stable")
			eventLog.AddSystem("DATA_READY", "data streams stable", map[string]interface{}{
				"eth_msgs": eth,
				"btc_msgs": btc,
			})
			close(warmupDone)
			return
		}
	}()

	// 共享的最新市场状态（原子写入，引擎评估时读取）
	var (
		latestETHMark    atomic.Value // decimal.Decimal
		latestETHIndex   atomic.Value
		latestFunding    atomic.Value
		latestNextFund   atomic.Int64
		latestBTCMark    atomic.Value
		latestBTCTrend1m atomic.Int32 // datafeed.Direction
		latestBTCTrend5m atomic.Int32
		latestPriceMom   atomic.Value // decimal.Decimal

		prevBTCClose1m atomic.Value
		prevBTCClose5m atomic.Value
		prevETHClose   atomic.Value
	)
	latestETHMark.Store(decimal.Zero)
	latestETHIndex.Store(decimal.Zero)
	latestFunding.Store(decimal.Zero)
	latestBTCMark.Store(decimal.Zero)
	latestPriceMom.Store(decimal.Zero)
	prevBTCClose1m.Store(decimal.Zero)
	prevBTCClose5m.Store(decimal.Zero)
	prevETHClose.Store(decimal.Zero)

	// ── 7. 启动 goroutines ────────────────────────────────────────
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
	start("btc_ws", func() { btcWS.Run(ctx) })
	start("state_machine", func() { sm.Run(ctx) })
	start("oi_tracker", func() { oiTracker.Run(ctx) })
	start("reconciliation", func() { recon.Run(ctx) })

	// ETH aggTrade
	start("eth_aggtrade_handler", func() {
		for {
			select {
			case <-ctx.Done():
				return
			case t, ok := <-chs.ETHAggTrade:
				if !ok {
					return
				}
				atomic.AddInt64(&ethMsgCount, 1)
				if !datafeed.ValidateLatency(t.EventTime, t.LocalTime, latencyThresholdMs(sm)) {
					metrics.DataError("eth_aggtrade", "stale")
					continue
				}
				tradeFlow.Add(t)
				price, _ := t.Price.Float64()
				btcAnchor.AddETHPrice(price)
			}
		}
	})

	// ETH depth
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
				orderBook.Update(d)
				microstructure.UpdateSpreadBaseline(orderBook.SpreadBps())
			}
		}
	})

	// ETH markPrice → PnL 本地监控（双保险）
	start("eth_markprice_handler", func() {
		for {
			select {
			case <-ctx.Done():
				return
			case mp, ok := <-chs.ETHMarkPrice:
				if !ok {
					return
				}
				atomic.AddInt64(&ethMsgCount, 1)
				if !datafeed.ValidateLatency(mp.EventTime, mp.LocalTime, latencyThresholdMs(sm)) {
					metrics.DataError("eth_markprice", "stale")
					continue
				}
				basisTracker.Update(mp)
				latestETHMark.Store(mp.MarkPrice)
				latestETHIndex.Store(mp.IndexPrice)
				latestFunding.Store(mp.FundingRate)
				latestNextFund.Store(mp.NextFundingTime)

				// 本地价格监控（止盈止损双保险，最高优先级）
				tradeHandler.CheckAndExit(ctx, mp.MarkPrice)
			}
		}
	})

	// ETH kline 1m → volatility + price momentum
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

	// BTC markPrice → trend direction
	start("btc_markprice_handler", func() {
		var btcKline1mClose, btcKline5mClose [5]decimal.Decimal
		var idx1m, idx5m int
		var count1m, count5m int64
		ticker1m := time.NewTicker(time.Minute)
		ticker5m := time.NewTicker(5 * time.Minute)
		defer ticker1m.Stop()
		defer ticker5m.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case mp, ok := <-chs.BTCMarkPrice:
				if !ok {
					return
				}
				atomic.AddInt64(&btcMsgCount, 1)
				latestBTCMark.Store(mp.MarkPrice)
				price, _ := mp.MarkPrice.Float64()
				btcAnchor.AddBTCPrice(price)
			case <-ticker1m.C:
				cur := latestBTCMark.Load().(decimal.Decimal)
				prev := prevBTCClose1m.Load().(decimal.Decimal)
				if !prev.IsZero() && !cur.IsZero() {
					btcKline1mClose[int(count1m)%5] = cur
					count1m++
					// 趋势：cur vs 5 个样本中最旧的
					oldest := btcKline1mClose[int(count1m)%5]
					if !oldest.IsZero() {
						if cur.GreaterThan(oldest) {
							latestBTCTrend1m.Store(int32(datafeed.DirectionLong))
						} else {
							latestBTCTrend1m.Store(int32(datafeed.DirectionShort))
						}
					}
					_ = idx1m
				}
				prevBTCClose1m.Store(cur)
			case <-ticker5m.C:
				cur := latestBTCMark.Load().(decimal.Decimal)
				prev := prevBTCClose5m.Load().(decimal.Decimal)
				if !prev.IsZero() && !cur.IsZero() {
					btcKline5mClose[int(count5m)%5] = cur
					count5m++
					oldest := btcKline5mClose[int(count5m)%5]
					if !oldest.IsZero() {
						if cur.GreaterThan(oldest) {
							latestBTCTrend5m.Store(int32(datafeed.DirectionLong))
						} else {
							latestBTCTrend5m.Store(int32(datafeed.DirectionShort))
						}
					}
					_ = idx5m
				}
				prevBTCClose5m.Store(cur)
			}
		}
	})

	// BTC aggTrade 计数（不做额外处理）
	start("btc_aggtrade_handler", func() {
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-chs.BTCAggTrade:
				if !ok {
					return
				}
				atomic.AddInt64(&btcMsgCount, 1)
			}
		}
	})

	// 引擎评估（每 FastIntervalMs 一次，预热+观察期后启动）
	start("engine_evaluator", func() {
		select {
		case <-warmupDone:
		case <-ctx.Done():
			return
		}
		// 5 分钟初始观察期
		select {
		case <-time.After(5 * time.Minute):
		case <-ctx.Done():
			return
		}

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
					OIVelocity: oi.Velocity,
					OIAccel:    oi.Accel,
					OIBaseline: oi.Baseline,

					BasisBps:      basis.BasisBps,
					BasisVelocity: basis.Velocity,
					BasisZScore:   basis.ZScore,

					OrderbookSurvivalDecay: orderBook.SurvivalDecayRate(),
					SpreadWideningRate:     orderBook.SpreadWideningRate(spreadBaseline),

					BTCMarkPrice: latestBTCMark.Load().(decimal.Decimal),
					BTCTrend1m:   datafeed.Direction(latestBTCTrend1m.Load()),
					BTCTrend5m:   datafeed.Direction(latestBTCTrend5m.Load()),

					RealizedVol1m: volTracker.StdDev(60_000),
					RealizedVol5m: volTracker.StdDev(300_000),
					VolBaseline1h: volTracker.StdDev(3_600_000),

					PriceMomentum1m: latestPriceMom.Load().(decimal.Decimal),

					SnapshotTime: datafeed.NowMs(),
				}

				basisZf, _ := basis.ZScore.Float64()
				metrics.SetBasisZScore(basisZf)

				// 热更新引擎开关和阈值（从 LiveParams 读取）
				trendEng.SetConfig(lp.TrendEnabled, lp.TrendConfidence, lp.OIDeltaThreshold)
				squeezeEng.SetConfig(lp.SqueezeEnabled, lp.SqueezeConfidence, lp.BasisZScoreThreshold)
				transEng.SetConfig(lp.TransitionEnabled, lp.TransitionConfidence, lp.VolCompressionRatio)

				for _, eng := range engines {
					if (eng.Name() == engine.EngineTrend || eng.Name() == engine.EngineTransition) &&
						!sm.CanOpenTrend() {
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

					rawEngine := sig.Engine
					rawDir := sig.Direction
					sig = btcAnchor.Adjust(sig, mctx.BTCTrend1m, mctx.BTCTrend5m)
					if sig == nil {
						eventLog.AddReject("SIGNAL_REJECTED", "BTC anchor rejected signal", map[string]interface{}{
							"engine":     string(rawEngine),
							"dir":        dirLabel(rawDir),
							"btc_trend1": dirLabel(mctx.BTCTrend1m),
							"btc_trend5": dirLabel(mctx.BTCTrend5m),
						})
						continue
					}

					// 滑点检查（用 liveParams 的 max_slippage_bps）
					spreadBps, _ := orderBook.SpreadBps().Float64()
					lotDecimal, err := decimal.NewFromString(lp.LotSize)
					if err != nil || lotDecimal.IsZero() || lotDecimal.IsNegative() {
						log.Error().Str("lot_size", lp.LotSize).Msg("invalid live lot size, signal rejected")
						eventLog.AddReject("SIGNAL_REJECTED", "invalid live lot size", map[string]interface{}{
							"engine":   string(sig.Engine),
							"dir":      dirLabel(sig.Direction),
							"lot_size": lp.LotSize,
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

					tradeHandler.TryReversal(ctx, sig.Direction)
					tradeHandler.TryOpen(ctx, sig)
					break
				}
			}
		}
	})

	// ── 8. OS 信号退出 ────────────────────────────────────────────
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
		log.Fatal().Err(err).Msg("boot: cannot fetch position risk")
	}
	for _, r := range risks {
		if r.Symbol == cfg.Trading.Symbol && !r.PositionAmt.IsZero() {
			log.Fatal().Str("amt", r.PositionAmt.String()).
				Msg("boot: existing position detected, please close manually before starting")
		}
	}
	if err := rest.CancelAllOrders(ctx, cfg.Trading.Symbol); err != nil {
		log.Warn().Err(err).Msg("boot: cancel all orders (may be empty)")
	}
	if err := rest.SetLeverage(ctx, cfg.Trading.Symbol, cfg.Trading.Leverage); err != nil {
		log.Fatal().Err(err).Msg("boot: set leverage failed")
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

func latencyThresholdMs(sm *statemachine.StateMachine) int64 {
	switch sm.Current() {
	case statemachine.StateStress, statemachine.StateDegradation:
		return 1000
	default:
		return 300
	}
}
