// Package manager 是运行时调度层：为每个交易对启动一个独立的网格交易协程，
// 周期性调用策略引擎的 OnTick，把产生的事件写入存储，并通过风控包装的
// 交易所实例保证所有下单都经过硬约束校验。
//
// Manager 支持在运行期间热切换底层交易所（绑定/解绑 API Key 时调用
// SetExchange），不需要重启进程。切换交易所时会先停止所有正在运行的网格
// （避免旧交易所上的挂单/仓位被新交易所的调度循环误操作）。
package manager

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"gridbot/exchange"
	"gridbot/risk"
	"gridbot/store"
	"gridbot/strategy"
)

// maxConsecutiveErrors 是某个网格连续执行失败多少次后自动停止（保护措施：
// 避免交易对拼写错误、API失效等问题导致无人值守时一直空转刷错误日志）
const maxConsecutiveErrors = 5

// Trader 是单个交易对的运行状态
type Trader struct {
	Symbol  string
	engine  *strategy.Engine
	cancel  context.CancelFunc
	running bool

	mu sync.RWMutex
	// peakPnLPct 按持仓方向（多/空）分别记录历史最高浮盈百分比，
	// 用于风控引擎的回撤保护判断——neutral模式下多空仓位是独立的，
	// 不能共用一个峰值，否则会出现"多头刚开仓但空头已经在回撤"这类误判。
	peakPnLPct map[exchange.PositionSide]float64
	// lastPrice 记录"上一个tick周期"观察到的价格，用于风控引擎判断
	// 单次波动幅度是否触发极端行情熔断——不能用"当前价"和自己比较，
	// 那样永远不会触发。
	lastPrice float64
	// dailyBaselinePnL/dailyBaselineDate 用于计算"当日已实现盈亏"：
	// 每天第一次tick时把当时的累计已实现盈亏记为基准，之后
	// (当前累计已实现盈亏 - 基准) 就是当日盈亏，供风控引擎的
	// 每日亏损熔断使用。
	dailyBaselinePnL  float64
	dailyBaselineDate string

	consecutiveErrors int
	lastError         string
	tickInterval      time.Duration
}

// Manager 管理所有交易对的 Trader 实例，以及当前生效的交易所连接
type Manager struct {
	rk *risk.Engine
	st *store.Store

	exMu       sync.RWMutex
	rawEx      exchange.Exchange
	guardedEx  *risk.GuardedExchange
	exchangeID string // 当前交易所标识，如 "paper"/"binance_futures"/"okx"
	quoteAsset string // 当前交易所的计价货币，用于从余额列表里找到"净值"

	mu      sync.RWMutex
	traders map[string]*Trader
}

// New 创建一个尚未绑定交易所的 Manager，调用方需紧接着调用 SetExchange
// 设置初始交易所（通常是模拟盘或恢复的已绑定交易所）。
func New(rk *risk.Engine, st *store.Store) *Manager {
	return &Manager{
		rk:      rk,
		st:      st,
		traders: map[string]*Trader{},
	}
}

// SetExchange 切换当前生效的交易所。会先停止所有正在运行的网格策略，
// 因为旧交易所上未成交的挂单/仓位在切换后不再被追踪，继续运行可能导致状态不一致。
// exchangeID 用于展示和持久化标识（如 "paper"/"binance_futures"/"okx"）。
func (m *Manager) SetExchange(rawEx exchange.Exchange, exchangeID, quoteAsset string) {
	for _, sym := range m.ListSymbols() {
		m.StopGrid(sym)
	}

	m.exMu.Lock()
	defer m.exMu.Unlock()
	m.rawEx = rawEx
	m.exchangeID = exchangeID
	m.quoteAsset = quoteAsset
	m.guardedEx = risk.NewGuardedExchange(rawEx, m.rk, m.buildAccountState(), func(symbol, reason string) {
		_ = m.st.LogEvent(symbol, "risk_reject", reason, time.Now())
		log.Printf("[风控拒绝] %s: %s", symbol, reason)
	})
}

// buildAccountState 构造供风控引擎使用的账户状态查询函数。
// 注意：函数体内通过 m.RawExchange()/m.QuoteAsset() 动态读取当前交易所，
// 而不是在闭包创建时固定捕获，这样 SetExchange 切换后立刻对风控生效。
func (m *Manager) buildAccountState() risk.AccountStateProvider {
	return func(ctx context.Context, symbol string, orderQuoteAmount float64) (risk.AccountState, error) {
		rawEx := m.RawExchange()
		quoteAsset := m.QuoteAsset()

		balances, err := rawEx.GetBalances(ctx)
		if err != nil {
			return risk.AccountState{}, err
		}
		var equity float64
		for _, b := range balances {
			if b.Asset == quoteAsset {
				equity = b.Total
				break
			}
		}
		if equity == 0 && len(balances) > 0 {
			equity = balances[0].Total // 找不到指定计价资产时退化取第一个余额，避免风控因equity=0而全部拒单
		}

		m.mu.RLock()
		t, hasTrader := m.traders[symbol]
		m.mu.RUnlock()

		var trader *Trader
		if hasTrader {
			trader = t
		}
		positions, err := m.currentPositions(ctx, rawEx, trader, symbol)
		if err != nil {
			return risk.AccountState{}, err
		}
		var symbolPositionQuote, usedMargin float64
		for _, p := range positions {
			symbolPositionQuote += absF(p.Quantity) * p.MarkPrice
			usedMargin += p.MarginUsed
		}
		ticker, err := rawEx.GetTicker(ctx, symbol)
		if err != nil {
			return risk.AccountState{}, err
		}

		// PrevPrice 用"上一个tick周期"记录的价格，而不是当前价格本身，
		// 否则极端行情熔断永远不会触发（自己和自己比较，波动恒为0）。
		prevPrice := ticker.Price
		var dailyPnL float64
		var peakPct, curPct float64
		if hasTrader {
			t.mu.RLock()
			if t.lastPrice > 0 {
				prevPrice = t.lastPrice
			}
			baseline := t.dailyBaselinePnL
			baselineDate := t.dailyBaselineDate
			if len(positions) > 0 {
				peakPct = t.peakPnLPct[positions[0].PositionSide]
			}
			t.mu.RUnlock()

			today := time.Now().Format("2006-01-02")
			if baselineDate == today {
				currentRealized := t.engine.Snapshot(ticker.Price).RealizedPnL
				dailyPnL = currentRealized - baseline
			}
		}
		if len(positions) > 0 && positions[0].EntryPrice > 0 {
			curPct = (ticker.Price - positions[0].EntryPrice) / positions[0].EntryPrice * 100 * positions[0].Leverage
		}

		return risk.AccountState{
			EquityQuote:             equity,
			UsedMarginQuote:         usedMargin,
			SymbolPositionQuote:     symbolPositionQuote,
			DailyRealizedPnL:        dailyPnL,
			LastPrice:               ticker.Price,
			PrevPrice:               prevPrice,
			PeakUnrealizedPnLPct:    peakPct,
			CurrentUnrealizedPnLPct: curPct,
		}, nil
	}
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// RawExchange 暴露当前生效的原始（未包装）交易所实例，供 API 层查询余额/持仓等只读信息
func (m *Manager) RawExchange() exchange.Exchange {
	m.exMu.RLock()
	defer m.exMu.RUnlock()
	return m.rawEx
}

func (m *Manager) guardedExchange() *risk.GuardedExchange {
	m.exMu.RLock()
	defer m.exMu.RUnlock()
	return m.guardedEx
}

// ExchangeID 返回当前生效交易所的标识
func (m *Manager) ExchangeID() string {
	m.exMu.RLock()
	defer m.exMu.RUnlock()
	return m.exchangeID
}

// IsSpotExchange 判断当前生效交易所是否为现货交易所（标识以 "_spot" 结尾）
func (m *Manager) IsSpotExchange() bool {
	return strings.HasSuffix(m.ExchangeID(), "_spot")
}

// currentPositions 统一获取"当前持仓"，屏蔽现货/合约的差异：
//   - 合约：直接查询交易所返回的真实持仓；
//   - 现货：交易所没有原生"持仓"概念（钱包余额可能还混有与本网格无关的资产），
//     改用网格引擎自己的记账数据（PositionSummary）合成一个虚拟持仓，
//     PositionSide 固定为多头（现货只能做多）。
func (m *Manager) currentPositions(ctx context.Context, rawEx exchange.Exchange, t *Trader, symbol string) ([]exchange.Position, error) {
	if !m.IsSpotExchange() {
		return rawEx.GetPositions(ctx, symbol)
	}
	if t == nil {
		return nil, nil
	}
	qty, avgEntry := t.engine.PositionSummary()
	if qty <= 0 {
		return nil, nil
	}
	ticker, err := rawEx.GetTicker(ctx, symbol)
	if err != nil {
		return nil, err
	}
	return []exchange.Position{{
		Symbol:        symbol,
		PositionSide:  exchange.PositionLong,
		Quantity:      qty,
		EntryPrice:    avgEntry,
		MarkPrice:     ticker.Price,
		Leverage:      1,
		MarginUsed:    qty * avgEntry, // 现货没有杠杆，占用资金=全部名义价值
		UnrealizedPnL: (ticker.Price - avgEntry) * qty,
	}}, nil
}

// QuoteAsset 返回当前交易所的计价货币
func (m *Manager) QuoteAsset() string {
	m.exMu.RLock()
	defer m.exMu.RUnlock()
	return m.quoteAsset
}

// StartGrid 为指定交易对启动（或重启）移动网格策略
// StartGrid 为指定交易对启动（或重启）移动网格策略。
// 返回值里的 Config 是校正过 MarketType 之后的最终版本（Go 结构体按值传递，
// 调用方传入的 cfg 不会被这里的修改影响，所以需要显式返回校正结果，
// 供调用方展示/持久化时使用准确的数据，而不是调用方自己那份未校正的副本）。
func (m *Manager) StartGrid(cfg strategy.Config, tickInterval time.Duration) (strategy.Config, error) {
	// MarketType 以"当前实际生效的交易所"为准，不信任调用方传入的值——
	// 避免界面/客户端状态过期导致"现货交易所却按合约语义启动网格"这种错配。
	if m.IsSpotExchange() {
		cfg.MarketType = "spot"
	} else {
		cfg.MarketType = "futures"
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.traders[cfg.Symbol]; ok && existing.running {
		existing.cancel()
	}

	engine := strategy.NewEngine(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t := &Trader{
		Symbol:       cfg.Symbol,
		engine:       engine,
		cancel:       cancel,
		running:      true,
		tickInterval: tickInterval,
		peakPnLPct:   map[exchange.PositionSide]float64{},
	}
	m.traders[cfg.Symbol] = t

	_ = m.st.SaveGridConfig(cfg.Symbol, cfg, true)
	_ = m.st.LogEvent(cfg.Symbol, "info", "启动移动网格策略", time.Now())

	go m.runLoop(ctx, t)
	return cfg, nil
}

// StopGrid 停止某交易对的网格策略（不会自动撤单/平仓，避免误操作导致仓位失控）
func (m *Manager) StopGrid(symbol string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.traders[symbol]; ok && t.running {
		t.cancel()
		t.running = false
		_ = m.st.LogEvent(symbol, "info", "停止移动网格策略", time.Now())
	}
}

func (m *Manager) runLoop(ctx context.Context, t *Trader) {
	ticker := time.NewTicker(t.tickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.tick(ctx, t)
		}
	}
}

func (m *Manager) tick(ctx context.Context, t *Trader) {
	guardedEx := m.guardedExchange()
	rawEx := m.RawExchange()
	if guardedEx == nil || rawEx == nil {
		return
	}

	// 每天第一次tick时重置"当日已实现盈亏"基准，必须在调用 OnTick 之前完成，
	// 因为 OnTick 内部下单会触发风控校验，风控校验需要用到当天的基准值。
	today := time.Now().Format("2006-01-02")
	t.mu.Lock()
	if t.dailyBaselineDate != today {
		t.dailyBaselineDate = today
		t.dailyBaselinePnL = t.engine.Snapshot(0).RealizedPnL
	}
	t.mu.Unlock()

	// 独立检测一次极端行情，不依赖本轮是否恰好有新订单要下——
	// 否则熔断完全取决于"这个tick周期网格引擎正好要不要挂新单"这个偶然条件，极不可靠。
	if currentTicker, tickerErr := rawEx.GetTicker(ctx, t.Symbol); tickerErr == nil {
		t.mu.RLock()
		prevPrice := t.lastPrice
		t.mu.RUnlock()
		m.rk.CheckPriceMove(currentTicker.Price, prevPrice)
	}

	events, err := t.engine.OnTick(ctx, guardedEx)
	if err != nil {
		t.mu.Lock()
		t.lastError = err.Error()
		t.consecutiveErrors++
		count := t.consecutiveErrors
		t.mu.Unlock()
		_ = m.st.LogEvent(t.Symbol, "error", err.Error(), time.Now())

		if count >= maxConsecutiveErrors {
			_ = m.st.LogEvent(t.Symbol, "auto_paused", fmt.Sprintf(
				"连续 %d 次执行失败，已自动停止该网格，请检查交易对拼写/网络/交易所凭证是否正确后手动重新启动",
				count), time.Now())
			log.Printf("[自动暂停] %s 连续 %d 次执行失败，已自动停止", t.Symbol, count)
			m.StopGrid(t.Symbol)
		}
		return
	}
	t.mu.Lock()
	t.consecutiveErrors = 0
	t.mu.Unlock()

	for _, e := range events {
		_ = m.st.LogEvent(t.Symbol, e.Type, e.Message, e.Time)
	}

	ticker, err := rawEx.GetTicker(ctx, t.Symbol)
	if err != nil {
		return
	}
	snap := t.engine.Snapshot(ticker.Price)
	_ = m.st.RecordPnLSnapshot(t.Symbol, snap.RealizedPnL, snap.TotalPositionQuote, time.Now())

	// 更新历史最高浮盈百分比（按持仓方向分别记录），并检查是否需要触发
	// 回撤保护性强平——这里不再只记录日志，而是真正对交易所下达市价平仓单。
	// currentPositions 会根据当前是否为现货交易所自动选择数据来源
	// （合约查交易所真实持仓；现货用网格引擎自己的记账合成虚拟持仓）。
	positions, err := m.currentPositions(ctx, rawEx, t, t.Symbol)
	if err == nil {
		for _, p := range positions {
			if p.EntryPrice <= 0 {
				continue
			}
			pct := (ticker.Price - p.EntryPrice) / p.EntryPrice * 100 * p.Leverage
			if p.PositionSide == exchange.PositionShort {
				pct = -pct
			}

			t.mu.Lock()
			if pct > t.peakPnLPct[p.PositionSide] {
				t.peakPnLPct[p.PositionSide] = pct
			}
			curPeak := t.peakPnLPct[p.PositionSide]
			t.mu.Unlock()

			state := risk.AccountState{
				PeakUnrealizedPnLPct:    curPeak,
				CurrentUnrealizedPnLPct: pct,
			}
			shouldClose, reason := m.rk.ShouldForceClose(state)
			if !shouldClose {
				continue
			}

			_ = m.st.LogEvent(t.Symbol, "force_close", reason, time.Now())
			log.Printf("[强制平仓保护] %s(%s): %s", t.Symbol, p.PositionSide, reason)

			closeSide := exchange.SideSell
			if p.PositionSide == exchange.PositionShort {
				closeSide = exchange.SideBuy
			}
			_, closeErr := guardedEx.PlaceOrder(ctx, exchange.OrderRequest{
				Symbol:        t.Symbol,
				Side:          closeSide,
				PositionSide:  p.PositionSide,
				Type:          exchange.OrderTypeMarket,
				Quantity:      p.Quantity,
				ReduceOnly:    true,
				ClientOrderID: fmt.Sprintf("%s-FORCECLOSE-%d", t.Symbol, time.Now().UnixNano()),
			})
			if closeErr != nil {
				_ = m.st.LogEvent(t.Symbol, "error", "强平下单失败: "+closeErr.Error(), time.Now())
				log.Printf("[强平下单失败] %s(%s): %v", t.Symbol, p.PositionSide, closeErr)
				continue
			}
			_ = m.st.LogEvent(t.Symbol, "force_close", fmt.Sprintf("已对 %s 方向下达市价平仓单，数量=%.6f", p.PositionSide, p.Quantity), time.Now())

			t.mu.Lock()
			t.peakPnLPct[p.PositionSide] = 0
			t.mu.Unlock()

			// 强平是绕过网格引擎自身状态机直接对交易所下的单，引擎内部记录的
			// 挂单/持仓状态已经和交易所真实状态不一致，整体重置后下次tick会
			// 重新初始化整个网格，是保证状态一致性最简单可靠的做法。
			if resetErr := t.engine.ForceReset(ctx, guardedEx); resetErr != nil {
				log.Printf("[网格重置失败] %s: %v", t.Symbol, resetErr)
			}
		}
	}

	// 记录本次观察到的价格，供下一个tick周期的极端行情熔断判断使用
	t.mu.Lock()
	t.lastPrice = ticker.Price
	t.mu.Unlock()
}

// Snapshot 返回某交易对当前网格状态快照
func (m *Manager) Snapshot(ctx context.Context, symbol string) (strategy.Snapshot, bool) {
	m.mu.RLock()
	t, ok := m.traders[symbol]
	m.mu.RUnlock()
	if !ok {
		return strategy.Snapshot{}, false
	}
	rawEx := m.RawExchange()
	price := 0.0
	if rawEx != nil {
		if ticker, err := rawEx.GetTicker(ctx, symbol); err == nil {
			price = ticker.Price
		}
	}
	return t.engine.Snapshot(price), true
}

// ListSymbols 返回当前所有已配置的交易对
func (m *Manager) ListSymbols() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.traders))
	for s := range m.traders {
		out = append(out, s)
	}
	return out
}

// IsRunning 判断某交易对策略是否在运行
func (m *Manager) IsRunning(symbol string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.traders[symbol]
	return ok && t.running
}

// Store 暴露存储层，供 API 查询事件/历史
func (m *Manager) Store() *store.Store { return m.st }
