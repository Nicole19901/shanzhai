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
	"time"

	"github.com/shopspring/decimal"
)

type RESTClient struct {
	baseURL   string
	apiKey    string
	apiSecret string
	http      *http.Client
}

func NewRESTClient(baseURL, apiKey, apiSecret string) *RESTClient {
	return &RESTClient{
		baseURL:   baseURL,
		apiKey:    apiKey,
		apiSecret: apiSecret,
		http:      &http.Client{Timeout: 10 * time.Second},
	}
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
	req.Header.Set("X-MBX-APIKEY", c.apiKey)
	return c.do(req)
}

func (c *RESTClient) signedPost(ctx context.Context, path string, params map[string]string) ([]byte, error) {
	q := c.signedQuery(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path+"?"+q, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-MBX-APIKEY", c.apiKey)
	return c.do(req)
}

func (c *RESTClient) signedDelete(ctx context.Context, path string, params map[string]string) ([]byte, error) {
	q := c.signedQuery(params)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+path+"?"+q, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-MBX-APIKEY", c.apiKey)
	return c.do(req)
}

func (c *RESTClient) signedQuery(params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	q.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	raw := q.Encode()
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
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
