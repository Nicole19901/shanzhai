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

// MarketProvider 返回最新市场指标快照（供前端可视化）
type MarketProvider func() map[string]interface{}

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
	marketProvider MarketProvider

	watchlistAdd    func(ctx context.Context, sym string) error
	watchlistRemove func(sym string)
	watchlistGet    func() []string

	// modeChange is called when the user changes PositionMode or MarginMode in the UI;
	// implementer is expected to push the new mode to Binance for all watched symbols.
	// Empty strings mean "no change for this field".
	modeChange func(ctx context.Context, newPositionMode, newMarginMode string)
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

func (s *Server) SetMarketProvider(fn MarketProvider) { s.marketProvider = fn }

// SetWatchlistHandlers registers the watchlist add/remove/get functions.
func (s *Server) SetWatchlistHandlers(
	add func(ctx context.Context, sym string) error,
	remove func(sym string),
	get func() []string,
) {
	s.watchlistAdd = add
	s.watchlistRemove = remove
	s.watchlistGet = get
}

// SetModeChangeHandler registers a callback fired when PositionMode or MarginMode
// is changed via /api/params. Receives only fields that actually changed (others empty).
func (s *Server) SetModeChangeHandler(fn func(ctx context.Context, newPositionMode, newMarginMode string)) {
	s.modeChange = fn
}

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
	mux.HandleFunc("/api/market", s.auth(s.handleMarket))
	mux.HandleFunc("/api/watchlist", s.auth(s.handleWatchlist))

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
			s.events.AddOperation("KEY_APPLIED", "API Key 已应用，时钟已同步",
				map[string]interface{}{"clock_offset_ms": s.rest.TimeOffset()})
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
		s.events.AddOperation("KEY_SWITCHED", fmt.Sprintf("已切换到密钥: %s", req.Label),
			map[string]interface{}{"label": req.Label})
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
		// 服务端再次验证交易对是否真实存在于 Binance Futures（防止前端绕过）
		{
			vctx, vcancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer vcancel()
			ok, err := s.rest.ValidateSymbol(vctx, sym)
			if err != nil || !ok {
				errMsg := fmt.Sprintf("交易对 %s 在 Binance Futures 不存在", sym)
				if err != nil {
					errMsg = err.Error()
				}
				http.Error(w, errMsg, http.StatusBadRequest)
				return
			}
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
			s.events.AddOperation("SYMBOL_SWITCHED",
				fmt.Sprintf("交易对已切换到: %s，WS 连接建立中", sym),
				map[string]interface{}{"symbol": sym})
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
	// 至少要有币名部分（USDT=4位后缀 + 至少1位币名 = 5位），真正合法性由 ValidateSymbol 调 Binance 验证
	if len(sym) < 5 {
		return ""
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

func (s *Server) handleMarket(w http.ResponseWriter, r *http.Request) {
	if s.marketProvider != nil {
		writeJSON(w, s.marketProvider())
		return
	}
	writeJSON(w, map[string]interface{}{"symbols": map[string]interface{}{}, "active": s.getSymbol()})
}

func (s *Server) handleWatchlist(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		var syms []string
		if s.watchlistGet != nil {
			syms = s.watchlistGet()
		}
		writeJSON(w, map[string]interface{}{"symbols": syms})
	case http.MethodPost:
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
			http.Error(w, errMsg, http.StatusBadRequest)
			return
		}
		if s.watchlistAdd != nil {
			if err := s.watchlistAdd(r.Context(), sym); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if s.events != nil {
			s.events.AddOperation("WATCHLIST_ADD",
				fmt.Sprintf("监控列表: 已加入 %s，WS 数据连接建立中", sym),
				map[string]interface{}{"symbol": sym})
		}
		writeJSON(w, map[string]interface{}{"status": "added", "symbol": sym})
	case http.MethodDelete:
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
		if s.watchlistRemove != nil {
			s.watchlistRemove(sym)
		}
		if s.events != nil {
			s.events.AddOperation("WATCHLIST_REMOVE",
				fmt.Sprintf("监控列表: 已移除 %s，WS 连接已断开", sym),
				map[string]interface{}{"symbol": sym})
		}
		writeJSON(w, map[string]interface{}{"status": "removed", "symbol": sym})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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
		old := s.params.Get()
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
		// 检测持仓模式 / 保证金模式变化，并通过回调推到 Binance（覆盖所有 watchlist 中的 symbol）
		if s.modeChange != nil && s.rest.HasCredentials() {
			var newPos, newMargin string
			if snap.PositionMode != "" && snap.PositionMode != old.PositionMode {
				newPos = snap.PositionMode
			}
			if snap.MarginMode != "" && snap.MarginMode != old.MarginMode {
				newMargin = snap.MarginMode
			}
			if newPos != "" || newMargin != "" {
				modeCtx, modeCancel := context.WithTimeout(context.Background(), 15*time.Second)
				go func() {
					defer modeCancel()
					s.modeChange(modeCtx, newPos, newMargin)
				}()
			}
		}
		// 生成操作日志（仅记录变更字段）
		if s.events != nil {
			diffs := paramsDiff(old, snap)
			if len(diffs) > 0 {
				s.events.AddOperation("PARAMS_CHANGED",
					fmt.Sprintf("参数已修改: %s", strings.Join(diffs, "，")),
					map[string]interface{}{"changes": diffs})
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
		case "trade", "system", "reject", "operation", "all":
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
		"entries":   s.events.Recent(),
		"trades":    s.events.RecentByCategory("trade"),
		"system":    s.events.RecentByCategory("system"),
		"rejects":   s.events.RecentByCategory("reject"),
		"operation": s.events.RecentByCategory("operation"),
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

// paramsDiff returns human-readable descriptions of changed fields between old and new snapshot.
func paramsDiff(old, next LiveParamsSnapshot) []string {
	var d []string
	b2s := func(v bool) string {
		if v {
			return "开"
		}
		return "关"
	}
	db := func(p *bool) bool { return p != nil && *p }
	boolPtrChanged := func(a, b *bool) bool {
		if a == nil && b == nil {
			return false
		}
		if a == nil || b == nil {
			return true
		}
		return *a != *b
	}
	if old.Leverage != next.Leverage {
		d = append(d, fmt.Sprintf("杠杆 %dx→%dx", old.Leverage, next.Leverage))
	}
	if old.MarginUSDT != next.MarginUSDT {
		d = append(d, fmt.Sprintf("保证金 %s→%s U", old.MarginUSDT, next.MarginUSDT))
	}
	if old.LongTPPct != next.LongTPPct {
		d = append(d, fmt.Sprintf("多止盈 %.2f%%→%.2f%%", old.LongTPPct*100, next.LongTPPct*100))
	}
	if old.LongSLPct != next.LongSLPct {
		d = append(d, fmt.Sprintf("多止损 %.2f%%→%.2f%%", old.LongSLPct*100, next.LongSLPct*100))
	}
	if old.ShortTPPct != next.ShortTPPct {
		d = append(d, fmt.Sprintf("空止盈 %.2f%%→%.2f%%", old.ShortTPPct*100, next.ShortTPPct*100))
	}
	if old.ShortSLPct != next.ShortSLPct {
		d = append(d, fmt.Sprintf("空止损 %.2f%%→%.2f%%", old.ShortSLPct*100, next.ShortSLPct*100))
	}
	if boolPtrChanged(old.LongEnabled, next.LongEnabled) {
		d = append(d, fmt.Sprintf("做多 %s→%s", b2s(db(old.LongEnabled)), b2s(db(next.LongEnabled))))
	}
	if boolPtrChanged(old.ShortEnabled, next.ShortEnabled) {
		d = append(d, fmt.Sprintf("做空 %s→%s", b2s(db(old.ShortEnabled)), b2s(db(next.ShortEnabled))))
	}
	if boolPtrChanged(old.TrendEnabled, next.TrendEnabled) {
		d = append(d, fmt.Sprintf("趋势引擎 %s→%s", b2s(db(old.TrendEnabled)), b2s(db(next.TrendEnabled))))
	}
	if boolPtrChanged(old.SqueezeEnabled, next.SqueezeEnabled) {
		d = append(d, fmt.Sprintf("Squeeze引擎 %s→%s", b2s(db(old.SqueezeEnabled)), b2s(db(next.SqueezeEnabled))))
	}
	if boolPtrChanged(old.TransitionEnabled, next.TransitionEnabled) {
		d = append(d, fmt.Sprintf("Transition引擎 %s→%s", b2s(db(old.TransitionEnabled)), b2s(db(next.TransitionEnabled))))
	}
	if old.SignalBasedExit != next.SignalBasedExit {
		d = append(d, fmt.Sprintf("信号平仓 %s→%s", b2s(old.SignalBasedExit), b2s(next.SignalBasedExit)))
	}
	if old.TrendLongConfidence != next.TrendLongConfidence {
		d = append(d, fmt.Sprintf("趋势多置信度 %.2f→%.2f", old.TrendLongConfidence, next.TrendLongConfidence))
	}
	if old.TrendShortConfidence != next.TrendShortConfidence {
		d = append(d, fmt.Sprintf("趋势空置信度 %.2f→%.2f", old.TrendShortConfidence, next.TrendShortConfidence))
	}
	if old.MaxSlippageBps != next.MaxSlippageBps {
		d = append(d, fmt.Sprintf("最大滑点 %.1f→%.1fbps", old.MaxSlippageBps, next.MaxSlippageBps))
	}
	if boolPtrChanged(old.LiquidationEnabled, next.LiquidationEnabled) {
		d = append(d, fmt.Sprintf("踩踏引擎 %s→%s", b2s(db(old.LiquidationEnabled)), b2s(db(next.LiquidationEnabled))))
	}
	if old.LiquidationLongConf != next.LiquidationLongConf {
		d = append(d, fmt.Sprintf("踩踏多置信度 %.2f→%.2f", old.LiquidationLongConf, next.LiquidationLongConf))
	}
	if old.LiquidationShortConf != next.LiquidationShortConf {
		d = append(d, fmt.Sprintf("踩踏空置信度 %.2f→%.2f", old.LiquidationShortConf, next.LiquidationShortConf))
	}
	if old.OILiquidationThreshold != next.OILiquidationThreshold {
		d = append(d, fmt.Sprintf("踩踏OI阈值 %.3f→%.3f", old.OILiquidationThreshold, next.OILiquidationThreshold))
	}
	if old.MarginMode != next.MarginMode {
		d = append(d, fmt.Sprintf("保证金模式 %s→%s", old.MarginMode, next.MarginMode))
	}
	if old.PositionMode != next.PositionMode {
		d = append(d, fmt.Sprintf("持仓模式 %s→%s", old.PositionMode, next.PositionMode))
	}
	return d
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
	for _, v := range []float64{
		s.TrendLongConfidence, s.TrendShortConfidence,
		s.SqueezeLongConfidence, s.SqueezeShortConfidence,
		s.TransitionLongConfidence, s.TransitionShortConfidence,
		s.LiquidationLongConf, s.LiquidationShortConf,
	} {
		if v < 0 || v > 1 {
			return fmt.Errorf("confidence values must be in [0, 1]")
		}
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
	if s.QuantityPrecision < 0 || s.QuantityPrecision > 8 {
		return fmt.Errorf("quantity_precision must be in [0, 8]")
	}
	if s.MarginMode != "" && s.MarginMode != "ISOLATED" && s.MarginMode != "CROSS" {
		return fmt.Errorf("margin_mode must be ISOLATED or CROSS")
	}
	if s.PositionMode != "" && s.PositionMode != "ONE_WAY" && s.PositionMode != "HEDGE" {
		return fmt.Errorf("position_mode must be ONE_WAY or HEDGE")
	}
	if s.OILiquidationThreshold != 0 && (s.OILiquidationThreshold < 0.0001 || s.OILiquidationThreshold > 0.05) {
		return fmt.Errorf("oi_liquidation_threshold must be in [0.0001, 0.05]")
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
.dir-grid{display:grid;grid-template-columns:1fr 1fr;gap:8px;margin-bottom:10px}
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
.dir-indicators{margin-top:8px;border-top:1px solid rgba(255,255,255,.07);padding-top:6px}
.di-title{font-size:10px;color:#8b949e;text-transform:uppercase;letter-spacing:.4px;margin-bottom:4px}
.di-row{display:flex;justify-content:space-between;align-items:center;margin-bottom:3px}
.di-label{font-size:10px;color:#8b949e;min-width:52px}
/* 市场指标面板 */
.mkt{background:#0d1116;border:1px solid #29333d;border-radius:8px;padding:10px;margin-bottom:10px}
.mkt-title{font-size:11px;color:#79c0ff;text-transform:uppercase;letter-spacing:.5px;margin-bottom:8px;display:flex;align-items:center;gap:8px}
.mkt-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:8px}
.mkt-group{background:#13191f;border-radius:6px;padding:8px}
.mkt-group-title{font-size:10px;color:#8b949e;margin-bottom:6px;text-transform:uppercase;letter-spacing:.4px}
.mkt-row{display:flex;justify-content:space-between;align-items:center;margin-bottom:3px}
.mkt-label{font-size:10px;color:#8b949e}
.mkt-val{font-size:11px;font-family:Consolas,monospace;font-weight:600}
.mkt-val.pos{color:#56d364}.mkt-val.neg{color:#ff7b72}.mkt-val.neu{color:#79c0ff}
/* 迷你方向条 */
.bar-wrap{display:flex;align-items:center;gap:4px;flex:1;margin-left:8px}
.bar-track{flex:1;height:5px;background:#202a33;border-radius:3px;position:relative;overflow:hidden}
.bar-fill{height:100%;border-radius:3px;position:absolute;transition:width .3s,left .3s}
.bar-fill.pos{background:#238636;right:50%}.bar-fill.neg{background:#da3633;left:50%}
/* 引擎信号卡 */
.eng-grid{display:grid;grid-template-columns:repeat(3,1fr);gap:6px;margin-top:0}
.eng-card{background:#13191f;border-radius:6px;padding:7px;border:1px solid #202a33}
.eng-name{font-size:10px;color:#8b949e;margin-bottom:4px}
.eng-dir{font-size:12px;font-weight:700;margin-bottom:3px}
.eng-conf-track{height:5px;background:#202a33;border-radius:3px;overflow:hidden;position:relative;margin-bottom:2px}
.eng-conf-fill{height:100%;border-radius:3px;background:#0969da;transition:width .3s}
.eng-conf-thresh{position:absolute;top:0;width:2px;height:100%;background:#e3b341}
.eng-conf-fill.fired{background:#238636}.eng-conf-fill.fired-short{background:#da3633}
.eng-label{font-size:10px;color:#8b949e}
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
<div class="top"><h1>量化交易控制台</h1><div style="display:flex;gap:8px;flex-wrap:wrap"><span class="badge symbol" id="symbolBadge">交易对 --</span><span class="badge" id="wsMsgBadge" style="background:#0d1116;color:#8b949e;border-color:#3b434c">WS: --</span><span class="badge" id="posBadge">持仓: --</span></div></div>

<!-- 币种 Tab 栏 -->
<div id="mktTabBar" style="display:flex;gap:4px;flex-wrap:wrap;margin-bottom:6px"></div>
<!-- 市场指标可视化面板 -->
<div class="mkt" id="mktPanel">
<div class="mkt-title">📊 实时市场指标 <span id="mktAge" style="color:#8b949e;font-size:10px;font-weight:400"></span><span id="mktNotReady" style="color:#8b949e;font-size:10px">引擎启动后显示</span></div>
<div class="mkt-grid" id="mktGrid" style="display:none">
  <!-- 价格与资金费率 -->
  <div class="mkt-group">
    <div class="mkt-group-title">价格 & 资金费率</div>
    <div class="mkt-row"><span class="mkt-label">Mark 价格</span><span class="mkt-val neu" id="mv_mark">--</span></div>
    <div class="mkt-row"><span class="mkt-label">Index 价格</span><span class="mkt-val neu" id="mv_index">--</span></div>
    <div class="mkt-row"><span class="mkt-label">基差 bps</span><span class="mkt-val" id="mv_basis_bps">--</span></div>
    <div class="mkt-row"><span class="mkt-label">基差 ZScore</span><span class="mkt-val" id="mv_basis_z">--</span></div>
    <div class="mkt-row"><span class="mkt-label">资金费率</span><span class="mkt-val" id="mv_funding">--</span></div>
    <div class="mkt-row"><span class="mkt-label">下次资金费</span><span class="mkt-val neu" id="mv_next_fund">--</span></div>
    <div class="mkt-row"><span class="mkt-label">1m 动量</span><span class="mkt-val" id="mv_mom">--</span></div>
  </div>
  <!-- RCVD 成交量方向 -->
  <div class="mkt-group">
    <div class="mkt-group-title">RCVD 成交方向（买卖压力）</div>
    <div class="mkt-row"><span class="mkt-label">5s</span><div class="bar-wrap"><div class="bar-track"><div class="bar-fill" id="rb_5s"></div></div><span class="mkt-val" id="rv_5s" style="min-width:60px;text-align:right">--</span></div></div>
    <div class="mkt-row"><span class="mkt-label">30s</span><div class="bar-wrap"><div class="bar-track"><div class="bar-fill" id="rb_30s"></div></div><span class="mkt-val" id="rv_30s" style="min-width:60px;text-align:right">--</span></div></div>
    <div class="mkt-row"><span class="mkt-label">5m</span><div class="bar-wrap"><div class="bar-track"><div class="bar-fill" id="rb_5m"></div></div><span class="mkt-val" id="rv_5m" style="min-width:60px;text-align:right">--</span></div></div>
  </div>
  <!-- OI 持仓量变化 -->
  <div class="mkt-group">
    <div class="mkt-group-title">OI 持仓量变化</div>
    <div class="mkt-row"><span class="mkt-label">Delta 5s</span><div class="bar-wrap"><div class="bar-track"><div class="bar-fill" id="ob_5s"></div></div><span class="mkt-val" id="ov_5s" style="min-width:60px;text-align:right">--</span></div></div>
    <div class="mkt-row"><span class="mkt-label">Delta 30s</span><div class="bar-wrap"><div class="bar-track"><div class="bar-fill" id="ob_30s"></div></div><span class="mkt-val" id="ov_30s" style="min-width:60px;text-align:right">--</span></div></div>
    <div class="mkt-row"><span class="mkt-label">Delta 5m</span><div class="bar-wrap"><div class="bar-track"><div class="bar-fill" id="ob_5m"></div></div><span class="mkt-val" id="ov_5m" style="min-width:60px;text-align:right">--</span></div></div>
    <div class="mkt-row"><span class="mkt-label">速度</span><span class="mkt-val" id="ov_vel">--</span></div>
    <div class="mkt-row"><span class="mkt-label">加速度</span><span class="mkt-val" id="ov_acc">--</span></div>
  </div>
  <!-- 波动率 & 订单簿 -->
  <div class="mkt-group">
    <div class="mkt-group-title">波动率 & 订单簿</div>
    <div class="mkt-row"><span class="mkt-label">RealVol 1m</span><span class="mkt-val neu" id="vv_1m">--</span></div>
    <div class="mkt-row"><span class="mkt-label">RealVol 5m</span><span class="mkt-val neu" id="vv_5m">--</span></div>
    <div class="mkt-row"><span class="mkt-label">Vol基准 1h</span><span class="mkt-val neu" id="vv_1h">--</span></div>
    <div class="mkt-row"><span class="mkt-label">压缩比 5m/1h</span><span class="mkt-val" id="vv_comp">--</span></div>
    <div class="mkt-row"><span class="mkt-label">挂单存活衰减</span><span class="mkt-val" id="vv_decay">--</span></div>
    <div class="mkt-row"><span class="mkt-label">价差扩张率</span><span class="mkt-val" id="vv_spread">--</span></div>
  </div>
</div>
</div>

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

<!-- ▲ 做多独立控制面板 -->
<div class="dir-card long-card" id="longCard" style="margin-bottom:8px">
  <div class="dir-head">
    <span class="dir-title">▲ 做多 (Long)</span>
    <span class="dir-status" id="longStatus"></span>
    <button class="dir-toggle" id="longToggle" onclick="toggleDir('long')"></button>
  </div>
  <div style="display:flex;gap:12px;flex-wrap:wrap;align-items:flex-end;margin-top:6px">
    <div class="dir-params" style="flex:0 0 auto">
      <div class="field"><label for="long_tp_pct">止盈% <span id="v_long_tp_pct"></span></label><input id="long_tp_pct" type="range" min="0.001" max="0.05" step="0.001"></div>
      <div class="field"><label for="long_sl_pct">止损% <span id="v_long_sl_pct"></span></label><input id="long_sl_pct" type="range" min="0.001" max="0.02" step="0.001"></div>
    </div>
    <div style="flex:1;min-width:240px;display:grid;grid-template-columns:repeat(3,1fr);gap:6px;border-left:1px solid rgba(255,255,255,.08);padding-left:12px">
      <div class="field" style="margin:0"><label for="trend_long_confidence" style="font-size:10px">趋势置信度 <span id="v_trend_long_confidence"></span></label><input id="trend_long_confidence" type="range" min="0.3" max="0.95" step="0.01"></div>
      <div class="field" style="margin:0"><label for="squeeze_long_confidence" style="font-size:10px">Squeeze 置信度 <span id="v_squeeze_long_confidence"></span></label><input id="squeeze_long_confidence" type="range" min="0.3" max="0.95" step="0.01"></div>
      <div class="field" style="margin:0"><label for="transition_long_confidence" style="font-size:10px">Transition 置信度 <span id="v_transition_long_confidence"></span></label><input id="transition_long_confidence" type="range" min="0.3" max="0.95" step="0.01"></div>
    </div>
  </div>
</div>
<!-- ▼ 做空独立控制面板 -->
<div class="dir-card short-card" id="shortCard" style="margin-bottom:10px">
  <div class="dir-head">
    <span class="dir-title">▼ 做空 (Short)</span>
    <span class="dir-status" id="shortStatus"></span>
    <button class="dir-toggle" id="shortToggle" onclick="toggleDir('short')"></button>
  </div>
  <div style="display:flex;gap:12px;flex-wrap:wrap;align-items:flex-end;margin-top:6px">
    <div class="dir-params" style="flex:0 0 auto">
      <div class="field"><label for="short_tp_pct">止盈% <span id="v_short_tp_pct"></span></label><input id="short_tp_pct" type="range" min="0.001" max="0.05" step="0.001"></div>
      <div class="field"><label for="short_sl_pct">止损% <span id="v_short_sl_pct"></span></label><input id="short_sl_pct" type="range" min="0.001" max="0.02" step="0.001"></div>
    </div>
    <div style="flex:1;min-width:240px;display:grid;grid-template-columns:repeat(3,1fr);gap:6px;border-left:1px solid rgba(255,255,255,.08);padding-left:12px">
      <div class="field" style="margin:0"><label for="trend_short_confidence" style="font-size:10px">趋势置信度 <span id="v_trend_short_confidence"></span></label><input id="trend_short_confidence" type="range" min="0.3" max="0.95" step="0.01"></div>
      <div class="field" style="margin:0"><label for="squeeze_short_confidence" style="font-size:10px">Squeeze 置信度 <span id="v_squeeze_short_confidence"></span></label><input id="squeeze_short_confidence" type="range" min="0.3" max="0.95" step="0.01"></div>
      <div class="field" style="margin:0"><label for="transition_short_confidence" style="font-size:10px">Transition 置信度 <span id="v_transition_short_confidence"></span></label><input id="transition_short_confidence" type="range" min="0.3" max="0.95" step="0.01"></div>
    </div>
  </div>
</div>

<!-- 参数表单 + 引擎信号并排 -->
<div style="display:flex;gap:12px;align-items:flex-start">
<form id="form" style="flex:1;min-width:0"><div id="grid" style="display:grid;grid-template-columns:1fr 1fr;gap:8px;align-items:start"></div><div class="actions"><button class="btn-save" type="submit">保存参数</button><button class="btn-restart" type="button" id="testParamsBtn">测试参数</button><button class="btn-init" type="button" id="initBtn">初始化</button><button class="btn-reset" type="button" id="resetBtn">恢复默认</button><button class="btn-start" type="button" id="startBtn">启动服务</button><button class="btn-restart" type="button" id="restartBtn">重启服务</button><button class="btn-stop" type="button" id="stopBtn">停止服务</button></div><div id="msg" class="msg"></div></form>
<!-- 引擎信号（参数调节右侧） -->
<div id="engSection" style="width:270px;flex-shrink:0"><div class="card" style="margin-bottom:0;padding:10px"><h2>引擎信号</h2><div class="eng-grid" style="grid-template-columns:1fr" id="engGrid"></div>
<div style="margin-top:10px;border-top:1px solid #29333d;padding-top:8px">
<div class="di-title" style="color:#56d364;margin-bottom:5px">▲ 多头信号指标</div>
<div class="di-row"><span class="di-label">买压 5s</span><div class="bar-wrap"><div class="bar-track"><div class="bar-fill" id="dl_rcvd_f"></div></div><span class="mkt-val" id="dl_rcvd_v" style="min-width:54px;text-align:right;font-size:10px">--</span></div></div>
<div class="di-row"><span class="di-label">OI Δ 5s</span><div class="bar-wrap"><div class="bar-track"><div class="bar-fill" id="dl_oi_f"></div></div><span class="mkt-val" id="dl_oi_v" style="min-width:54px;text-align:right;font-size:10px">--</span></div></div>
<div class="di-row"><span class="di-label">Vol 1m</span><span class="mkt-val neu" id="dl_vol" style="font-size:10px">--</span></div>
<div class="di-row"><span class="di-label">资金费率</span><span class="mkt-val" id="dl_fund" style="font-size:10px">--</span></div>
</div>
<div style="margin-top:8px;border-top:1px solid #29333d;padding-top:8px">
<div class="di-title" style="color:#ff7b72;margin-bottom:5px">▼ 空头信号指标</div>
<div class="di-row"><span class="di-label">卖压 5s</span><div class="bar-wrap"><div class="bar-track"><div class="bar-fill" id="ds_rcvd_f"></div></div><span class="mkt-val" id="ds_rcvd_v" style="min-width:54px;text-align:right;font-size:10px">--</span></div></div>
<div class="di-row"><span class="di-label">OI Δ 5s</span><div class="bar-wrap"><div class="bar-track"><div class="bar-fill" id="ds_oi_f"></div></div><span class="mkt-val" id="ds_oi_v" style="min-width:54px;text-align:right;font-size:10px">--</span></div></div>
<div class="di-row"><span class="di-label">基差 ZScore</span><span class="mkt-val" id="ds_basisz" style="font-size:10px">--</span></div>
<div class="di-row"><span class="di-label">价差 bps</span><span class="mkt-val neu" id="ds_spread" style="font-size:10px">--</span></div>
</div>
</div></div>
</div>

<div class="grid2" style="margin-top:10px">
<section class="card"><h2>交易对管理（最多5个，加入即开始交易）</h2><div style="display:flex;gap:6px;margin-bottom:6px"><input id="watchlistInput" type="text" placeholder="如 ETH 或 LABUSDT" style="flex:1" autocomplete="off" spellcheck="false"><button class="btn-apply" type="button" onclick="addToWatchlist()">加入</button></div><div id="watchlistResult" class="muted" style="margin-bottom:6px"></div><div id="watchlistItems"></div><div class="muted" style="margin-top:4px;font-size:10px">加入后自动验证、建立 WS 连接并开始交易；有持仓时移除仍会断开引擎（需手动平仓）</div>
</section>
<section class="card"><h2>API 密钥管理</h2><div class="grid2" style="gap:6px;margin-bottom:8px"><div class="field"><label for="apiKey">API Key</label><input id="apiKey" autocomplete="off" spellcheck="false"></div><div class="field"><label for="apiSecret">API Secret</label><input id="apiSecret" type="password" autocomplete="off"></div></div><div class="actions"><button class="btn-verify" id="verifyKeyBtn">验证余额和持仓</button><button class="btn-apply" id="applyKeyBtn">验证并应用</button><button class="btn-neutral" id="clearBalanceBtn">清除</button></div><div style="display:flex;gap:6px;align-items:flex-end;flex-wrap:wrap;border-top:1px solid #29333d;padding-top:8px;margin-top:8px"><div class="field" style="flex:1;min-width:100px;margin:0"><label for="keyLabel">标签</label><input id="keyLabel" type="text" placeholder="主账户"></div><button class="btn-neutral" id="saveKeyBtn">保存</button></div><div id="savedKeys" style="margin-top:8px"></div></section>
</div>

<div class="modal-bg" id="verifyModal"><div class="modal"><h3>账户验证结果</h3><div id="modalContent"></div><div class="modal-actions"><button class="btn-apply" id="modalApplyBtn">应用此密钥</button><button class="btn-neutral" id="modalCancelBtn">取消</button></div></div></div>
<section style="display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:10px;margin-top:10px"><div class="card"><div class="logbar"><h2 style="margin:0">操作日志</h2><span class="muted" id="opHint"></span><button class="btn-neutral" style="padding:3px 7px;font-size:11px" id="clearOpBtn">清除</button></div><div class="loglist" id="opLogs"></div></div><div class="card"><div class="logbar"><h2 style="margin:0">交易日志</h2><span class="muted" id="tradeHint"></span><button class="btn-neutral" style="padding:3px 7px;font-size:11px" id="clearTradeBtn">清除</button></div><div class="loglist" id="tradeLogs"></div></div><div class="card"><div class="logbar"><h2 style="margin:0">系统日志</h2><span class="muted" id="systemHint"></span><button class="btn-neutral" style="padding:3px 7px;font-size:11px" id="clearSystemBtn">清除</button></div><div class="loglist" id="systemLogs"></div></div><div class="card"><div class="logbar"><h2 style="margin:0">拒绝日志</h2><span class="muted" id="rejectHint"></span><button class="btn-neutral" style="padding:3px 7px;font-size:11px" id="clearRejectBtn">清除</button></div><div class="loglist" id="rejectLogs"></div></div></section>

<script>
const groups=[
{title:'持仓时间 & 冷却',items:[['cooldown_after_exit_sec','number','冷却秒',0,300,1,v=>v+'s'],['max_holding_time_sec','number','最长持仓秒',60,86400,60,v=>v+'s'],['min_holding_time_sec','number','最短持仓秒',0,3600,5,v=>v+'s']]},
{title:'交易执行',items:[['margin_usdt','number','保证金 USDT',1,100000,1,v=>v+' U'],['leverage','number','杠杆',1,10,1,v=>v+'x'],['margin_mode','select','保证金模式',0,0,0,v=>v==='CROSS'?'全仓':'逐仓'],['position_mode','select','持仓模式',0,0,0,v=>v==='HEDGE'?'双向':'单向'],['use_maker_mode','checkbox','Maker 模式',0,0,0,v=>v?'开':'关'],['maker_offset_bps','range','Maker 偏移 bps',0,20,0.1,v=>v.toFixed(1)+' bps'],['guard_deadline_ms','number','保护单截止 ms',100,180,1,v=>v+'ms'],['depth_levels','number','深度档位',1,20,1,v=>v+'档'],['quantity_precision','number','下单精度(ETH=3,小币=0)',0,8,1,v=>v+'位']]},
{title:'引擎阈值',items:[['trend_enabled','checkbox','趋势引擎',0,0,0,v=>v?'开':'关'],['oi_delta_threshold','range','OI Delta',0.001,0.02,0.001,v=>(v*100).toFixed(2)+'%'],['squeeze_enabled','checkbox','Squeeze 引擎',0,0,0,v=>v?'开':'关'],['basis_zscore_threshold','range','Basis ZScore',1,5,0.1,v=>v.toFixed(1)],['transition_enabled','checkbox','Transition 引擎',0,0,0,v=>v?'开':'关'],['vol_compression_ratio','range','波动压缩比',0.1,0.9,0.05,v=>v.toFixed(2)],['liquidation_enabled','checkbox','踩踏/逼空引擎',0,0,0,v=>v?'开':'关'],['oi_liquidation_threshold','range','踩踏 OI 变化率',0.001,0.02,0.001,v=>(v*100).toFixed(2)+'%']]},
{title:'风控',items:[['max_slippage_bps','range','最大滑点 bps',1,100,0.5,v=>v.toFixed(1)+' bps'],['daily_loss_limit_pct','range','日亏损限制',0.001,0.2,0.001,v=>(v*100).toFixed(2)+'%'],['consecutive_loss_limit','number','连续亏损限制',1,20,1,v=>v+'次']]}
];
let dirState={long_enabled:true,short_enabled:true};
const dirTPSL={long_tp:0.008,long_sl:0.004,short_tp:0.008,short_sl:0.004};
const fields=[];const grid=document.getElementById('grid');
for(const g of groups){const card=document.createElement('div');card.className='card';card.style='padding:6px 8px';const h=document.createElement('h2');h.style='margin-bottom:4px';h.textContent=g.title;card.appendChild(h);const inner=document.createElement('div');inner.style='display:grid;grid-template-columns:repeat(auto-fill,minmax(120px,1fr));gap:3px';for(const it of g.items){fields.push(it);const[id,type,label,min,max,step]=it;const wrap=document.createElement('div');if(type==='checkbox'){wrap.className='check';wrap.innerHTML='<input id="'+id+'" type="checkbox"><label for="'+id+'">'+label+' <span id="v_'+id+'"></span></label>';}else if(type==='select'){wrap.className='field';wrap.style='margin-bottom:2px';const opts=id==='position_mode'?'<option value="ONE_WAY">单向持仓</option><option value="HEDGE">双向持仓(Hedge)</option>':'<option value="ISOLATED">逐仓 ISOLATED</option><option value="CROSS">全仓 CROSS</option>';wrap.innerHTML='<label for="'+id+'" style="font-size:10px">'+label+'</label><select id="'+id+'" style="width:100%;background:#0d1116;border:1px solid #34414d;border-radius:5px;color:#e8edf2;padding:4px 6px;font-size:12px">'+opts+'</select>';}else{wrap.className='field';wrap.style='margin-bottom:2px';wrap.innerHTML='<label for="'+id+'" style="font-size:10px">'+label+' <span id="v_'+id+'"></span></label><input id="'+id+'" type="'+type+'" min="'+min+'" max="'+max+'" step="'+step+'">';}inner.appendChild(wrap);}card.appendChild(inner);grid.appendChild(card);}
// 方向卡 TP/SL 滑块事件
['long_tp_pct','long_sl_pct','short_tp_pct','short_sl_pct'].forEach(id=>{const el=document.getElementById(id);el.addEventListener('input',()=>{const pct=(parseFloat(el.value)*100).toFixed(2)+'%';document.getElementById('v_'+id).textContent=pct;const k=id.replace('_pct','').replace('_','_');dirTPSL[id.replace('_pct','')]=parseFloat(el.value);});});
function fmt(it,val){return it[6](it[1]==='checkbox'?!!val:parseFloat(val))}
function refreshLabel(it){const el=document.getElementById(it[0]);document.getElementById('v_'+it[0]).textContent=fmt(it,it[1]==='checkbox'?el.checked:el.value)}
function setDirSlider(id,val){const el=document.getElementById(id);if(el){el.value=val;document.getElementById('v_'+id).textContent=(val*100).toFixed(2)+'%';}}
function populate(cur){
  for(const it of fields){const el=document.getElementById(it[0]);if(cur[it[0]]===undefined)continue;if(it[1]==='checkbox')el.checked=!!cur[it[0]];else if(it[1]==='select')el.value=cur[it[0]]||el.options[0].value;else el.value=cur[it[0]];if(it[1]!=='select')refreshLabel(it);}
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
  for(const it of fields){const el=document.getElementById(it[0]);if(it[1]==='checkbox')o[it[0]]=el.checked;else if(it[1]==='select')o[it[0]]=el.value;else o[it[0]]=(it[0]==='margin_usdt'?String(el.value):Number(el.value));}
  o.leverage=parseInt(o.leverage);o.cooldown_after_exit_sec=parseInt(o.cooldown_after_exit_sec);o.max_holding_time_sec=parseInt(o.max_holding_time_sec);o.min_holding_time_sec=parseInt(o.min_holding_time_sec);o.guard_deadline_ms=parseInt(o.guard_deadline_ms);o.consecutive_loss_limit=parseInt(o.consecutive_loss_limit);o.depth_levels=parseInt(o.depth_levels);o.quantity_precision=parseInt(o.quantity_precision);
  o.long_enabled=dirState.long_enabled;o.short_enabled=dirState.short_enabled;
  o.signal_based_exit=document.getElementById('signal_based_exit').checked;
  o.long_tp_pct=parseFloat(document.getElementById('long_tp_pct').value);
  o.long_sl_pct=parseFloat(document.getElementById('long_sl_pct').value);
  o.short_tp_pct=parseFloat(document.getElementById('short_tp_pct').value);
  o.short_sl_pct=parseFloat(document.getElementById('short_sl_pct').value);
  return o;
}
function msg(t,ok,el){const m=el||document.getElementById('msg');m.textContent=t;m.className='msg '+(ok?'ok':'err');setTimeout(()=>{m.className='msg'},5000)}
// 方向卡置信度字段（在 dir-card 内，不在 #grid，手动加入 fields 以支持 populate/collect）
const fmtC=v=>parseFloat(v).toFixed(2);
['trend_long_confidence','squeeze_long_confidence','transition_long_confidence','trend_short_confidence','squeeze_short_confidence','transition_short_confidence'].forEach(id=>{const it=[id,'range','',0.3,0.95,0.01,fmtC];fields.push(it);});
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
async function loadLogs(){const now='刷新于 '+new Date().toLocaleTimeString();try{const r=await fetch('/api/logs');const d=await r.json();renderLogBox('tradeLogs',d.trades||[],'暂无开仓/平仓记录');renderLogBox('systemLogs',d.system||[],'暂无系统日志');renderLogBox('rejectLogs',d.rejects||[],'暂无拒绝记录');renderLogBox('opLogs',d.operation||[],'暂无操作记录');['tradeHint','systemHint','rejectHint','opHint'].forEach(id=>document.getElementById(id).textContent=now)}catch(e){['tradeHint','systemHint','rejectHint','opHint'].forEach(id=>document.getElementById(id).textContent='加载失败')}}
async function clearLogs(cat){const r=await fetch('/api/logs?category='+cat,{method:'DELETE'});if(r.ok)loadLogs()}
document.getElementById('clearTradeBtn').onclick=()=>clearLogs('trade');
document.getElementById('clearSystemBtn').onclick=()=>clearLogs('system');
document.getElementById('clearRejectBtn').onclick=()=>clearLogs('reject');
document.getElementById('clearOpBtn').onclick=()=>clearLogs('operation');
async function loadWatchlistBadge(){try{const r=await fetch('/api/watchlist');const d=await r.json();const syms=d.symbols||[];document.getElementById('symbolBadge').textContent=syms.length?syms.join(' · '):'交易对 --'}catch(e){}}
function dirBar(fillId,valId,raw,scale){
  const fill=document.getElementById(fillId),val=document.getElementById(valId);if(!fill||!val)return;
  const pct=Math.min(Math.abs(raw/scale)*50,50);
  if(raw>=0){fill.className='bar-fill pos';fill.style.width=pct+'%';fill.style.left='50%';fill.style.right=''}
  else{fill.className='bar-fill neg';fill.style.width=pct+'%';fill.style.right='50%';fill.style.left=''}
  val.className='mkt-val '+(raw>0?'pos':raw<0?'neg':'neu');
  val.textContent=(raw>0?'+':'')+raw.toFixed(4);
}
function colorVal(id,v,fmtFn,posThresh=0,negThresh=0){
  const el=document.getElementById(id);if(!el)return;
  el.textContent=fmtFn(v);
  el.className='mkt-val '+(v>posThresh?'pos':v<negThresh?'neg':'neu');
}
// 当前选中的市场指标显示币种
let selectedMktSym='';
function renderWatchTabs(symKeys,active){
  const bar=document.getElementById('mktTabBar');if(!bar)return;
  bar.innerHTML='';
  if(!symKeys||!symKeys.length){return;}
  symKeys.forEach(s=>{
    const btn=document.createElement('button');
    const isActive=s===selectedMktSym||(selectedMktSym===''&&s===active);
    btn.textContent=s+(s===active?' ★':'');
    btn.style.cssText='border:0;border-radius:4px;padding:3px 9px;font-size:11px;cursor:pointer;font-weight:600;'
      +(isActive?'background:#0969da;color:#fff':'background:#1c2128;color:#8b949e');
    btn.onclick=()=>{selectedMktSym=s;};
    bar.appendChild(btn);
  });
}
async function loadMarket(){try{
  const r=await fetch('/api/market');if(!r.ok)return;
  const resp=await r.json();
  const syms=resp.symbols||{};
  const active=resp.active||'';
  const symKeys=Object.keys(syms);
  renderWatchTabs(symKeys,active);
  renderWatchlistItems(symKeys);
  // 选择要展示的币种
  if(!selectedMktSym||!syms[selectedMktSym])selectedMktSym=active;
  const d=syms[selectedMktSym];
  if(!d||!d.ready){
    document.getElementById('mktNotReady').style.display='';
    document.getElementById('mktGrid').style.display='none';
    const cnt=d?d.msg_count||0:0;
    document.getElementById('mktNotReady').textContent=cnt>0?'引擎预热中（'+cnt+'/100 条消息）':'引擎启动后显示（等待 WS 数据）';
    return;
  }
  document.getElementById('mktNotReady').style.display='none';
  document.getElementById('mktGrid').style.display='';
  document.getElementById('mktAge').textContent='延迟 '+(d.snapshot_age_ms||0)+'ms ['+selectedMktSym+']';
  // 价格
  document.getElementById('mv_mark').textContent=d.mark_price||'--';
  document.getElementById('mv_index').textContent=d.index_price||'--';
  colorVal('mv_basis_bps',d.basis_bps||0,v=>v.toFixed(3)+' bps');
  colorVal('mv_basis_z',d.basis_zscore||0,v=>v.toFixed(2));
  colorVal('mv_funding',d.funding_rate||0,v=>(v*100).toFixed(4)+'%');
  const nf=d.next_funding_ms||0;if(nf>0){const rem=Math.max(0,nf-Date.now());const m=Math.floor(rem/60000);const s=Math.floor((rem%60000)/1000);document.getElementById('mv_next_fund').textContent=m+'m'+s+'s'}
  colorVal('mv_mom',d.price_momentum||0,v=>(v*100).toFixed(3)+'%');
  // RCVD 方向条
  dirBar('rb_5s','rv_5s',d.rcvd_5s||0,0.05);
  dirBar('rb_30s','rv_30s',d.rcvd_30s||0,0.05);
  dirBar('rb_5m','rv_5m',d.rcvd_5m||0,0.05);
  // OI Delta
  const base=Math.max(Math.abs(d.oi_baseline||1),1);
  dirBar('ob_5s','ov_5s',d.oi_delta_5s||0,base*0.02);
  dirBar('ob_30s','ov_30s',d.oi_delta_30s||0,base*0.05);
  dirBar('ob_5m','ov_5m',d.oi_delta_5m||0,base*0.1);
  colorVal('ov_vel',d.oi_velocity||0,v=>v.toFixed(1));
  colorVal('ov_acc',d.oi_accel||0,v=>v.toFixed(2));
  // 波动率
  const v1m=d.vol_1m||0,v5m=d.vol_5m||0,v1h=d.vol_baseline_1h||1;
  document.getElementById('vv_1m').textContent=(v1m*10000).toFixed(2)+' bps';
  document.getElementById('vv_5m').textContent=(v5m*10000).toFixed(2)+' bps';
  document.getElementById('vv_1h').textContent=(v1h*10000).toFixed(2)+' bps';
  const comp=v1h>0?v5m/v1h:0;
  colorVal('vv_comp',comp,v=>v.toFixed(3),1.2,0.6);
  colorVal('vv_decay',d.survival_decay||0,v=>v.toFixed(3),0.7,0);
  colorVal('vv_spread',d.spread_widening||0,v=>v.toFixed(3),0,0);
  // 引擎信号
  const engs=d.engines||[];const eg=document.getElementById('engGrid');eg.innerHTML='';
  for(const e of engs){
    const dirColor=e.dir==='LONG'?'#56d364':e.dir==='SHORT'?'#ff7b72':'#8b949e';
    const fillCls=e.fired?(e.dir==='LONG'?'fired':'fired-short'):'';
    const pct=Math.min((e.confidence||0)*100,100);
    const thresh=Math.min((e.threshold||0)*100,100);
    eg.innerHTML+='<div class="eng-card">'
      +'<div class="eng-name">'+e.name+'</div>'
      +'<div class="eng-dir" style="color:'+dirColor+'">'+e.dir+(e.fired?' ▶':'')+'</div>'
      +'<div class="eng-conf-track"><div class="eng-conf-fill '+fillCls+'" style="width:'+pct+'%"></div>'
      +'<div class="eng-conf-thresh" style="left:'+thresh+'%"></div></div>'
      +'<div class="eng-label">置信度 '+(e.confidence||0).toFixed(2)+' / 阈值 '+(e.threshold||0).toFixed(2)+'</div>'
      +'</div>';
  }
  // 多空方向卡片的实时信号指标进度条
  const oidB=Math.max(Math.abs(d.oi_baseline||1),1);
  // 做多：买压=正RCVD，OI正增长，低资金费率有利
  dirBar('dl_rcvd_f','dl_rcvd_v',d.rcvd_5s||0,0.05);
  dirBar('dl_oi_f','dl_oi_v',d.oi_delta_5s||0,oidB*0.02);
  const dlVol=document.getElementById('dl_vol');if(dlVol)dlVol.textContent=((d.vol_1m||0)*10000).toFixed(1)+' bps';
  const dlFund=document.getElementById('dl_fund');
  if(dlFund){const fr=d.funding_rate||0;dlFund.textContent=(fr*100).toFixed(4)+'%';dlFund.className='mkt-val '+(fr>0.0001?'neg':fr<-0.0001?'pos':'neu');}
  // 做空：卖压=负RCVD取反，OI负增长取反，高基差ZScore利空
  dirBar('ds_rcvd_f','ds_rcvd_v',-(d.rcvd_5s||0),0.05);
  dirBar('ds_oi_f','ds_oi_v',-(d.oi_delta_5s||0),oidB*0.02);
  const dsBz=document.getElementById('ds_basisz');if(dsBz){const bz=d.basis_zscore||0;dsBz.textContent=bz.toFixed(2);dsBz.className='mkt-val '+(bz>1?'neg':bz<-1?'pos':'neu');}
  const dsSp=document.getElementById('ds_spread');if(dsSp)dsSp.textContent=((d.spread_bps||0)).toFixed(2)+' bps';
}catch(e){}}
// 交易对列表（加入即交易）
let _statusCache={warmup:{},ws_msg_count:{}};
function renderWatchlistItems(symKeys){
  const box=document.getElementById('watchlistItems');if(!box)return;
  if(!symKeys||!symKeys.length){box.innerHTML='<div class="muted">暂未加入交易对</div>';return;}
  const warmup=_statusCache.warmup||{};const counts=_statusCache.ws_msg_count||{};
  box.innerHTML=symKeys.map(s=>{
    const ready=warmup[s];const n=counts[s]||0;
    const sc=ready?'#56d364':n>0?'#e3b341':'#ff7b72';
    const st=ready?'就绪':n>0?'预热 '+n+'/100':'未连接';
    return '<div style="display:flex;align-items:center;gap:6px;padding:4px 0;border-bottom:1px solid #202a33">'
      +'<span style="flex:1;font-size:12px;font-family:Consolas,monospace;color:#d6dee6">'+s+'</span>'
      +'<span style="font-size:11px;color:'+sc+'">'+st+'</span>'
      +'<button class="btn-neutral" style="padding:2px 7px;font-size:11px" onclick="removeFromWatchlist(\''+s+'\')">移除</button>'
      +'</div>';
  }).join('');
}
async function addToWatchlist(){
  const inp=document.getElementById('watchlistInput');
  const sym=(inp.value||'').trim().toUpperCase();
  if(!sym){return;}
  const el=document.getElementById('watchlistResult');
  el.textContent='验证中...';el.style.color='#8b949e';
  const r=await fetch('/api/watchlist',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({symbol:sym})});
  if(r.ok){
    el.style.color='#56d364';el.textContent=sym+' 已加入，WS 连接建立中，开始交易';
    inp.value='';selectedMktSym=sym;loadLogs();loadWatchlistBadge();
  } else {
    el.style.color='#ff7b72';el.textContent='加入失败: '+await r.text();
  }
}
async function removeFromWatchlist(sym){
  if(!confirm('移除 '+sym+'？引擎将停止，WS 断开。若有持仓请先手动平仓。'))return;
  const r=await fetch('/api/watchlist',{method:'DELETE',headers:{'Content-Type':'application/json'},body:JSON.stringify({symbol:sym})});
  if(r.ok){if(selectedMktSym===sym)selectedMktSym='';loadLogs();loadWatchlistBadge();}
  else msg('移除失败: '+await r.text(),false);
}
async function loadStatus(){try{const r=await fetch('/api/status');if(!r.ok)return;const d=await r.json();_statusCache={warmup:d.warmup||{},ws_msg_count:d.ws_msg_count||{}};const el=document.getElementById('wsMsgBadge');const syms=d.symbols||[];if(!syms.length){el.style.background='#1a0d0d';el.style.color='#ff7b72';el.style.borderColor='#8e2b31';el.textContent='WS: 无交易对';return;}const allReady=syms.every(s=>d.warmup&&d.warmup[s]);const anyData=syms.some(s=>(d.ws_msg_count&&d.ws_msg_count[s]||0)>0);if(allReady){el.style.background='#0d2231';el.style.color='#56d364';el.style.borderColor='#238636';el.textContent='WS: 就绪 '+syms.length+'个'}else if(anyData){el.style.background='#1a1a0d';el.style.color='#e3b341';el.style.borderColor='#9e6a03';const w=syms.filter(s=>!d.warmup||!d.warmup[s]).length;el.textContent='WS: 预热中 '+w+'/'+syms.length}else{el.style.background='#1a0d0d';el.style.color='#ff7b72';el.style.borderColor='#8e2b31';el.textContent='WS: 未收到数据'}}catch(e){}}
(async()=>{try{const r=await fetch('/api/params');const d=await r.json();populate(d.current)}catch(e){msg('加载参数失败',false)}loadLogs();loadKeys();loadWatchlistBadge();loadStatus();loadMarket();setInterval(loadLogs,3000);setInterval(loadWatchlistBadge,5000);setInterval(loadStatus,2000);setInterval(loadMarket,800);})();
</script>
</body>
</html>`
