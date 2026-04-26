package execution

import (
	"sync"
	"time"

	"github.com/shopspring/decimal"

	"github.com/yourorg/eth-perp-system/internal/datafeed"
)

// Position 当前持仓状态（唯一真相来源）
type Position struct {
	Symbol    string
	Direction datafeed.Direction
	Quantity  decimal.Decimal // 0 = 无仓
	EntryPrice decimal.Decimal
	EntryTime  int64 // ms

	StopLossOrderID   string
	TakeProfitOrderID string

	UnrealizedPnL    decimal.Decimal
	UnrealizedPnLPct decimal.Decimal
}

func (p *Position) IsFlat() bool {
	return p.Quantity.IsZero()
}

func (p *Position) DirectionSign() int {
	if p.Direction == datafeed.DirectionLong {
		return 1
	}
	return -1
}

// PositionManager 持仓管理器（单仓位约束）
type PositionManager struct {
	mu       sync.RWMutex
	position Position
}

func NewPositionManager(symbol string) *PositionManager {
	return &PositionManager{
		position: Position{Symbol: symbol},
	}
}

// Snapshot 返回只读快照
func (pm *PositionManager) Snapshot() Position {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.position
}

// IsFlat 是否无持仓
func (pm *PositionManager) IsFlat() bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.position.IsFlat()
}

// Open 建仓（fill 确认后调用）
func (pm *PositionManager) Open(dir datafeed.Direction, qty, fillPrice decimal.Decimal) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.position = Position{
		Symbol:     pm.position.Symbol,
		Direction:  dir,
		Quantity:   qty,
		EntryPrice: fillPrice,
		EntryTime:  time.Now().UnixMilli(),
	}
}

// SetGuardOrders 设置兜底订单 ID
func (pm *PositionManager) SetGuardOrders(slOrderID, tpOrderID string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.position.StopLossOrderID = slOrderID
	pm.position.TakeProfitOrderID = tpOrderID
}

// UpdatePnL 用 markPrice 更新未实现盈亏
func (pm *PositionManager) UpdatePnL(markPrice decimal.Decimal) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	p := &pm.position
	if p.IsFlat() || p.EntryPrice.IsZero() {
		return
	}
	diff := markPrice.Sub(p.EntryPrice).Div(p.EntryPrice)
	sign := decimal.NewFromInt(int64(p.DirectionSign()))
	p.UnrealizedPnLPct = diff.Mul(sign).Mul(decimal.NewFromInt(100))
	p.UnrealizedPnL = diff.Mul(sign).Mul(p.EntryPrice).Mul(p.Quantity)
}

// Close 平仓，重置为 FLAT
func (pm *PositionManager) Close() (prev Position) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	prev = pm.position
	pm.position = Position{Symbol: pm.position.Symbol}
	return prev
}

// HoldingMs 持仓时长（ms）
func (pm *PositionManager) HoldingMs() int64 {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if pm.position.IsFlat() || pm.position.EntryTime == 0 {
		return 0
	}
	return time.Now().UnixMilli() - pm.position.EntryTime
}
