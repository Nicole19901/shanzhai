package datafeed

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

type RESTClient struct {
	mu         sync.RWMutex
	baseURL    string
	apiKey     string
	apiSecret  string
	http       *http.Client
	timeOffset int64 // 本地时钟与 Binance 服务器时钟的偏移量(ms)，不依赖服务器时钟推进
}

func NewRESTClient(baseURL, apiKey, apiSecret string) *RESTClient {
	return &RESTClient{
		baseURL:   baseURL,
		apiKey:    apiKey,
		apiSecret: apiSecret,
		http:      &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *RESTClient) BaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL
}

func (c *RESTClient) UpdateCredentials(apiKey, apiSecret string) {
	c.mu.Lock()
	c.apiKey = apiKey
	c.apiSecret = apiSecret
	c.mu.Unlock()
}

func (c *RESTClient) HasCredentials() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.apiKey != "" && c.apiSecret != ""
}

// SyncTime 用 Binance 服务器时间校准本地时钟偏移，解决签名 -1021 错误。
// 只在 API Key 验证通过后调用一次；运行中签名始终用本地时钟+偏移，不再依赖服务器。
func (c *RESTClient) SyncTime(ctx context.Context) error {
	before := time.Now().UnixMilli()
	resp, err := c.get(ctx, "/fapi/v1/time", nil)
	if err != nil {
		return fmt.Errorf("sync time fetch failed: %w", err)
	}
	after := time.Now().UnixMilli()
	rtt := after - before
	var result struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return fmt.Errorf("sync time parse failed: %w", err)
	}
	// 用 RTT/2 补偿网络延迟，使偏移量更准确
	localMid := before + rtt/2
	offset := result.ServerTime - localMid
	atomic.StoreInt64(&c.timeOffset, offset)
	log.Info().Int64("offset_ms", offset).Int64("rtt_ms", rtt).Msg("local clock synced with Binance server time")
	return nil
}

// TimeOffset 返回当前时钟偏移量(ms)，供调试。
func (c *RESTClient) TimeOffset() int64 {
	return atomic.LoadInt64(&c.timeOffset)
}

// OpenInterest GET /fapi/v1/openInterest
func (c *RESTClient) OpenInterest(ctx context.Context, symbol string) (decimal.Decimal, error) {
	resp, err := c.get(ctx, "/fapi/v1/openInterest", map[string]string{"symbol": symbol})
	if err != nil {
		return decimal.Zero, err
	}
	var result struct {
		OpenInterest string `json:"openInterest"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return decimal.Zero, err
	}
	return decimal.NewFromString(result.OpenInterest)
}

// PositionRisk GET /fapi/v2/positionRisk (signed)
type PositionRisk struct {
	Symbol           string
	PositionAmt      decimal.Decimal
	EntryPrice       decimal.Decimal
	UnRealizedProfit decimal.Decimal
	PositionSide     string
}

func (c *RESTClient) PositionRisk(ctx context.Context, symbol string) ([]PositionRisk, error) {
	resp, err := c.signedGet(ctx, "/fapi/v2/positionRisk", map[string]string{"symbol": symbol})
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Symbol           string `json:"symbol"`
		PositionAmt      string `json:"positionAmt"`
		EntryPrice       string `json:"entryPrice"`
		UnRealizedProfit string `json:"unRealizedProfit"`
		PositionSide     string `json:"positionSide"`
	}
	if err := json.Unmarshal(resp, &raw); err != nil {
		return nil, err
	}
	out := make([]PositionRisk, 0, len(raw))
	for _, r := range raw {
		amt, _ := decimal.NewFromString(r.PositionAmt)
		ep, _ := decimal.NewFromString(r.EntryPrice)
		pnl, _ := decimal.NewFromString(r.UnRealizedProfit)
		out = append(out, PositionRisk{
			Symbol:           r.Symbol,
			PositionAmt:      amt,
			EntryPrice:       ep,
			UnRealizedProfit: pnl,
			PositionSide:     r.PositionSide,
		})
	}
	return out, nil
}

type FuturesBalance struct {
	Asset              string          `json:"asset"`
	Balance            decimal.Decimal `json:"balance"`
	AvailableBalance   decimal.Decimal `json:"available_balance"`
	CrossWalletBalance decimal.Decimal `json:"cross_wallet_balance"`
}

func (c *RESTClient) FuturesBalances(ctx context.Context) ([]FuturesBalance, error) {
	resp, err := c.signedGet(ctx, "/fapi/v2/balance", nil)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Asset              string `json:"asset"`
		Balance            string `json:"balance"`
		AvailableBalance   string `json:"availableBalance"`
		CrossWalletBalance string `json:"crossWalletBalance"`
	}
	if err := json.Unmarshal(resp, &raw); err != nil {
		return nil, err
	}
	out := make([]FuturesBalance, 0, len(raw))
	for _, r := range raw {
		bal, _ := decimal.NewFromString(r.Balance)
		available, _ := decimal.NewFromString(r.AvailableBalance)
		cross, _ := decimal.NewFromString(r.CrossWalletBalance)
		out = append(out, FuturesBalance{
			Asset:              r.Asset,
			Balance:            bal,
			AvailableBalance:   available,
			CrossWalletBalance: cross,
		})
	}
	return out, nil
}

// CancelOrder DELETE /fapi/v1/order (signed)
func (c *RESTClient) CancelOrder(ctx context.Context, symbol, orderID string) error {
	params := map[string]string{
		"symbol":  symbol,
		"orderId": orderID,
	}
	_, err := c.signedDelete(ctx, "/fapi/v1/order", params)
	return err
}

// PlaceOrder POST /fapi/v1/order (signed)
type OrderRequest struct {
	Symbol           string
	Side             string // BUY / SELL
	PositionSide     string // LONG / SHORT / BOTH
	Type             string // MARKET / STOP_MARKET / TAKE_PROFIT_MARKET
	Quantity         decimal.Decimal
	StopPrice        decimal.Decimal
	ReduceOnly       bool
	TimeInForce      string
	NewClientOrderID string
}

type OrderResponse struct {
	OrderID       string
	ClientOrderID string
	Status        string
	AvgPrice      decimal.Decimal
	ExecutedQty   decimal.Decimal
}

func (c *RESTClient) PlaceOrder(ctx context.Context, req OrderRequest) (*OrderResponse, error) {
	params := map[string]string{
		"symbol":           req.Symbol,
		"side":             req.Side,
		"type":             req.Type,
		"quantity":         req.Quantity.String(),
		"newClientOrderId": req.NewClientOrderID,
	}
	if req.ReduceOnly {
		params["reduceOnly"] = "true"
	}
	if !req.StopPrice.IsZero() {
		params["stopPrice"] = req.StopPrice.String()
	}
	if req.TimeInForce != "" {
		params["timeInForce"] = req.TimeInForce
	}
	if req.PositionSide != "" {
		params["positionSide"] = req.PositionSide
	}

	resp, err := c.signedPost(ctx, "/fapi/v1/order", params)
	if err != nil {
		return nil, err
	}
	var raw struct {
		OrderID       int64  `json:"orderId"`
		ClientOrderID string `json:"clientOrderId"`
		Status        string `json:"status"`
		AvgPrice      string `json:"avgPrice"`
		ExecutedQty   string `json:"executedQty"`
	}
	if err := json.Unmarshal(resp, &raw); err != nil {
		return nil, err
	}
	avg, _ := decimal.NewFromString(raw.AvgPrice)
	qty, _ := decimal.NewFromString(raw.ExecutedQty)
	return &OrderResponse{
		OrderID:       strconv.FormatInt(raw.OrderID, 10),
		ClientOrderID: raw.ClientOrderID,
		Status:        raw.Status,
		AvgPrice:      avg,
		ExecutedQty:   qty,
	}, nil
}

// SymbolInfo holds Binance exchange precision rules for a symbol.
type SymbolInfo struct {
	StepSize decimal.Decimal // qty step size (e.g. 0.001 for ETHUSDT)
	QtyPrec  int             // decimal places for quantity
}

// symbolInfoCache caches SymbolInfo per symbol to avoid repeated API calls.
var symbolInfoCache sync.Map // map[string]SymbolInfo

// exchangeInfoRaw is the parsed structure for /fapi/v1/exchangeInfo.
type exchangeInfoRaw struct {
	Symbols []struct {
		Symbol  string `json:"symbol"`
		Status  string `json:"status"`
		Filters []struct {
			FilterType string `json:"filterType"`
			StepSize   string `json:"stepSize"`
		} `json:"filters"`
	} `json:"symbols"`
}

// loadExchangeInfoCache fetches /fapi/v1/exchangeInfo and populates symbolInfoCache for all symbols.
func (c *RESTClient) loadExchangeInfoCache(ctx context.Context) (*exchangeInfoRaw, error) {
	resp, err := c.get(ctx, "/fapi/v1/exchangeInfo", nil)
	if err != nil {
		return nil, fmt.Errorf("exchangeInfo fetch failed: %w", err)
	}
	var result exchangeInfoRaw
	if err := json.Unmarshal(resp, &result); err != nil {
		return nil, fmt.Errorf("exchangeInfo parse failed: %w", err)
	}
	for _, s := range result.Symbols {
		for _, f := range s.Filters {
			if f.FilterType == "LOT_SIZE" && f.StepSize != "" {
				step, err := decimal.NewFromString(f.StepSize)
				if err != nil || step.IsZero() {
					break
				}
				prec := countDecimalPlaces(f.StepSize)
				symbolInfoCache.Store(s.Symbol, SymbolInfo{StepSize: step, QtyPrec: prec})
				break
			}
		}
	}
	return &result, nil
}

// countDecimalPlaces counts the number of decimal places in a string like "0.001".
func countDecimalPlaces(s string) int {
	for i, c := range s {
		if c == '.' {
			return len(s) - i - 1
		}
	}
	return 0
}

// GetSymbolInfo returns cached SymbolInfo for symbol, fetching exchangeInfo if not cached.
func (c *RESTClient) GetSymbolInfo(ctx context.Context, symbol string) (SymbolInfo, error) {
	if v, ok := symbolInfoCache.Load(symbol); ok {
		return v.(SymbolInfo), nil
	}
	if _, err := c.loadExchangeInfoCache(ctx); err != nil {
		return SymbolInfo{}, err
	}
	if v, ok := symbolInfoCache.Load(symbol); ok {
		return v.(SymbolInfo), nil
	}
	return SymbolInfo{}, fmt.Errorf("symbol %s not found in exchangeInfo", symbol)
}

// ValidateSymbol 检查币对是否在币安合约市场存在且状态为 TRADING（无需签名）。
// 使用 exchangeInfo 而非 ticker/price，后者对无流量合约也可能返回旧价格导致误判。
func (c *RESTClient) ValidateSymbol(ctx context.Context, symbol string) (bool, error) {
	result, err := c.loadExchangeInfoCache(ctx)
	if err != nil {
		return false, err
	}
	for _, s := range result.Symbols {
		if s.Symbol == symbol {
			if s.Status != "TRADING" {
				return false, fmt.Errorf("symbol %s exists but status is %s (not TRADING)", symbol, s.Status)
			}
			return true, nil
		}
	}
	return false, fmt.Errorf("symbol %s not found on Binance Futures (check spelling, e.g. BTCUSDT not BTCUSD)", symbol)
}

// SetLeverage POST /fapi/v1/leverage (signed)
func (c *RESTClient) SetLeverage(ctx context.Context, symbol string, leverage int) error {
	_, err := c.signedPost(ctx, "/fapi/v1/leverage", map[string]string{
		"symbol":   symbol,
		"leverage": strconv.Itoa(leverage),
	})
	return err
}

// SetMarginType POST /fapi/v1/marginType (signed)
func (c *RESTClient) SetMarginType(ctx context.Context, symbol, marginType string) error {
	_, err := c.signedPost(ctx, "/fapi/v1/marginType", map[string]string{
		"symbol":     symbol,
		"marginType": marginType,
	})
	return err
}

// OpenOrders GET /fapi/v1/openOrders (signed)
func (c *RESTClient) OpenOrders(ctx context.Context, symbol string) ([]string, error) {
	resp, err := c.signedGet(ctx, "/fapi/v1/openOrders", map[string]string{"symbol": symbol})
	if err != nil {
		return nil, err
	}
	var raw []struct {
		OrderID int64 `json:"orderId"`
	}
	if err := json.Unmarshal(resp, &raw); err != nil {
		return nil, err
	}
	ids := make([]string, len(raw))
	for i, r := range raw {
		ids[i] = strconv.FormatInt(r.OrderID, 10)
	}
	return ids, nil
}

// CancelAllOrders DELETE /fapi/v1/allOpenOrders (signed)
func (c *RESTClient) CancelAllOrders(ctx context.Context, symbol string) error {
	_, err := c.signedDelete(ctx, "/fapi/v1/allOpenOrders", map[string]string{"symbol": symbol})
	return err
}

func (c *RESTClient) get(ctx context.Context, path string, params map[string]string) ([]byte, error) {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *RESTClient) signedGet(ctx context.Context, path string, params map[string]string) ([]byte, error) {
	q := c.signedQuery(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+q, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-MBX-APIKEY", c.apiKeySnapshot())
	return c.do(req)
}

func (c *RESTClient) signedPost(ctx context.Context, path string, params map[string]string) ([]byte, error) {
	q := c.signedQuery(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path+"?"+q, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-MBX-APIKEY", c.apiKeySnapshot())
	return c.do(req)
}

func (c *RESTClient) signedDelete(ctx context.Context, path string, params map[string]string) ([]byte, error) {
	q := c.signedQuery(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path+"?"+q, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-MBX-APIKEY", c.apiKeySnapshot())
	return c.do(req)
}

func (c *RESTClient) apiKeySnapshot() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.apiKey
}

func (c *RESTClient) signedQuery(params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	q.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli()+atomic.LoadInt64(&c.timeOffset), 10))
	raw := q.Encode()
	c.mu.RLock()
	secret := c.apiSecret
	c.mu.RUnlock()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(raw))
	sig := fmt.Sprintf("%x", mac.Sum(nil))
	return raw + "&signature=" + sig
}

func (c *RESTClient) do(req *http.Request) ([]byte, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("binance API error %d: %s", resp.StatusCode, body)
	}
	return body, nil
}
