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

	// activeSymbol 启动时为空，等前端验证 API Key 和币种后再设置
	var activeSymbol atomic.Value
	activeSymbol.Store("")
	currentSymbol := func() string { return activeSymbol.Load().(string) }

	rest := datafeed.NewRESTClient(cfg.Binance.RESTEndpoint, cfg.Binance.APIKey, cfg.Binance.APISecret)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if rest.HasCredentials() {
		if err := rest.SyncTime(ctx); err != nil {
			log.Warn().Err(err).Msg("boot: clock sync failed, using local time")
		}
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
		rest, pm, omgr, sm, tradeHandler, cfg.Trading.Symbol,
		cfg.Execution.ReconciliationIntervalSec,
	)
	recon.SetSymbolProvider(currentSymbol)

	// WS クライアント: 起動時は空ストリーム、watcher がシンボル追加後にストリームを確立
	ethWS, err := datafeed.NewWSClient(cfg.Binance.WSEndpoint, nil, nil)
	if err != nil {
		log.Fatal().Err(err).Msg("eth ws init failed")
	}
	log.Info().Msg("ws client created; waiting for symbol to be set via webui before connecting")

	// watcher = 全シンボルのデータ収集 + エンジン評価の単一ソース
	watcher := NewSymbolWatcher(ethWS, rest, liveParams, cfg)
	watcher.SetParentContext(ctx)

	// latestETHMark: トレードハンドラ用のマーク価格（watcher の mark price hook で更新）
	var latestETHMark atomic.Value
	latestETHMark.Store(decimal.Zero)

	tradeHandler.SetMarkPriceProvider(func() decimal.Decimal {
		return latestETHMark.Load().(decimal.Decimal)
	})

	// Mark price hook: 只做原子存储 + 预热日志，绝不阻塞 WS goroutine
	var wsFirstMsg atomic.Bool
	var wsWarmupDone atomic.Bool
	watcher.SetMarkPriceHook(func(sym string, price decimal.Decimal) {
		if sym != currentSymbol() {
			return
		}
		latestETHMark.Store(price) // 原子写，纳秒级，不会阻塞

		cnt := watcher.GetMsgCount(sym)
		if !wsFirstMsg.Load() {
			wsFirstMsg.Store(true)
			eventLog.AddSystem("WS_CONNECTED",
				fmt.Sprintf("数据流首条消息已收到（交易对 %s），WS 连接正常", sym), nil)
		}
		if !wsWarmupDone.Load() && cnt >= 100 {
			wsWarmupDone.Store(true)
			eventLog.AddSystem("WS_WARMUP_OK",
				fmt.Sprintf("已收到 100 条消息，数据流预热完成（交易对 %s）", sym), nil)
		}
	})

	// signalQueue: 解耦 evaluator goroutine 与下单 REST 调用，防止 WS goroutine 被阻塞
	type signalQueueEntry struct {
		sym  string
		sig  *engine.Signal
		mctx *datafeed.MarketContext
	}
	signalQueue := make(chan signalQueueEntry, 1)

	// Signal hook: 仅做轻量过滤 + 非阻塞投递，不做任何 REST 调用
	watcher.SetSignalHook(func(sym string, sig *engine.Signal, mctx *datafeed.MarketContext) {
		if sym != currentSymbol() {
			return
		}
		// 状态机过滤（无锁读，快速路径）
		switch sig.Engine {
		case engine.EngineTrend:
			if !sm.CanOpenTrend() {
				return
			}
		default:
			if !sm.CanOpen() {
				return
			}
		}
		if !guard.CanTrade() || !omgr.HasCredentials() {
			return
		}
		// 非阻塞投递；队列满时丢弃（上一个信号仍在处理中）
		select {
		case signalQueue <- signalQueueEntry{sym: sym, sig: sig, mctx: mctx}:
		default:
			log.Debug().Str("engine", string(sig.Engine)).Msg("signal queue full, dropping signal")
		}
	})

	// trade_executor goroutine 在 start 辅助函数定义后启动（见下方）

	// 状态提供者：WS 预热状态（使用 watcher 的消息计数）
	admin.SetStatusProvider(func() map[string]interface{} {
		sym := currentSymbol()
		msgs := watcher.GetMsgCount(sym)
		return map[string]interface{}{
			"symbol":          sym,
			"ws_msg_count":    msgs,
			"warmup_done":     msgs >= 100,
			"has_credentials": rest.HasCredentials(),
		}
	})

	// 市场快照提供者：直接从 watcher 读取所有监控币种的数据
	admin.SetMarketProvider(func() map[string]interface{} {
		return map[string]interface{}{
			"symbols": watcher.AllSnapshots(),
			"active":  currentSymbol(),
		}
	})

	// Watchlist 处理器
	admin.SetWatchlistHandlers(
		func(addCtx context.Context, sym string) error {
			return watcher.Add(addCtx, sym)
		},
		func(sym string) {
			watcher.Remove(sym)
		},
		func() []string {
			return watcher.Symbols()
		},
	)

	// Symbol switcher: API Key 验证 → 选币 → 建立 WS 连接 → 进入引擎
	admin.SetSymbolSwitcher(func(switchCtx context.Context, sym string) error {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" {
			return fmt.Errorf("symbol is required")
		}
		if !rest.HasCredentials() {
			return fmt.Errorf("请先在 WebUI 验证并应用 API Key，再设置交易对")
		}
		if !pm.SetSymbol(sym) {
			return fmt.Errorf("cannot switch symbol while local position is open")
		}
		oldSymbol := currentSymbol()
		if oldSymbol != "" && oldSymbol != sym {
			if err := rest.CancelAllOrders(switchCtx, oldSymbol); err != nil {
				log.Warn().Err(err).Str("symbol", oldSymbol).Msg("cancel orders on symbol switch failed")
			}
		}
		if err := rest.SetLeverage(switchCtx, sym, liveParams.Get().Leverage); err != nil {
			log.Warn().Err(err).Str("symbol", sym).Msg("set leverage on symbol switch failed")
		}
		if err := rest.SetMarginType(switchCtx, sym, cfg.Trading.MarginType); err != nil {
			log.Warn().Err(err).Str("symbol", sym).Msg("set margin type on symbol switch failed")
		}

		activeSymbol.Store(sym)
		admin.SetSymbol(sym)

		// 重置活跃品种的本地状态
		latestETHMark.Store(decimal.Zero)
		wsFirstMsg.Store(false)
		wsWarmupDone.Store(false)

		// watcher 负责建立 WS 连接并启动引擎 goroutine
		return watcher.Add(switchCtx, sym)
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
	start("reconciliation", func() { recon.Run(ctx) })

	// trade_executor: 串行消费信号队列，执行滑点检查 + TryReversal/TryOpen（允许阻塞 REST）
	start("trade_executor", func() {
		for {
			select {
			case <-ctx.Done():
				return
			case entry := <-signalQueue:
				sym, sig, mctx := entry.sym, entry.sig, entry.mctx
				if sym != currentSymbol() {
					continue
				}
				lp := liveParams.Get()
				if sig.Direction == datafeed.DirectionLong && (lp.LongEnabled == nil || !*lp.LongEnabled) {
					continue
				}
				if sig.Direction == datafeed.DirectionShort && (lp.ShortEnabled == nil || !*lp.ShortEnabled) {
					continue
				}

				metrics.SignalGenerated(string(sig.Engine), dirLabel(sig.Direction))

				ethMark := latestETHMark.Load().(decimal.Decimal)
				lotDecimal, lotErr := decimal.NewFromString(lp.MarginUSDT)
				if lotErr == nil && lotDecimal.IsPositive() {
					lotDecimal = lotDecimal.Mul(decimal.NewFromInt(int64(lp.Leverage))).Div(ethMark).Truncate(6)
				}
				if lotErr != nil || lotDecimal.IsZero() || lotDecimal.IsNegative() {
					log.Error().Str("margin_usdt", lp.MarginUSDT).Msg("invalid live margin, signal rejected")
					eventLog.AddReject("SIGNAL_REJECTED", "invalid live lot size", map[string]interface{}{
						"engine": string(sig.Engine), "dir": dirLabel(sig.Direction),
						"margin_usdt": lp.MarginUSDT,
					})
					continue
				}

				spreadBps, _ := mctx.SpreadBps.Float64()
				lot, _ := lotDecimal.Float64()
				var depth decimal.Decimal
				if sig.Direction == datafeed.DirectionLong {
					depth = mctx.TotalBidDepth
				} else {
					depth = mctx.TotalAskDepth
				}
				depthF, _ := depth.Float64()
				vol1m, _ := mctx.RealizedVol1m.Float64()
				estSlip := slippageEst.Estimate(spreadBps, lot, depthF, vol1m*10000)
				if estSlip > lp.MaxSlippageBps {
					log.Debug().Float64("est_slip_bps", estSlip).Msg("slippage too high, signal rejected")
					eventLog.AddReject("SIGNAL_REJECTED", "estimated slippage exceeds max", map[string]interface{}{
						"engine": string(sig.Engine), "dir": dirLabel(sig.Direction),
						"est_slip_bps": estSlip, "max_bps": lp.MaxSlippageBps,
					})
					continue
				}
				tpPct := lp.LongTPPct
				if sig.Direction == datafeed.DirectionShort {
					tpPct = lp.ShortTPPct
				}
				if slippageEst.ShouldReject(tpPct, sig.Confidence, estSlip, 0.04) {
					eventLog.AddReject("SIGNAL_REJECTED", "expected pnl does not cover trading cost", map[string]interface{}{
						"engine": string(sig.Engine), "dir": dirLabel(sig.Direction),
						"confidence": sig.Confidence, "est_slip_bps": estSlip,
					})
					continue
				}

				tradeHandler.TryReversal(ctx, sig.Direction, mctx)
				tradeHandler.TryOpen(ctx, sig)
			}
		}
	})

	// exit_monitor: 独立 goroutine 轮询最新 mark price 检查平仓条件
	// 与 WS goroutine 完全解耦，避免 REST 调用阻塞 markPrice channel
	start("exit_monitor", func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				price := latestETHMark.Load().(decimal.Decimal)
				if !price.IsZero() {
					tradeHandler.CheckAndExit(ctx, price)
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

