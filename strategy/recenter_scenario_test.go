package strategy

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"gridbot/exchange"
)

// 场景测试所需的小工具 ---------------------------------------------------

func scenarioConfig(symbol string) Config {
	return Config{
		Symbol: symbol, GridCount: 8, EMAPeriod: 20, ATRPeriod: 14,
		ATRSpacingMultiplier: 0.6, MinSpacingPercent: 0.15, MaxSpacingPercent: 3,
		RecenterThresholdGrids: 6, MinRecenterIntervalSec: 0,
		PerGridQuoteAmount: 50, Leverage: 3, Mode: ModeLongOnly,
		MaxTotalPositionQuote: 5000, MarketType: "futures",
	}
}

// pushKlines 让模拟盘在当前价位附近追加若干根K线（波动率压到几乎为0），
// 这样 EMA 中心才会跟着价格移动，模拟"行情走了一段时间之后"的真实情况。
func pushKlines(p *exchange.PaperExchange, symbol string, n int) {
	p.SetVolatility(1e-9)
	for i := 0; i < n; i++ {
		p.Tick(symbol)
	}
}

func dumpEngine(t *testing.T, e *Engine, p *exchange.PaperExchange, title string) {
	t.Helper()
	ctx := context.Background()
	t.Logf("---- %s ----", title)
	t.Logf("center=%.4f spacing=%.4f recenterCount=%d", e.center, e.spacing, e.recenterCount)
	idxs := make([]int, 0, len(e.levels))
	for i := range e.levels {
		idxs = append(idxs, i)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(idxs)))
	for _, i := range idxs {
		l := e.levels[i]
		if l.Status == LevelEmpty {
			continue
		}
		t.Logf("  level %5d price=%.4f status=%-10s exit=%-5v pair=%-5d qty=%.4f filledPx=%.4f short=%v",
			i, l.Price, l.Status, l.IsExitOrder, l.PairWithIndex, l.FilledQty, l.FilledPrice, l.IsShort)
	}
	orders, _ := p.GetOpenOrders(ctx, e.cfg.Symbol)
	buys, sells := 0, 0
	for _, o := range orders {
		if o.Side == exchange.SideBuy {
			buys++
		} else {
			sells++
		}
	}
	pos, _ := p.GetPositions(ctx, e.cfg.Symbol)
	realQty := 0.0
	for _, x := range pos {
		realQty += x.Quantity
	}
	t.Logf("  交易所: 挂买单=%d 挂卖单(止盈)=%d 真实持仓数量=%.4f | 引擎记账持仓数量=%.4f",
		buys, sells, realQty, trackedLongQty(e))
}

func trackedLongQty(e *Engine) float64 {
	total := 0.0
	for _, l := range e.levels {
		if l.Status == LevelFilled && !l.IsShort {
			total += l.FilledQty
		}
	}
	return total
}

// 关键不变式：交易所上每一份多头持仓，都必须有止盈卖单覆盖；引擎记账必须与
// 交易所真实持仓一致；且引擎里不能出现任何"错误"事件。
func checkInvariants(t *testing.T, e *Engine, p *exchange.PaperExchange, events []Event, title string) {
	t.Helper()
	ctx := context.Background()
	for _, ev := range events {
		if ev.Type == "error" {
			t.Errorf("[%s] 出现错误事件: %s", title, ev.Message)
		}
	}
	pos, _ := p.GetPositions(ctx, e.cfg.Symbol)
	realQty := 0.0
	for _, x := range pos {
		realQty += x.Quantity
	}
	tracked := trackedLongQty(e)
	if diff := realQty - tracked; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("[%s] 引擎记账持仓(%.6f) 与交易所真实持仓(%.6f) 不一致", title, tracked, realQty)
	}
	orders, _ := p.GetOpenOrders(ctx, e.cfg.Symbol)
	sellQty := 0.0
	for _, o := range orders {
		if o.Side == exchange.SideSell {
			sellQty += o.Quantity
			// 止盈价必须高于对应持仓的成本，否则等于主动锁定亏损
		}
	}
	if realQty-sellQty > 1e-6 {
		t.Errorf("[%s] 有 %.6f 的持仓没有止盈单覆盖（持仓=%.6f 止盈单合计=%.6f）", title, realQty-sellQty, realQty, sellQty)
	}
	// 止盈单价格必须高于持仓最低成本价以上：逐笔检查引擎里记录的配对关系
	for _, l := range e.levels {
		if l.Status == LevelOrderOpen && l.IsExitOrder {
			entry, ok := e.levels[l.PairWithIndex]
			if !ok || entry.Status != LevelFilled {
				t.Errorf("[%s] 止盈单(level=%d)配对的建仓层 %d 不存在或不是持仓状态", title, l.Index, l.PairWithIndex)
				continue
			}
			if l.Price <= entry.FilledPrice {
				t.Errorf("[%s] 止盈单价格 %.4f 不高于持仓成本 %.4f，会直接锁定亏损", title, l.Price, entry.FilledPrice)
			}
		}
	}
}

func joinErrs(events []Event) string {
	var sb strings.Builder
	for _, ev := range events {
		if ev.Type == "error" {
			sb.WriteString(ev.Message)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// newScenario 创建一个"中心已经贴近当前价"的干净场景：先在当前价位追加足够多的
// K线，让 EMA 中心等于当前价，再初始化网格。
func newScenario(t *testing.T, sym string, price float64) (*Engine, *exchange.PaperExchange) {
	t.Helper()
	p := exchange.NewPaperExchange(map[string]float64{sym: price}, "USDT", 1_000_000)
	pushKlines(p, sym, 80)
	e := NewEngine(scenarioConfig(sym))
	if _, err := e.Initialize(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return e, p
}

// TestScenarioDropThenRecenter 复现现场问题：价格下跌多层成交，随后触发重新居中，
// 新中心比旧持仓成本低，旧持仓在新网格里落到"正数层"。
func TestScenarioDropThenRecenter(t *testing.T) {
	ctx := context.Background()
	sym := "SIMUSDT"
	e, p := newScenario(t, sym, 1000)
	dumpEngine(t, e, p, "初始化后")

	tk, _ := p.GetTicker(ctx, sym)
	drop := tk.Price - 6.5*e.spacing
	p.InjectPrice(sym, drop)
	events, err := e.OnTick(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	dumpEngine(t, e, p, fmt.Sprintf("下跌到 %.2f 并处理成交后", drop))
	checkInvariants(t, e, p, events, "下跌成交后")

	// 行情在低位盘整一段时间，K线让 EMA 中心下移，触发重新居中
	pushKlines(p, sym, 40)
	var all []Event
	for i := 0; i < 5; i++ {
		evs, err := e.OnTick(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
	}
	dumpEngine(t, e, p, "低位盘整+重新居中之后")
	if e.recenterCount == 0 {
		t.Fatalf("场景应该触发过重新居中，但 recenterCount=0")
	}
	t.Logf("累计错误事件:\n%s", joinErrs(all))
	checkInvariants(t, e, p, all, "重新居中后")
}

// ---------------------------------------------------------------------------
// 以下是更多场景，覆盖 重新居中之后的止盈结算 / 重启认领 / 强平收尾 / 失败重试 / 中性模式
// ---------------------------------------------------------------------------

func mustTick(t *testing.T, e *Engine, p *exchange.PaperExchange) []Event {
	t.Helper()
	evs, err := e.OnTick(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func openOrdersBy(p *exchange.PaperExchange, sym string, side exchange.Side) []exchange.Order {
	orders, _ := p.GetOpenOrders(context.Background(), sym)
	var out []exchange.Order
	for _, o := range orders {
		if o.Side == side {
			out = append(out, o)
		}
	}
	return out
}

// 跌 -> 多笔成交 -> 重新居中 -> 反弹：所有止盈单自然成交，已实现盈亏为正，
// 溢出层的持仓被正确释放，最后账上不剩任何持仓/止盈单。
func TestScenarioRecenterThenRally(t *testing.T) {
	ctx := context.Background()
	sym := "SIMUSDT"
	e, p := newScenario(t, sym, 1000)
	tk, _ := p.GetTicker(ctx, sym)
	p.InjectPrice(sym, tk.Price-6.5*e.spacing)
	mustTick(t, e, p)
	pushKlines(p, sym, 40)
	var all []Event
	for i := 0; i < 4; i++ {
		all = append(all, mustTick(t, e, p)...)
	}
	if e.recenterCount == 0 {
		t.Fatal("应该触发过重新居中")
	}
	checkInvariants(t, e, p, all, "重新居中后")

	// 反弹到所有止盈价之上
	p.InjectPrice(sym, 1020)
	all = all[:0]
	for i := 0; i < 3; i++ {
		all = append(all, mustTick(t, e, p)...)
	}
	dumpEngine(t, e, p, "反弹止盈之后")
	checkInvariants(t, e, p, all, "反弹止盈后")
	if e.realizedPnL <= 0 {
		t.Errorf("反弹止盈后已实现盈亏应为正，实际=%.6f", e.realizedPnL)
	}
	if q := trackedLongQty(e); q > 1e-9 {
		t.Errorf("止盈后不应再有持仓记账，实际=%.6f", q)
	}
	for idx, l := range e.levels {
		if l.Status == LevelFilled || l.IsExitOrder {
			t.Errorf("止盈后不应残留持仓/止盈层: level=%d %+v", idx, *l)
		}
		if idx < -e.cfg.GridCount {
			t.Errorf("溢出层 %d 没有被释放", idx)
		}
	}
	if n := len(openOrdersBy(p, sym, exchange.SideSell)); n != 0 {
		t.Errorf("止盈后交易所不应残留卖单，实际=%d", n)
	}
	t.Logf("已实现盈亏=%.4f", e.realizedPnL)
}

// 每个 tick 反复兜底扫描，不能重复挂出止盈单（幂等）。
func TestScenarioEnsureTakeProfitIsIdempotent(t *testing.T) {
	ctx := context.Background()
	sym := "SIMUSDT"
	e, p := newScenario(t, sym, 1000)
	tk, _ := p.GetTicker(ctx, sym)
	p.InjectPrice(sym, tk.Price-3.5*e.spacing)
	mustTick(t, e, p)
	before := len(openOrdersBy(p, sym, exchange.SideSell))
	for i := 0; i < 20; i++ {
		mustTick(t, e, p)
	}
	after := len(openOrdersBy(p, sym, exchange.SideSell))
	if before == 0 || before != after {
		t.Errorf("止盈单数量应稳定不变: 之前=%d 之后=%d", before, after)
	}
}

// 重启：全新 Engine，交易所上已有 6 笔持仓的止盈单 + 遗留基础买单。
// 期望：基础买单被清理（不叠加重复买单）；6 张止盈单原样认领、一张不撤、不多挂；
// 6 笔持仓被还原成 6 笔而不是合并成 1 笔。
func TestScenarioRestartAdoptsExitsAndCleansEntries(t *testing.T) {
	ctx := context.Background()
	sym := "SIMUSDT"
	e1, p := newScenario(t, sym, 1000)
	tk, _ := p.GetTicker(ctx, sym)
	p.InjectPrice(sym, tk.Price-6.5*e1.spacing)
	mustTick(t, e1, p)

	sellsBefore := openOrdersBy(p, sym, exchange.SideSell)
	idsBefore := map[string]bool{}
	for _, o := range sellsBefore {
		idsBefore[o.ExchangeOrderID] = true
	}
	posBefore, _ := p.GetPositions(ctx, sym)
	realQty := posBefore[0].Quantity
	if len(sellsBefore) != 6 {
		t.Fatalf("前置条件不满足：期望 6 张止盈单，实际 %d", len(sellsBefore))
	}

	// 模拟重启
	e2 := NewEngine(scenarioConfig(sym))
	events, err := e2.Initialize(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		t.Logf("[%s] %s", ev.Type, ev.Message)
	}
	checkInvariants(t, e2, p, events, "重启后")
	dumpEngine(t, e2, p, "重启认领之后")

	sellsAfter := openOrdersBy(p, sym, exchange.SideSell)
	if len(sellsAfter) != len(sellsBefore) {
		t.Errorf("止盈单数量应保持 %d 张不变，实际 %d", len(sellsBefore), len(sellsAfter))
	}
	for _, o := range sellsAfter {
		if !idsBefore[o.ExchangeOrderID] {
			t.Errorf("出现了新挂的止盈单 %s：旧止盈单应该原样认领，而不是重挂", o.ExchangeOrderID)
		}
	}
	entries := 0
	for _, l := range e2.levels {
		if l.Status == LevelFilled {
			entries++
		}
	}
	if entries != 6 {
		t.Errorf("重启后应还原出 6 笔持仓，实际 %d 笔", entries)
	}
	if diff := trackedLongQty(e2) - realQty; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("认领后记账数量 %.6f 与真实持仓 %.6f 不一致", trackedLongQty(e2), realQty)
	}
	// 买单：只有新网格的 8 张，旧的必须已被撤掉，不能叠加
	if buys := len(openOrdersBy(p, sym, exchange.SideBuy)); buys > 8 {
		t.Errorf("买单出现重复叠加：%d 张（新网格最多 8 张）", buys)
	}
}

// 重启时交易所持仓比遗留止盈单覆盖的更多（比如止盈单曾经挂失败）：
// 多出来的部分要单独认领并补挂止盈单，总覆盖量等于持仓量。
func TestScenarioRestartWithUncoveredRemainder(t *testing.T) {
	ctx := context.Background()
	sym := "SIMUSDT"
	e1, p := newScenario(t, sym, 1000)
	tk, _ := p.GetTicker(ctx, sym)
	p.InjectPrice(sym, tk.Price-3.5*e1.spacing)
	mustTick(t, e1, p)
	// 手动撤掉其中一张止盈单，制造"有一笔持仓没有止盈单"
	sells := openOrdersBy(p, sym, exchange.SideSell)
	if len(sells) < 2 {
		t.Fatal("前置条件不满足")
	}
	_ = p.CancelOrder(ctx, sym, sells[0].ExchangeOrderID)

	e2 := NewEngine(scenarioConfig(sym))
	events, err := e2.Initialize(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		t.Logf("[%s] %s", ev.Type, ev.Message)
	}
	checkInvariants(t, e2, p, events, "重启后(有未覆盖持仓)")
	gotAdopt, gotRemainder := false, false
	for _, ev := range events {
		if strings.Contains(ev.Message, "认领了上一次遗留的止盈单") {
			gotAdopt = true
		}
		if strings.Contains(ev.Message, "启动时发现交易所遗留持仓") {
			gotRemainder = true
		}
	}
	if !gotAdopt || !gotRemainder {
		t.Errorf("应同时出现\"认领遗留止盈单\"和\"接管未覆盖持仓\"两类事件: adopt=%v remainder=%v", gotAdopt, gotRemainder)
	}
	entries, exits := 0, 0
	for _, l := range e2.levels {
		if l.Status == LevelFilled {
			entries++
		}
		if l.IsExitOrder && l.Status == LevelOrderOpen {
			exits++
		}
	}
	if entries != exits || entries < 3 {
		t.Errorf("每笔持仓都应有一张止盈单: 持仓=%d 止盈单=%d", entries, exits)
	}
}

// 强平收尾：风控把多头市价平掉之后，ResetPositionSide 必须清空持仓记录并撤掉对应止盈单，
// 但不能动其它挂单（基础买单）。
func TestScenarioResetPositionSide(t *testing.T) {
	ctx := context.Background()
	sym := "SIMUSDT"
	e, p := newScenario(t, sym, 1000)
	tk, _ := p.GetTicker(ctx, sym)
	p.InjectPrice(sym, tk.Price-3.5*e.spacing)
	mustTick(t, e, p)
	buysBefore := len(openOrdersBy(p, sym, exchange.SideBuy))

	// 模拟风控市价平仓
	pos, _ := p.GetPositions(ctx, sym)
	_, err := p.PlaceOrder(ctx, exchange.OrderRequest{Symbol: sym, Side: exchange.SideSell,
		PositionSide: exchange.PositionLong, Type: exchange.OrderTypeMarket, Quantity: pos[0].Quantity, ReduceOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	evs := e.ResetPositionSide(ctx, p, exchange.PositionLong)
	for _, ev := range evs {
		t.Logf("[%s] %s", ev.Type, ev.Message)
	}
	if q := trackedLongQty(e); q != 0 {
		t.Errorf("强平后不应再有持仓记账: %.6f", q)
	}
	if n := len(openOrdersBy(p, sym, exchange.SideSell)); n != 0 {
		t.Errorf("强平后止盈单应全部撤销，实际剩 %d 张", n)
	}
	if n := len(openOrdersBy(p, sym, exchange.SideBuy)); n != buysBefore {
		t.Errorf("强平收尾不应动基础买单：之前 %d 张，之后 %d 张", buysBefore, n)
	}
}

// failingExchange 包一层模拟盘：前 N 次"减仓单"下单失败，用来验证失败重试与日志限流。
type failingExchange struct {
	*exchange.PaperExchange
	failsLeft int
	attempts  int
}

func (f *failingExchange) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (*exchange.Order, error) {
	if req.ReduceOnly {
		f.attempts++
		if f.failsLeft > 0 {
			f.failsLeft--
			return nil, fmt.Errorf("模拟交易所拒单 [-2022]: ReduceOnly Order is rejected")
		}
	}
	return f.PaperExchange.PlaceOrder(ctx, req)
}

func TestScenarioTakeProfitFailureRetriesWithBackoff(t *testing.T) {
	ctx := context.Background()
	sym := "SIMUSDT"
	p := exchange.NewPaperExchange(map[string]float64{sym: 1000}, "USDT", 1_000_000)
	pushKlines(p, sym, 80)
	fx := &failingExchange{PaperExchange: p, failsLeft: 3}
	e := NewEngine(scenarioConfig(sym))
	if _, err := e.Initialize(ctx, fx); err != nil {
		t.Fatal(err)
	}
	tk, _ := p.GetTicker(ctx, sym)
	p.InjectPrice(sym, tk.Price-1.5*e.spacing) // 只成交第 1 层
	var errCount int
	count := func(evs []Event) {
		for _, ev := range evs {
			if ev.Type == "error" {
				errCount++
			}
		}
	}
	evs, _ := e.OnTick(ctx, fx)
	count(evs)
	if len(openOrdersBy(p, sym, exchange.SideSell)) != 0 {
		t.Fatal("第一次下单应失败")
	}
	// 退避期内连续多个 tick：不应再尝试，也不应刷新的错误日志
	attemptsAfterFirst := fx.attempts
	for i := 0; i < 10; i++ {
		evs, _ := e.OnTick(ctx, fx)
		count(evs)
	}
	if fx.attempts != attemptsAfterFirst {
		t.Errorf("退避期内不应再次尝试下单：首次后=%d 现在=%d", attemptsAfterFirst, fx.attempts)
	}
	if errCount != 1 {
		t.Errorf("退避期内错误日志应只有 1 条，实际 %d 条", errCount)
	}
	// 退避结束后自动重试，直到成功
	for round := 0; round < 5 && len(openOrdersBy(p, sym, exchange.SideSell)) == 0; round++ {
		for k := range e.tpRetryAfter {
			e.tpRetryAfter[k] = time.Now().Add(-time.Second)
		}
		evs, _ := e.OnTick(ctx, fx)
		count(evs)
	}
	if n := len(openOrdersBy(p, sym, exchange.SideSell)); n != 1 {
		t.Fatalf("重试后应成功挂出 1 张止盈单，实际 %d 张", n)
	}
	checkInvariants(t, e, p, nil, "重试成功后")
}

// 中性模式（空头方向）：上涨开空 -> 止盈是买单且价格低于成本；重新居中（中心上移，
// 空头成本落到中心下方）后持仓与止盈单一笔不丢；回落后空头止盈，已实现盈亏为正；
// 空头浮动盈亏方向正确（价格高于成本 => 浮亏）。
func TestScenarioNeutralShortSide(t *testing.T) {
	ctx := context.Background()
	sym := "SIMUSDT"
	p := exchange.NewPaperExchange(map[string]float64{sym: 1000}, "USDT", 1_000_000)
	pushKlines(p, sym, 80)
	cfg := scenarioConfig(sym)
	cfg.Mode = ModeNeutral
	e := NewEngine(cfg)
	if _, err := e.Initialize(ctx, p); err != nil {
		t.Fatal(err)
	}
	tk, _ := p.GetTicker(ctx, sym)
	p.InjectPrice(sym, tk.Price+6.5*e.spacing) // 涨：多层空单成交
	mustTick(t, e, p)
	dumpEngine(t, e, p, "中性模式：上涨开空之后")

	shorts := 0
	for _, l := range e.levels {
		if l.Status == LevelFilled {
			if !l.IsShort {
				t.Errorf("上涨行情里不应有多头持仓: %+v", *l)
			}
			shorts++
		}
		if l.IsExitOrder && l.Status == LevelOrderOpen {
			entry := e.levels[l.PairWithIndex]
			if entry == nil || !entry.IsShort {
				t.Errorf("止盈单 %d 应配对一笔空头持仓", l.Index)
			} else if l.Price >= entry.FilledPrice {
				t.Errorf("空头止盈价 %.4f 应低于成本 %.4f", l.Price, entry.FilledPrice)
			}
		}
	}
	if shorts < 3 {
		t.Fatalf("应有多笔空头持仓，实际 %d", shorts)
	}
	buyExits := len(openOrdersBy(p, sym, exchange.SideBuy))
	cur, _ := p.GetTicker(ctx, sym)
	if pnl := e.UnrealizedPnL(cur.Price); pnl >= 0 {
		t.Errorf("价格高于空头成本，应为浮亏，实际 %.4f", pnl)
	}

	// 高位盘整 -> 中心上移 -> 重新居中
	pushKlines(p, sym, 40)
	for i := 0; i < 4; i++ {
		mustTick(t, e, p)
	}
	if e.recenterCount == 0 {
		t.Fatal("应触发重新居中")
	}
	trackedShort := 0.0
	for _, l := range e.levels {
		if l.Status == LevelFilled && l.IsShort {
			trackedShort += l.FilledQty
		}
	}
	pos, _ := p.GetPositions(ctx, sym)
	realShort := 0.0
	for _, x := range pos {
		if x.PositionSide == exchange.PositionShort {
			realShort += x.Quantity
		}
	}
	if d := trackedShort - realShort; d > 1e-6 || d < -1e-6 {
		t.Errorf("重新居中后空头记账 %.6f 与真实 %.6f 不一致", trackedShort, realShort)
	}
	exitsAfter := 0
	for _, l := range e.levels {
		if l.IsExitOrder && l.Status == LevelOrderOpen {
			exitsAfter++
		}
	}
	if exitsAfter != shorts {
		t.Errorf("重新居中后止盈单应为 %d 张，实际 %d", shorts, exitsAfter)
	}

	// 回落到低位：空头止盈，已实现盈亏为正
	p.InjectPrice(sym, 980)
	for i := 0; i < 3; i++ {
		mustTick(t, e, p)
	}
	if e.realizedPnL <= 0 {
		t.Errorf("空头止盈后已实现盈亏应为正，实际 %.6f", e.realizedPnL)
	}
	t.Logf("空头止盈订单数(重新居中前)=%d 已实现盈亏=%.4f", buyExits, e.realizedPnL)
}

// 重新居中之后落到溢出层的多头，浮动盈亏必须按"多头"计算（价格低于成本 => 浮亏）。
func TestScenarioOverflowLongUnrealizedPnLSign(t *testing.T) {
	ctx := context.Background()
	sym := "SIMUSDT"
	e, p := newScenario(t, sym, 1000)
	tk, _ := p.GetTicker(ctx, sym)
	p.InjectPrice(sym, tk.Price-6.5*e.spacing)
	mustTick(t, e, p)
	pushKlines(p, sym, 40)
	for i := 0; i < 4; i++ {
		mustTick(t, e, p)
	}
	cur, _ := p.GetTicker(ctx, sym)
	pnl := e.UnrealizedPnL(cur.Price)
	if pnl >= 0 {
		t.Errorf("价格低于所有持仓成本，多头应为浮亏，实际浮动盈亏=%.4f", pnl)
	}
}

// ClientOrderID 不能超过币安的 36 字符限制（止盈单层号现在是 4 位数）。
func TestClientOrderIDLength(t *testing.T) {
	e := NewEngine(Config{Symbol: "1000SHIBUSDT"})
	for _, seq := range []int64{1, 99999, 12345678} {
		id := fmt.Sprintf("%s-TP-%d-%s-%d", e.cfg.Symbol, exitIndexBase+30, e.instanceID, seq)
		if len(id) > 36 {
			t.Errorf("ClientOrderID 超长(%d>36): %s", len(id), id)
		}
	}
}
