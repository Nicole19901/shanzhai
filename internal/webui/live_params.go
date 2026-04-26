package webui

import (
	"sync"

	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/config"
)

// LiveParams 运行时可热更新的全量参数（线程安全）
type LiveParams struct {
	mu sync.RWMutex

	// ── 止盈止损 ────────────────────────────────────────
	TakeProfitPct        float64
	StopLossPct          float64
	CooldownAfterExitSec int64
	MaxHoldingTimeSec    int64
	MinHoldingTimeSec    int64

	// ── 开仓参数 ────────────────────────────────────────
	LotSize         decimal.Decimal // 每笔手数
	Leverage        int             // 杠杆倍数 1-10
	UseMakerMode    bool            // true=挂限价单(maker), false=市价(taker)
	MakerOffsetBps  float64         // maker 模式下限价偏移(bps)，多单=bid-offset，空单=ask+offset
	GuardDeadlineMs int64           // 兜底订单挂出最大延迟(ms)，规范下限100ms

	// ── 引擎开关 + 置信度 ───────────────────────────────
	TrendEnabled     bool
	TrendConfidence  float64
	OIDeltaThreshold float64

	SqueezeEnabled       bool
	SqueezeConfidence    float64
	BasisZScoreThreshold float64

	TransitionEnabled    bool
	TransitionConfidence float64
	VolCompressionRatio  float64

	// ── 风控 ────────────────────────────────────────────
	MaxSlippageBps              float64
	DailyLossLimitPct           float64
	ConsecutiveLossLimit        int
	BTCCorrelationLockThreshold float64

	// 初始默认值（只读，Init 时固定）
	defaults LiveParamsSnapshot
}

// LiveParamsSnapshot JSON 序列化快照
type LiveParamsSnapshot struct {
	TakeProfitPct        float64 `json:"take_profit_pct"`
	StopLossPct          float64 `json:"stop_loss_pct"`
	CooldownAfterExitSec int64   `json:"cooldown_after_exit_sec"`
	MaxHoldingTimeSec    int64   `json:"max_holding_time_sec"`
	MinHoldingTimeSec    int64   `json:"min_holding_time_sec"`

	LotSize         string  `json:"lot_size"` // decimal 用 string 传输
	Leverage        int     `json:"leverage"`
	UseMakerMode    bool    `json:"use_maker_mode"`
	MakerOffsetBps  float64 `json:"maker_offset_bps"`
	GuardDeadlineMs int64   `json:"guard_deadline_ms"`

	TrendEnabled     bool    `json:"trend_enabled"`
	TrendConfidence  float64 `json:"trend_confidence"`
	OIDeltaThreshold float64 `json:"oi_delta_threshold"`

	SqueezeEnabled       bool    `json:"squeeze_enabled"`
	SqueezeConfidence    float64 `json:"squeeze_confidence"`
	BasisZScoreThreshold float64 `json:"basis_zscore_threshold"`

	TransitionEnabled    bool    `json:"transition_enabled"`
	TransitionConfidence float64 `json:"transition_confidence"`
	VolCompressionRatio  float64 `json:"vol_compression_ratio"`

	MaxSlippageBps              float64 `json:"max_slippage_bps"`
	DailyLossLimitPct           float64 `json:"daily_loss_limit_pct"`
	ConsecutiveLossLimit        int     `json:"consecutive_loss_limit"`
	BTCCorrelationLockThreshold float64 `json:"btc_correlation_lock_threshold"`
}

func NewLiveParams(cfg *config.Config) *LiveParams {
	snap := LiveParamsSnapshot{
		TakeProfitPct:        cfg.Exits.TakeProfitPct,
		StopLossPct:          cfg.Exits.StopLossPct,
		CooldownAfterExitSec: cfg.Exits.CooldownAfterExitSec,
		MaxHoldingTimeSec:    cfg.Exits.MaxHoldingTimeSec,
		MinHoldingTimeSec:    cfg.Exits.MinHoldingTimeSec,

		LotSize:         cfg.Trading.LotSize.String(),
		Leverage:        cfg.Trading.Leverage,
		UseMakerMode:    cfg.Execution.UseMakerMode,
		MakerOffsetBps:  2.0, // 默认 2bps 偏移
		GuardDeadlineMs: 150, // 默认 150ms（规范要求 <200ms）

		TrendEnabled:     cfg.Engines.Trend.Enabled,
		TrendConfidence:  cfg.Engines.Trend.ConfidenceThreshold,
		OIDeltaThreshold: cfg.Engines.Trend.OIDeltaThreshold,

		SqueezeEnabled:       cfg.Engines.Squeeze.Enabled,
		SqueezeConfidence:    cfg.Engines.Squeeze.ConfidenceThreshold,
		BasisZScoreThreshold: cfg.Engines.Squeeze.BasisZScoreThreshold,

		TransitionEnabled:    cfg.Engines.Transition.Enabled,
		TransitionConfidence: cfg.Engines.Transition.ConfidenceThreshold,
		VolCompressionRatio:  cfg.Engines.Transition.VolCompressionRatio,

		MaxSlippageBps:              cfg.Risk.MaxSlippageBps,
		DailyLossLimitPct:           cfg.Risk.DailyLossLimitPct,
		ConsecutiveLossLimit:        cfg.Risk.ConsecutiveLossLimit,
		BTCCorrelationLockThreshold: cfg.Risk.BTCCorrelationLockThreshold,
	}
	lp := &LiveParams{defaults: snap}
	lp.apply(snap)
	return lp
}

func (lp *LiveParams) Get() LiveParamsSnapshot {
	lp.mu.RLock()
	defer lp.mu.RUnlock()
	return LiveParamsSnapshot{
		TakeProfitPct:        lp.TakeProfitPct,
		StopLossPct:          lp.StopLossPct,
		CooldownAfterExitSec: lp.CooldownAfterExitSec,
		MaxHoldingTimeSec:    lp.MaxHoldingTimeSec,
		MinHoldingTimeSec:    lp.MinHoldingTimeSec,

		LotSize:         lp.LotSize.String(),
		Leverage:        lp.Leverage,
		UseMakerMode:    lp.UseMakerMode,
		MakerOffsetBps:  lp.MakerOffsetBps,
		GuardDeadlineMs: lp.GuardDeadlineMs,

		TrendEnabled:     lp.TrendEnabled,
		TrendConfidence:  lp.TrendConfidence,
		OIDeltaThreshold: lp.OIDeltaThreshold,

		SqueezeEnabled:       lp.SqueezeEnabled,
		SqueezeConfidence:    lp.SqueezeConfidence,
		BasisZScoreThreshold: lp.BasisZScoreThreshold,

		TransitionEnabled:    lp.TransitionEnabled,
		TransitionConfidence: lp.TransitionConfidence,
		VolCompressionRatio:  lp.VolCompressionRatio,

		MaxSlippageBps:              lp.MaxSlippageBps,
		DailyLossLimitPct:           lp.DailyLossLimitPct,
		ConsecutiveLossLimit:        lp.ConsecutiveLossLimit,
		BTCCorrelationLockThreshold: lp.BTCCorrelationLockThreshold,
	}
}

func (lp *LiveParams) Update(s LiveParamsSnapshot) {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	lp.apply(s)
}

func (lp *LiveParams) Reset() {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	lp.apply(lp.defaults)
}

func (lp *LiveParams) SetDefaultsToCurrent() LiveParamsSnapshot {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	lp.defaults = lp.snapshotLocked()
	return lp.defaults
}

func (lp *LiveParams) Defaults() LiveParamsSnapshot {
	lp.mu.RLock()
	defer lp.mu.RUnlock()
	return lp.defaults
}

func (lp *LiveParams) snapshotLocked() LiveParamsSnapshot {
	return LiveParamsSnapshot{
		TakeProfitPct:        lp.TakeProfitPct,
		StopLossPct:          lp.StopLossPct,
		CooldownAfterExitSec: lp.CooldownAfterExitSec,
		MaxHoldingTimeSec:    lp.MaxHoldingTimeSec,
		MinHoldingTimeSec:    lp.MinHoldingTimeSec,

		LotSize:         lp.LotSize.String(),
		Leverage:        lp.Leverage,
		UseMakerMode:    lp.UseMakerMode,
		MakerOffsetBps:  lp.MakerOffsetBps,
		GuardDeadlineMs: lp.GuardDeadlineMs,

		TrendEnabled:     lp.TrendEnabled,
		TrendConfidence:  lp.TrendConfidence,
		OIDeltaThreshold: lp.OIDeltaThreshold,

		SqueezeEnabled:       lp.SqueezeEnabled,
		SqueezeConfidence:    lp.SqueezeConfidence,
		BasisZScoreThreshold: lp.BasisZScoreThreshold,

		TransitionEnabled:    lp.TransitionEnabled,
		TransitionConfidence: lp.TransitionConfidence,
		VolCompressionRatio:  lp.VolCompressionRatio,

		MaxSlippageBps:              lp.MaxSlippageBps,
		DailyLossLimitPct:           lp.DailyLossLimitPct,
		ConsecutiveLossLimit:        lp.ConsecutiveLossLimit,
		BTCCorrelationLockThreshold: lp.BTCCorrelationLockThreshold,
	}
}

func (lp *LiveParams) apply(s LiveParamsSnapshot) {
	lp.TakeProfitPct = s.TakeProfitPct
	lp.StopLossPct = s.StopLossPct
	lp.CooldownAfterExitSec = s.CooldownAfterExitSec
	lp.MaxHoldingTimeSec = s.MaxHoldingTimeSec
	lp.MinHoldingTimeSec = s.MinHoldingTimeSec

	lot, err := decimal.NewFromString(s.LotSize)
	if err != nil || lot.IsZero() || lot.IsNegative() {
		lot = decimal.NewFromFloat(0.01)
	}
	lp.LotSize = lot
	lp.Leverage = s.Leverage
	lp.UseMakerMode = s.UseMakerMode
	lp.MakerOffsetBps = s.MakerOffsetBps
	if s.GuardDeadlineMs < 100 {
		s.GuardDeadlineMs = 100 // 硬下限 100ms
	}
	if s.GuardDeadlineMs > 180 {
		s.GuardDeadlineMs = 180 // 硬上限 180ms（留余量给网络）
	}
	lp.GuardDeadlineMs = s.GuardDeadlineMs

	lp.TrendEnabled = s.TrendEnabled
	lp.TrendConfidence = s.TrendConfidence
	lp.OIDeltaThreshold = s.OIDeltaThreshold

	lp.SqueezeEnabled = s.SqueezeEnabled
	lp.SqueezeConfidence = s.SqueezeConfidence
	lp.BasisZScoreThreshold = s.BasisZScoreThreshold

	lp.TransitionEnabled = s.TransitionEnabled
	lp.TransitionConfidence = s.TransitionConfidence
	lp.VolCompressionRatio = s.VolCompressionRatio

	lp.MaxSlippageBps = s.MaxSlippageBps
	lp.DailyLossLimitPct = s.DailyLossLimitPct
	lp.ConsecutiveLossLimit = s.ConsecutiveLossLimit
	lp.BTCCorrelationLockThreshold = s.BTCCorrelationLockThreshold
}
