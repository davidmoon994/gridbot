package manager

import (
	"context"
	"math"
	"testing"
	"time"

	"gridbot/exchange"
	"gridbot/risk"
	"gridbot/strategy"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// 各个窗口 = 当前累计 - 窗口起点那一刻的累计；起点之前没有记录时按0算。
func TestPnLWindows(t *testing.T) {
	st := newTestStore(t)
	mgr := New(risk.NewEngine(risk.DefaultLimits()), st)
	now := time.Now()
	day := 24 * time.Hour

	// 模拟一年多的累计已实现盈亏曲线
	points := []struct {
		ago time.Duration
		v   float64
	}{
		{400 * day, 0}, {200 * day, 10}, {100 * day, 20}, {10 * day, 30},
		{2 * day, 40}, {1 * time.Hour, 45}, {0, 50},
	}
	for _, p := range points {
		if err := st.RecordPnLSnapshot("AAAUSDT", p.v, 0, now.Add(-p.ago)); err != nil {
			t.Fatal(err)
		}
	}
	w, found, err := mgr.PnLWindows("AAAUSDT")
	if err != nil || !found {
		t.Fatalf("应能算出窗口盈亏: found=%v err=%v", found, err)
	}
	want := map[string]float64{"24h": 10, "7d": 20, "90d": 30, "180d": 40, "365d": 50}
	got := map[string]float64{"24h": w.Last24h, "7d": w.Last7d, "90d": w.Last90d, "180d": w.Last180d, "365d": w.Last365d}
	for k, v := range want {
		if !near(got[k], v) {
			t.Errorf("窗口 %s: 期望 %.2f 实际 %.2f", k, v, got[k])
		}
	}

	// 历史比窗口还短：全部累计都算在窗口内
	_ = st.RecordPnLSnapshot("NEWUSDT", 5, 0, now.Add(-time.Hour))
	w, found, _ = mgr.PnLWindows("NEWUSDT")
	if !found || !near(w.Last24h, 5) || !near(w.Last365d, 5) {
		t.Errorf("历史比窗口短时应全部计入: %+v found=%v", w, found)
	}

	// 完全没有历史
	if _, found, _ := mgr.PnLWindows("NOPEUSDT"); found {
		t.Error("没有任何历史记录时 found 应为 false")
	}
}

// 网格停止之后窗口盈亏不归零、不变化；重启（新建引擎）后累计盈亏从历史接续而不是从0开始。
func TestRealizedPnLSurvivesStopAndRestart(t *testing.T) {
	st := newTestStore(t)
	paperEx := exchange.NewPaperExchange(map[string]float64{"TESTUSDT": 1000}, "USDT", 100000)
	mgr := New(risk.NewEngine(risk.DefaultLimits()), st)
	mgr.SetExchange(paperEx, "paper", false, "USDT")

	// 重启前已经攒下的累计已实现盈亏
	_ = st.RecordPnLSnapshot("TESTUSDT", 15.13, 0, time.Now().Add(-3*time.Hour))

	cfg := strategy.Config{
		Symbol: "TESTUSDT", GridCount: 4, EMAPeriod: 20, ATRPeriod: 14,
		ATRSpacingMultiplier: 0.6, MinSpacingPercent: 0.5, MaxSpacingPercent: 3,
		RecenterThresholdGrids: 6, MinRecenterIntervalSec: 900,
		PerGridQuoteAmount: 50, Leverage: 5, Mode: strategy.ModeLongOnly, MaxTotalPositionQuote: 2000,
	}
	if _, err := mgr.StartGrid(cfg, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	snap, ok := mgr.Snapshot(context.Background(), "TESTUSDT")
	if !ok {
		t.Fatal("应能取到快照")
	}
	if !near(snap.RealizedPnL, 15.13) {
		t.Errorf("重新启动后累计已实现盈亏应接续历史 15.13，实际 %.4f（被归零了）", snap.RealizedPnL)
	}
	if !near(snap.Leverage, 5) {
		t.Errorf("运行中应显示配置的杠杆 5，实际 %.2f", snap.Leverage)
	}

	mgr.StopGrid("TESTUSDT")
	snap, _ = mgr.Snapshot(context.Background(), "TESTUSDT")
	if !near(snap.Leverage, 0) {
		t.Errorf("网格停止后杠杆应显示 0，实际 %.2f", snap.Leverage)
	}
	if !near(snap.RealizedPnL, 15.13) {
		t.Errorf("网格停止后累计已实现盈亏不应归零，实际 %.4f", snap.RealizedPnL)
	}
	w, found, _ := mgr.PnLWindows("TESTUSDT")
	if !found || !near(w.Last24h, 15.13) {
		t.Errorf("停止后窗口盈亏应保持，实际 %+v found=%v", w, found)
	}
}
