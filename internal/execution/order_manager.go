package execution

import (
	"context"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/config"
	"github.com/yourorg/eth-perp-system/internal/datafeed"
	"github.com/yourorg/eth-perp-system/internal/telemetry"
)

// OrderManager 串行处理所有交易所订单请求（唯一允许调用 REST API 的模块）
type OrderManager struct {
	rest    *datafeed.RESTClient
	cfg     *config.Config
	metrics *telemetry.Metrics
}

func NewOrderManager(rest *datafeed.RESTClient, cfg *config.Config, m *telemetry.Metrics) *OrderManager {
	return &OrderManager{rest: rest, cfg: cfg, metrics: m}
}

// OpenMarket 市价开仓，返回成交均价
func (om *OrderManager) OpenMarket(ctx context.Context, dir datafeed.Direction, qty decimal.Decimal) (decimal.Decimal, string, error) {
	side := "BUY"
	if dir == datafeed.DirectionShort {
		side = "SELL"
	}
	clientID := fmt.Sprintf("open_%d", time.Now().UnixMilli())
	req := datafeed.OrderRequest{
		Symbol:           om.cfg.Trading.Symbol,
		Side:             side,
		Type:             "MARKET",
		Quantity:         qty,
		ReduceOnly:       false,
		NewClientOrderID: clientID,
	}

	ctx2, cancel := context.WithTimeout(ctx, time.Duration(om.cfg.Execution.OrderTimeoutMs)*time.Millisecond)
	defer cancel()

	resp, err := om.rest.PlaceOrder(ctx2, req)
	if err != nil {
		om.metrics.OrderSubmitted("MARKET_OPEN", "failed")
		return decimal.Zero, "", fmt.Errorf("open market order: %w", err)
	}
	om.metrics.OrderSubmitted("MARKET_OPEN", "ok")
	log.Info().Str("orderID", resp.OrderID).Str("status", resp.Status).Msg("open market order placed")
	return resp.AvgPrice, resp.OrderID, nil
}

// PlaceStopLoss reduceOnly STOP_MARKET 止损兜底
func (om *OrderManager) PlaceStopLoss(ctx context.Context, dir datafeed.Direction, qty, stopPrice decimal.Decimal) (string, error) {
	side := "SELL"
	if dir == datafeed.DirectionShort {
		side = "BUY"
	}
	clientID := fmt.Sprintf("sl_%d", time.Now().UnixMilli())
	req := datafeed.OrderRequest{
		Symbol:           om.cfg.Trading.Symbol,
		Side:             side,
		Type:             "STOP_MARKET",
		Quantity:         qty,
		StopPrice:        stopPrice,
		ReduceOnly:       true,
		NewClientOrderID: clientID,
	}
	ctx2, cancel := context.WithTimeout(ctx, time.Duration(om.cfg.Execution.OrderTimeoutMs)*time.Millisecond)
	defer cancel()

	resp, err := om.rest.PlaceOrder(ctx2, req)
	if err != nil {
		om.metrics.OrderSubmitted("STOP_MARKET", "failed")
		return "", fmt.Errorf("place stop loss: %w", err)
	}
	om.metrics.OrderSubmitted("STOP_MARKET", "ok")
	log.Info().Str("orderID", resp.OrderID).Str("stopPrice", stopPrice.String()).Msg("stop loss order placed")
	return resp.OrderID, nil
}

// PlaceTakeProfit reduceOnly TAKE_PROFIT_MARKET 止盈兜底
func (om *OrderManager) PlaceTakeProfit(ctx context.Context, dir datafeed.Direction, qty, targetPrice decimal.Decimal) (string, error) {
	side := "SELL"
	if dir == datafeed.DirectionShort {
		side = "BUY"
	}
	clientID := fmt.Sprintf("tp_%d", time.Now().UnixMilli())
	req := datafeed.OrderRequest{
		Symbol:           om.cfg.Trading.Symbol,
		Side:             side,
		Type:             "TAKE_PROFIT_MARKET",
		Quantity:         qty,
		StopPrice:        targetPrice,
		ReduceOnly:       true,
		NewClientOrderID: clientID,
	}
	ctx2, cancel := context.WithTimeout(ctx, time.Duration(om.cfg.Execution.OrderTimeoutMs)*time.Millisecond)
	defer cancel()

	resp, err := om.rest.PlaceOrder(ctx2, req)
	if err != nil {
		om.metrics.OrderSubmitted("TP_MARKET", "failed")
		return "", fmt.Errorf("place take profit: %w", err)
	}
	om.metrics.OrderSubmitted("TP_MARKET", "ok")
	log.Info().Str("orderID", resp.OrderID).Str("targetPrice", targetPrice.String()).Msg("take profit order placed")
	return resp.OrderID, nil
}

// CloseMarket 市价全平（reduceOnly）
func (om *OrderManager) CloseMarket(ctx context.Context, dir datafeed.Direction, qty decimal.Decimal) (decimal.Decimal, error) {
	side := "SELL"
	if dir == datafeed.DirectionShort {
		side = "BUY"
	}
	clientID := fmt.Sprintf("close_%d", time.Now().UnixMilli())
	req := datafeed.OrderRequest{
		Symbol:           om.cfg.Trading.Symbol,
		Side:             side,
		Type:             "MARKET",
		Quantity:         qty,
		ReduceOnly:       true,
		NewClientOrderID: clientID,
	}

	var lastErr error
	for attempt := 1; attempt <= om.cfg.Execution.MaxOrderRetry; attempt++ {
		ctx2, cancel := context.WithTimeout(ctx, time.Duration(om.cfg.Execution.OrderTimeoutMs)*time.Millisecond)
		resp, err := om.rest.PlaceOrder(ctx2, req)
		cancel()
		if err == nil {
			om.metrics.OrderSubmitted("MARKET_CLOSE", "ok")
			log.Info().Str("orderID", resp.OrderID).Str("avgPrice", resp.AvgPrice.String()).Msg("close market order filled")
			return resp.AvgPrice, nil
		}
		lastErr = err
		om.metrics.OrderSubmitted("MARKET_CLOSE", "failed")
		log.Error().Err(err).Int("attempt", attempt).Msg("close market order failed, retrying")
		time.Sleep(100 * time.Millisecond)
	}
	return decimal.Zero, fmt.Errorf("close market failed after %d retries: %w", om.cfg.Execution.MaxOrderRetry, lastErr)
}

// CancelOrder 取消单个订单（失败仅报警，不阻止平仓）
func (om *OrderManager) CancelOrder(ctx context.Context, orderID string) {
	if orderID == "" {
		return
	}
	if err := om.rest.CancelOrder(ctx, om.cfg.Trading.Symbol, orderID); err != nil {
		log.Error().Err(err).Str("orderID", orderID).Msg("cancel order failed (warning only)")
	}
}

// CancelAll 取消所有未成交订单
func (om *OrderManager) CancelAll(ctx context.Context) error {
	return om.rest.CancelAllOrders(ctx, om.cfg.Trading.Symbol)
}
