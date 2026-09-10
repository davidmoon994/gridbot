package strategy

import (
	"context"
	"strings"
	"testing"

	"gridbot/exchange"
)

// 验证修复：程序重启后（模拟：创建一个全新的Engine实例），如果交易所侧
// 存在这个symbol的真实持仓（模拟：之前的强平失败、或进程被杀掉留下的仓位），
// 新的Engine在Initialize时应该主动核对到这笔持仓，把它接管进网格状态，
// 并挂出对应的止盈单——而不是假装自己一无所有。
func TestReconcileExistingPositionsOnRestart(t *testing.T) {
	ctx := context.Background()
	paperEx := exchange.NewPaperExchange(map[string]float64{"TESTUSDT": 1000}, "USDT", 100000)

	// 第一步：模拟"重启前"的世界——直接用市价单在交易所造出一笔真实持仓，
	// 不通过Engine（模拟这笔仓位是在软件不知情的情况下产生/遗留的）
	_, err := paperEx.PlaceOrder(ctx, exchange.OrderRequest{
		Symbol: "TESTUSDT", Side: exchange.SideBuy, PositionSide: exchange.PositionLong,
		Type: exchange.OrderTypeMarket, Quantity: 0.1,
	})
	if err != nil {
		t.Fatalf("造仓失败: %v", err)
	}
	positionsBefore, _ := paperEx.GetPositions(ctx, "TESTUSDT")
	if len(positionsBefore) == 0 {
		t.Fatalf("造仓后应该能查到持仓，但没有")
	}
	t.Logf("重启前交易所真实持仓: %+v", positionsBefore[0])

	// 第二步：模拟"程序重启"——创建一个全新的Engine实例（内存状态清空），
	// 它对刚才那笔持仓一无所知，看它Initialize时能不能主动核对回来
	cfg := Config{
		Symbol: "TESTUSDT", GridCount: 8, EMAPeriod: 20, ATRPeriod: 14,
		ATRSpacingMultiplier: 0.6, MinSpacingPercent: 0.15, MaxSpacingPercent: 3,
		RecenterThresholdGrids: 6, MinRecenterIntervalSec: 900,
		PerGridQuoteAmount: 50, Leverage: 3, Mode: ModeLongOnly, MaxTotalPositionQuote: 2000,
		MarketType: "futures",
	}
	e := NewEngine(cfg)
	events, err := e.Initialize(ctx, paperEx)
	if err != nil {
		t.Fatalf("Initialize失败: %v", err)
	}

	foundReconcileEvent := false
	for _, ev := range events {
		t.Logf("事件: [%s] %s", ev.Type, ev.Message)
		if ev.Type == "info" && strings.Contains(ev.Message, "启动时发现交易所遗留持仓") {
			foundReconcileEvent = true
		}
	}
	if !foundReconcileEvent {
		t.Fatalf("期望看到'启动时发现交易所遗留持仓'的事件，但没有")
	}

	snap := e.Snapshot(1000)
	foundFilledLevel := false
	for _, lvl := range snap.Levels {
		if lvl.Status == LevelFilled && lvl.Index < 0 {
			foundFilledLevel = true
			if lvl.FilledQty < 0.099 || lvl.FilledQty > 0.101 {
				t.Fatalf("接管的持仓数量不对，期望约0.1，实际=%.6f", lvl.FilledQty)
			}
			t.Logf("网格正确接管了持仓: level=%d qty=%.6f price=%.4f", lvl.Index, lvl.FilledQty, lvl.FilledPrice)
		}
	}
	if !foundFilledLevel {
		t.Fatalf("期望网格里有一层被标记为已接管的持仓，但没有找到")
	}

	// 第三步：确认止盈单真的被挂出去了
	openOrders, _ := paperEx.GetOpenOrders(ctx, "TESTUSDT")
	foundTPOrder := false
	for _, o := range openOrders {
		if o.Side == exchange.SideSell {
			foundTPOrder = true
			t.Logf("找到止盈卖单: price=%.4f qty=%.6f", o.Price, o.Quantity)
		}
	}
	if !foundTPOrder {
		t.Fatalf("期望接管持仓后自动挂出止盈卖单，但没有找到")
	}
}
