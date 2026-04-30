package execution

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/config"
	"github.com/yourorg/eth-perp-system/internal/datafeed"
	"github.com/yourorg/eth-perp-system/internal/engine"
	"github.com/yourorg/eth-perp-system/internal/risk"
	"github.com/yourorg/eth-perp-system/internal/statemachine"
	"github.com/yourorg/eth-perp-system/internal/telemetry"
	"github.com/yourorg/eth-perp-system/internal/webui"
)

type ExitReason string

const (
	ExitReasonStopLoss       ExitReason = "STOP_LOSS"
	ExitReasonEmergency      ExitReason = "EMERGENCY_EXIT"
	ExitReasonTakeProfit     ExitReason = "TAKE_PROFIT"
	ExitReasonSignalReversal ExitReason = "SIGNAL_REVERSAL"
	ExitReasonTimeout        ExitReason = "TIMEOUT_EXIT"
	ExitReasonManual         ExitReason = "MANUAL_CLOSE"
)

// TradeHandler 开仓/平仓全流程管理（串行，单 goroutine）
type TradeHandler struct {
	mu sync.Mutex // 防止并发平仓

	pm        *PositionManager
	om        *OrderManager
	sm        *statemachine.StateMachine
	guard     *risk.Guardrails
	cfg       *config.Config
	params    *webui.LiveParams
	events    *webui.EventLog
	metrics   *telemetry.Metrics
	markPrice func() decimal.Decimal

	cooldownUntil int64 // ms，平仓后冷却
}

func NewTradeHandler(
	pm *PositionManager,
	om *OrderManager,
	sm *statemachine.StateMachine,
	guard *risk.Guardrails,
	cfg *config.Config,
	params *webui.LiveParams,
	events *webui.EventLog,
	m *telemetry.Metrics,
) *TradeHandler {
	return &TradeHandler{pm: pm, om: om, sm: sm, guard: guard, cfg: cfg, params: params, events: events, metrics: m}
}

func (h *TradeHandler) SetMarkPriceProvider(fn func() decimal.Decimal) {
	if fn != nil {
		h.markPrice = fn
	}
}

func (h *TradeHandler) orderQty(lp webui.LiveParamsSnapshot, fallbackPrice decimal.Decimal) (decimal.Decimal, error) {
	margin, err := decimal.NewFromString(lp.MarginUSDT)
	if err == nil && margin.IsPositive() {
		price := fallbackPrice
		if h.markPrice != nil {
			price = h.markPrice()
		}
		if price.IsZero() || price.IsNegative() {
			return decimal.Zero, fmt.Errorf("mark price unavailable for margin sizing")
		}
		return margin.Mul(decimal.NewFromInt(int64(lp.Leverage))).Div(price).Truncate(6), nil
	}
	qty, err := decimal.NewFromString(lp.LotSize)
	if err != nil || qty.IsZero() || qty.IsNegative() {
		return decimal.Zero, fmt.Errorf("invalid order size")
	}
	return qty, nil
}

// TryOpen 尝试开仓（严格按照 spec 顺序检查）
func (h *TradeHandler) TryOpen(ctx context.Context, sig *engine.Signal) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Step 2: 必须无持仓
	if !h.pm.IsFlat() {
		log.Debug().Msg("open rejected: already in position")
		h.addReject("OPEN_REJECTED", "already in position", nil)
		return
	}
	// Step 3: 状态检查
	if !h.sm.CanOpen() {
		log.Debug().Str("state", h.sm.Current().String()).Msg("open rejected: state not allow")
		h.addReject("OPEN_REJECTED", "state does not allow opening", map[string]interface{}{"state": h.sm.Current().String()})
		return
	}
	if !h.om.HasCredentials() {
		log.Warn().Msg("open rejected: missing Binance API credentials")
		h.addReject("OPEN_REJECTED", "missing Binance API credentials", nil)
		return
	}
	// Step 4: BTC anchor 已在上游 Adjust 过，signal 到这里已通过
	// Step 5: 滑点检查在上游完成
	// Step 6: 冷却期检查
	now := time.Now().UnixMilli()
	if now < h.cooldownUntil {
		log.Debug().Int64("cooldown_ms", h.cooldownUntil-now).Msg("open rejected: cooldown")
		h.addReject("OPEN_REJECTED", "cooldown active", map[string]interface{}{"cooldown_ms": h.cooldownUntil - now})
		return
	}
	// Step 7: 计算入场参数
	lp := h.params.Get()
	qty, err := h.orderQty(lp, sig.EntryPrice)
	if err != nil || qty.IsZero() {
		log.Error().Err(err).Str("margin_usdt", lp.MarginUSDT).Msg("invalid live order size, open rejected")
		h.addReject("OPEN_REJECTED", "invalid live order size", map[string]interface{}{"margin_usdt": lp.MarginUSDT})
		return
	}
	dir := sig.Direction

	// Step 8: 提交开仓订单
	fillPrice, _, err := h.om.OpenMarket(ctx, dir, qty)
	if err != nil {
		log.Error().Err(err).Msg("open market order failed")
		h.addSystem("ORDER_ERROR", "open market order failed", map[string]interface{}{"error": err.Error()})
		return
	}
	if fillPrice.IsZero() {
		log.Error().Msg("fill price is zero, aborting open")
		h.addSystem("ORDER_ERROR", "fill price is zero after open order", nil)
		return
	}

	// Step 9a: 更新持仓状态
	h.pm.Open(dir, qty, fillPrice)
	h.metrics.PositionChanged("open")

	// Step 9b/c: hang guard orders within the configured risk deadline.
	guardDeadline := lp.GuardDeadlineMs
	if guardDeadline <= 0 {
		guardDeadline = 150
	}
	deadline := time.Now().Add(time.Duration(guardDeadline) * time.Millisecond)
	guardCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	slPrice, tpPrice := h.computeGuardPrices(dir, fillPrice)

	slID, slErr := h.om.PlaceStopLoss(guardCtx, dir, qty, slPrice)
	tpID, tpErr := h.om.PlaceTakeProfit(guardCtx, dir, qty, tpPrice)

	if slErr != nil || tpErr != nil {
		// Step 10: 兜底订单失败 → 立即市价平仓
		log.Error().
			AnErr("sl_err", slErr).AnErr("tp_err", tpErr).
			Msg("guard order failed, emergency close")
		h.addSystem("ORDER_ERROR", "guard order failed, emergency close", map[string]interface{}{
			"sl_error": errorString(slErr),
			"tp_error": errorString(tpErr),
		})
		h.doClose(ctx, ExitReasonEmergency)
		return
	}

	h.pm.SetGuardOrders(slID, tpID)

	// Deadline check.
	if time.Now().After(deadline) {
		log.Error().Int64("deadline_ms", guardDeadline).Msg("guard orders exceeded deadline, emergency close")
		h.addSystem("ORDER_ERROR", "guard orders exceeded deadline", map[string]interface{}{"deadline_ms": guardDeadline})
		h.doClose(ctx, ExitReasonEmergency)
		return
	}

	log.Info().
		Str("dir", dirStr(dir)).
		Str("qty", qty.String()).
		Str("entry", fillPrice.String()).
		Str("sl", slPrice.String()).
		Str("tp", tpPrice.String()).
		Msg("position opened with guard orders")
	if h.events != nil {
		h.events.Add("OPEN", "position opened", map[string]interface{}{
			"dir":         dirStr(dir),
			"qty":         qty.String(),
			"entry":       fillPrice.String(),
			"stop_loss":   slPrice.String(),
			"take_profit": tpPrice.String(),
		})
	}
}

func (h *TradeHandler) computeGuardPrices(dir datafeed.Direction, entryPrice decimal.Decimal) (sl, tp decimal.Decimal) {
	lp := h.params.Get()
	slPct := decimal.NewFromFloat(lp.StopLossPct)
	tpPct := decimal.NewFromFloat(lp.TakeProfitPct)

	if dir == datafeed.DirectionLong {
		sl = entryPrice.Mul(decimal.NewFromInt(1).Sub(slPct))
		tp = entryPrice.Mul(decimal.NewFromInt(1).Add(tpPct))
	} else {
		sl = entryPrice.Mul(decimal.NewFromInt(1).Add(slPct))
		tp = entryPrice.Mul(decimal.NewFromInt(1).Sub(tpPct))
	}
	return
}

// CheckAndExit 本地监控：按优先级检查退出条件，在独立 goroutine 定时调用
func (h *TradeHandler) CheckAndExit(ctx context.Context, markPrice decimal.Decimal) {
	if h.pm.IsFlat() {
		return
	}

	h.pm.UpdatePnL(markPrice)
	pos := h.pm.Snapshot()

	lp := h.params.Get()
	slPct := decimal.NewFromFloat(lp.StopLossPct).Neg()
	tpPct := decimal.NewFromFloat(lp.TakeProfitPct)

	// Priority 1: 止损（最高优先级）
	if pos.UnrealizedPnLPct.LessThanOrEqual(slPct) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.pm.IsFlat() { // double-check under lock
			h.doClose(ctx, ExitReasonStopLoss)
		}
		return
	}

	// Priority 2: 状态机紧急平仓
	if h.sm.MustClose() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.pm.IsFlat() {
			h.doClose(ctx, ExitReasonEmergency)
		}
		return
	}

	// Priority 3: 固定止盈（信号模式下跳过，由 TryReversal 窗口确认控制）
	if !lp.SignalBasedExit && pos.UnrealizedPnLPct.GreaterThanOrEqual(tpPct) {
		minMs := lp.MinHoldingTimeSec * 1000
		if h.pm.HoldingMs() >= minMs {
			h.mu.Lock()
			defer h.mu.Unlock()
			if !h.pm.IsFlat() {
				h.doClose(ctx, ExitReasonTakeProfit)
			}
			return
		}
	}

	// Priority 5: 持仓超时
	maxMs := lp.MaxHoldingTimeSec * 1000
	if h.pm.HoldingMs() > maxMs {
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.pm.IsFlat() {
			h.doClose(ctx, ExitReasonTimeout)
		}
	}
}

// TryReversal 反向信号触发平仓（Priority 4）
// 传入 mctx 供信号模式下做多窗口确认
func (h *TradeHandler) TryReversal(ctx context.Context, newDir datafeed.Direction, mctx *datafeed.MarketContext) {
	if h.pm.IsFlat() {
		return
	}
	pos := h.pm.Snapshot()
	if pos.Direction == newDir {
		return
	}
	lp := h.params.Get()
	minMs := lp.MinHoldingTimeSec * 1000
	if h.pm.HoldingMs() < minMs {
		return
	}

	if lp.SignalBasedExit {
		// 信号模式：多窗口一致性确认才平仓，防短期噪音
		if mctx == nil || !windowsConfirmReversal(newDir, mctx) {
			return
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.pm.IsFlat() {
		h.doClose(ctx, ExitReasonSignalReversal)
	}
}

// windowsConfirmReversal 检查多个时间窗口是否均支持反转方向
// RCVD 两个窗口都要满足（防短期噪音），OI 满足其一即可（OI 变化比价格慢）
func windowsConfirmReversal(newDir datafeed.Direction, mctx *datafeed.MarketContext) bool {
	isLong := newDir == datafeed.DirectionLong

	rcvd5sOk := mctx.RCVD5s.IsPositive() == isLong
	rcvd30sOk := mctx.RCVD30s.IsPositive() == isLong
	oi5sOk := mctx.OIDelta5s.IsPositive() == isLong
	oi30sOk := mctx.OIDelta30s.IsPositive() == isLong

	return rcvd5sOk && rcvd30sOk && (oi5sOk || oi30sOk)
}

// doClose 执行平仓（调用者必须持有 h.mu）
func (h *TradeHandler) doClose(ctx context.Context, reason ExitReason) {
	pos := h.pm.Snapshot()
	if pos.IsFlat() {
		return
	}

	log.Info().Str("reason", string(reason)).Str("pnl_pct", pos.UnrealizedPnLPct.StringFixed(4)).Msg("closing position")

	// 取消兜底订单（失败不阻止平仓）
	h.om.CancelOrder(ctx, pos.StopLossOrderID)
	h.om.CancelOrder(ctx, pos.TakeProfitOrderID)

	// 市价全平
	fillPrice, err := h.om.CloseMarket(ctx, pos.Direction, pos.Quantity)
	if err != nil {
		log.Error().Err(err).Msg("close market failed, entering FAILURE")
		h.addSystem("ORDER_ERROR", "close market failed, entering FAILURE", map[string]interface{}{"error": err.Error()})
		h.sm.RequestTransition(statemachine.StateFailure, "close market failed")
		return
	}

	// 计算实际 PnL
	var pnlPct float64
	if !pos.EntryPrice.IsZero() {
		diff := fillPrice.Sub(pos.EntryPrice).Div(pos.EntryPrice)
		sign := decimal.NewFromInt(int64(pos.DirectionSign()))
		pnlD := diff.Mul(sign)
		pnlPct, _ = pnlD.Float64()
	}

	prev := h.pm.Close()
	h.guard.RecordTrade(pnlPct)
	h.metrics.PositionChanged("close")

	h.cooldownUntil = time.Now().UnixMilli() + h.params.Get().CooldownAfterExitSec*1000

	log.Info().
		Str("reason", string(reason)).
		Str("entry", prev.EntryPrice.String()).
		Str("exit", fillPrice.String()).
		Float64("pnl_pct", pnlPct).
		Int64("holding_ms", time.Now().UnixMilli()-prev.EntryTime).
		Msg("position closed")
	if h.events != nil {
		h.events.Add("CLOSE", "position closed", map[string]interface{}{
			"reason":     string(reason),
			"dir":        dirStr(prev.Direction),
			"qty":        prev.Quantity.String(),
			"entry":      prev.EntryPrice.String(),
			"exit":       fillPrice.String(),
			"pnl_pct":    pnlPct,
			"holding_ms": time.Now().UnixMilli() - prev.EntryTime,
		})
	}
}

// ManualOpen 手动开仓（绕过引擎信号，保留安全检查）
func (h *TradeHandler) ManualOpen(ctx context.Context, dir datafeed.Direction) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.pm.IsFlat() {
		return fmt.Errorf("already in position")
	}
	if !h.om.HasCredentials() {
		return fmt.Errorf("missing API credentials")
	}
	if !h.sm.CanOpen() {
		return fmt.Errorf("state %s does not allow opening", h.sm.Current())
	}

	lp := h.params.Get()
	qty, err := h.orderQty(lp, decimal.Zero)
	if err != nil || qty.IsZero() {
		return fmt.Errorf("invalid order size: %w", err)
	}

	fillPrice, _, err := h.om.OpenMarket(ctx, dir, qty)
	if err != nil {
		return fmt.Errorf("open market: %w", err)
	}
	if fillPrice.IsZero() {
		return fmt.Errorf("fill price is zero after open")
	}

	h.pm.Open(dir, qty, fillPrice)
	h.metrics.PositionChanged("open")

	guardDeadline := lp.GuardDeadlineMs
	if guardDeadline <= 0 {
		guardDeadline = 150
	}
	deadline := time.Now().Add(time.Duration(guardDeadline) * time.Millisecond)
	guardCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	slPrice, tpPrice := h.computeGuardPrices(dir, fillPrice)
	slID, slErr := h.om.PlaceStopLoss(guardCtx, dir, qty, slPrice)
	tpID, tpErr := h.om.PlaceTakeProfit(guardCtx, dir, qty, tpPrice)

	if slErr != nil || tpErr != nil {
		h.doClose(ctx, ExitReasonEmergency)
		return fmt.Errorf("guard orders failed (sl=%v tp=%v), emergency closed", slErr, tpErr)
	}
	h.pm.SetGuardOrders(slID, tpID)

	if h.events != nil {
		h.events.Add("MANUAL_OPEN", "手动开仓", map[string]interface{}{
			"dir":   dirStr(dir),
			"qty":   qty.String(),
			"entry": fillPrice.String(),
			"sl":    slPrice.String(),
			"tp":    tpPrice.String(),
		})
	}
	return nil
}

// ManualClose 手动平仓
func (h *TradeHandler) ManualClose(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pm.IsFlat() {
		return fmt.Errorf("no open position to close")
	}
	h.doClose(ctx, ExitReasonManual)
	return nil
}

func (h *TradeHandler) addReject(eventType, message string, fields map[string]interface{}) {
	if h.events != nil {
		h.events.AddReject(eventType, message, fields)
	}
}

func (h *TradeHandler) addSystem(eventType, message string, fields map[string]interface{}) {
	if h.events != nil {
		h.events.AddSystem(eventType, message, fields)
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func dirStr(d datafeed.Direction) string {
	switch d {
	case datafeed.DirectionLong:
		return "LONG"
	case datafeed.DirectionShort:
		return "SHORT"
	default:
		return "FLAT"
	}
}
