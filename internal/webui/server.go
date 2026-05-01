package webui

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/yourorg/eth-perp-system/internal/datafeed"
)

const (
	adminUser = "vera@1030"
	adminPass = "XTa4IsndYQe_NdEy"
)

// ManualTrader allows the UI to request guarded manual trades.
type ManualTrader interface {
	ManualOpen(ctx context.Context, dir datafeed.Direction) error
	ManualClose(ctx context.Context) error
}

type keyEntry struct {
	Label     string `json:"label"`
	APIKey    string `json:"api_key_masked"`
	apiKey    string
	apiSecret string
	CreatedAt time.Time `json:"created_at"`
}

type keyStore struct {
	mu      sync.RWMutex
	entries []keyEntry
}

func (ks *keyStore) add(label, apiKey, apiSecret string) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	for i, e := range ks.entries {
		if e.Label == label {
			ks.entries = append(ks.entries[:i], ks.entries[i+1:]...)
			break
		}
	}
	masked := apiKey
	if len(masked) > 8 {
		masked = masked[:8] + "****"
	}
	ks.entries = append(ks.entries, keyEntry{
		Label:     label,
		APIKey:    masked,
		apiKey:    apiKey,
		apiSecret: apiSecret,
		CreatedAt: time.Now(),
	})
}

func (ks *keyStore) remove(label string) bool {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	for i, e := range ks.entries {
		if e.Label == label {
			ks.entries = append(ks.entries[:i], ks.entries[i+1:]...)
			return true
		}
	}
	return false
}

func (ks *keyStore) get(label string) (keyEntry, bool) {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	for _, e := range ks.entries {
		if e.Label == label {
			return e, true
		}
	}
	return keyEntry{}, false
}

func (ks *keyStore) list() []keyEntry {
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	out := make([]keyEntry, len(ks.entries))
	copy(out, ks.entries)
	return out
}

// StatusProvider 返回运行时状态快照（WS 收包数、预热状态等）
type StatusProvider func() map[string]interface{}

type Server struct {
	params      *LiveParams
	events      *EventLog
	rest        *datafeed.RESTClient
	serviceName string
	keys        *keyStore

	symbolMu     sync.RWMutex
	activeSymbol string

	trader         ManualTrader
	switchSymbol   func(context.Context, string) error
	statusProvider StatusProvider
}

func NewServer(params *LiveParams, events *EventLog, rest *datafeed.RESTClient, serviceName, symbol string) *Server {
	if serviceName == "" {
		serviceName = "eth-perp-system"
	}
	return &Server{
		params:       params,
		events:       events,
		rest:         rest,
		serviceName:  serviceName,
		keys:         &keyStore{},
		activeSymbol: symbol,
	}
}

func (s *Server) SetManualTrader(t ManualTrader) { s.trader = t }

func (s *Server) SetSymbolSwitcher(fn func(context.Context, string) error) { s.switchSymbol = fn }

func (s *Server) SetStatusProvider(fn StatusProvider) { s.statusProvider = fn }

func (s *Server) SetSymbol(sym string) {
	s.symbolMu.Lock()
	s.activeSymbol = sym
	s.symbolMu.Unlock()
}

func (s *Server) getSymbol() string {
	s.symbolMu.RLock()
	defer s.symbolMu.RUnlock()
	return s.activeSymbol
}

func (s *Server) Listen(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.auth(s.handleUI))
	mux.HandleFunc("/api/params", s.auth(s.handleParams))
	mux.HandleFunc("/api/params/reset", s.auth(s.handleReset))
	mux.HandleFunc("/api/params/initialize", s.auth(s.handleInitialize))
	mux.HandleFunc("/api/credentials/verify", s.auth(s.handleCredentialVerify))
	mux.HandleFunc("/api/credentials/apply", s.auth(s.handleCredentialApply))
	mux.HandleFunc("/api/logs", s.auth(s.handleLogs))
	mux.HandleFunc("/api/control/start", s.auth(s.handleStart))
	mux.HandleFunc("/api/control/stop", s.auth(s.handleStop))
	mux.HandleFunc("/api/control/restart", s.auth(s.handleRestart))
	mux.HandleFunc("/api/keys", s.auth(s.handleKeys))
	mux.HandleFunc("/api/keys/activate", s.auth(s.handleKeyActivate))
	mux.HandleFunc("/api/keys/delete", s.auth(s.handleKeyDelete))
	mux.HandleFunc("/api/symbol", s.auth(s.handleSymbol))
	mux.HandleFunc("/api/symbol/validate", s.auth(s.handleSymbolValidate))
	mux.HandleFunc("/api/trade/open", s.auth(s.handleTradeOpen))
	mux.HandleFunc("/api/trade/close", s.auth(s.handleTradeClose))
	mux.HandleFunc("/api/status", s.auth(s.handleStatus))

	log.Info().Str("addr", addr).Msg("webui server starting")
	if s.events != nil {
		s.events.AddSystem("SYSTEM_START", "webui server starting", map[string]interface{}{"addr": addr})
	}
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Error().Err(err).Msg("webui server error")
		}
	}()
}

type credentialRequest struct {
	APIKey    string `json:"api_key"`
	APISecret string `json:"api_secret"`
}

func (s *Server) handleCredentialVerify(w http.ResponseWriter, r *http.Request) {
	s.handleCredentials(w, r, false)
}

func (s *Server) handleCredentialApply(w http.ResponseWriter, r *http.Request) {
	s.handleCredentials(w, r, true)
}

func (s *Server) handleCredentials(w http.ResponseWriter, r *http.Request, apply bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req credentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.APIKey == "" || req.APISecret == "" {
		http.Error(w, "api_key and api_secret are required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	probe := datafeed.NewRESTClient(s.rest.BaseURL(), req.APIKey, req.APISecret)

	balances, err := probe.FuturesBalances(ctx)
	if err != nil {
		log.Error().Err(err).Msg("webui: credential balance verification failed")
		if s.events != nil {
			s.events.AddSystem("KEY_VERIFY_FAILED", "credential balance verification failed", map[string]interface{}{"error": err.Error()})
		}
		http.Error(w, "verify failed: "+err.Error(), http.StatusBadRequest)
		return
	}

	positions, posErr := probe.PositionRisk(ctx, s.getSymbol())
	if posErr != nil {
		log.Warn().Err(posErr).Msg("webui: position risk fetch failed during key verify")
	}

	if apply {
		s.rest.UpdateCredentials(req.APIKey, req.APISecret)
		// 应用凭证后立即同步本地时钟偏移，避免签名 -1021 错误
		if syncErr := s.rest.SyncTime(r.Context()); syncErr != nil {
			log.Warn().Err(syncErr).Msg("webui: clock sync failed after key apply")
		}
		log.Info().Int64("clock_offset_ms", s.rest.TimeOffset()).Msg("webui: runtime Binance credentials updated")
		if s.events != nil {
			s.events.AddSystem("KEY_APPLIED", "API Key 验证成功，时钟已同步，请继续设置交易对", map[string]interface{}{
				"clock_offset_ms": s.rest.TimeOffset(),
			})
		}
	} else if s.events != nil {
		s.events.AddSystem("KEY_VERIFIED", "credential balance verification passed", nil)
	}

	posInfo := make([]map[string]interface{}, 0, len(positions))
	for _, p := range positions {
		posInfo = append(posInfo, map[string]interface{}{
			"symbol": p.Symbol,
			"amount": p.PositionAmt.String(),
			"entry":  p.EntryPrice.String(),
			"pnl":    p.UnRealizedProfit.String(),
			"side":   positionSide(p),
		})
	}

	writeJSON(w, map[string]interface{}{
		"status":    "ok",
		"applied":   apply,
		"balances":  balances,
		"positions": posInfo,
	})
}

func positionSide(p datafeed.PositionRisk) string {
	if p.PositionAmt.IsPositive() {
		return "LONG"
	}
	if p.PositionAmt.IsNegative() {
		return "SHORT"
	}
	return "FLAT"
}

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]interface{}{"keys": s.keys.list()})
	case http.MethodPost:
		var req struct {
			Label     string `json:"label"`
			APIKey    string `json:"api_key"`
			APISecret string `json:"api_secret"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		if req.Label == "" || req.APIKey == "" || req.APISecret == "" {
			http.Error(w, "label, api_key and api_secret are required", http.StatusBadRequest)
			return
		}
		s.keys.add(req.Label, req.APIKey, req.APISecret)
		log.Info().Str("label", req.Label).Msg("webui: API key saved")
		writeJSON(w, map[string]interface{}{"status": "saved", "label": req.Label})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleKeyActivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	entry, ok := s.keys.get(req.Label)
	if !ok {
		http.Error(w, "key label not found", http.StatusNotFound)
		return
	}
	s.rest.UpdateCredentials(entry.apiKey, entry.apiSecret)
	log.Info().Str("label", req.Label).Msg("webui: switched to saved API key")
	if s.events != nil {
		s.events.AddSystem("KEY_SWITCHED", "switched to saved API key", map[string]interface{}{"label": req.Label})
	}
	writeJSON(w, map[string]interface{}{"status": "activated", "label": req.Label})
}

func (s *Server) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if s.keys.remove(req.Label) {
		writeJSON(w, map[string]interface{}{"status": "deleted", "label": req.Label})
	} else {
		http.Error(w, "key label not found", http.StatusNotFound)
	}
}

func (s *Server) handleSymbol(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]string{"symbol": s.getSymbol()})
	case http.MethodPost:
		var req struct {
			Symbol  string `json:"symbol"`
			Confirm string `json:"confirm"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		sym := normalizeSymbol(req.Symbol)
		if sym == "" || req.Confirm != sym {
			http.Error(w, "confirm must exactly match normalized symbol "+sym, http.StatusBadRequest)
			return
		}
		if s.switchSymbol == nil {
			http.Error(w, "runtime symbol switcher not initialized", http.StatusServiceUnavailable)
			return
		}
		if err := s.switchSymbol(r.Context(), sym); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.SetSymbol(sym)
		if s.events != nil {
			s.events.AddSystem("SYMBOL_SWITCHED", "runtime symbol switched", map[string]interface{}{"symbol": sym})
		}
		writeJSON(w, map[string]string{"status": "ok", "symbol": sym})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSymbolValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Symbol string `json:"symbol"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	sym := normalizeSymbol(req.Symbol)
	if sym == "" {
		http.Error(w, "symbol is required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ok, err := s.rest.ValidateSymbol(ctx, sym)
	if err != nil || !ok {
		errMsg := "symbol not found on Binance Futures"
		if err != nil {
			errMsg = err.Error()
		}
		writeJSON(w, map[string]interface{}{"valid": false, "symbol": sym, "error": errMsg})
		return
	}
	writeJSON(w, map[string]interface{}{"valid": true, "symbol": sym})
}

func normalizeSymbol(input string) string {
	sym := strings.ToUpper(strings.TrimSpace(input))
	if sym == "" {
		return ""
	}
	if !strings.HasSuffix(sym, "USDT") && !strings.HasSuffix(sym, "BUSD") && !strings.HasSuffix(sym, "USDC") {
		sym += "USDT"
	}
	return sym
}

func (s *Server) handleTradeOpen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.trader == nil {
		http.Error(w, "trading not initialized", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Direction string `json:"direction"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	var dir datafeed.Direction
	switch strings.ToUpper(req.Direction) {
	case "LONG", "BUY":
		dir = datafeed.DirectionLong
	case "SHORT", "SELL":
		dir = datafeed.DirectionShort
	default:
		http.Error(w, "direction must be 'long' or 'short'", http.StatusBadRequest)
		return
	}
	if err := s.trader.ManualOpen(r.Context(), dir); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "direction": strings.ToUpper(req.Direction)})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if s.statusProvider != nil {
		writeJSON(w, s.statusProvider())
		return
	}
	writeJSON(w, map[string]interface{}{})
}

func (s *Server) handleTradeClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.trader == nil {
		http.Error(w, "trading not initialized", http.StatusServiceUnavailable)
		return
	}
	if err := s.trader.ManualClose(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		expectedUser := getenvDefault("WEBUI_USER", adminUser)
		expectedPass := getenvDefault("WEBUI_PASSWORD", adminPass)
		if !ok ||
			subtle.ConstantTimeCompare([]byte(user), []byte(expectedUser)) != 1 ||
			subtle.ConstantTimeCompare([]byte(pass), []byte(expectedPass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="eth-perp-admin"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func getenvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (s *Server) handleParams(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]interface{}{
			"current":  s.params.Get(),
			"defaults": s.params.Defaults(),
		})
	case http.MethodPost:
		var snap LiveParamsSnapshot
		if err := json.NewDecoder(r.Body).Decode(&snap); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := validateSnapshot(snap); err != nil {
			http.Error(w, "validation failed: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.params.Update(snap)
		if s.rest.HasCredentials() {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			if err := s.rest.SetLeverage(ctx, s.getSymbol(), snap.Leverage); err != nil {
				http.Error(w, "set leverage failed: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
		log.Info().Interface("params", snap).Msg("webui: params updated")
		writeJSON(w, map[string]interface{}{"status": "ok", "current": s.params.Get()})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.params.Reset()
	log.Info().Msg("webui: params reset to initialized defaults")
	writeJSON(w, map[string]interface{}{"status": "reset", "current": s.params.Get()})
}

func (s *Server) handleInitialize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defaults := s.params.SetDefaultsToCurrent()
	log.Info().Interface("params", defaults).Msg("webui: current params saved as initialized defaults")
	writeJSON(w, map[string]interface{}{
		"status":   "initialized",
		"defaults": defaults,
		"current":  s.params.Get(),
	})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		category := r.URL.Query().Get("category")
		switch category {
		case "trade", "system", "reject", "all":
			s.events.ClearCategory(category)
			writeJSON(w, map[string]string{"status": "cleared", "category": category})
		default:
			http.Error(w, "invalid category", http.StatusBadRequest)
		}
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]interface{}{
		"entries": s.events.Recent(),
		"trades":  s.events.RecentByCategory("trade"),
		"system":  s.events.RecentByCategory("system"),
		"rejects": s.events.RecentByCategory("reject"),
	})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request)  { s.handleControl(w, r, "stop") }
func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) { s.handleControl(w, r, "start") }
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	s.handleControl(w, r, "restart")
}

func (s *Server) handleControl(w http.ResponseWriter, r *http.Request, action string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if (action == "start" || action == "restart") && !s.rest.HasCredentials() {
		http.Error(w, "请先填写 API Key 和 Secret，验证通过并应用后再启动", http.StatusBadRequest)
		return
	}
	log.Warn().Str("action", action).Str("service", s.serviceName).Msg("webui: service control requested")

	scExe, scErr := s.runServiceControl(action)
	if scErr != nil {
		log.Error().Err(scErr).Str("action", action).Msg("service control failed")
		if s.events != nil {
			s.events.AddSystem("SERVICE_CONTROL_FAILED", "服务控制命令执行失败: "+scErr.Error(), map[string]interface{}{"action": action, "cmd": scExe})
		}
		http.Error(w, "服务控制失败: "+scErr.Error(), http.StatusInternalServerError)
		return
	}
	if s.events != nil {
		s.events.AddSystem("SERVICE_CONTROL", "服务控制命令已执行", map[string]interface{}{"action": action, "cmd": scExe})
	}
	writeJSON(w, map[string]string{"status": "ok", "action": action, "cmd": scExe})
}

func validateSnapshot(s LiveParamsSnapshot) error {
	type tpsl struct{ tp, sl float64 }
	for name, v := range map[string]tpsl{
		"做多(long)":  {s.LongTPPct, s.LongSLPct},
		"做空(short)": {s.ShortTPPct, s.ShortSLPct},
	} {
		if v.sl <= 0 || v.sl > 0.02 {
			return fmt.Errorf("%s 止损比例须在 (0, 2%%]", name)
		}
		if v.tp <= 0 || v.tp > 0.05 {
			return fmt.Errorf("%s 止盈比例须在 (0, 5%%]", name)
		}
		if v.tp/v.sl < 1.5 {
			return fmt.Errorf("%s 止盈/止损比须 ≥ 1.5", name)
		}
	}
	if s.CooldownAfterExitSec < 0 || s.MaxHoldingTimeSec <= 0 || s.MinHoldingTimeSec < 0 {
		return fmt.Errorf("holding and cooldown seconds must be valid positive values")
	}
	if s.MinHoldingTimeSec > s.MaxHoldingTimeSec {
		return fmt.Errorf("min_holding_time_sec cannot exceed max_holding_time_sec")
	}
	if s.LotSize == "" && s.MarginUSDT == "" {
		return fmt.Errorf("margin_usdt is required")
	}
	if s.Leverage < 1 || s.Leverage > 10 {
		return fmt.Errorf("leverage must be in [1, 10]")
	}
	if s.MakerOffsetBps < 0 || s.MakerOffsetBps > 20 {
		return fmt.Errorf("maker_offset_bps must be in [0, 20]")
	}
	if s.GuardDeadlineMs < 100 || s.GuardDeadlineMs > 180 {
		return fmt.Errorf("guard_deadline_ms must be in [100, 180]")
	}
	if s.TrendConfidence < 0 || s.TrendConfidence > 1 ||
		s.SqueezeConfidence < 0 || s.SqueezeConfidence > 1 ||
		s.TransitionConfidence < 0 || s.TransitionConfidence > 1 {
		return fmt.Errorf("confidence values must be in [0, 1]")
	}
	if s.MaxSlippageBps <= 0 || s.MaxSlippageBps > 100 {
		return fmt.Errorf("max_slippage_bps must be in (0, 100]")
	}
	if s.DailyLossLimitPct <= 0 || s.DailyLossLimitPct > 1 {
		return fmt.Errorf("daily_loss_limit_pct must be in (0, 1]")
	}
	if s.ConsecutiveLossLimit <= 0 {
		return fmt.Errorf("consecutive_loss_limit must be > 0")
	}
	if s.DepthLevels != 0 && (s.DepthLevels < 1 || s.DepthLevels > 20) {
		return fmt.Errorf("depth_levels must be in [1, 20]")
	}
	return nil
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(adminHTML))
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

const adminHTML = `<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1.0">
<title>量化交易控制台</title>
<style>
*{box-sizing:border-box}body{margin:0;font-family:Segoe UI,Arial,sans-serif;background:#101418;color:#e8edf2;padding:16px}h1{font-size:18px;margin:0;color:#f2f6fa}.top{display:flex;justify-content:space-between;align-items:center;margin-bottom:12px;gap:8px;flex-wrap:wrap}.badge{font-size:12px;color:#79c0ff;border:1px solid #27547a;border-radius:999px;padding:3px 9px}.badge.symbol{background:#0d2231;color:#56d364;border-color:#238636}.grid2{display:grid;grid-template-columns:1fr 1fr;gap:10px}.grid4{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:10px}.card{background:#171d23;border:1px solid #29333d;border-radius:8px;padding:12px;margin-bottom:10px}.card h2{font-size:12px;margin:0 0 8px;color:#a9b7c4;text-transform:uppercase;letter-spacing:.5px}.field{margin-bottom:8px}.field label{display:flex;justify-content:space-between;font-size:12px;color:#d6dee6;margin-bottom:3px}.field span{color:#79c0ff;font-family:Consolas,monospace;font-size:12px}input[type=text],input[type=number],input[type=password]{width:100%;background:#0d1116;border:1px solid #34414d;border-radius:5px;color:#e8edf2;padding:5px 7px;font-size:12px}input[type=range]{width:100%;accent-color:#2f81f7}.check{display:flex;align-items:center;gap:6px;font-size:12px;color:#d6dee6;margin-bottom:8px}.check input{width:auto}.actions{display:flex;flex-wrap:wrap;gap:6px;margin-top:10px}button{border:0;border-radius:5px;padding:6px 11px;color:#fff;font-weight:600;cursor:pointer;font-size:12px}button:disabled{opacity:.4;cursor:not-allowed}.btn-save,.btn-apply,.btn-start{background:#238636}.btn-init{background:#8957e5}.btn-reset,.btn-neutral{background:#3b434c}.btn-restart,.btn-verify{background:#0969da}.btn-stop{background:#da3633}.msg{margin-top:8px;border-radius:5px;padding:7px 10px;font-size:12px;display:none}.ok{display:block;background:#102b1a;color:#56d364;border:1px solid #238636}.err{display:block;background:#341416;color:#ff7b72;border:1px solid #8e2b31}.modal-bg{display:none;position:fixed;inset:0;background:rgba(0,0,0,.75);z-index:100;align-items:center;justify-content:center}.modal-bg.open{display:flex}.modal{background:#171d23;border:1px solid #29333d;border-radius:10px;padding:18px;max-width:520px;width:90%;max-height:80vh;overflow:auto}.modal h3{margin:0 0 10px;font-size:14px}.modal-actions{display:flex;gap:8px;margin-top:12px;justify-content:flex-end}.pos-badge{display:inline-block;padding:2px 7px;border-radius:4px;font-size:11px;font-weight:700}.pos-long{background:#0d2b12;color:#56d364;border:1px solid #238636}.pos-short{background:#2d0c0c;color:#ff7b72;border:1px solid #8e2b31}.pos-flat{background:#1c2128;color:#8b949e;border:1px solid #3b434c}.key-row{display:flex;align-items:center;gap:8px;padding:6px 0;border-bottom:1px solid #202a33;font-size:12px}.key-label{flex:1;color:#d6dee6;font-family:Consolas,monospace}.key-masked{flex:2;color:#8b949e;font-family:Consolas,monospace}.logbar{display:flex;justify-content:space-between;align-items:center;margin-bottom:6px}.loglist{height:220px;overflow:auto;background:#0d1116;border:1px solid #29333d;border-radius:5px}.row{display:grid;grid-template-columns:140px 100px 1fr;gap:6px;padding:6px 8px;border-bottom:1px solid #202a33;font-size:11px}.time{color:#8b949e}.type{font-family:Consolas,monospace;color:#79c0ff}.details{color:#d6dee6;word-break:break-word}.muted{color:#8b949e;font-size:11px}
/* 方向控制 */
.dir-col{display:flex;flex-direction:column;gap:8px;margin-bottom:10px}
.dir-card{border-radius:8px;padding:8px 10px;transition:border .2s}
.dir-card.long-card{background:#0d1e12;border:2px solid #238636}
.dir-card.short-card{background:#1e0d0d;border:2px solid #8e2b31}
.dir-card.disabled{opacity:.5;filter:grayscale(.4)}
.dir-head{display:flex;align-items:center;gap:8px;margin-bottom:6px}
.dir-title{font-size:12px;font-weight:700;flex:1}
.dir-card.long-card .dir-title{color:#56d364}
.dir-card.short-card .dir-title{color:#ff7b72}
.dir-status{font-size:11px;color:#8b949e;flex:2}
.dir-toggle{padding:3px 10px;font-size:11px;font-weight:700;border-radius:4px;border:0;cursor:pointer;color:#fff;white-space:nowrap}
.dir-card.long-card .dir-toggle{background:#238636}
.dir-card.short-card .dir-toggle{background:#da3633}
.dir-toggle.off{background:#3b434c !important}
.dir-params{display:grid;grid-template-columns:1fr 1fr;gap:6px}
.dir-params .field{margin:0}
.dir-params label{font-size:10px}
.dir-params span{font-size:10px}
.tp-dim{opacity:.45}
/* 信号平仓说明框 */
.exit-mode-box{background:#0d1116;border:1px solid #29333d;border-radius:8px;padding:10px;margin-bottom:8px}
.exit-mode-box .mode-row{display:flex;align-items:center;gap:10px;margin-bottom:6px}
.exit-mode-box .mode-title{font-size:12px;font-weight:700;color:#79c0ff}
.exit-note{font-size:11px;line-height:1.5;color:#8b949e}
.exit-note strong{color:#e3b341}
/* 参数说明 */
.info-box{background:#0d1116;border:1px solid #29333d;border-radius:8px;padding:12px;margin-bottom:10px}.info-box h2{font-size:12px;margin:0 0 8px;color:#79c0ff;text-transform:uppercase;letter-spacing:.5px}.info-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:8px}.info-item{font-size:11px;line-height:1.5;color:#8b949e}.info-item strong{display:block;color:#c9d1d9;margin-bottom:2px}
@media(max-width:680px){body{padding:10px}.grid2,.dir-grid{grid-template-columns:1fr}.row{grid-template-columns:1fr}.top{flex-direction:column;align-items:flex-start}}
</style>
</head>
<body>
<div class="top"><h1>量化交易控制台</h1><div style="display:flex;gap:8px;flex-wrap:wrap"><span class="badge symbol" id="symbolBadge">交易对 ...</span><span class="badge" id="wsMsgBadge" style="background:#0d1116;color:#8b949e;border-color:#3b434c">WS: --</span><span class="badge" id="posBadge">持仓: --</span></div></div>

<!-- 信号平仓模式说明 + 开关 -->
<div class="exit-mode-box">
<div class="mode-row">
  <span class="mode-title">平仓模式</span>
  <label style="display:flex;align-items:center;gap:5px;font-size:12px;color:#d6dee6;cursor:pointer">
    <input type="checkbox" id="signal_based_exit" onchange="onExitModeChange()">
    <span>信号确认平仓</span>
  </label>
  <span id="exitModeLabel" style="font-size:11px;color:#8b949e;margin-left:4px"></span>
</div>
<div class="exit-note" id="exitModeNote"></div>
</div>

<!-- 做多/做空方向控制（竖排，各含独立 TP/SL） -->
<div class="dir-col">
<div class="dir-card long-card" id="longCard">
  <div class="dir-head">
    <span class="dir-title">▲ 做多 (Long)</span>
    <span class="dir-status" id="longStatus"></span>
    <button class="dir-toggle" id="longToggle" onclick="toggleDir('long')"></button>
  </div>
  <div class="dir-params">
    <div class="field"><label for="long_tp_pct">止盈% <span id="v_long_tp_pct"></span></label><input id="long_tp_pct" type="range" min="0.001" max="0.05" step="0.001"></div>
    <div class="field"><label for="long_sl_pct">止损% <span id="v_long_sl_pct"></span></label><input id="long_sl_pct" type="range" min="0.001" max="0.02" step="0.001"></div>
  </div>
</div>
<div class="dir-card short-card" id="shortCard">
  <div class="dir-head">
    <span class="dir-title">▼ 做空 (Short)</span>
    <span class="dir-status" id="shortStatus"></span>
    <button class="dir-toggle" id="shortToggle" onclick="toggleDir('short')"></button>
  </div>
  <div class="dir-params">
    <div class="field"><label for="short_tp_pct">止盈% <span id="v_short_tp_pct"></span></label><input id="short_tp_pct" type="range" min="0.001" max="0.05" step="0.001"></div>
    <div class="field"><label for="short_sl_pct">止损% <span id="v_short_sl_pct"></span></label><input id="short_sl_pct" type="range" min="0.001" max="0.02" step="0.001"></div>
  </div>
</div>
</div>

<!-- 参数表单（单列紧凑布局） -->
<form id="form"><div id="grid" style="display:flex;flex-direction:column;gap:8px"></div><div class="actions"><button class="btn-save" type="submit">保存参数</button><button class="btn-restart" type="button" id="testParamsBtn">测试参数</button><button class="btn-init" type="button" id="initBtn">初始化</button><button class="btn-reset" type="button" id="resetBtn">恢复默认</button><button class="btn-start" type="button" id="startBtn">启动服务</button><button class="btn-restart" type="button" id="restartBtn">重启服务</button><button class="btn-stop" type="button" id="stopBtn">停止服务</button></div><div id="msg" class="msg"></div></form>

<div class="grid2" style="margin-top:10px">
<section class="card"><h2>交易对切换</h2><div style="display:flex;gap:6px;align-items:flex-end;flex-wrap:wrap"><div class="field" style="flex:1;min-width:140px;margin:0"><label for="symbolInput">输入币种，如 ETH 或 ETHUSDT</label><input id="symbolInput" type="text" placeholder="ETHUSDT" autocomplete="off" spellcheck="false"></div><button class="btn-verify" type="button" id="validateSymbolBtn">验证</button><button class="btn-apply" type="button" id="applySymbolBtn" disabled>切换</button></div><div class="field" style="margin-top:6px"><label for="symbolConfirm">防误触确认：再次输入完整交易对</label><input id="symbolConfirm" type="text" placeholder="ETHUSDT" autocomplete="off" spellcheck="false"></div><div id="symbolResult" class="muted" style="margin-top:6px"></div><div class="muted" style="margin-top:4px">有持仓时拒绝切换；切换后数据流、OI、下单均跟随新交易对。</div></section>
<section class="card"><h2>API 密钥管理</h2><div class="grid2" style="gap:6px;margin-bottom:8px"><div class="field"><label for="apiKey">API Key</label><input id="apiKey" autocomplete="off" spellcheck="false"></div><div class="field"><label for="apiSecret">API Secret</label><input id="apiSecret" type="password" autocomplete="off"></div></div><div class="actions"><button class="btn-verify" id="verifyKeyBtn">验证余额和持仓</button><button class="btn-apply" id="applyKeyBtn">验证并应用</button><button class="btn-neutral" id="clearBalanceBtn">清除</button></div><div style="display:flex;gap:6px;align-items:flex-end;flex-wrap:wrap;border-top:1px solid #29333d;padding-top:8px;margin-top:8px"><div class="field" style="flex:1;min-width:100px;margin:0"><label for="keyLabel">标签</label><input id="keyLabel" type="text" placeholder="主账户"></div><button class="btn-neutral" id="saveKeyBtn">保存</button></div><div id="savedKeys" style="margin-top:8px"></div></section>
</div>

<div class="modal-bg" id="verifyModal"><div class="modal"><h3>账户验证结果</h3><div id="modalContent"></div><div class="modal-actions"><button class="btn-apply" id="modalApplyBtn">应用此密钥</button><button class="btn-neutral" id="modalCancelBtn">取消</button></div></div></div>
<section style="display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:10px;margin-top:10px"><div class="card"><div class="logbar"><h2 style="margin:0">交易日志</h2><span class="muted" id="tradeHint"></span><button class="btn-neutral" style="padding:3px 7px;font-size:11px" id="clearTradeBtn">清除</button></div><div class="loglist" id="tradeLogs"></div></div><div class="card"><div class="logbar"><h2 style="margin:0">系统日志</h2><span class="muted" id="systemHint"></span><button class="btn-neutral" style="padding:3px 7px;font-size:11px" id="clearSystemBtn">清除</button></div><div class="loglist" id="systemLogs"></div></div><div class="card"><div class="logbar"><h2 style="margin:0">拒绝日志</h2><span class="muted" id="rejectHint"></span><button class="btn-neutral" style="padding:3px 7px;font-size:11px" id="clearRejectBtn">清除</button></div><div class="loglist" id="rejectLogs"></div></div></section>

<script>
const groups=[
{title:'持仓时间 & 冷却',items:[['cooldown_after_exit_sec','number','冷却秒',0,300,1,v=>v+'s'],['max_holding_time_sec','number','最长持仓秒',60,86400,60,v=>v+'s'],['min_holding_time_sec','number','最短持仓秒',0,3600,5,v=>v+'s']]},
{title:'交易执行',items:[['margin_usdt','number','保证金 USDT',1,100000,1,v=>v+' U'],['leverage','number','杠杆',1,10,1,v=>v+'x'],['use_maker_mode','checkbox','Maker 模式',0,0,0,v=>v?'开':'关'],['maker_offset_bps','range','Maker 偏移 bps',0,20,0.1,v=>v.toFixed(1)+' bps'],['guard_deadline_ms','number','保护单截止 ms',100,180,1,v=>v+'ms'],['depth_levels','number','深度档位',1,20,1,v=>v+'档']]},
{title:'引擎阈值',items:[['trend_enabled','checkbox','趋势引擎',0,0,0,v=>v?'开':'关'],['trend_confidence','range','趋势置信度',0.3,0.95,0.01,v=>v.toFixed(2)],['oi_delta_threshold','range','OI Delta',0.001,0.02,0.001,v=>(v*100).toFixed(2)+'%'],['squeeze_enabled','checkbox','Squeeze 引擎',0,0,0,v=>v?'开':'关'],['squeeze_confidence','range','Squeeze 置信度',0.3,0.95,0.01,v=>v.toFixed(2)],['basis_zscore_threshold','range','Basis ZScore',1,5,0.1,v=>v.toFixed(1)],['transition_enabled','checkbox','Transition 引擎',0,0,0,v=>v?'开':'关'],['transition_confidence','range','Transition 置信度',0.3,0.95,0.01,v=>v.toFixed(2)],['vol_compression_ratio','range','波动压缩比',0.1,0.9,0.05,v=>v.toFixed(2)]]},
{title:'风控',items:[['max_slippage_bps','range','最大滑点 bps',1,100,0.5,v=>v.toFixed(1)+' bps'],['daily_loss_limit_pct','range','日亏损限制',0.001,0.2,0.001,v=>(v*100).toFixed(2)+'%'],['consecutive_loss_limit','number','连续亏损限制',1,20,1,v=>v+'次']]}
];
let dirState={long_enabled:true,short_enabled:true};
const dirTPSL={long_tp:0.008,long_sl:0.004,short_tp:0.008,short_sl:0.004};
const fields=[];const grid=document.getElementById('grid');
for(const g of groups){const card=document.createElement('div');card.className='card';card.style='padding:8px 10px';const h=document.createElement('h2');h.textContent=g.title;card.appendChild(h);const inner=document.createElement('div');inner.style='display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:4px';for(const it of g.items){fields.push(it);const[id,type,label,min,max,step]=it;const wrap=document.createElement('div');if(type==='checkbox'){wrap.className='check';wrap.innerHTML='<input id="'+id+'" type="checkbox"><label for="'+id+'">'+label+' <span id="v_'+id+'"></span></label>';}else{wrap.className='field';wrap.style='margin-bottom:4px';wrap.innerHTML='<label for="'+id+'" style="font-size:11px">'+label+' <span id="v_'+id+'"></span></label><input id="'+id+'" type="'+type+'" min="'+min+'" max="'+max+'" step="'+step+'">';}inner.appendChild(wrap);}card.appendChild(inner);grid.appendChild(card);}
// 方向卡 TP/SL 滑块事件
['long_tp_pct','long_sl_pct','short_tp_pct','short_sl_pct'].forEach(id=>{const el=document.getElementById(id);el.addEventListener('input',()=>{const pct=(parseFloat(el.value)*100).toFixed(2)+'%';document.getElementById('v_'+id).textContent=pct;const k=id.replace('_pct','').replace('_','_');dirTPSL[id.replace('_pct','')]=parseFloat(el.value);});});
function fmt(it,val){return it[6](it[1]==='checkbox'?!!val:parseFloat(val))}
function refreshLabel(it){const el=document.getElementById(it[0]);document.getElementById('v_'+it[0]).textContent=fmt(it,it[1]==='checkbox'?el.checked:el.value)}
function setDirSlider(id,val){const el=document.getElementById(id);if(el){el.value=val;document.getElementById('v_'+id).textContent=(val*100).toFixed(2)+'%';}}
function populate(cur){
  for(const it of fields){const el=document.getElementById(it[0]);if(cur[it[0]]===undefined)continue;if(it[1]==='checkbox')el.checked=!!cur[it[0]];else el.value=cur[it[0]];refreshLabel(it)}
  if(cur.long_tp_pct!==undefined){setDirSlider('long_tp_pct',cur.long_tp_pct);dirTPSL.long_tp=cur.long_tp_pct;}
  if(cur.long_sl_pct!==undefined){setDirSlider('long_sl_pct',cur.long_sl_pct);dirTPSL.long_sl=cur.long_sl_pct;}
  if(cur.short_tp_pct!==undefined){setDirSlider('short_tp_pct',cur.short_tp_pct);dirTPSL.short_tp=cur.short_tp_pct;}
  if(cur.short_sl_pct!==undefined){setDirSlider('short_sl_pct',cur.short_sl_pct);dirTPSL.short_sl=cur.short_sl_pct;}
  if(cur.long_enabled!==undefined)dirState.long_enabled=!!cur.long_enabled;
  if(cur.short_enabled!==undefined)dirState.short_enabled=!!cur.short_enabled;
  if(cur.signal_based_exit!==undefined){document.getElementById('signal_based_exit').checked=!!cur.signal_based_exit;}
  renderDirCards();onExitModeChange();
}
function collect(){
  const o={};
  for(const it of fields){const el=document.getElementById(it[0]);o[it[0]]=it[1]==='checkbox'?el.checked:(it[0]==='margin_usdt'?String(el.value):Number(el.value));}
  o.leverage=parseInt(o.leverage);o.cooldown_after_exit_sec=parseInt(o.cooldown_after_exit_sec);o.max_holding_time_sec=parseInt(o.max_holding_time_sec);o.min_holding_time_sec=parseInt(o.min_holding_time_sec);o.guard_deadline_ms=parseInt(o.guard_deadline_ms);o.consecutive_loss_limit=parseInt(o.consecutive_loss_limit);o.depth_levels=parseInt(o.depth_levels);
  o.long_enabled=dirState.long_enabled;o.short_enabled=dirState.short_enabled;
  o.signal_based_exit=document.getElementById('signal_based_exit').checked;
  o.long_tp_pct=parseFloat(document.getElementById('long_tp_pct').value);
  o.long_sl_pct=parseFloat(document.getElementById('long_sl_pct').value);
  o.short_tp_pct=parseFloat(document.getElementById('short_tp_pct').value);
  o.short_sl_pct=parseFloat(document.getElementById('short_sl_pct').value);
  return o;
}
function msg(t,ok,el){const m=el||document.getElementById('msg');m.textContent=t;m.className='msg '+(ok?'ok':'err');setTimeout(()=>{m.className='msg'},5000)}
for(const it of fields){document.getElementById(it[0]).addEventListener('input',()=>refreshLabel(it))}
function onExitModeChange(){
  const on=document.getElementById('signal_based_exit').checked;
  const lbl=document.getElementById('exitModeLabel');
  const note=document.getElementById('exitModeNote');
  if(on){
    lbl.textContent='已开启';lbl.style.color='#e3b341';
    note.innerHTML='<strong>信号确认平仓模式（开启）：</strong>到达止盈比例时<strong>不</strong>立即平仓，等待反向信号多窗口确认后才平仓，适合趋势延续行情。<br>'
      +'✅ <strong>有效参数：</strong>止损%（本地监控+交易所保护单）、止盈%（仅用于交易所挂单价格，本地不触发）、最短/最长持仓秒、冷却秒。<br>'
      +'⚠️ <strong>注意：</strong>止盈%只定义保护单挂单价位，<u>本地止盈监控已关闭</u>。持仓将持续到反向信号出现或超时/止损触发。';
    // 半透明止盈滑块提示
    document.querySelectorAll('.dir-params .field:first-child').forEach(el=>el.classList.add('tp-dim'));
  } else {
    lbl.textContent='已关闭（固定止盈）';lbl.style.color='#8b949e';
    note.innerHTML='<strong>固定止盈模式（关闭）：</strong>价格达到止盈%时立即市价平仓，不等信号确认，适合快进快出波动行情。<br>'
      +'✅ <strong>有效参数：</strong>止盈%（本地监控+保护单）、止损%、最短/最长持仓秒、冷却秒。';
    document.querySelectorAll('.dir-params .field:first-child').forEach(el=>el.classList.remove('tp-dim'));
  }
}
function renderDirCards(){
  const lc=document.getElementById('longCard'),sc=document.getElementById('shortCard');
  const ls=document.getElementById('longStatus'),ss=document.getElementById('shortStatus');
  const lt=document.getElementById('longToggle'),st=document.getElementById('shortToggle');
  if(dirState.long_enabled){lc.classList.remove('disabled');ls.textContent='● 已启用';ls.style.color='#56d364';lt.textContent='禁用';lt.classList.remove('off')}
  else{lc.classList.add('disabled');ls.textContent='○ 禁用';ls.style.color='#8b949e';lt.textContent='启用';lt.classList.add('off')}
  if(dirState.short_enabled){sc.classList.remove('disabled');ss.textContent='● 已启用';ss.style.color='#ff7b72';st.textContent='禁用';st.classList.remove('off')}
  else{sc.classList.add('disabled');ss.textContent='○ 禁用';ss.style.color='#8b949e';st.textContent='启用';st.classList.add('off')}
}
async function toggleDir(dir){
  dirState[dir+'_enabled']=!dirState[dir+'_enabled'];renderDirCards();
  const r=await fetch('/api/params',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(collect())});
  if(r.ok){msg((dir==='long'?'做多':'做空')+'方向已'+(dirState[dir+'_enabled']?'启用':'禁用'),true)}
  else{dirState[dir+'_enabled']=!dirState[dir+'_enabled'];renderDirCards();msg('保存失败: '+await r.text(),false)}
}
document.getElementById('form').addEventListener('submit',async e=>{
  e.preventDefault();const p=collect();
  const errs=[];
  if(p.long_tp_pct/p.long_sl_pct<1.5)errs.push('做多 TP/SL<1.5');
  if(p.short_tp_pct/p.short_sl_pct<1.5)errs.push('做空 TP/SL<1.5');
  if(errs.length){msg(errs.join('，'),false);return}
  const r=await fetch('/api/params',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(p)});
  if(r.ok){const d=await r.json();populate(d.current);msg('参数已保存',true)}else msg('保存失败: '+await r.text(),false)});
document.getElementById('testParamsBtn').onclick=async()=>{const p=Object.assign(collect(),{long_tp_pct:0.002,long_sl_pct:0.001,short_tp_pct:0.002,short_sl_pct:0.001,margin_usdt:'10',cooldown_after_exit_sec:5,min_holding_time_sec:0,max_holding_time_sec:600,trend_enabled:true,trend_confidence:0.30,oi_delta_threshold:0.001,squeeze_enabled:true,squeeze_confidence:0.40,basis_zscore_threshold:1.0,transition_enabled:true,transition_confidence:0.30,vol_compression_ratio:0.90,max_slippage_bps:20});populate(p);const r=await fetch('/api/params',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(p)});if(r.ok){const d=await r.json();populate(d.current);msg('测试参数已保存',true)}else msg('设置测试参数失败: '+await r.text(),false)};
document.getElementById('resetBtn').onclick=async()=>{if(!confirm('恢复到初始化参数？'))return;const r=await fetch('/api/params/reset',{method:'POST'});if(r.ok){const d=await r.json();populate(d.current);msg('已恢复默认参数',true)}else msg('恢复失败',false)};
document.getElementById('initBtn').onclick=async()=>{if(!confirm('把当前参数保存为新的默认值？'))return;const r=await fetch('/api/params/initialize',{method:'POST'});msg(r.ok?'当前参数已设为默认值':'保存失败',r.ok)};
document.getElementById('startBtn').onclick=async()=>{if(!confirm('确认启动系统服务？'))return;const r=await fetch('/api/control/start',{method:'POST'});msg(r.ok?'启动命令已发送':'启动失败: '+await r.text(),r.ok)};
document.getElementById('restartBtn').onclick=async()=>{if(!confirm('确认重启系统服务？'))return;const r=await fetch('/api/control/restart',{method:'POST'});msg(r.ok?'重启命令已发送':'重启失败: '+await r.text(),r.ok)};
document.getElementById('stopBtn').onclick=async()=>{if(!confirm('确认停止系统服务？'))return;const r=await fetch('/api/control/stop',{method:'POST'});msg(r.ok?'停止命令已发送':'停止失败: '+await r.text(),r.ok)};
let verifiedSymbol='';
document.getElementById('validateSymbolBtn').onclick=async()=>{const sym=document.getElementById('symbolInput').value.trim();const el=document.getElementById('symbolResult');verifiedSymbol='';document.getElementById('applySymbolBtn').disabled=true;if(!sym){el.textContent='请输入币种';return}el.style.color='#8b949e';el.textContent='验证中...';const r=await fetch('/api/symbol/validate',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({symbol:sym})});const d=await r.json();if(d.valid){verifiedSymbol=d.symbol;document.getElementById('symbolConfirm').value='';document.getElementById('applySymbolBtn').disabled=false;el.style.color='#56d364';el.textContent='验证通过：'+d.symbol+'，请在确认框再次输入'}else{el.style.color='#ff7b72';el.textContent='不存在：'+(d.error||d.symbol)}};
document.getElementById('applySymbolBtn').onclick=async()=>{const el=document.getElementById('symbolResult');const ct=document.getElementById('symbolConfirm').value.trim().toUpperCase();if(!verifiedSymbol){el.style.color='#ff7b72';el.textContent='请先验证币种';return}if(ct!==verifiedSymbol){el.style.color='#ff7b72';el.textContent='确认不匹配，需输入 '+verifiedSymbol;return}const r=await fetch('/api/symbol',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({symbol:verifiedSymbol,confirm:ct})});if(r.ok){const d=await r.json();el.style.color='#56d364';el.textContent='已切换到 '+d.symbol;document.getElementById('applySymbolBtn').disabled=true;loadSymbol()}else{el.style.color='#ff7b72';el.textContent='切换失败: '+await r.text()}};
let pendingVerifyData=null;
function keyPayload(){return{api_key:document.getElementById('apiKey').value.trim(),api_secret:document.getElementById('apiSecret').value.trim()}}
function renderModal(data,apply){const c=document.getElementById('modalContent');let html='<div style="margin-bottom:10px"><strong style="color:#a9b7c4">账户余额</strong><div style="font-family:Consolas,monospace;font-size:12px;margin-top:5px;color:#d6dee6">';const bals=data.balances||[];if(!bals.length){html+='暂无余额数据'}else{bals.forEach(b=>{const bal=parseFloat(b.balance||0);if(bal>0)html+=b.asset+': <span style="color:#56d364">'+parseFloat(b.balance).toFixed(4)+'</span> 可用: '+parseFloat(b.available_balance).toFixed(4)+'<br>'})}html+='</div></div><div><strong style="color:#a9b7c4">持仓状态</strong><div style="margin-top:5px">';const pos=data.positions||[];if(!pos.length){html+='<span style="color:#8b949e;font-size:12px">无持仓</span>'}else{pos.forEach(p=>{const amt=parseFloat(p.amount||0);if(Math.abs(amt)<0.000001)return;const cls=p.side==='LONG'?'pos-long':p.side==='SHORT'?'pos-short':'pos-flat';html+='<div style="display:flex;gap:8px;align-items:center;margin-bottom:5px;font-size:12px"><span class="pos-badge '+cls+'">'+p.side+'</span><span style="color:#d6dee6">'+p.symbol+'</span><span style="color:#79c0ff">'+p.amount+'</span><span style="color:#8b949e">入场 '+parseFloat(p.entry).toFixed(2)+'</span>';const pnl=parseFloat(p.pnl||0);html+='<span style="color:'+(pnl>=0?'#56d364':'#ff7b72')+'">'+(pnl>=0?'+':'')+pnl.toFixed(4)+'</span></div>'})}html+='</div></div>';c.innerHTML=html;pendingVerifyData=keyPayload();document.getElementById('modalApplyBtn').style.display=apply?'none':'';document.getElementById('verifyModal').classList.add('open')}
document.getElementById('modalCancelBtn').onclick=()=>document.getElementById('verifyModal').classList.remove('open');
document.getElementById('verifyModal').onclick=e=>{if(e.target===document.getElementById('verifyModal'))document.getElementById('verifyModal').classList.remove('open')};
document.getElementById('modalApplyBtn').onclick=async()=>{if(!pendingVerifyData)return;const r=await fetch('/api/credentials/apply',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(pendingVerifyData)});document.getElementById('verifyModal').classList.remove('open');msg(r.ok?'密钥已应用':'应用失败: '+await r.text(),r.ok)};
document.getElementById('verifyKeyBtn').onclick=async()=>{const p=keyPayload();if(!p.api_key||!p.api_secret){msg('请填写 API Key 和 Secret',false);return}const r=await fetch('/api/credentials/verify',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(p)});if(r.ok){const d=await r.json();renderModal(d,false)}else msg('验证失败: '+await r.text(),false)};
document.getElementById('applyKeyBtn').onclick=async()=>{const p=keyPayload();if(!p.api_key||!p.api_secret){msg('请填写 API Key 和 Secret',false);return}if(!confirm('确认验证并应用到运行中的交易客户端？'))return;const r=await fetch('/api/credentials/apply',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(p)});if(r.ok){const d=await r.json();renderModal(d,true);msg('密钥已应用',true)}else msg('应用失败: '+await r.text(),false)};
document.getElementById('clearBalanceBtn').onclick=()=>{document.getElementById('apiKey').value='';document.getElementById('apiSecret').value='';pendingVerifyData=null};
document.getElementById('saveKeyBtn').onclick=async()=>{const p=keyPayload();const label=document.getElementById('keyLabel').value.trim();if(!label||!p.api_key||!p.api_secret){msg('请填写标签、Key 和 Secret',false);return}const r=await fetch('/api/keys',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({label,api_key:p.api_key,api_secret:p.api_secret})});if(r.ok){msg('密钥已保存: '+label,true);loadKeys()}else msg('保存失败: '+await r.text(),false)};
async function loadKeys(){const r=await fetch('/api/keys');if(!r.ok)return;const d=await r.json();const keys=d.keys||[];const box=document.getElementById('savedKeys');if(!keys.length){box.innerHTML='<div class="muted">暂无保存的密钥</div>';return}box.innerHTML='<div style="border-top:1px solid #29333d;padding-top:6px;margin-top:4px">'+keys.map(k=>'<div class="key-row"><span class="key-label">'+k.label+'</span><span class="key-masked">'+k.api_key_masked+'</span><button class="btn-apply" style="padding:2px 7px;font-size:11px" onclick="activateKey(\''+k.label+'\')">激活</button><button class="btn-neutral" style="padding:2px 7px;font-size:11px" onclick="deleteKey(\''+k.label+'\')">删除</button></div>').join('')+'</div>'}
async function activateKey(label){if(!confirm('切换到密钥 '+label+'？'))return;const r=await fetch('/api/keys/activate',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({label})});msg(r.ok?'已切换到: '+label:'切换失败: '+await r.text(),r.ok)}
async function deleteKey(label){if(!confirm('删除密钥 '+label+'？'))return;const r=await fetch('/api/keys/delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({label})});if(r.ok){msg('已删除 '+label,true);loadKeys()}else msg('删除失败',false)}
function formatLog(e){const f=e.fields||{};return[e.message,f.engine&&('引擎:'+f.engine),f.dir&&('方向:'+f.dir),f.qty&&('数量:'+f.qty),f.entry&&('开仓:'+f.entry),f.exit&&('平仓:'+f.exit),f.pnl_pct!==undefined&&('PnL:'+(Number(f.pnl_pct)*100).toFixed(4)+'%'),f.reason&&('原因:'+f.reason),f.error&&('错误:'+f.error),f.est_slip_bps!==undefined&&('滑点:'+Number(f.est_slip_bps).toFixed(2)+'bps')].filter(Boolean).join(' | ')}
function renderLogBox(id,entries,empty){const box=document.getElementById(id);box.innerHTML='';if(!entries.length){box.innerHTML='<div class="row"><div class="details">'+empty+'</div></div>';return}for(const e of entries.slice().reverse()){const row=document.createElement('div');row.className='row';row.innerHTML='<div class="time">'+new Date(e.time).toLocaleString()+'</div><div class="type">'+e.type+'</div><div class="details">'+formatLog(e)+'</div>';box.appendChild(row)}}
async function loadLogs(){const now='刷新于 '+new Date().toLocaleTimeString();try{const r=await fetch('/api/logs');const d=await r.json();renderLogBox('tradeLogs',d.trades||[],'暂无开仓/平仓记录');renderLogBox('systemLogs',d.system||[],'暂无系统日志');renderLogBox('rejectLogs',d.rejects||[],'暂无拒绝记录');['tradeHint','systemHint','rejectHint'].forEach(id=>document.getElementById(id).textContent=now)}catch(e){['tradeHint','systemHint','rejectHint'].forEach(id=>document.getElementById(id).textContent='加载失败')}}
async function clearLogs(cat){const r=await fetch('/api/logs?category='+cat,{method:'DELETE'});if(r.ok)loadLogs()}
document.getElementById('clearTradeBtn').onclick=()=>clearLogs('trade');
document.getElementById('clearSystemBtn').onclick=()=>clearLogs('system');
document.getElementById('clearRejectBtn').onclick=()=>clearLogs('reject');
async function loadSymbol(){try{const r=await fetch('/api/symbol');const d=await r.json();document.getElementById('symbolBadge').textContent='交易对 '+(d.symbol||'--')}catch(e){}}
async function loadStatus(){try{const r=await fetch('/api/status');if(!r.ok)return;const d=await r.json();const el=document.getElementById('wsMsgBadge');const n=d.ws_msg_count||0;const done=d.warmup_done||false;if(done){el.style.background='#0d2231';el.style.color='#56d364';el.style.borderColor='#238636';el.textContent='WS: 就绪 '+n+' 条'}else if(n>0){el.style.background='#1a1a0d';el.style.color='#e3b341';el.style.borderColor='#9e6a03';el.textContent='WS: 预热中 '+n+'/100'}else{el.style.background='#1a0d0d';el.style.color='#ff7b72';el.style.borderColor='#8e2b31';el.textContent='WS: 未收到数据'}}catch(e){}}
(async()=>{try{const r=await fetch('/api/params');const d=await r.json();populate(d.current)}catch(e){msg('加载参数失败',false)}loadLogs();loadKeys();loadSymbol();loadStatus();setInterval(loadLogs,3000);setInterval(loadSymbol,10000);setInterval(loadStatus,2000)})();
</script>
</body>
</html>`
