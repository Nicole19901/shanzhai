package datafeed

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// Normalizer 将 Binance 原始 JSON 解析为内部类型

type Channels struct {
	ETHAggTrade  chan *AggTrade
	ETHDepth     chan *DepthUpdate
	ETHMarkPrice chan *MarkPrice
	ETHKline1s   chan *Kline
	ETHKline1m   chan *Kline
	BTCAggTrade  chan *AggTrade
	BTCMarkPrice chan *MarkPrice
}

func NewChannels() *Channels {
	return &Channels{
		ETHAggTrade:  make(chan *AggTrade, 512),
		ETHDepth:     make(chan *DepthUpdate, 256),
		ETHMarkPrice: make(chan *MarkPrice, 128),
		ETHKline1s:   make(chan *Kline, 128),
		ETHKline1m:   make(chan *Kline, 64),
		BTCAggTrade:  make(chan *AggTrade, 512),
		BTCMarkPrice: make(chan *MarkPrice, 128),
	}
}

func MakeAggTradeHandler(ch chan<- *AggTrade) func(json.RawMessage, int64) {
	return func(data json.RawMessage, localTime int64) {
		raw, err := parseAggTrade(data)
		if err != nil {
			log.Warn().Err(err).Msg("aggTrade parse failed")
			return
		}
		price, err1 := decimal.NewFromString(raw.Price)
		qty, err2 := decimal.NewFromString(raw.Quantity)
		if err1 != nil || err2 != nil {
			log.Warn().Msg("aggTrade decimal parse failed")
			return
		}
		select {
		case ch <- &AggTrade{
			Symbol:       raw.Symbol,
			AggTradeID:   raw.AggTradeID,
			Price:        price,
			Quantity:     qty,
			FirstTradeID: raw.FirstTradeID,
			LastTradeID:  raw.LastTradeID,
			EventTime:    raw.EventTime,
			LocalTime:    localTime,
			IsBuyerMaker: raw.IsBuyerMaker,
		}:
		default:
			log.Warn().Msg("aggTrade channel full, dropping")
		}
	}
}

type rawAggTrade struct {
	EventTime    int64
	TradeTime    int64
	AggTradeID   int64
	Price        string
	Quantity     string
	FirstTradeID int64
	LastTradeID  int64
	IsBuyerMaker bool
	Symbol       string
}

func parseAggTrade(data json.RawMessage) (rawAggTrade, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return rawAggTrade{}, err
	}

	eventTime, err := int64Field(fields, "E")
	if err != nil {
		return rawAggTrade{}, err
	}
	tradeTime, err := int64Field(fields, "T")
	if err != nil {
		return rawAggTrade{}, err
	}
	aggTradeID, err := int64Field(fields, "a")
	if err != nil {
		return rawAggTrade{}, err
	}
	firstTradeID, err := int64Field(fields, "f")
	if err != nil {
		return rawAggTrade{}, err
	}
	lastTradeID, err := int64Field(fields, "l")
	if err != nil {
		return rawAggTrade{}, err
	}
	price, err := stringField(fields, "p")
	if err != nil {
		return rawAggTrade{}, err
	}
	qty, err := stringField(fields, "q")
	if err != nil {
		return rawAggTrade{}, err
	}
	symbol, err := stringField(fields, "s")
	if err != nil {
		return rawAggTrade{}, err
	}
	isBuyerMaker, err := boolField(fields, "m")
	if err != nil {
		return rawAggTrade{}, err
	}

	return rawAggTrade{
		EventTime:    eventTime,
		TradeTime:    tradeTime,
		AggTradeID:   aggTradeID,
		Price:        price,
		Quantity:     qty,
		FirstTradeID: firstTradeID,
		LastTradeID:  lastTradeID,
		IsBuyerMaker: isBuyerMaker,
		Symbol:       symbol,
	}, nil
}

func int64Field(fields map[string]json.RawMessage, key string) (int64, error) {
	raw, ok := fields[key]
	if !ok {
		return 0, fmt.Errorf("missing field %q", key)
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("field %q must be int64", key)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("field %q must be int64: %w", key, err)
	}
	return n, nil
}

func stringField(fields map[string]json.RawMessage, key string) (string, error) {
	raw, ok := fields[key]
	if !ok {
		return "", fmt.Errorf("missing field %q", key)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("field %q must be string", key)
	}
	return s, nil
}

func boolField(fields map[string]json.RawMessage, key string) (bool, error) {
	raw, ok := fields[key]
	if !ok {
		return false, fmt.Errorf("missing field %q", key)
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, fmt.Errorf("field %q must be bool", key)
	}
	return b, nil
}

func MakeDepthHandler(ch chan<- *DepthUpdate) func(json.RawMessage, int64) {
	return func(data json.RawMessage, localTime int64) {
		var raw struct {
			E  int64      `json:"E"`
			U  int64      `json:"U"`
			U2 int64      `json:"u"` // FinalUpdateID（小写字段不能被 json 反序列化）
			B  [][]string `json:"b"`
			A  [][]string `json:"a"`
			S  string     `json:"s"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			log.Warn().Err(err).Msg("depth parse failed")
			return
		}
		bids := parseLevels(raw.B)
		asks := parseLevels(raw.A)
		select {
		case ch <- &DepthUpdate{
			Symbol:        raw.S,
			FirstUpdateID: raw.U,
			FinalUpdateID: raw.U2,
			Bids:          bids,
			Asks:          asks,
			EventTime:     raw.E,
			LocalTime:     localTime,
		}:
		default:
			log.Warn().Msg("depth channel full, dropping")
		}
	}
}

func MakeMarkPriceHandler(ch chan<- *MarkPrice) func(json.RawMessage, int64) {
	return func(data json.RawMessage, localTime int64) {
		var raw struct {
			E int64  `json:"E"`
			S string `json:"s"`
			P string `json:"p"` // mark price
			I string `json:"i"` // index price
			R string `json:"r"` // funding rate
			T int64  `json:"T"` // next funding time
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			log.Warn().Err(err).Msg("markPrice parse failed")
			return
		}
		mp, _ := decimal.NewFromString(raw.P)
		ip, _ := decimal.NewFromString(raw.I)
		fr, _ := decimal.NewFromString(raw.R)
		select {
		case ch <- &MarkPrice{
			Symbol:          raw.S,
			MarkPrice:       mp,
			IndexPrice:      ip,
			FundingRate:     fr,
			NextFundingTime: raw.T,
			EventTime:       raw.E,
			LocalTime:       localTime,
		}:
		default:
			log.Warn().Msg("markPrice channel full, dropping")
		}
	}
}

func MakeKlineHandler(ch chan<- *Kline) func(json.RawMessage, int64) {
	return func(data json.RawMessage, localTime int64) {
		var raw struct {
			E int64  `json:"E"`
			S string `json:"s"`
			K struct {
				T  int64  `json:"t"`
				T2 int64  `json:"T"`
				I  string `json:"i"`
				O  string `json:"o"`
				H  string `json:"h"`
				L  string `json:"l"`
				C  string `json:"c"`
				V  string `json:"v"`
				X  bool   `json:"x"`
			} `json:"k"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}
		k := raw.K
		o, _ := decimal.NewFromString(k.O)
		h, _ := decimal.NewFromString(k.H)
		l, _ := decimal.NewFromString(k.L)
		c, _ := decimal.NewFromString(k.C)
		v, _ := decimal.NewFromString(k.V)
		select {
		case ch <- &Kline{
			Symbol:    raw.S,
			Interval:  k.I,
			OpenTime:  k.T,
			CloseTime: k.T2,
			Open:      o,
			High:      h,
			Low:       l,
			Close:     c,
			Volume:    v,
			IsClosed:  k.X,
			EventTime: raw.E,
			LocalTime: localTime,
		}:
		default:
		}
	}
}

func parseLevels(raw [][]string) []PriceLevel {
	levels := make([]PriceLevel, 0, len(raw))
	for _, r := range raw {
		if len(r) < 2 {
			continue
		}
		p, err1 := decimal.NewFromString(r[0])
		q, err2 := decimal.NewFromString(r[1])
		if err1 != nil || err2 != nil {
			continue
		}
		levels = append(levels, PriceLevel{Price: p, Quantity: q})
	}
	return levels
}

// ValidateLatency 检查消息延迟，返回是否有效
func ValidateLatency(eventTimeMs, localTimeMs int64, thresholdMs int64) bool {
	latency := localTimeMs - eventTimeMs
	return latency <= thresholdMs
}

func NowMs() int64 {
	return time.Now().UnixMilli()
}
