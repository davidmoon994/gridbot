package manager

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"gridbot/exchange"
	"gridbot/risk"
	"gridbot/strategy"
)

// stopLossScenario 建立一笔多头仓位，然后让价格从成本价下跌 2.5%（杠杆 3x，对应保证金亏损 7.5%）。
// 间距故意设成 5%，保证这次下跌不会穿越下一层买单（否则会多成交一笔、让成本价变化）。
// 返回：manager、模拟盘、成本价、持仓数量。
func stopLossScenario(t *testing.T, stopLossPct float64) (*Manager, *exchange.PaperExchange, float64, float64) {
	t.Helper()
	st := newTestStore(t)
	paperEx := exchange.NewPaperExchange(map[string]float64{"TESTUSDT": 1000}, "USDT", 100000)
	mgr := New(risk.NewEngine(risk.DefaultLimits()), st) // 默认止损线 15，回撤保护关闭
	mgr.SetExchange(paperEx, "paper", false, "USDT")

	cfg := strategy.Config{
		Symbol: "TESTUSDT", GridCount: 4, EMAPeriod: 20, ATRPeriod: 14,
		ATRSpacingMultiplier: 0.6, MinSpacingPercent: 5, MaxSpacingPercent: 8,
		RecenterThresholdGrids: 20, MinRecenterIntervalSec: 900,
		PerGridQuoteAmount: 50, Leverage: 3, Mode: strategy.ModeLongOnly,
		MaxTotalPositionQuote: 2000, StopLossPct: stopLossPct,
	}
	if _, err := mgr.StartGrid(cfg, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mgr.StopGrid("TESTUSDT") })
	time.Sleep(300 * time.Millisecond)

	snap, ok := mgr.Snapshot(context.Background(), "TESTUSDT")
	if !ok || snap.Spacing <= 0 {
		t.Fatalf("网格没有初始化完成: %+v", snap)
	}
	orders, _ := paperEx.GetOpenOrders(context.Background(), "TESTUSDT")
	topBuy := 0.0
	for _, o := range orders {
		if o.Side == exchange.SideBuy && o.Price > topBuy {
			topBuy = o.Price
		}
	}
	if topBuy == 0 {
		t.Fatal("没有挂出任何买单")
	}
	paperEx.InjectPrice("TESTUSDT", topBuy-0.25*snap.Spacing)

	var pos []exchange.Position
	for d := time.Now().Add(3 * time.Second); time.Now().Before(d); time.Sleep(50 * time.Millisecond) {
		if pos, _ = paperEx.GetPositions(context.Background(), "TESTUSDT"); len(pos) > 0 {
			break
		}
	}
	if len(pos) == 0 {
		t.Fatal("没有建立持仓")
	}
	entry, qty := pos[0].EntryPrice, pos[0].Quantity
	time.Sleep(150 * time.Millisecond) // 让引擎先把止盈单挂好
	paperEx.InjectPrice("TESTUSDT", entry*0.975)
	return mgr, paperEx, entry, qty
}

func eventsText(t *testing.T, mgr *Manager) string {
	t.Helper()
	evs, _ := mgr.Store().RecentEvents("TESTUSDT", 100)
	var sb strings.Builder
	for _, e := range evs {
		sb.WriteString("[" + e.EventType + "] " + e.Message + "\n")
	}
	return sb.String()
}

// 网格自己配置的止损线(5%)比引擎默认(15%)更严：浮亏 7.5% 就该强平，
// 并且这次强平的亏损要计入已实现盈亏（以前完全不记账，卡片只反映赚的部分）。
func TestPerGridStopLossForceClosesAndRecordsRealizedLoss(t *testing.T) {
	mgr, paperEx, entry, qty := stopLossScenario(t, 5)

	closed := false
	for d := time.Now().Add(3 * time.Second); time.Now().Before(d); time.Sleep(50 * time.Millisecond) {
		if pos, _ := paperEx.GetPositions(context.Background(), "TESTUSDT"); len(pos) == 0 {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatalf("止损线 5%%、浮亏 7.5%% 应已强平，但仍有持仓。事件:\n%s", eventsText(t, mgr))
	}
	text := eventsText(t, mgr)
	if !strings.Contains(text, "绝对止损线 5.0%") {
		t.Errorf("强平原因应写明这个网格自己的止损线 5.0%%，事件:\n%s", text)
	}
	if !strings.Contains(text, "强平盈亏已计入已实现盈亏") {
		t.Errorf("应记录强平盈亏已计入，事件:\n%s", text)
	}

	snap, _ := mgr.Snapshot(context.Background(), "TESTUSDT")
	want := -0.025 * entry * qty // 平仓价 = 成本 × 0.975
	if snap.RealizedPnL >= 0 || math.Abs(snap.RealizedPnL-want) > math.Abs(want)*0.02 {
		t.Errorf("已实现盈亏应约为 %.4f（强平亏损），实际 %.4f", want, snap.RealizedPnL)
	}
	if !near(snap.StopLossPct, 5) {
		t.Errorf("运行中应显示这个网格自己的止损线 5，实际 %.2f", snap.StopLossPct)
	}
}

// 没有单独配置止损线（0）：沿用引擎默认 15%，同样的 7.5% 浮亏不会触发，已实现盈亏也不变。
func TestDefaultStopLossDoesNotTriggerAtSmallerLoss(t *testing.T) {
	mgr, paperEx, _, _ := stopLossScenario(t, 0)

	time.Sleep(800 * time.Millisecond) // 足够跑十几个 tick
	if pos, _ := paperEx.GetPositions(context.Background(), "TESTUSDT"); len(pos) == 0 {
		t.Fatalf("默认止损线 15%%，浮亏 7.5%% 不应强平，但持仓已没了。事件:\n%s", eventsText(t, mgr))
	}
	snap, _ := mgr.Snapshot(context.Background(), "TESTUSDT")
	if !near(snap.RealizedPnL, 0) {
		t.Errorf("没有强平，已实现盈亏应为 0，实际 %.4f", snap.RealizedPnL)
	}
	if !near(snap.StopLossPct, 15) {
		t.Errorf("没有单独配置时应显示引擎默认止损线 15，实际 %.2f", snap.StopLossPct)
	}

	mgr.StopGrid("TESTUSDT")
	snap, _ = mgr.Snapshot(context.Background(), "TESTUSDT")
	if !near(snap.StopLossPct, 0) {
		t.Errorf("网格停止后止损线应显示 0，实际 %.2f", snap.StopLossPct)
	}
}
