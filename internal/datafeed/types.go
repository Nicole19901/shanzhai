package datafeed

import "github.com/shopspring/decimal"

type Direction int8

const (
	DirectionFlat  Direction = 0
	DirectionLong  Direction = 1
	DirectionShort Direction = -1
)

type PriceLevel struct {
	Price    decimal.Decimal
	Quantity decimal.Decimal
}

// AggTrade 归一化后的成交数据，所有字段不可变
type AggTrade struct {
	Symbol       string
	AggTradeID   int64
	Price        decimal.Decimal
	Quantity     decimal.Decimal
	FirstTradeID int64
	LastTradeID  int64
	EventTime    int64 // ms
	LocalTime    int64 // ms
	IsBuyerMaker bool  // false = 主动买入
}

// DepthUpdate 深度更新
type DepthUpdate struct {
	Symbol        string
	FirstUpdateID int64
	FinalUpdateID int64
	Bids          []PriceLevel
	Asks          []PriceLevel
	EventTime     int64
	LocalTime     int64
}

// MarkPrice markPrice 数据
type MarkPrice struct {
	Symbol          string
	MarkPrice       decimal.Decimal
	IndexPrice      decimal.Decimal
	FundingRate     decimal.Decimal
	NextFundingTime int64
	EventTime       int64
	LocalTime       int64
}

// Kline K线数据
type Kline struct {
	Symbol    string
	Interval  string
	OpenTime  int64
	CloseTime int64
	Open      decimal.Decimal
	High      decimal.Decimal
	Low       decimal.Decimal
	Close     decimal.Decimal
	Volume    decimal.Decimal
	IsClosed  bool
	EventTime int64
	LocalTime int64
}

// MarketContext 传递给引擎的只读市场快照
type MarketContext struct {
	ETHMarkPrice   decimal.Decimal
	ETHIndexPrice  decimal.Decimal
	FundingRate    decimal.Decimal
	NextFundingMs  int64

	// microstructure
	RCVD5s  decimal.Decimal
	RCVD30s decimal.Decimal
	RCVD5m  decimal.Decimal

	OIDelta5s   decimal.Decimal
	OIDelta30s  decimal.Decimal
	OIDelta5m   decimal.Decimal // 真实5分钟OI变化量
	OIVelocity  decimal.Decimal
	OIAccel     decimal.Decimal
	OIBaseline  decimal.Decimal

	BasisBps      decimal.Decimal
	BasisVelocity decimal.Decimal
	BasisZScore   decimal.Decimal

	OrderbookSurvivalDecay decimal.Decimal // 0~1
	SpreadWideningRate     decimal.Decimal

	// BTC
	BTCMarkPrice decimal.Decimal
	BTCTrend1m   Direction
	BTCTrend5m   Direction

	// vol
	RealizedVol1m  decimal.Decimal
	RealizedVol5m  decimal.Decimal
	VolBaseline1h  decimal.Decimal

	// kline price momentum
	PriceMomentum1m decimal.Decimal

	SnapshotTime int64 // ms
}
