package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/config"
	"github.com/yourorg/eth-perp-system/internal/datafeed"
	"github.com/yourorg/eth-perp-system/internal/engine"
	"github.com/yourorg/eth-perp-system/internal/risk"
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

	lp := webui.NewLiveParams(cfg)
	eventLog := webui.NewEventLog(300)

	admin := webui.NewServer(lp, eventLog, rest, cfg.WebUI.ServiceName, cfg.Trading.Symbol)
	admin.Listen(cfg.WebUI.Addr)

	// 共享风控：全局日损/连亏保护，跨所有交易对
	guard := risk.NewGuardrails(cfg.Risk.DailyLossLimitPct, cfg.Risk.ConsecutiveLossLimit)
	slipEst := risk.NewSlippageEstimator()

	// workers: sym → symbolWorker，最多 maxSymbols 个
	var workersMu sync.RWMutex
	// addMu 串行化 watchlist add 操作（防止并发越过 maxSymbols 上限的 TOCTOU）
	var addMu sync.Mutex
	workers := make(map[string]*symbolWorker)

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

	ethWS, err := datafeed.NewWSClient(cfg.Binance.WSEndpoint, nil, nil)
	if err != nil {
		log.Fatal().Err(err).Msg("eth ws init failed")
	}
	log.Info().Msg("ws client created; waiting for symbols to be added via webui")

	watcher := NewSymbolWatcher(ethWS, rest, lp, cfg)
	watcher.SetParentContext(ctx)

	// Mark price hook: 按 sym 路由到对应 worker
	watcher.SetMarkPriceHook(func(sym string, price decimal.Decimal, msgCount int64) {
		workersMu.RLock()
		w, ok := workers[sym]
		workersMu.RUnlock()
		if !ok {
			return
		}
		w.markPrice.Store(price)
		if !w.wsFirstMsg.Load() {
			w.wsFirstMsg.Store(true)
			eventLog.AddSystem("WS_CONNECTED",
				fmt.Sprintf("数据流首条消息已收到（%s），WS 连接正常", sym), nil)
		}
		if !w.wsWarmupDone.Load() && msgCount >= 100 {
			w.wsWarmupDone.Store(true)
			eventLog.AddSystem("WS_WARMUP_OK",
				fmt.Sprintf("已收到 100 条消息，数据流预热完成（%s）", sym), nil)
		}
	})

	// Signal hook: 按 sym 路由到对应 worker 的信号队列
	watcher.SetSignalHook(func(sym string, sig *engine.Signal, mctx *datafeed.MarketContext) {
		workersMu.RLock()
		w, ok := workers[sym]
		workersMu.RUnlock()
		if !ok {
			return
		}
		switch sig.Engine {
		case engine.EngineTrend:
			if !w.sm.CanOpenTrend() {
				return
			}
		default:
			if !w.sm.CanOpen() {
				return
			}
		}
		if !guard.CanTrade() || !rest.HasCredentials() {
			return
		}
		select {
		case w.signalQueue <- signalQueueEntry{sym: sym, sig: sig, mctx: mctx}:
		default:
			log.Debug().Str("sym", sym).Str("engine", string(sig.Engine)).Msg("signal queue full, dropping")
		}
	})

	// addWorker: 创建新的 symbolWorker（调用前须持写锁）
	addWorker := func(sym string) {
		if _, exists := workers[sym]; exists {
			return
		}
		w := newSymbolWorker(ctx, sym, rest, guard, slipEst, cfg, lp, eventLog, metrics, start)
		workers[sym] = w
		log.Info().Str("sym", sym).Msg("symbol worker started")
	}

	removeWorker := func(sym string) {
		workersMu.Lock()
		w, ok := workers[sym]
		if ok {
			delete(workers, sym)
		}
		workersMu.Unlock()
		if ok {
			w.cancel()
			log.Info().Str("sym", sym).Msg("symbol worker stopped")
		}
	}

	// Watchlist handlers: 加入 = 验证(server侧已做) + 杠杆设置 + 建立 WS + 创建 worker
	admin.SetWatchlistHandlers(
		func(addCtx context.Context, sym string) error {
			// 串行化所有 add 操作，避免并发越过 maxSymbols 限制的 TOCTOU
			addMu.Lock()
			defer addMu.Unlock()

			workersMu.RLock()
			count := len(workers)
			_, exists := workers[sym]
			workersMu.RUnlock()
			if exists {
				return nil // 幂等
			}
			if count >= maxSymbols {
				return fmt.Errorf("最多同时交易 %d 个交易对，请先移除一个", maxSymbols)
			}
			curLP := lp.Get()
			// 持仓模式（全局，不区分币种）
			dualSide := curLP.PositionMode == "HEDGE"
			if err := rest.SetPositionMode(addCtx, dualSide); err != nil {
				log.Warn().Err(err).Bool("dual", dualSide).Msg("set position mode failed (non-fatal, may already be set)")
			}
			if err := rest.SetLeverage(addCtx, sym, curLP.Leverage); err != nil {
				log.Warn().Err(err).Str("sym", sym).Msg("set leverage failed (non-fatal)")
			}
			marginMode := curLP.MarginMode
			if marginMode == "" {
				marginMode = cfg.Trading.MarginType
			}

			if err := rest.SetMarginType(addCtx, sym, marginMode); err != nil {
				log.Warn().Err(err).Str("sym", sym).Msg("set margin type failed (non-fatal)")
			}
			if err := watcher.Add(addCtx, sym); err != nil {
				return err
			}
			workersMu.Lock()
			addWorker(sym)
			workersMu.Unlock()
			return nil
		},
		func(sym string) {
			removeWorker(sym)
			watcher.Remove(sym)
		},
		func() []string {
			return watcher.Symbols()
		},
	)

	// 当 PositionMode / MarginMode 在 UI 改变时，把新模式推送到 Binance（覆盖所有 watchlist 中的 symbol）
	admin.SetModeChangeHandler(func(modeCtx context.Context, newPositionMode, newMarginMode string) {
		if newPositionMode != "" {
			dualSide := newPositionMode == "HEDGE"
			if err := rest.SetPositionMode(modeCtx, dualSide); err != nil {
				log.Warn().Err(err).Bool("dual", dualSide).Msg("mode change: SetPositionMode failed (may be already set, or has open positions)")
			} else {
				log.Info().Bool("dual", dualSide).Msg("mode change: position mode synced to Binance")
			}
		}
		if newMarginMode != "" {
			workersMu.RLock()
			syms := make([]string, 0, len(workers))
			for sym := range workers {
				syms = append(syms, sym)
			}
			workersMu.RUnlock()
			for _, sym := range syms {
				if err := rest.SetMarginType(modeCtx, sym, newMarginMode); err != nil {
					log.Warn().Err(err).Str("sym", sym).Str("margin", newMarginMode).Msg("mode change: SetMarginType failed (may be already set, or has open position)")
				} else {
					log.Info().Str("sym", sym).Str("margin", newMarginMode).Msg("mode change: margin mode synced to Binance")
				}
			}
		}
	})

	// Status provider
	admin.SetStatusProvider(func() map[string]interface{} {
		workersMu.RLock()
		syms := make([]string, 0, len(workers))
		warmupMap := make(map[string]bool, len(workers))
		msgMap := make(map[string]int64, len(workers))
		for sym, w := range workers {
			syms = append(syms, sym)
			warmupMap[sym] = w.wsWarmupDone.Load()
			msgMap[sym] = watcher.GetMsgCount(sym)
		}
		workersMu.RUnlock()
		return map[string]interface{}{
			"symbols":         syms,
			"warmup":          warmupMap,
			"ws_msg_count":    msgMap,
			"has_credentials": rest.HasCredentials(),
		}
	})

	// Market provider
	admin.SetMarketProvider(func() map[string]interface{} {
		return map[string]interface{}{
			"symbols": watcher.AllSnapshots(),
			"active":  "", // 多币模式无单一 active，由前端自主选择
		}
	})

	start("eth_ws", func() { ethWS.Run(ctx) })

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
		log.Warn().Err(err).Msg("boot: cannot fetch position risk, skipping")
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
