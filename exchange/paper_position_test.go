package exchange

import (
	"context"
	"math"
	"testing"
)

func placeFill(t *testing.T, p *PaperExchange, side Side, ps PositionSide, qty, price float64, reduce bool) {
	t.Helper()
	p.InjectPrice("T", price)
	if _, err := p.PlaceOrder(context.Background(), OrderRequest{
		Symbol: "T", Side: side, PositionSide: ps, Type: OrderTypeMarket, Quantity: qty, ReduceOnly: reduce,
	}); err != nil {
		t.Fatal(err)
	}
}

func pos(t *testing.T, p *PaperExchange, ps PositionSide) (qty, entry float64) {
	t.Helper()
	list, _ := p.GetPositions(context.Background(), "T")
	for _, x := range list {
		if x.PositionSide == ps {
			return x.Quantity, x.EntryPrice
		}
	}
	return 0, 0
}

// 加仓按数量加权；减仓不改变成本价（回归：以前减仓也走加权公式，成本价会被放大好几倍）。
func TestPaperLongEntryPriceOnAddAndReduce(t *testing.T) {
	p := NewPaperExchange(map[string]float64{"T": 100}, "USDT", 1e6)
	placeFill(t, p, SideBuy, PositionLong, 1, 100, false)
	placeFill(t, p, SideBuy, PositionLong, 1, 110, false)
	q, e := pos(t, p, PositionLong)
	if math.Abs(q-2) > 1e-9 || math.Abs(e-105) > 1e-9 {
		t.Fatalf("加仓后应为 数量2 均价105，实际 %.4f / %.4f", q, e)
	}
	placeFill(t, p, SideSell, PositionLong, 1, 120, true) // 减仓
	q, e = pos(t, p, PositionLong)
	if math.Abs(q-1) > 1e-9 || math.Abs(e-105) > 1e-9 {
		t.Fatalf("减仓后成本价应保持105不变，数量1，实际 %.4f / %.4f", q, e)
	}
	placeFill(t, p, SideSell, PositionLong, 1, 120, true) // 平完
	if q, _ = pos(t, p, PositionLong); q != 0 {
		t.Fatalf("平完后数量应为0，实际 %.4f", q)
	}
	placeFill(t, p, SideBuy, PositionLong, 1, 90, false) // 平完后重新开仓，成本价从头算
	q, e = pos(t, p, PositionLong)
	if math.Abs(q-1) > 1e-9 || math.Abs(e-90) > 1e-9 {
		t.Fatalf("平仓后重新开仓成本价应为90，实际 %.4f / %.4f", q, e)
	}
}

// 空头：卖出加仓、买入减仓，同样减仓不改变成本价。
func TestPaperShortEntryPriceOnAddAndReduce(t *testing.T) {
	p := NewPaperExchange(map[string]float64{"T": 100}, "USDT", 1e6)
	placeFill(t, p, SideSell, PositionShort, 1, 100, false)
	placeFill(t, p, SideSell, PositionShort, 1, 110, false)
	q, e := pos(t, p, PositionShort)
	if math.Abs(q-2) > 1e-9 || math.Abs(e-105) > 1e-9 {
		t.Fatalf("空头加仓后应为 数量2 均价105，实际 %.4f / %.4f", q, e)
	}
	placeFill(t, p, SideBuy, PositionShort, 1, 95, true)
	q, e = pos(t, p, PositionShort)
	if math.Abs(q-1) > 1e-9 || math.Abs(e-105) > 1e-9 {
		t.Fatalf("空头减仓后成本价应保持105，实际 %.4f / %.4f", q, e)
	}
}
