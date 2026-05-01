package webui

import (
	"sync"

	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/config"
)

// LiveParams 运行时可热更新的全量参数（线程安全）
type LiveParams struct {
	mu sync.RWMutex

	// ── 止盈止损（多空独立）────────────────────────────────
	LongTPPct  float64 // 做多止盈比例
	LongSLPct  float64 // 做多止损比例
	ShortTPPct float64 // 做空止盈比例
	ShortSLPct float64 // 做空止损比例
	// 保留兼容字段（从 config 初始化后分配给多空）
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
	TrendEnabled              bool
	TrendLongConfidence       float64
	TrendShortConfidence      float64
	OIDeltaThreshold          float64

	SqueezeEnabled            bool
	SqueezeLongConfidence     float64
	SqueezeShortConfidence    float64
	BasisZScoreThreshold      float64

	TransitionEnabled          bool
	TransitionLongConfidence   float64
	TransitionShortConfidence  float64
	VolCompressionRatio        float64

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
	// 多空独立止盈止损
	LongTPPct  float64 `json:"long_tp_pct"`
	LongSLPct  float64 `json:"long_sl_pct"`
	ShortTPPct float64 `json:"short_tp_pct"`
	ShortSLPct float64 `json:"short_sl_pct"`
	// 兼容旧字段（NewLiveParams 从 config 初始化时使用，后续 UI 不再直接使用）
	TakeProfitPct        float64 `json:"take_profit_pct,omitempty"`
	StopLossPct          float64 `json:"stop_loss_pct,omitempty"`
	CooldownAfterExitSec int64   `json:"cooldown_after_exit_sec"`
	MaxHoldingTimeSec    int64   `json:"max_holding_time_sec"`
	MinHoldingTimeSec    int64   `json:"min_holding_time_sec"`

	LotSize         string  `json:"lot_size"` // decimal 用 string 传输
	MarginUSDT      string  `json:"margin_usdt"`
	Leverage        int     `json:"leverage"`
	UseMakerMode    bool    `json:"use_maker_mode"`
	MakerOffsetBps  float64 `json:"maker_offset_bps"`
	GuardDeadlineMs int64   `json:"guard_deadline_ms"`

	TrendEnabled              bool    `json:"trend_enabled"`
	TrendLongConfidence       float64 `json:"trend_long_confidence"`
	TrendShortConfidence      float64 `json:"trend_short_confidence"`
	OIDeltaThreshold          float64 `json:"oi_delta_threshold"`

	SqueezeEnabled            bool    `json:"squeeze_enabled"`
	SqueezeLongConfidence     float64 `json:"squeeze_long_confidence"`
	SqueezeShortConfidence    float64 `json:"squeeze_short_confidence"`
	BasisZScoreThreshold      float64 `json:"basis_zscore_threshold"`

	TransitionEnabled          bool    `json:"transition_enabled"`
	TransitionLongConfidence   float64 `json:"transition_long_confidence"`
	TransitionShortConfidence  float64 `json:"transition_short_confidence"`
	VolCompressionRatio        float64 `json:"vol_compression_ratio"`

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
		LongTPPct:  cfg.Exits.TakeProfitPct,
		LongSLPct:  cfg.Exits.StopLossPct,
		ShortTPPct: cfg.Exits.TakeProfitPct,
		ShortSLPct: cfg.Exits.StopLossPct,
		CooldownAfterExitSec: cfg.Exits.CooldownAfterExitSec,
		MaxHoldingTimeSec:    cfg.Exits.MaxHoldingTimeSec,
		MinHoldingTimeSec:    cfg.Exits.MinHoldingTimeSec,

		LotSize:         cfg.Trading.LotSize.String(),
		MarginUSDT:      "10",
		Leverage:        cfg.Trading.Leverage,
		UseMakerMode:    cfg.Execution.UseMakerMode,
		MakerOffsetBps:  2.0, // 默认 2bps 偏移
		GuardDeadlineMs: 150, // 默认 150ms（规范要求 <200ms）

		TrendEnabled:             cfg.Engines.Trend.Enabled,
		TrendLongConfidence:      cfg.Engines.Trend.ConfidenceThreshold,
		TrendShortConfidence:     cfg.Engines.Trend.ConfidenceThreshold,
		OIDeltaThreshold:         cfg.Engines.Trend.OIDeltaThreshold,

		SqueezeEnabled:           cfg.Engines.Squeeze.Enabled,
		SqueezeLongConfidence:    cfg.Engines.Squeeze.ConfidenceThreshold,
		SqueezeShortConfidence:   cfg.Engines.Squeeze.ConfidenceThreshold,
		BasisZScoreThreshold:     cfg.Engines.Squeeze.BasisZScoreThreshold,

		TransitionEnabled:         cfg.Engines.Transition.Enabled,
		TransitionLongConfidence:  cfg.Engines.Transition.ConfidenceThreshold,
		TransitionShortConfidence: cfg.Engines.Transition.ConfidenceThreshold,
		VolCompressionRatio:       cfg.Engines.Transition.VolCompressionRatio,

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
	return lp.snapshotLocked()
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
		LongTPPct:  lp.LongTPPct,
		LongSLPct:  lp.LongSLPct,
		ShortTPPct: lp.ShortTPPct,
		ShortSLPct: lp.ShortSLPct,

		CooldownAfterExitSec: lp.CooldownAfterExitSec,
		MaxHoldingTimeSec:    lp.MaxHoldingTimeSec,
		MinHoldingTimeSec:    lp.MinHoldingTimeSec,

		LotSize:         lp.LotSize.String(),
		MarginUSDT:      lp.MarginUSDT.String(),
		Leverage:        lp.Leverage,
		UseMakerMode:    lp.UseMakerMode,
		MakerOffsetBps:  lp.MakerOffsetBps,
		GuardDeadlineMs: lp.GuardDeadlineMs,

		TrendEnabled:             lp.TrendEnabled,
		TrendLongConfidence:      lp.TrendLongConfidence,
		TrendShortConfidence:     lp.TrendShortConfidence,
		OIDeltaThreshold:         lp.OIDeltaThreshold,

		SqueezeEnabled:           lp.SqueezeEnabled,
		SqueezeLongConfidence:    lp.SqueezeLongConfidence,
		SqueezeShortConfidence:   lp.SqueezeShortConfidence,
		BasisZScoreThreshold:     lp.BasisZScoreThreshold,

		TransitionEnabled:         lp.TransitionEnabled,
		TransitionLongConfidence:  lp.TransitionLongConfidence,
		TransitionShortConfidence: lp.TransitionShortConfidence,
		VolCompressionRatio:       lp.VolCompressionRatio,

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
	// 多空独立 TP/SL；若字段为 0 则回退到兼容旧字段
	setOrFallback := func(v, fallback float64) float64 {
		if v > 0 {
			return v
		}
		return fallback
	}
	lp.LongTPPct = setOrFallback(s.LongTPPct, s.TakeProfitPct)
	lp.LongSLPct = setOrFallback(s.LongSLPct, s.StopLossPct)
	lp.ShortTPPct = setOrFallback(s.ShortTPPct, s.TakeProfitPct)
	lp.ShortSLPct = setOrFallback(s.ShortSLPct, s.StopLossPct)
	// 兼容字段也同步（供旧代码引用）
	lp.TakeProfitPct = lp.LongTPPct
	lp.StopLossPct = lp.LongSLPct
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
	lp.TrendLongConfidence = s.TrendLongConfidence
	lp.TrendShortConfidence = s.TrendShortConfidence
	lp.OIDeltaThreshold = s.OIDeltaThreshold

	lp.SqueezeEnabled = s.SqueezeEnabled
	lp.SqueezeLongConfidence = s.SqueezeLongConfidence
	lp.SqueezeShortConfidence = s.SqueezeShortConfidence
	lp.BasisZScoreThreshold = s.BasisZScoreThreshold

	lp.TransitionEnabled = s.TransitionEnabled
	lp.TransitionLongConfidence = s.TransitionLongConfidence
	lp.TransitionShortConfidence = s.TransitionShortConfidence
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
