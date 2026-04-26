package datafeed

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

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
		ETHAggTrade:  make(chan *AggTrade, 8192),
		ETHDepth:     make(chan *DepthUpdate, 1024),
		ETHMarkPrice: make(chan *MarkPrice, 128),
		ETHKline1s:   make(chan *Kline, 128),
		ETHKline1m:   make(chan *Kline, 64),
		BTCAggTrade:  make(chan *AggTrade, 4096),
		BTCMarkPrice: make(chan *MarkPrice, 128),
	}
}

func MakeAggTradeHandler(ch chan<- *AggTrade) func(json.RawMessage, int64) {
	return func(data json.RawMessage, localTime int64) {
		fields, err := objectFields(data)
		if err != nil {
			log.Warn().Err(err).Msg("aggTrade parse failed")
			return
		}
		eventTime, err := int64Field(fields, "E")
		if err != nil {
			log.Warn().Err(err).Msg("aggTrade parse failed")
			return
		}
		aggTradeID, err := int64Field(fields, "a")
		if err != nil {
			log.Warn().Err(err).Msg("aggTrade parse failed")
			return
		}
		firstTradeID, err := int64Field(fields, "f")
		if err != nil {
			log.Warn().Err(err).Msg("aggTrade parse failed")
			return
		}
		lastTradeID, err := int64Field(fields, "l")
		if err != nil {
			log.Warn().Err(err).Msg("aggTrade parse failed")
			return
		}
		priceStr, err := stringField(fields, "p")
		if err != nil {
			log.Warn().Err(err).Msg("aggTrade parse failed")
			return
		}
		qtyStr, err := stringField(fields, "q")
		if err != nil {
			log.Warn().Err(err).Msg("aggTrade parse failed")
			return
		}
		symbol, err := stringField(fields, "s")
		if err != nil {
			log.Warn().Err(err).Msg("aggTrade parse failed")
			return
		}
		isBuyerMaker, err := boolField(fields, "m")
		if err != nil {
			log.Warn().Err(err).Msg("aggTrade parse failed")
			return
		}

		price, err1 := decimal.NewFromString(priceStr)
		qty, err2 := decimal.NewFromString(qtyStr)
		if err1 != nil || err2 != nil {
			log.Warn().Msg("aggTrade decimal parse failed")
			return
		}
		select {
		case ch <- &AggTrade{
			Symbol:       symbol,
			AggTradeID:   aggTradeID,
			Price:        price,
			Quantity:     qty,
			FirstTradeID: firstTradeID,
			LastTradeID:  lastTradeID,
			EventTime:    eventTime,
			LocalTime:    localTime,
			IsBuyerMaker: isBuyerMaker,
		}:
		default:
			log.Debug().Msg("aggTrade channel full, dropping")
		}
	}
}

func MakeDepthHandler(ch chan<- *DepthUpdate) func(json.RawMessage, int64) {
	return func(data json.RawMessage, localTime int64) {
		fields, err := objectFields(data)
		if err != nil {
			log.Warn().Err(err).Msg("depth parse failed")
			return
		}
		eventTime, err := int64Field(fields, "E")
		if err != nil {
			log.Warn().Err(err).Msg("depth parse failed")
			return
		}
		firstUpdateID, err := int64Field(fields, "U")
		if err != nil {
			log.Warn().Err(err).Msg("depth parse failed")
			return
		}
		finalUpdateID, err := int64Field(fields, "u")
		if err != nil {
			log.Warn().Err(err).Msg("depth parse failed")
			return
		}
		bidsRaw, err := levelsField(fields, "b")
		if err != nil {
			log.Warn().Err(err).Msg("depth parse failed")
			return
		}
		asksRaw, err := levelsField(fields, "a")
		if err != nil {
			log.Warn().Err(err).Msg("depth parse failed")
			return
		}
		symbol, err := stringField(fields, "s")
		if err != nil {
			log.Warn().Err(err).Msg("depth parse failed")
			return
		}

		select {
		case ch <- &DepthUpdate{
			Symbol:        symbol,
			FirstUpdateID: firstUpdateID,
			FinalUpdateID: finalUpdateID,
			Bids:          parseLevels(bidsRaw),
			Asks:          parseLevels(asksRaw),
			EventTime:     eventTime,
			LocalTime:     localTime,
		}:
		default:
			log.Debug().Msg("depth channel full, dropping")
		}
	}
}

func MakeMarkPriceHandler(ch chan<- *MarkPrice) func(json.RawMessage, int64) {
	return func(data json.RawMessage, localTime int64) {
		fields, err := objectFields(data)
		if err != nil {
			log.Warn().Err(err).Msg("markPrice parse failed")
			return
		}
		eventTime, err := int64Field(fields, "E")
		if err != nil {
			log.Warn().Err(err).Msg("markPrice parse failed")
			return
		}
		nextFundingTime, err := int64Field(fields, "T")
		if err != nil {
			log.Warn().Err(err).Msg("markPrice parse failed")
			return
		}
		symbol, err := stringField(fields, "s")
		if err != nil {
			log.Warn().Err(err).Msg("markPrice parse failed")
			return
		}
		markPriceStr, err := stringField(fields, "p")
		if err != nil {
			log.Warn().Err(err).Msg("markPrice parse failed")
			return
		}
		indexPriceStr, err := stringField(fields, "i")
		if err != nil {
			log.Warn().Err(err).Msg("markPrice parse failed")
			return
		}
		fundingRateStr, err := stringField(fields, "r")
		if err != nil {
			log.Warn().Err(err).Msg("markPrice parse failed")
			return
		}

		mp, _ := decimal.NewFromString(markPriceStr)
		ip, _ := decimal.NewFromString(indexPriceStr)
		fr, _ := decimal.NewFromString(fundingRateStr)
		select {
		case ch <- &MarkPrice{
			Symbol:          symbol,
			MarkPrice:       mp,
			IndexPrice:      ip,
			FundingRate:     fr,
			NextFundingTime: nextFundingTime,
			EventTime:       eventTime,
			LocalTime:       localTime,
		}:
		default:
			log.Warn().Msg("markPrice channel full, dropping")
		}
	}
}

func MakeKlineHandler(ch chan<- *Kline) func(json.RawMessage, int64) {
	return func(data json.RawMessage, localTime int64) {
		fields, err := objectFields(data)
		if err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}
		eventTime, err := int64Field(fields, "E")
		if err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}
		symbol, err := stringField(fields, "s")
		if err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}
		klineRaw, ok := fields["k"]
		if !ok {
			log.Warn().Err(fmt.Errorf("missing field %q", "k")).Msg("kline parse failed")
			return
		}
		kFields, err := objectFields(klineRaw)
		if err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}
		openTime, err := int64Field(kFields, "t")
		if err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}
		closeTime, err := int64Field(kFields, "T")
		if err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}
		interval, err := stringField(kFields, "i")
		if err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}
		openStr, err := stringField(kFields, "o")
		if err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}
		highStr, err := stringField(kFields, "h")
		if err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}
		lowStr, err := stringField(kFields, "l")
		if err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}
		closeStr, err := stringField(kFields, "c")
		if err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}
		volumeStr, err := stringField(kFields, "v")
		if err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}
		isClosed, err := boolField(kFields, "x")
		if err != nil {
			log.Warn().Err(err).Msg("kline parse failed")
			return
		}

		o, _ := decimal.NewFromString(openStr)
		h, _ := decimal.NewFromString(highStr)
		l, _ := decimal.NewFromString(lowStr)
		c, _ := decimal.NewFromString(closeStr)
		v, _ := decimal.NewFromString(volumeStr)
		select {
		case ch <- &Kline{
			Symbol:    symbol,
			Interval:  interval,
			OpenTime:  openTime,
			CloseTime: closeTime,
			Open:      o,
			High:      h,
			Low:       l,
			Close:     c,
			Volume:    v,
			IsClosed:  isClosed,
			EventTime: eventTime,
			LocalTime: localTime,
		}:
		default:
		}
	}
}

func objectFields(data json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	return fields, nil
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

func levelsField(fields map[string]json.RawMessage, key string) ([][]string, error) {
	raw, ok := fields[key]
	if !ok {
		return nil, fmt.Errorf("missing field %q", key)
	}
	var levels [][]string
	if err := json.Unmarshal(raw, &levels); err != nil {
		return nil, fmt.Errorf("field %q must be price levels", key)
	}
	return levels, nil
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

func ValidateLatency(eventTimeMs, localTimeMs int64, thresholdMs int64) bool {
	latency := localTimeMs - eventTimeMs
	return latency <= thresholdMs
}

func NowMs() int64 {
	return time.Now().UnixMilli()
}
