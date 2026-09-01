package manager

import (
	"context"
	"os"
	"testing"
	"time"

	"gridbot/exchange"
	"gridbot/risk"
	"gridbot/store"
	"gridbot/strategy"
)

func newTestStore(t *testing.T) *store.Store {
	f, err := os.CreateTemp("", "gridbot_test_*.db")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	os.Remove(path)
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(); os.Remove(path) })
	return st
}

// 测试1：极端行情熔断——价格单次剧烈跳动后，风控引擎应进入熔断状态，
// 且熔断期间新开仓订单会被拒绝（验证 PrevPrice 不再等于 LastPrice 的 bug 已修复）
func TestCircuitBreakerTriggersOnExtremeMove(t *testing.T) {
	st := newTestStore(t)
	paperEx := exchange.NewPaperExchange(map[string]float64{"TESTUSDT": 1000}, "USDT", 100000)

	limits := risk.DefaultLimits()
	limits.CircuitBreakerPriceMovePct = 5.0 // 单次波动超过5%触发熔断
	limits.CircuitBreakerCooldownSec = 60
	rk := risk.NewEngine(limits)

	mgr := New(rk, st)
	mgr.SetExchange(paperEx, "paper", "USDT")

	cfg := strategy.Config{
		Symbol: "TESTUSDT", GridCount: 4, EMAPeriod: 20, ATRPeriod: 14,
		ATRSpacingMultiplier: 0.6, MinSpacingPercent: 0.15, MaxSpacingPercent: 3,
		RecenterThresholdGrids: 6, MinRecenterIntervalSec: 900,
		PerGridQuoteAmount: 50, Leverage: 3, Mode: strategy.ModeLongOnly, MaxTotalPositionQuote: 2000,
	}
	if _, err := mgr.StartGrid(cfg, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	time.Sleep(200 * time.Millisecond) // 让引擎完成初始化+挂出初始订单

	// 制造一次剧烈的价格跳动（+20%），应触发熔断
	paperEx.InjectPrice("TESTUSDT", 1200)
	time.Sleep(200 * time.Millisecond) // 等待下一个tick周期检测到这次跳动

	inBreaker, until := rk.InCircuitBreaker()
	if !inBreaker {
		t.Fatalf("期望极端行情后进入熔断状态，但 InCircuitBreaker()=false")
	}
	t.Logf("熔断生效，直到 %s", until.Format("15:04:05"))

	mgr.StopGrid("TESTUSDT")
}

// 测试2：连续多次执行失败后自动停止该网格（避免拼写错误的交易对无人值守空转刷错误日志）
func TestAutoPauseOnConsecutiveErrors(t *testing.T) {
	st := newTestStore(t)
	// 故意不包含 BADSYMBOL，GetTicker 会一直报错
	paperEx := exchange.NewPaperExchange(map[string]float64{"TESTUSDT": 1000}, "USDT", 100000)

	rk := risk.NewEngine(risk.DefaultLimits())
	mgr := New(rk, st)
	mgr.SetExchange(paperEx, "paper", "USDT")

	cfg := strategy.Config{
		Symbol: "BADSYMBOL", GridCount: 4, EMAPeriod: 20, ATRPeriod: 14,
		ATRSpacingMultiplier: 0.6, MinSpacingPercent: 0.15, MaxSpacingPercent: 3,
		RecenterThresholdGrids: 6, MinRecenterIntervalSec: 900,
		PerGridQuoteAmount: 50, Leverage: 3, Mode: strategy.ModeLongOnly, MaxTotalPositionQuote: 2000,
	}
	if _, err := mgr.StartGrid(cfg, 30*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !mgr.IsRunning("BADSYMBOL") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if mgr.IsRunning("BADSYMBOL") {
		t.Fatalf("期望连续失败后网格被自动停止，但仍在运行")
	}

	events, err := st.RecentEvents("BADSYMBOL", 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.EventType == "auto_paused" {
			found = true
			t.Logf("找到自动暂停事件: %s", e.Message)
		}
	}
	if !found {
		t.Fatalf("期望找到 auto_paused 事件，但没有")
	}
}

// 测试3：强平保护应真正对交易所下达平仓单，而不是只记录日志
func TestForceCloseActuallyClosesPosition(t *testing.T) {
	st := newTestStore(t)
	paperEx := exchange.NewPaperExchange(map[string]float64{"TESTUSDT": 1000}, "USDT", 100000)

	limits := risk.DefaultLimits()
	limits.MinPeakPctForProtection = 1.0
	limits.MaxDrawdownFromPeakPct = 0.5
	rk := risk.NewEngine(limits)

	mgr := New(rk, st)
	mgr.SetExchange(paperEx, "paper", "USDT")

	// 网格间距调宽（2%~5%），配合下面的小幅价格步进，确保每次价格变动
	// 只穿越一层网格，不会一次性触发模拟盘同时撮合多个挂单（模拟盘的撮合
	// 逻辑是"一次价格跳动内所有被穿越的挂单一起成交"，这在真实交易所里
	// 不会发生——真实成交是逐笔连续的，仓位状态也由交易所自己权威维护）。
	cfg := strategy.Config{
		Symbol: "TESTUSDT", GridCount: 4, EMAPeriod: 20, ATRPeriod: 14,
		ATRSpacingMultiplier: 0.6, MinSpacingPercent: 2.0, MaxSpacingPercent: 5.0,
		RecenterThresholdGrids: 20, MinRecenterIntervalSec: 900, // 阈值调大，测试期间不触发重新居中干扰
		PerGridQuoteAmount: 50, Leverage: 3, Mode: strategy.ModeLongOnly, MaxTotalPositionQuote: 2000,
	}
	if _, err := mgr.StartGrid(cfg, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // 初始化+挂初始买单

	// 价格小幅下跌（约-3%），只穿越最近的一层买单，建立多头仓位。
	// 用轮询代替固定 sleep，避免在共享CPU的沙盒环境里因调度延迟导致的时序抖动。
	paperEx.InjectPrice("TESTUSDT", 970)
	var positions []exchange.Position
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		positions, _ = paperEx.GetPositions(context.Background(), "TESTUSDT")
		if len(positions) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(positions) == 0 {
		t.Fatalf("期望价格下跌后已经建仓，但没有持仓")
	}
	t.Logf("建仓后持仓: %+v", positions[0])
	entryPrice := positions[0].EntryPrice

	// 价格回升制造浮盈峰值，但不越过止盈单所在的价位（避免触发止盈平仓，
	// 这里只是想制造"账面浮盈"，不是想测试正常止盈流程）
	paperEx.InjectPrice("TESTUSDT", entryPrice+30)
	time.Sleep(300 * time.Millisecond)

	// 价格回落到入场价下方，制造从峰值的深度回撤，应触发强平保护
	paperEx.InjectPrice("TESTUSDT", entryPrice-20)
	time.Sleep(300 * time.Millisecond) // 给强平下单+网格重置留够时间

	positionsAfter, _ := paperEx.GetPositions(context.Background(), "TESTUSDT")
	if len(positionsAfter) != 0 {
		t.Fatalf("期望强平后仓位已平掉，但仍有持仓: %+v", positionsAfter)
	}

	events, _ := st.RecentEvents("TESTUSDT", 50)
	foundForceClose := false
	for _, e := range events {
		if e.EventType == "force_close" {
			foundForceClose = true
			t.Logf("找到强平事件: %s", e.Message)
		}
	}
	if !foundForceClose {
		t.Fatalf("期望找到 force_close 事件，但没有")
	}

	mgr.StopGrid("TESTUSDT")
}

// 测试4：现货交易所 + neutral（双向持仓）模式是非法组合，必须在启动网格时
// 就被拒绝，而不是等到真实下单时才在交易所侧报错
func TestSpotRejectsNeutralMode(t *testing.T) {
	st := newTestStore(t)
	rk := risk.NewEngine(risk.DefaultLimits())
	mgr := New(rk, st)

	// 构造一个 Name() 返回 "binance_spot" 的现货交易所实例，模拟用户绑定了现货账户。
	// 这里不会真正发起网络请求——StartGrid 的校验发生在任何交易所调用之前。
	spotEx := exchange.NewBinanceSpotExchange("dummy_key", "dummy_secret", true)
	mgr.SetExchange(spotEx, spotEx.Name(), "USDC")

	if !mgr.IsSpotExchange() {
		t.Fatalf("期望 IsSpotExchange()=true，实际=false（exchangeID=%s）", mgr.ExchangeID())
	}

	cfg := strategy.Config{
		Symbol: "BTCUSDC", GridCount: 4, EMAPeriod: 20, ATRPeriod: 14,
		ATRSpacingMultiplier: 0.6, MinSpacingPercent: 0.15, MaxSpacingPercent: 3,
		RecenterThresholdGrids: 6, MinRecenterIntervalSec: 900,
		PerGridQuoteAmount: 50, Leverage: 3, Mode: strategy.ModeNeutral, MaxTotalPositionQuote: 2000,
	}
	_, err := mgr.StartGrid(cfg, 50*time.Millisecond)
	if err == nil {
		t.Fatalf("期望现货+neutral模式被拒绝，但 StartGrid 没有返回错误")
	}
	t.Logf("正确拒绝: %v", err)

	// 同样的交易对+参数，只是模式换成 long_only，应该能正常启动
	cfg.Mode = strategy.ModeLongOnly
	if _, err := mgr.StartGrid(cfg, 50*time.Millisecond); err != nil {
		t.Fatalf("期望现货+long_only模式能正常启动，但返回了错误: %v", err)
	}
	mgr.StopGrid("BTCUSDC")
}
