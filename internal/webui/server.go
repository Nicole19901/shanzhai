package webui

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
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

type Server struct {
	params      *LiveParams
	events      *EventLog
	rest        *datafeed.RESTClient
	serviceName string
	keys        *keyStore

	symbolMu     sync.RWMutex
	activeSymbol string

	trader       ManualTrader
	switchSymbol func(context.Context, string) error
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
	mux.HandleFunc("/api/keys/activate", s.auth(s.handleKeyActivate))
	mux.HandleFunc("/api/keys/delete", s.auth(s.handleKeyDelete))
	mux.HandleFunc("/api/symbol/validate", s.auth(s.handleSymbolValidate))
	mux.HandleFunc("/api/trade/open", s.auth(s.handleTradeOpen))
	mux.HandleFunc("/api/trade/close", s.auth(s.handleTradeClose))

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
		log.Info().Msg("webui: runtime Binance credentials updated")
		if s.events != nil {
			s.events.AddSystem("KEY_APPLIED", "runtime Binance credentials updated after balance verification", nil)
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
	log.Warn().Str("action", action).Str("service", s.serviceName).Msg("webui: service control requested")
	if s.events != nil {
		s.events.AddSystem("SERVICE_CONTROL", "service control requested", map[string]interface{}{"action": action, "service": s.serviceName})
	}
	writeJSON(w, map[string]string{"status": "accepted", "action": action})

	go func() {
		time.Sleep(500 * time.Millisecond)
		if err := exec.Command("systemctl", action, s.serviceName).Run(); err != nil {
			log.Error().Err(err).Str("action", action).Str("service", s.serviceName).Msg("systemctl command failed")
		}
	}()
}

func validateSnapshot(s LiveParamsSnapshot) error {
	if s.StopLossPct <= 0 || s.StopLossPct > 0.02 {
		return fmt.Errorf("stop_loss_pct must be in (0, 0.02]")
	}
	if s.TakeProfitPct <= 0 || s.TakeProfitPct > 0.05 {
		return fmt.Errorf("take_profit_pct must be in (0, 0.05]")
	}
	if s.TakeProfitPct/s.StopLossPct < 1.5 {
		return fmt.Errorf("take_profit/stop_loss ratio must be >= 1.5")
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
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>量化交易控制台</title>
<style>
*{box-sizing:border-box}
body{margin:0;font-family:Segoe UI,Arial,sans-serif;background:#101418;color:#e8edf2;padding:20px}
.top{display:flex;justify-content:space-between;align-items:center;margin-bottom:16px;gap:12px;flex-wrap:wrap}
h1{font-size:20px;margin:0;color:#f2f6fa}
.badge{font-size:12px;color:#79c0ff;border:1px solid #27547a;border-radius:999px;padding:4px 10px}
.badge.symbol{background:#0d2231;color:#56d364;border-color:#238636}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(290px,1fr));gap:12px}
.card{background:#171d23;border:1px solid #29333d;border-radius:8px;padding:14px}
.card h2{font-size:13px;margin:0 0 10px;color:#a9b7c4;text-transform:uppercase;letter-spacing:.5px}
.field{margin-bottom:10px}
.field label{display:flex;justify-content:space-between;font-size:13px;color:#d6dee6;margin-bottom:4px}
.field span{color:#79c0ff;font-family:Consolas,monospace}
input[type=text],input[type=number],input[type=password]{width:100%;background:#0d1116;border:1px solid #34414d;border-radius:5px;color:#e8edf2;padding:6px 8px;font-size:13px}
input[type=range]{width:100%;accent-color:#2f81f7}
.check{display:flex;align-items:center;gap:8px;font-size:13px;color:#d6dee6;margin-bottom:10px}
.check input{width:auto}
.actions{display:flex;flex-wrap:wrap;gap:8px;margin-top:14px}
button{border:0;border-radius:6px;padding:8px 13px;color:#fff;font-weight:600;cursor:pointer;font-size:13px}
button:disabled{opacity:.4;cursor:not-allowed}
.btn-save{background:#238636}.btn-init{background:#8957e5}.btn-reset{background:#3b434c}
.btn-start{background:#1f883d}.btn-restart{background:#0969da}.btn-stop{background:#da3633}
.btn-long{background:#1f883d}.btn-short{background:#da3633}.btn-close{background:#6e4000}
.btn-verify{background:#0969da}.btn-apply{background:#238636}.btn-neutral{background:#3b434c}
.msg{margin-top:10px;border-radius:6px;padding:8px 12px;font-size:13px;display:none}
.ok{display:block;background:#102b1a;color:#56d364;border:1px solid #238636}
.err{display:block;background:#341416;color:#ff7b72;border:1px solid #8e2b31}

/* Modal */
.modal-bg{display:none;position:fixed;inset:0;background:rgba(0,0,0,.7);z-index:100;align-items:center;justify-content:center}
.modal-bg.open{display:flex}
.modal{background:#171d23;border:1px solid #29333d;border-radius:10px;padding:20px;max-width:520px;width:90%;max-height:80vh;overflow-y:auto}
.modal h3{margin:0 0 12px;font-size:15px;color:#f2f6fa}
.modal-actions{display:flex;gap:8px;margin-top:14px;justify-content:flex-end}
.pos-badge{display:inline-block;padding:3px 8px;border-radius:4px;font-size:12px;font-weight:700}
.pos-long{background:#0d2b12;color:#56d364;border:1px solid #238636}
.pos-short{background:#2d0c0c;color:#ff7b72;border:1px solid #8e2b31}
.pos-flat{background:#1c2128;color:#8b949e;border:1px solid #3b434c}

/* Keys list */
.key-row{display:flex;align-items:center;gap:8px;padding:7px 0;border-bottom:1px solid #202a33;font-size:13px}
.key-row:last-child{border-bottom:0}
.key-label{flex:1;color:#d6dee6;font-family:Consolas,monospace}
.key-masked{flex:2;color:#8b949e;font-family:Consolas,monospace}

/* Logs */
.logbar{display:flex;justify-content:space-between;align-items:center;margin-bottom:8px}
.loglist{height:240px;overflow:auto;background:#0d1116;border:1px solid #29333d;border-radius:6px}
.row{display:grid;grid-template-columns:155px 110px 1fr;gap:8px;padding:8px 10px;border-bottom:1px solid #202a33;font-size:12px}
.row:last-child{border-bottom:0}
.time{color:#8b949e}.type{font-family:Consolas,monospace;color:#79c0ff}
.type.OPEN,.type.MANUAL_OPEN{color:#56d364}
.type.CLOSE,.type.MANUAL_CLOSE{color:#ffb86b}
.type.ORDER_ERROR,.type.DATA_ERROR,.type.KEY_VERIFY_FAILED{color:#ff7b72}
.type.OPEN_REJECTED,.type.SIGNAL_REJECTED{color:#d29922}
.details{color:#d6dee6;word-break:break-word}
.muted{color:#8b949e;font-size:12px}
@media(max-width:680px){body{padding:12px}.row{grid-template-columns:1fr}.top{flex-direction:column}}
</style>
</head>
<body>
<div class="top">
  <h1>閲忓寲浜ゆ槗鎺у埗鍙?/h1>
  <div style="display:flex;gap:8px;flex-wrap:wrap">
    <span class="badge symbol" id="symbolBadge">浜ゆ槗瀵?...</span>
    <span class="badge" id="posBadge">鎸佷粨: --</span>
  </div>
</div>

<form id="form">
<div class="grid" id="grid"></div>
<div class="actions">
  <button class="btn-save" type="submit">淇濆瓨鍙傛暟</button>
  <button class="btn-restart" type="button" id="testParamsBtn">测试开单参数</button>
  <button class="btn-init" type="button" id="initBtn">鍒濆鍖栦负褰撳墠鍙傛暟</button>
  <button class="btn-reset" type="button" id="resetBtn">鎭㈠鍒濆鍖栧€?/button>
  <button class="btn-start" type="button" id="startBtn">鍚姩绯荤粺</button>
  <button class="btn-restart" type="button" id="restartBtn">閲嶅惎绯荤粺</button>
  <button class="btn-stop" type="button" id="stopBtn">鍋滄绯荤粺</button>
</div>
<div id="msg" class="msg"></div>
</form>

<section class="card" style="margin-top:14px">
  <h2>鎵嬪姩浜ゆ槗</h2>
  <div style="font-size:13px;color:#8b949e;margin-bottom:10px">鎸夊畨鍏ㄦ潯浠舵墽琛屾墜鍔ㄦ搷浣滐細鏃犳寔浠撱€佹湁鍑瘉銆佺姸鎬佸厑璁搞€傛墜鍔ㄤ氦鏄撶粫杩囧紩鎿庝俊鍙枫€?/div>
  <div class="actions">
    <button class="btn-long" id="manualLongBtn">鎵嬪姩鍋氬</button>
    <button class="btn-short" id="manualShortBtn">鎵嬪姩鍋氱┖</button>
    <button class="btn-close" id="manualCloseBtn">鎵嬪姩骞充粨</button>
  </div>
  <div id="tradeMsg" class="msg"></div>
</section>

<section class="card" style="margin-top:14px">
  <h2>浜ゆ槗瀵瑰垏鎹?/h2>
  <div style="display:flex;gap:8px;align-items:flex-end;flex-wrap:wrap">
    <div class="field" style="flex:1;min-width:160px;margin:0">
      <label for="symbolInput">杈撳叆甯佺锛屽 BTC 鎴?BTCUSDT</label>
      <input id="symbolInput" type="text" placeholder="BTCUSDT" autocomplete="off" spellcheck="false">
    </div>
    <button class="btn-verify" type="button" id="validateSymbolBtn" style="flex-shrink:0">楠岃瘉甯佺</button>
    <button class="btn-apply" type="button" id="applySymbolBtn" style="flex-shrink:0" disabled>鍒囨崲</button>
  </div>
  <div class="field" style="max-width:260px;margin-top:8px">
    <label for="symbolConfirm">闃茶瑙︾‘璁わ細杈撳叆楠岃瘉鍚庣殑瀹屾暣浜ゆ槗瀵?/label>
    <input id="symbolConfirm" type="text" placeholder="渚嬪 BTCUSDT" autocomplete="off" spellcheck="false">
  </div>
  <div id="symbolResult" style="margin-top:8px;font-size:13px;color:#8b949e"></div>
  <div style="margin-top:8px;font-size:12px;color:#6e7681">楠岃瘉閫氳繃鍚庡彲鐑垏鎹㈣鎯呫€丱I銆佷笅鍗曚笌瀵硅处浜ゆ槗瀵癸紱宸叉湁鎸佷粨鏃朵細鎷掔粷鍒囨崲銆?/div>
</section>

<section class="card" style="margin-top:14px">
  <h2>API 瀵嗛挜绠＄悊</h2>
  <div class="grid" style="grid-template-columns:1fr 1fr;gap:8px;margin-bottom:10px">
    <div class="field" style="margin:0"><label for="apiKey">API Key</label><input id="apiKey" autocomplete="off" spellcheck="false"></div>
    <div class="field" style="margin:0"><label for="apiSecret">API Secret</label><input id="apiSecret" type="password" autocomplete="off"></div>
  </div>
  <div style="display:flex;gap:8px;flex-wrap:wrap;margin-bottom:8px">
    <button class="btn-verify" id="verifyKeyBtn">楠岃瘉浣欓鍜屾寔浠?/button>
    <button class="btn-apply" id="applyKeyBtn">楠岃瘉骞跺簲鐢?/button>
    <button class="btn-neutral" id="clearBalanceBtn">娓呴櫎</button>
  </div>
  <div style="display:flex;gap:8px;align-items:flex-end;flex-wrap:wrap;border-top:1px solid #29333d;padding-top:10px;margin-top:6px">
    <div class="field" style="flex:1;min-width:120px;margin:0"><label for="keyLabel">鏍囩锛岀敤浜庝繚瀛?/label><input id="keyLabel" type="text" placeholder="涓昏处鎴?></div>
    <button class="btn-neutral" id="saveKeyBtn" style="flex-shrink:0">淇濆瓨瀵嗛挜</button>
  </div>
  <div id="savedKeys" style="margin-top:10px"></div>
</section>

<div class="modal-bg" id="verifyModal">
  <div class="modal">
    <h3>璐︽埛楠岃瘉缁撴灉</h3>
    <div id="modalContent"></div>
    <div class="modal-actions">
      <button class="btn-apply" id="modalApplyBtn">搴旂敤姝ゅ瘑閽?/button>
      <button class="btn-neutral" id="modalCancelBtn">取消</button>
    </div>
  </div>
</div>


<section style="display:grid;grid-template-columns:repeat(auto-fit,minmax(260px,1fr));gap:12px;margin-top:14px">
  <div class="card">
        <div class="logbar"><h2 style="margin:0">交易日志</h2><span class="muted" id="tradeHint"></span><button class="btn-neutral" style="padding:4px 8px;font-size:12px" id="clearTradeBtn">清除</button></div>
    <div class="loglist" id="tradeLogs"></div>
  </div>
  <div class="card">
        <div class="logbar"><h2 style="margin:0">系统日志</h2><span class="muted" id="systemHint"></span><button class="btn-neutral" style="padding:4px 8px;font-size:12px" id="clearSystemBtn">清除</button></div>
    <div class="loglist" id="systemLogs"></div>
  </div>
  <div class="card">
        <div class="logbar"><h2 style="margin:0">拒绝日志</h2><span class="muted" id="rejectHint"></span><button class="btn-neutral" style="padding:4px 8px;font-size:12px" id="clearRejectBtn">清除</button></div>
    <div class="loglist" id="rejectLogs"></div>
  </div>
</section>

<script>
const groups=[
{title:'姝㈢泩姝㈡崯',items:[
['take_profit_pct','range','姝㈢泩姣斾緥',0.001,0.05,0.001,v=>(v*100).toFixed(2)+'%'],
['stop_loss_pct','range','姝㈡崯姣斾緥',0.001,0.02,0.001,v=>(v*100).toFixed(2)+'%'],
['cooldown_after_exit_sec','number','骞充粨鍐峰嵈绉?,0,300,1,v=>v+'s'],
['max_holding_time_sec','number','鏈€闀挎寔浠撶',60,86400,60,v=>v+'s'],
['min_holding_time_sec','number','鏈€鐭寔浠撶',0,3600,5,v=>v+'s']]},
{title:'浜ゆ槗鎵ц',items:[
['margin_usdt','number','姣忕瑪淇濊瘉閲?USDT',1,100000,1,v=>String(v)+' U'],
['leverage','number','鏉犳潌鍊嶆暟',1,10,1,v=>v+'x'],
['use_maker_mode','checkbox','Maker 妯″紡',0,0,0,v=>v?'寮€':'鍏?],
['maker_offset_bps','range','Maker 鍋忕Щ bps',0,20,0.1,v=>v.toFixed(1)+' bps'],
['guard_deadline_ms','number','淇濇姢鍗曟埅姝?ms',100,180,1,v=>v+'ms'],
['depth_levels','number','鍚冨崟娣卞害妗ｄ綅',1,20,1,v=>v+'妗?],
['signal_based_exit','checkbox','淇″彿绐楀彛骞充粨妯″紡',0,0,0,v=>v?'寮€':'鍏?]]},
{title:'寮曟搸闃堝€?,items:[
['trend_enabled','checkbox','瓒嬪娍寮曟搸',0,0,0,v=>v?'寮€':'鍏?],
['trend_confidence','range','瓒嬪娍缃俊搴?,0.3,0.95,0.01,v=>v.toFixed(2)],
['oi_delta_threshold','range','OI Delta 闃堝€?,0.001,0.02,0.001,v=>(v*100).toFixed(2)+'%'],
['squeeze_enabled','checkbox','Squeeze 寮曟搸',0,0,0,v=>v?'寮€':'鍏?],
['squeeze_confidence','range','Squeeze 缃俊搴?,0.3,0.95,0.01,v=>v.toFixed(2)],
['basis_zscore_threshold','range','Basis ZScore',1,5,0.1,v=>v.toFixed(1)],
['transition_enabled','checkbox','Transition 寮曟搸',0,0,0,v=>v?'寮€':'鍏?],
['transition_confidence','range','Transition 缃俊搴?,0.3,0.95,0.01,v=>v.toFixed(2)],
['vol_compression_ratio','range','娉㈠姩鍘嬬缉姣斾緥',0.1,0.9,0.05,v=>v.toFixed(2)]]},
{title:'椋庢帶',items:[
['max_slippage_bps','range','鏈€澶ф粦鐐?bps',1,100,0.5,v=>v.toFixed(1)+' bps'],
['daily_loss_limit_pct','range','鏃ヤ簭鎹熼檺鍒?,0.001,0.2,0.001,v=>(v*100).toFixed(2)+'%'],
['consecutive_loss_limit','number','杩炵画浜忔崯娆℃暟',1,20,1,v=>v]]}
];
const fields=[];
const grid=document.getElementById('grid');
for(const g of groups){
  const card=document.createElement('div');card.className='card';
  const h=document.createElement('h2');h.textContent=g.title;card.appendChild(h);
  for(const it of g.items){
    fields.push(it);
    const[id,type,label,min,max,step]=it;
    const wrap=document.createElement('div');
    if(type==='checkbox'){
      wrap.className='check';
      wrap.innerHTML='<input id="'+id+'" type="checkbox"><label for="'+id+'">'+label+' <span id="v_'+id+'"></span></label>';
    }else{
      wrap.className='field';
      wrap.innerHTML='<label for="'+id+'">'+label+' <span id="v_'+id+'"></span></label><input id="'+id+'" type="'+type+'" min="'+min+'" max="'+max+'" step="'+step+'">';
    }
    card.appendChild(wrap);
  }
  grid.appendChild(card);
}
function fmt(it,val){return it[6](it[1]==='checkbox'?!!val:parseFloat(val))}
function refreshLabel(it){
  const el=document.getElementById(it[0]);
  document.getElementById('v_'+it[0]).textContent=fmt(it,it[1]==='checkbox'?el.checked:el.value);
}
function populate(cur){
  for(const it of fields){
    const el=document.getElementById(it[0]);
    if(cur[it[0]]===undefined)continue;
    if(it[1]==='checkbox')el.checked=!!cur[it[0]];
    else el.value=cur[it[0]];
    refreshLabel(it);
  }
}
function collect(){
  const o={};
  for(const it of fields){
    const el=document.getElementById(it[0]);
    o[it[0]]=it[1]==='checkbox'?el.checked:(it[0]==='lot_size'||it[0]==='margin_usdt'?String(el.value):Number(el.value));
  }
  o.leverage=parseInt(o.leverage);o.cooldown_after_exit_sec=parseInt(o.cooldown_after_exit_sec);
  o.max_holding_time_sec=parseInt(o.max_holding_time_sec);o.min_holding_time_sec=parseInt(o.min_holding_time_sec);
  o.guard_deadline_ms=parseInt(o.guard_deadline_ms);o.consecutive_loss_limit=parseInt(o.consecutive_loss_limit);
  o.depth_levels=parseInt(o.depth_levels);
  return o;
}
function msg(t,ok,el){
  const m=el||document.getElementById('msg');
  m.textContent=t;m.className='msg '+(ok?'ok':'err');
  setTimeout(()=>{m.className='msg'},4000);
}
for(const it of fields){document.getElementById(it[0]).addEventListener('input',()=>refreshLabel(it));}

document.getElementById('form').addEventListener('submit',async e=>{
  e.preventDefault();const p=collect();
  if(p.take_profit_pct/p.stop_loss_pct<1.5){msg('鐩堜簭姣斿繀椤?>= 1.5',false);return}
  const r=await fetch('/api/params',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(p)});
  if(r.ok){const d=await r.json();populate(d.current);msg('鍙傛暟宸蹭繚瀛?,true);}
  else msg('淇濆瓨澶辫触: '+await r.text(),false);
});
document.getElementById('testParamsBtn').onclick=async()=>{
  const p=Object.assign(collect(),{margin_usdt:'10',take_profit_pct:0.002,stop_loss_pct:0.001,
    cooldown_after_exit_sec:5,min_holding_time_sec:0,max_holding_time_sec:600,
    trend_enabled:true,trend_confidence:0.30,oi_delta_threshold:0.001,
    squeeze_enabled:true,squeeze_confidence:0.40,basis_zscore_threshold:1.0,
    transition_enabled:true,transition_confidence:0.30,vol_compression_ratio:0.90,max_slippage_bps:20});
  populate(p);
  const r=await fetch('/api/params',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(p)});
  if(r.ok){const d=await r.json();populate(d.current);msg('测试参数已保存',true);}
  else msg('测试参数失败: '+await r.text(),false);
};
document.getElementById('resetBtn').onclick=async()=>{
  if(!confirm('鎭㈠鍒板垵濮嬪寲鍙傛暟锛?))return;
  const r=await fetch('/api/params/reset',{method:'POST'});
  if(r.ok){const d=await r.json();populate(d.current);msg('宸叉仮澶嶅垵濮嬪寲鍊?,true);}
  else msg('鎭㈠澶辫触',false);
};
document.getElementById('initBtn').onclick=async()=>{
  if(!confirm('鎶婂綋鍓嶅弬鏁颁繚瀛樹负鏂扮殑鍒濆鍖栧€硷紵'))return;
  const r=await fetch('/api/params/initialize',{method:'POST'});
  if(r.ok)msg('褰撳墠鍙傛暟宸蹭繚瀛樹负鍒濆鍖栧€?,true);else msg('鍒濆鍖栧け璐?,false);
};
document.getElementById('startBtn').onclick=async()=>{
  if(!confirm('纭鍚姩绯荤粺鏈嶅姟锛?))return;
  const r=await fetch('/api/control/start',{method:'POST'});msg(r.ok?'宸插彂閫佸惎鍔ㄥ懡浠?:'鍚姩鍛戒护澶辫触',r.ok);
};
document.getElementById('restartBtn').onclick=async()=>{
  if(!confirm('纭閲嶅惎绯荤粺鏈嶅姟锛?))return;
  const r=await fetch('/api/control/restart',{method:'POST'});msg(r.ok?'宸插彂閫侀噸鍚懡浠?:'閲嶅惎鍛戒护澶辫触',r.ok);
};
document.getElementById('stopBtn').onclick=async()=>{
  if(!confirm('纭鍋滄绯荤粺鏈嶅姟锛?))return;
  const r=await fetch('/api/control/stop',{method:'POST'});msg(r.ok?'宸插彂閫佸仠姝㈠懡浠?:'鍋滄鍛戒护澶辫触',r.ok);
};

const tradeMsg=document.getElementById('tradeMsg');
document.getElementById('manualLongBtn').onclick=async()=>{
  if(!confirm('纭鎵嬪姩鍋氬锛?))return;
  const r=await fetch('/api/trade/open',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({direction:'long'})});
  msg(r.ok?'鍋氬鎸囦护宸插彂閫?:'鍋氬澶辫触: '+await r.text(),r.ok,tradeMsg);
};
document.getElementById('manualShortBtn').onclick=async()=>{
  if(!confirm('纭鎵嬪姩鍋氱┖锛?))return;
  const r=await fetch('/api/trade/open',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({direction:'short'})});
  msg(r.ok?'鍋氱┖鎸囦护宸插彂閫?:'鍋氱┖澶辫触: '+await r.text(),r.ok,tradeMsg);
};
document.getElementById('manualCloseBtn').onclick=async()=>{
  if(!confirm('纭鎵嬪姩骞充粨锛?))return;
  const r=await fetch('/api/trade/close',{method:'POST'});
  msg(r.ok?'骞充粨鎸囦护宸插彂閫?:'骞充粨澶辫触: '+await r.text(),r.ok,tradeMsg);
};

let verifiedSymbol='';
document.getElementById('validateSymbolBtn').onclick=async()=>{
  const sym=document.getElementById('symbolInput').value.trim();
  const el=document.getElementById('symbolResult');
  verifiedSymbol='';
  document.getElementById('applySymbolBtn').disabled=true;
  if(!sym){el.textContent='璇疯緭鍏ュ竵绉?;return;}
  el.style.color='#8b949e';el.textContent='姝ｅ湪楠岃瘉...';
  const r=await fetch('/api/symbol/validate',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({symbol:sym})});
  const d=await r.json();
  if(d.valid){
    verifiedSymbol=d.symbol;
    document.getElementById('symbolConfirm').value='';
    document.getElementById('applySymbolBtn').disabled=false;
    el.style.color='#56d364';
    el.textContent='楠岃瘉閫氳繃锛?+d.symbol+'銆傚闇€鍒囨崲锛岃鍦ㄧ‘璁ゆ鍐嶆杈撳叆瀹屾暣浜ゆ槗瀵广€?;
  }else{
    el.style.color='#ff7b72';
    el.textContent='涓嶅瓨鍦細'+d.symbol+' - '+d.error;
  }
};
document.getElementById('applySymbolBtn').onclick=async()=>{
  const el=document.getElementById('symbolResult');
  const confirmText=document.getElementById('symbolConfirm').value.trim().toUpperCase();
  if(!verifiedSymbol){el.style.color='#ff7b72';el.textContent='璇峰厛楠岃瘉甯佺';return;}
  if(confirmText!==verifiedSymbol){el.style.color='#ff7b72';el.textContent='闃茶瑙︾‘璁や笉鍖归厤锛岄渶瑕佽緭鍏?'+verifiedSymbol;return;}
  const r=await fetch('/api/symbol',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({symbol:verifiedSymbol,confirm:confirmText})});
  if(r.ok){
    const d=await r.json();
    el.style.color='#56d364';el.textContent='宸插垏鎹㈠埌 '+d.symbol;
    document.getElementById('applySymbolBtn').disabled=true;
    loadSymbol();
  }else{
    el.style.color='#ff7b72';el.textContent='鍒囨崲澶辫触: '+await r.text();
  }
};

let pendingVerifyData=null;

function keyPayload(){
  return{api_key:document.getElementById('apiKey').value.trim(),api_secret:document.getElementById('apiSecret').value.trim()};
}

function renderModal(data,apply){
  const c=document.getElementById('modalContent');
  let html='<div style="margin-bottom:10px"><strong style="color:#a9b7c4">账户余额</strong><div style="font-family:Consolas,monospace;font-size:13px;margin-top:6px;color:#d6dee6">';
  const bals=data.balances||[];
  if(!bals.length){html+='无余额数据';}
  else{
    bals.forEach(b=>{
      const bal=parseFloat(b.balance||0);
      if(bal>0)html+=b.asset+': <span style="color:#56d364">'+parseFloat(b.balance).toFixed(4)+'</span> 可用: '+parseFloat(b.available_balance).toFixed(4)+'<br>';
    });
  }
  html+='</div></div>';
  html+='<div><strong style="color:#a9b7c4">持仓状态</strong><div style="margin-top:6px">';
  const pos=data.positions||[];
  if(!pos.length){html+='<span style="color:#8b949e;font-size:13px">无持仓</span>';}
  else{
    pos.forEach(p=>{
      const amt=parseFloat(p.amount||0);
      if(Math.abs(amt)<0.000001)return;
      const cls=p.side==='LONG'?'pos-long':p.side==='SHORT'?'pos-short':'pos-flat';
      html+='<div style="display:flex;gap:10px;align-items:center;margin-bottom:6px;font-size:13px">';
      html+='<span class="pos-badge '+cls+'">'+p.side+'</span>';
      html+='<span style="color:#d6dee6">'+p.symbol+'</span>';
      html+='<span style="color:#79c0ff">数量: '+p.amount+'</span>';
      html+='<span style="color:#8b949e">入场: '+parseFloat(p.entry).toFixed(2)+'</span>';
      const pnl=parseFloat(p.pnl||0);
      html+='<span style="color:'+(pnl>=0?'#56d364':'#ff7b72')+'">PnL: '+(pnl>=0?'+':'')+pnl.toFixed(4)+'</span>';
      html+='</div>';
    });
  }
  html+='</div></div>';
  c.innerHTML=html;
  pendingVerifyData={api_key:document.getElementById('apiKey').value.trim(),api_secret:document.getElementById('apiSecret').value.trim()};
  document.getElementById('modalApplyBtn').style.display=apply?'none':'';
  document.getElementById('verifyModal').classList.add('open');
}

document.getElementById('modalCancelBtn').onclick=()=>document.getElementById('verifyModal').classList.remove('open');
document.getElementById('verifyModal').onclick=e=>{if(e.target===document.getElementById('verifyModal'))document.getElementById('verifyModal').classList.remove('open');};

document.getElementById('modalApplyBtn').onclick=async()=>{
  if(!pendingVerifyData)return;
  const r=await fetch('/api/credentials/apply',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(pendingVerifyData)});
  document.getElementById('verifyModal').classList.remove('open');
  if(r.ok)msg('密钥已应用',true);else msg('应用失败: '+await r.text(),false);
};

document.getElementById('verifyKeyBtn').onclick=async()=>{
  const p=keyPayload();if(!p.api_key||!p.api_secret){msg('请填写 API Key 和 Secret',false);return;}
  const r=await fetch('/api/credentials/verify',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(p)});
  if(r.ok){const d=await r.json();renderModal(d,false);}
  else msg('验证失败: '+await r.text(),false);
};

document.getElementById('applyKeyBtn').onclick=async()=>{
  const p=keyPayload();if(!p.api_key||!p.api_secret){msg('请填写 API Key 和 Secret',false);return;}
  if(!confirm('确认验证并应用这组密钥到当前运行中的交易客户端？'))return;
  const r=await fetch('/api/credentials/apply',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(p)});
  if(r.ok){const d=await r.json();renderModal(d,true);msg('密钥已应用',true);}
  else msg('应用失败: '+await r.text(),false);
};

document.getElementById('clearBalanceBtn').onclick=()=>{
  document.getElementById('apiKey').value='';
  document.getElementById('apiSecret').value='';
  pendingVerifyData=null;
};

document.getElementById('saveKeyBtn').onclick=async()=>{
  const p=keyPayload();const label=document.getElementById('keyLabel').value.trim();
  if(!label||!p.api_key||!p.api_secret){msg('请填写标签、Key 和 Secret',false);return;}
  const r=await fetch('/api/keys',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({label,api_key:p.api_key,api_secret:p.api_secret})});
  if(r.ok){msg('密钥已保存 '+label,true);loadKeys();}
  else msg('保存失败: '+await r.text(),false);
};

async function loadKeys(){
  const r=await fetch('/api/keys');if(!r.ok)return;
  const d=await r.json();const keys=d.keys||[];
  const box=document.getElementById('savedKeys');
  if(!keys.length){box.innerHTML='<div style="font-size:13px;color:#8b949e">暂无保存的密钥</div>';return;}
  box.innerHTML='<div style="border-top:1px solid #29333d;padding-top:8px;margin-top:4px">'+
    keys.map(k=>'<div class="key-row"><span class="key-label">'+k.label+'</span><span class="key-masked">'+k.api_key_masked+'</span><button class="btn-apply" style="padding:3px 8px;font-size:12px" onclick="activateKey(\''+k.label+'\')">激活</button><button class="btn-neutral" style="padding:3px 8px;font-size:12px" onclick="deleteKey(\''+k.label+'\')">删除</button></div>').join('')+
    '</div>';
}

async function activateKey(label){
  if(!confirm('切换到密钥 '+label+'？'))return;
  const r=await fetch('/api/keys/activate',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({label})});
  msg(r.ok?'已切换到: '+label:'切换失败: '+await r.text(),r.ok);
}
async function deleteKey(label){
  if(!confirm('删除密钥 '+label+'？'))return;
  const r=await fetch('/api/keys/delete',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({label})});
  if(r.ok){msg('已删除 '+label,true);loadKeys();}
  else msg('删除失败',false);
}

function formatLog(e){
  const f=e.fields||{};
  return[e.message,f.engine&&('引擎:'+f.engine),f.dir&&('方向:'+f.dir),
    f.qty&&('数量:'+f.qty),f.entry&&('开仓:'+f.entry),f.exit&&('平仓:'+f.exit),
    f.pnl_pct!==undefined&&('PnL:'+(Number(f.pnl_pct)*100).toFixed(4)+'%'),
    f.reason&&('原因:'+f.reason),f.error&&('错误:'+f.error),
    f.est_slip_bps!==undefined&&('滑点:'+Number(f.est_slip_bps).toFixed(2)+'bps')
  ].filter(Boolean).join(' | ');
}
function renderLogBox(id,entries,empty){
  const box=document.getElementById(id);box.innerHTML='';
  if(!entries.length){box.innerHTML='<div class="row"><div class="details">'+empty+'</div></div>';return;}
  for(const e of entries.slice().reverse()){
    const row=document.createElement('div');row.className='row';
    row.innerHTML='<div class="time">'+new Date(e.time).toLocaleString()+'</div><div class="type '+e.type+'">'+e.type+'</div><div class="details">'+formatLog(e)+'</div>';
    box.appendChild(row);
  }
}
async function loadLogs(){
  const now='刷新于 '+new Date().toLocaleTimeString();
  try{
    const r=await fetch('/api/logs');const d=await r.json();
    renderLogBox('tradeLogs',d.trades||[],'暂无开仓/平仓记录');
    renderLogBox('systemLogs',d.system||[],'暂无系统日志');
    renderLogBox('rejectLogs',d.rejects||[],'暂无拒绝记录');
    document.getElementById('tradeHint').textContent=now;
    document.getElementById('systemHint').textContent=now;
    document.getElementById('rejectHint').textContent=now;
  }catch(e){['tradeHint','systemHint','rejectHint'].forEach(id=>document.getElementById(id).textContent='日志加载失败');}
}
async function clearLogs(cat){
  const r=await fetch('/api/logs?category='+cat,{method:'DELETE'});
  if(r.ok)loadLogs();
}
document.getElementById('clearTradeBtn').onclick=()=>clearLogs('trade');
document.getElementById('clearSystemBtn').onclick=()=>clearLogs('system');
document.getElementById('clearRejectBtn').onclick=()=>clearLogs('reject');

async function loadSymbol(){
  try{const r=await fetch('/api/symbol');const d=await r.json();
    document.getElementById('symbolBadge').textContent='交易对 '+(d.symbol||'--');
  }catch(e){}
}
(async()=>{
  try{const r=await fetch('/api/params');const d=await r.json();populate(d.current);}
  catch(e){msg('加载参数失败: '+e,false);}
  setInterval(loadLogs,3000);setInterval(loadSymbol,10000);
})();
</script>
</body>
</html>`
