package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/shopspring/decimal"
	"github.com/spf13/viper"
)

type Config struct {
	Trading      TradingConfig      `mapstructure:"trading"`
	Exits        ExitsConfig        `mapstructure:"exits"`
	Engines      EnginesConfig      `mapstructure:"engines"`
	Risk         RiskConfig         `mapstructure:"risk"`
	WebUI        WebUIConfig        `mapstructure:"webui"`
	Sampling     SamplingConfig     `mapstructure:"sampling"`
	StateMachine StateMachineConfig `mapstructure:"state_machine"`
	Execution    ExecutionConfig    `mapstructure:"execution"`
	Binance      BinanceConfig      `mapstructure:"binance"`
}

type TradingConfig struct {
	Symbol     string `mapstructure:"symbol"`
	LotSizeStr string `mapstructure:"lot_size"`
	Leverage   int    `mapstructure:"leverage"`
	MarginType string `mapstructure:"margin_type"`

	LotSize decimal.Decimal
}

type ExitsConfig struct {
	TakeProfitPct        float64 `mapstructure:"take_profit_pct"`
	StopLossPct          float64 `mapstructure:"stop_loss_pct"`
	MaxHoldingTimeSec    int64   `mapstructure:"max_holding_time_sec"`
	MinHoldingTimeSec    int64   `mapstructure:"min_holding_time_sec"`
	CooldownAfterExitSec int64   `mapstructure:"cooldown_after_exit_sec"`
}

type EnginesConfig struct {
	Trend       TrendEngineConfig       `mapstructure:"trend"`
	Squeeze     SqueezeEngineConfig     `mapstructure:"squeeze"`
	Transition  TransitionEngineConfig  `mapstructure:"transition"`
	Liquidation LiquidationEngineConfig `mapstructure:"liquidation"`
}

type LiquidationEngineConfig struct {
	Enabled             bool    `mapstructure:"enabled"`
	ConfidenceThreshold float64 `mapstructure:"confidence_threshold"`
	OIThreshold         float64 `mapstructure:"oi_threshold"` // OI 30s 变化率阈值，默认 0.003 (0.3%)
}

type TrendEngineConfig struct {
	Enabled             bool    `mapstructure:"enabled"`
	ConfidenceThreshold float64 `mapstructure:"confidence_threshold"`
	OIDeltaThreshold    float64 `mapstructure:"oi_delta_threshold"`
}

type SqueezeEngineConfig struct {
	Enabled              bool    `mapstructure:"enabled"`
	ConfidenceThreshold  float64 `mapstructure:"confidence_threshold"`
	BasisZScoreThreshold float64 `mapstructure:"basis_zscore_threshold"`
}

type TransitionEngineConfig struct {
	Enabled             bool    `mapstructure:"enabled"`
	ConfidenceThreshold float64 `mapstructure:"confidence_threshold"`
	VolCompressionRatio float64 `mapstructure:"vol_compression_ratio"`
}

type RiskConfig struct {
	MaxSlippageBps       float64 `mapstructure:"max_slippage_bps"`
	DailyLossLimitPct    float64 `mapstructure:"daily_loss_limit_pct"`
	ConsecutiveLossLimit int     `mapstructure:"consecutive_loss_limit"`
}

type WebUIConfig struct {
	Addr        string `mapstructure:"addr"`
	ServiceName string `mapstructure:"service_name"`
}

type SamplingConfig struct {
	FastIntervalMs   int64 `mapstructure:"fast_interval_ms"`
	MidIntervalMs    int64 `mapstructure:"mid_interval_ms"`
	SlowIntervalMs   int64 `mapstructure:"slow_interval_ms"`
	OIPollIntervalMs int64 `mapstructure:"oi_poll_interval_ms"`
}

type StateMachineConfig struct {
	StressVolMultiplier          float64 `mapstructure:"stress_vol_multiplier"`
	DegradationLatencyBreachRate float64 `mapstructure:"degradation_latency_breach_rate"`
	FailureDataGapSec            int64   `mapstructure:"failure_data_gap_sec"`
}

type ExecutionConfig struct {
	UseMakerMode              bool  `mapstructure:"use_maker_mode"`
	OrderTimeoutMs            int64 `mapstructure:"order_timeout_ms"`
	ReconciliationIntervalSec int64 `mapstructure:"reconciliation_interval_sec"`
	MaxOrderRetry             int   `mapstructure:"max_order_retry"`
}

type BinanceConfig struct {
	APIKey       string `mapstructure:"api_key"`
	APISecret    string `mapstructure:"api_secret"`
	WSEndpoint   string `mapstructure:"ws_endpoint"`
	RESTEndpoint string `mapstructure:"rest_endpoint"`
	Testnet      bool   `mapstructure:"testnet"`
}

func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	if cfg.WebUI.Addr == "" {
		cfg.WebUI.Addr = ":8080"
	}
	if cfg.WebUI.ServiceName == "" {
		cfg.WebUI.ServiceName = "eth-perp-system"
	}

	// 展开环境变量
	cfg.Binance.APIKey = os.ExpandEnv(cfg.Binance.APIKey)
	cfg.Binance.APISecret = os.ExpandEnv(cfg.Binance.APISecret)

	// 解析 lot_size 为 decimal
	ls, err := decimal.NewFromString(cfg.Trading.LotSizeStr)
	if err != nil {
		return nil, fmt.Errorf("invalid lot_size: %w", err)
	}
	cfg.Trading.LotSize = ls

	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &cfg, nil
}

func validate(cfg *Config) error {
	tp := cfg.Exits.TakeProfitPct
	sl := cfg.Exits.StopLossPct

	if sl <= 0 || sl > 0.02 {
		return fmt.Errorf("stop_loss_pct must be in (0, 0.02], got %f", sl)
	}
	if tp <= 0 {
		return fmt.Errorf("take_profit_pct must be > 0, got %f", tp)
	}
	if tp/sl < 1.5 {
		return fmt.Errorf("take_profit_pct/stop_loss_pct must be >= 1.5, got %.2f", tp/sl)
	}
	if cfg.Trading.Leverage < 1 || cfg.Trading.Leverage > 10 {
		return fmt.Errorf("leverage must be in [1, 10], got %d", cfg.Trading.Leverage)
	}
	if cfg.Trading.MarginType != "ISOLATED" && cfg.Trading.MarginType != "CROSS" {
		return fmt.Errorf("margin_type must be ISOLATED or CROSS, got %s", cfg.Trading.MarginType)
	}
	if cfg.Trading.LotSize.IsZero() || cfg.Trading.LotSize.IsNegative() {
		return fmt.Errorf("lot_size must be > 0")
	}
	return nil
}
