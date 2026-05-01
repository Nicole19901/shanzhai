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
	MarginUSDT      decimal.Decimal // 每笔保证金，按 USDT 计价
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
	MaxSlippageBps       float64
	DailyLossLimitPct    float64
	ConsecutiveLossLimit int

	// ── 微观结构 ─────────────────────────────────────────
	DepthLevels int // 吃单深度档位 1-20，默认 5

	// ── 平仓模式 ─────────────────────────────────────────
	SignalBasedExit bool // true=信号窗口确认平仓，false=固定止盈

	// ── 方向开关 ─────────────────────────────────────────
	LongEnabled  bool // 允许开多（引擎信号为 LONG 时是否执行）
	ShortEnabled bool // 允许开空（引擎信号为 SHORT 时是否执行）

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
	MarginUSDT      string  `json:"margin_usdt"`
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

	MaxSlippageBps       float64 `json:"max_slippage_bps"`
	DailyLossLimitPct    float64 `json:"daily_loss_limit_pct"`
	ConsecutiveLossLimit int     `json:"consecutive_loss_limit"`
	DepthLevels          int     `json:"depth_levels"`
	SignalBasedExit      bool    `json:"signal_based_exit"`
	LongEnabled          bool    `json:"long_enabled"`
	ShortEnabled         bool    `json:"short_enabled"`
}

func NewLiveParams(cfg *config.Config) *LiveParams {
	snap := LiveParamsSnapshot{
		TakeProfitPct:        cfg.Exits.TakeProfitPct,
		StopLossPct:          cfg.Exits.StopLossPct,
		CooldownAfterExitSec: cfg.Exits.CooldownAfterExitSec,
		MaxHoldingTimeSec:    cfg.Exits.MaxHoldingTimeSec,
		MinHoldingTimeSec:    cfg.Exits.MinHoldingTimeSec,

		LotSize:         cfg.Trading.LotSize.String(),
		MarginUSDT:      "10",
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

		MaxSlippageBps:       cfg.Risk.MaxSlippageBps,
		DailyLossLimitPct:    cfg.Risk.DailyLossLimitPct,
		ConsecutiveLossLimit: cfg.Risk.ConsecutiveLossLimit,
		DepthLevels:          5,
		LongEnabled:          true,
		ShortEnabled:         true,
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
		MarginUSDT:      lp.MarginUSDT.String(),
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

		MaxSlippageBps:       lp.MaxSlippageBps,
		DailyLossLimitPct:    lp.DailyLossLimitPct,
		ConsecutiveLossLimit: lp.ConsecutiveLossLimit,
		DepthLevels:          lp.DepthLevels,
		SignalBasedExit:      lp.SignalBasedExit,
		LongEnabled:          lp.LongEnabled,
		ShortEnabled:         lp.ShortEnabled,
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
		MarginUSDT:      lp.MarginUSDT.String(),
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

		MaxSlippageBps:       lp.MaxSlippageBps,
		DailyLossLimitPct:    lp.DailyLossLimitPct,
		ConsecutiveLossLimit: lp.ConsecutiveLossLimit,
		DepthLevels:          lp.DepthLevels,
		SignalBasedExit:      lp.SignalBasedExit,
		LongEnabled:          lp.LongEnabled,
		ShortEnabled:         lp.ShortEnabled,
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
	margin, err := decimal.NewFromString(s.MarginUSDT)
	if err != nil || margin.IsZero() || margin.IsNegative() {
		margin = decimal.NewFromFloat(10)
	}
	lp.MarginUSDT = margin
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
	if s.DepthLevels < 1 {
		s.DepthLevels = 5
	}
	if s.DepthLevels > 20 {
		s.DepthLevels = 20
	}
	lp.DepthLevels = s.DepthLevels
	lp.SignalBasedExit = s.SignalBasedExit
	lp.LongEnabled = s.LongEnabled
	lp.ShortEnabled = s.ShortEnabled
}
