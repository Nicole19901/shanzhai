package main

import (
	"context"
	"sync/atomic"
	"time"

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

const maxSymbols = 5

type signalQueueEntry struct {
	sym  string
	sig  *engine.Signal
	mctx *datafeed.MarketContext
}

// symbolWorker 每个交易对独立的完整执行层
type symbolWorker struct {
	sym          string
	pm           *execution.PositionManager
	sm           *statemachine.StateMachine
	th           *execution.TradeHandler
	recon        *execution.ReconciliationLoop
	markPrice    atomic.Value // decimal.Decimal
	wsFirstMsg   atomic.Bool
	wsWarmupDone atomic.Bool
	signalQueue  chan signalQueueEntry
	cancel       context.CancelFunc
}

func newSymbolWorker(
	parentCtx context.Context,
	sym string,
	rest *datafeed.RESTClient,
	guard *risk.Guardrails,
	slipEst *risk.SlippageEstimator,
	cfg *config.Config,
	lp *webui.LiveParams,
	eventLog *webui.EventLog,
	metrics *telemetry.Metrics,
	start func(name string, fn func()),
) *symbolWorker {
	ctx, cancel := context.WithCancel(parentCtx)

	w := &symbolWorker{
		sym:         sym,
		pm:          execution.NewPositionManager(sym),
		signalQueue: make(chan signalQueueEntry, 1),
		cancel:      cancel,
	}
	w.markPrice.Store(decimal.Zero)

	w.sm = statemachine.New(func(from, to statemachine.State, reason string) {
		metrics.StateTransition(from.String(), to.String())
		metrics.SetState(float64(to))
	})

	omgr := execution.NewOrderManager(rest, cfg, metrics)
	omgr.SetSymbolProvider(func() string { return sym })
	omgr.SetPositionModeProvider(func() string { return lp.Get().PositionMode })

	w.th = execution.NewTradeHandler(w.pm, omgr, w.sm, guard, cfg, lp, eventLog, metrics)
	w.th.SetMarkPriceProvider(func() decimal.Decimal {
		return w.markPrice.Load().(decimal.Decimal)
	})

	w.recon = execution.NewReconciliationLoop(
		rest, w.pm, omgr, w.sm, w.th, sym,
		cfg.Execution.ReconciliationIntervalSec,
	)

	start("sm:"+sym, func() { w.sm.Run(ctx) })
	start("recon:"+sym, func() { w.recon.Run(ctx) })
	start("executor:"+sym, func() { w.runExecutor(ctx, lp, guard, slipEst, eventLog, metrics) })
	start("exit_mon:"+sym, func() { w.runExitMonitor(ctx) })

	return w
}

func (w *symbolWorker) runExecutor(
	ctx context.Context,
	lp *webui.LiveParams,
	guard *risk.Guardrails,
	slipEst *risk.SlippageEstimator,
	eventLog *webui.EventLog,
	metrics *telemetry.Metrics,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case entry := <-w.signalQueue:
			sig, mctx := entry.sig, entry.mctx
			lp_ := lp.Get()

			if sig.Direction == datafeed.DirectionLong && (lp_.LongEnabled == nil || !*lp_.LongEnabled) {
				continue
			}
			if sig.Direction == datafeed.DirectionShort && (lp_.ShortEnabled == nil || !*lp_.ShortEnabled) {
				continue
			}

			metrics.SignalGenerated(string(sig.Engine), dirLabel(sig.Direction))

			ethMark := w.markPrice.Load().(decimal.Decimal)
			if ethMark.IsZero() {
				log.Warn().Str("sym", w.sym).Msg("mark price not ready, signal dropped")
				continue
			}
			prec := int32(lp_.QuantityPrecision)
			if prec < 0 {
				prec = 0
			}
			lotDecimal, lotErr := decimal.NewFromString(lp_.MarginUSDT)
			if lotErr == nil && lotDecimal.IsPositive() {
				lotDecimal = lotDecimal.Mul(decimal.NewFromInt(int64(lp_.Leverage))).Div(ethMark).Truncate(prec)
			}
			if lotErr != nil || lotDecimal.IsZero() || lotDecimal.IsNegative() {
				log.Error().Str("sym", w.sym).Str("margin_usdt", lp_.MarginUSDT).Msg("invalid live margin, signal rejected")
				eventLog.AddReject("SIGNAL_REJECTED", "invalid live lot size", map[string]interface{}{
					"sym": w.sym, "engine": string(sig.Engine), "dir": dirLabel(sig.Direction),
					"margin_usdt": lp_.MarginUSDT,
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
			estSlip := slipEst.Estimate(spreadBps, lot, depthF, vol1m*10000)
			if estSlip > lp_.MaxSlippageBps {
				log.Debug().Str("sym", w.sym).Float64("est_slip_bps", estSlip).Msg("slippage too high, signal rejected")
				eventLog.AddReject("SIGNAL_REJECTED", "estimated slippage exceeds max", map[string]interface{}{
					"sym": w.sym, "engine": string(sig.Engine), "dir": dirLabel(sig.Direction),
					"est_slip_bps": estSlip, "max_bps": lp_.MaxSlippageBps,
				})
				continue
			}
			tpPct := lp_.LongTPPct
			if sig.Direction == datafeed.DirectionShort {
				tpPct = lp_.ShortTPPct
			}
			if slipEst.ShouldReject(tpPct, sig.Confidence, estSlip, 0.04) {
				eventLog.AddReject("SIGNAL_REJECTED", "expected pnl does not cover trading cost", map[string]interface{}{
					"sym": w.sym, "engine": string(sig.Engine), "dir": dirLabel(sig.Direction),
					"confidence": sig.Confidence, "est_slip_bps": estSlip,
				})
				continue
			}

			w.th.TryReversal(ctx, sig.Direction, mctx)
			w.th.TryOpen(ctx, sig)
		}
	}
}

func (w *symbolWorker) runExitMonitor(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			price := w.markPrice.Load().(decimal.Decimal)
			if !price.IsZero() {
				w.th.CheckAndExit(ctx, price)
			}
		}
	}
}
