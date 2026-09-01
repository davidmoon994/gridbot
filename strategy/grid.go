// Package strategy 实现"移动网格"策略引擎。
//
// 与传统固定网格的区别：
//  1. 网格中心跟随 EMA 移动，而不是固定在开仓时的价格；
//  2. 网格间距由 ATR（波动率）动态计算，而不是固定百分比；
//  3. 当价格偏离网格中心超过阈值（判定为趋势行情而非震荡）时，
//     自动撤销未成交挂单并围绕新中心重新布网（Recenter），
//     避免网格在单边趋势中被"晾在半山腰"。
package strategy

import (
	"context"
	"fmt"
	"math"
	"time"

	"gridbot/exchange"
)

// Mode 网格模式
type Mode string

const (
	// ModeLongOnly 只做多网格：低位买入开多/加仓，高位卖出平多（止盈），不开空。
	// 适合现货或不想承担双向风险的用户。
	ModeLongOnly Mode = "long_only"

	// ModeNeutral 中性网格：低位开多，高位开空，两个方向都能吃震荡利润。
	// 需要合约账户支持双向持仓，风险更高，务必配合杠杆限制使用。
	ModeNeutral Mode = "neutral"
)

// Config 是移动网格策略的参数配置
type Config struct {
	Symbol string

	// GridCount 中心线上下各布多少层网格（总层数约为 2*GridCount）
	GridCount int

	// EMAPeriod 用于计算网格中心的EMA周期
	EMAPeriod int

	// ATRPeriod 用于计算波动率的ATR周期
	ATRPeriod int

	// ATRSpacingMultiplier 网格间距 = ATR * 该系数
	// 数值越大网格越宽，成交频率越低但单格利润越大
	ATRSpacingMultiplier float64

	// MinSpacingPercent / MaxSpacingPercent 间距占价格的百分比上下限，
	// 防止极端行情下ATR算出的间距过窄（频繁交易吃手续费）或过宽（长期不成交）
	MinSpacingPercent float64
	MaxSpacingPercent float64

	// RecenterThresholdGrids 价格偏离中心超过多少"格"就触发重新居中
	// 例如设为 GridCount*0.7，表示价格突破网格70%范围外即视为趋势启动
	RecenterThresholdGrids float64

	// MinRecenterIntervalSec 两次重新居中之间的最小间隔（秒），防止价格在边界反复
	// 触发抖动式重建（每次重建都有撤单/挂单开销和滑点成本）
	MinRecenterIntervalSec int

	// PerGridQuoteAmount 每格下单的名义金额（计价货币，如 USDT）
	PerGridQuoteAmount float64

	// Leverage 合约杠杆倍数
	Leverage float64

	// Mode 网格模式
	Mode Mode

	// MaxTotalPositionQuote 本网格策略允许的最大总持仓名义价值（USDT）
	// 这是策略自身的仓位预算，风控引擎（risk包）会做二次独立校验，
	// 两层限制都必须满足才会真正下单。
	MaxTotalPositionQuote float64

	// MarketType "futures"（合约，默认，兼容旧数据留空即视为合约） | "spot"（现货）。
	// 现货没有杠杆/保证金/做空这些概念：
	//   - Mode 必须是 ModeLongOnly，ModeNeutral（会开空）在现货下不合法，
	//     由 Validate() 负责拦截；
	//   - Initialize 时不会调用 SetLeverage；
	//   - 持仓统计不依赖交易所的 GetPositions（现货没有这个概念），
	//     manager 层会改用 PositionSummary() 自己合成。
	MarketType string
}

// IsSpot 判断该配置是否为现货模式
func (cfg Config) IsSpot() bool { return cfg.MarketType == "spot" }

// Validate 校验配置合法性，在启动网格前调用。现货+双向持仓模式是一个无法
// 在真实交易所执行的非法组合（现货做空需要保证金/借币，不是这套现货实现覆盖的范围），
// 必须在启动前就拒绝，而不是等到真实下单时才在交易所侧报错。
func (cfg Config) Validate() error {
	if cfg.IsSpot() && cfg.Mode == ModeNeutral {
		return fmt.Errorf("现货交易不支持「中性双向」模式（现货无法做空），请改用「只做多」模式")
	}
	return nil
}

// LevelStatus 网格层状态
type LevelStatus string

const (
	LevelEmpty     LevelStatus = "empty"      // 空仓，无挂单
	LevelOrderOpen LevelStatus = "order_open" // 已挂单，等待成交
	LevelFilled    LevelStatus = "filled"     // 已成交，持有仓位，等待对侧平仓单
)

// Level 单个网格层
type Level struct {
	Index           int         `json:"index"` // 0为中心，负数在下方（买），正数在上方（卖/空）
	Price           float64     `json:"price"`
	Status          LevelStatus `json:"status"`
	OrderClientID   string      `json:"order_client_id"`
	ExchangeOrderID string      `json:"exchange_order_id"`
	FilledQty       float64     `json:"filled_qty"`
	FilledPrice     float64     `json:"filled_price"`
	FilledAt        time.Time   `json:"filled_at"`
}

// Snapshot 用于 Web 界面展示的网格快照（只读）
type Snapshot struct {
	Symbol             string    `json:"symbol"`
	Mode               Mode      `json:"mode"`
	Center             float64   `json:"center"`
	Spacing            float64   `json:"spacing"`
	SpacingPct         float64   `json:"spacing_pct"`
	CurrentPrice       float64   `json:"current_price"`
	Levels             []Level   `json:"levels"`
	LastRecenter       time.Time `json:"last_recenter"`
	RecenterCount      int       `json:"recenter_count"`
	TotalPositionQuote float64   `json:"total_position_quote"`
	RealizedPnL        float64   `json:"realized_pnl"`
}

// Event 引擎在一次 OnTick 中产生的事件，供上层记录日志/推送前端
type Event struct {
	Time    time.Time
	Type    string // "grid_filled" | "take_profit" | "recenter" | "error" | "info"
	Message string
}

// Engine 是移动网格的运行时状态机
type Engine struct {
	cfg    Config
	levels map[int]*Level

	center  float64
	spacing float64

	lastRecenter  time.Time
	recenterCount int
	realizedPnL   float64
	seq           int64

	initialized bool
}

// NewEngine 创建一个新的移动网格引擎
func NewEngine(cfg Config) *Engine {
	return &Engine{
		cfg:    cfg,
		levels: map[int]*Level{},
	}
}

// computeCenterAndSpacing 根据最新K线计算网格中心（EMA）与间距（ATR）
func (e *Engine) computeCenterAndSpacing(klines []exchange.Kline, currentPrice float64) (float64, float64) {
	center := LastEMA(klines, e.cfg.EMAPeriod)
	if center <= 0 {
		center = currentPrice
	}
	atr := ATR(klines, e.cfg.ATRPeriod)
	spacing := atr * e.cfg.ATRSpacingMultiplier

	minSpacing := center * e.cfg.MinSpacingPercent / 100
	maxSpacing := center * e.cfg.MaxSpacingPercent / 100
	if spacing < minSpacing {
		spacing = minSpacing
	}
	if spacing > maxSpacing {
		spacing = maxSpacing
	}
	return center, spacing
}

// buildLevels 围绕 center 按 spacing 生成 [-GridCount, +GridCount] 的网格层，
// 会清空旧的层状态（调用前应确保旧挂单已撤销）
func (e *Engine) buildLevels(center, spacing float64) {
	e.levels = map[int]*Level{}
	for i := -e.cfg.GridCount; i <= e.cfg.GridCount; i++ {
		if i == 0 {
			continue
		}
		price := center + float64(i)*spacing
		e.levels[i] = &Level{
			Index:  i,
			Price:  price,
			Status: LevelEmpty,
		}
	}
	e.center = center
	e.spacing = spacing
}

// Initialize 首次启动：拉取K线、计算中心与间距、铺设网格并挂出初始订单
func (e *Engine) Initialize(ctx context.Context, ex exchange.Exchange) ([]Event, error) {
	var events []Event
	klines, err := ex.GetKlines(ctx, e.cfg.Symbol, "3m", 200)
	if err != nil {
		return nil, fmt.Errorf("获取K线失败: %w", err)
	}
	ticker, err := ex.GetTicker(ctx, e.cfg.Symbol)
	if err != nil {
		return nil, fmt.Errorf("获取行情失败: %w", err)
	}

	if e.cfg.Leverage > 0 && !e.cfg.IsSpot() {
		_ = ex.SetLeverage(ctx, e.cfg.Symbol, e.cfg.Leverage)
	}

	center, spacing := e.computeCenterAndSpacing(klines, ticker.Price)
	e.buildLevels(center, spacing)
	e.lastRecenter = time.Now()
	e.initialized = true

	events = append(events, Event{
		Time: time.Now(), Type: "info",
		Message: fmt.Sprintf("初始化网格：中心=%.4f 间距=%.4f（%.3f%%）层数=%d",
			center, spacing, spacing/center*100, e.cfg.GridCount*2),
	})

	placeEvents, err := e.placeMissingOrders(ctx, ex, ticker.Price)
	if err != nil {
		return events, err
	}
	return append(events, placeEvents...), nil
}

// placeMissingOrders 为所有状态为 Empty 且"应该有挂单"的层补挂订单：
//   - 下方层（index<0）：始终挂买单（开多/加多）
//   - 上方层（index>0）：
//     ModeNeutral   -> 挂卖单（开空）
//     ModeLongOnly  -> 仅当该层持有"待平仓"的多头库存时才挂卖单，
//     由 onLevelFilled 负责在买单成交后于上一层挂出对应卖单，
//     因此这里对 long_only 模式下的裸多层不主动挂空卖单。
func (e *Engine) placeMissingOrders(ctx context.Context, ex exchange.Exchange, currentPrice float64) ([]Event, error) {
	var events []Event
	qty := e.cfg.PerGridQuoteAmount / currentPrice

	for i := -e.cfg.GridCount; i <= e.cfg.GridCount; i++ {
		if i == 0 {
			continue
		}
		lvl := e.levels[i]
		if lvl.Status != LevelEmpty {
			continue
		}

		if i < 0 {
			// 买入层：只在价格上方时才有意义挂限价买单（价格低于当前价）
			if lvl.Price >= currentPrice {
				continue
			}
			e.seq++
			clientID := fmt.Sprintf("%s-B-%d-%d", e.cfg.Symbol, i, e.seq)
			order, err := ex.PlaceOrder(ctx, exchange.OrderRequest{
				Symbol:        e.cfg.Symbol,
				Side:          exchange.SideBuy,
				PositionSide:  exchange.PositionLong,
				Type:          exchange.OrderTypeLimit,
				Price:         lvl.Price,
				Quantity:      qty,
				ClientOrderID: clientID,
			})
			if err != nil {
				events = append(events, Event{Time: time.Now(), Type: "error",
					Message: fmt.Sprintf("挂买单失败 level=%d price=%.4f: %v", i, lvl.Price, err)})
				continue
			}
			lvl.Status = LevelOrderOpen
			lvl.OrderClientID = clientID
			lvl.ExchangeOrderID = order.ExchangeOrderID
		} else {
			if e.cfg.Mode != ModeNeutral {
				continue // long_only 模式下的裸空层不主动开仓
			}
			if lvl.Price <= currentPrice {
				continue
			}
			e.seq++
			clientID := fmt.Sprintf("%s-S-%d-%d", e.cfg.Symbol, i, e.seq)
			order, err := ex.PlaceOrder(ctx, exchange.OrderRequest{
				Symbol:        e.cfg.Symbol,
				Side:          exchange.SideSell,
				PositionSide:  exchange.PositionShort,
				Type:          exchange.OrderTypeLimit,
				Price:         lvl.Price,
				Quantity:      qty,
				ClientOrderID: clientID,
			})
			if err != nil {
				events = append(events, Event{Time: time.Now(), Type: "error",
					Message: fmt.Sprintf("挂卖单失败 level=%d price=%.4f: %v", i, lvl.Price, err)})
				continue
			}
			lvl.Status = LevelOrderOpen
			lvl.OrderClientID = clientID
			lvl.ExchangeOrderID = order.ExchangeOrderID
		}
	}
	return events, nil
}

// OnTick 是策略的主循环入口，应由外部调度器（如每隔若干秒）周期调用：
//  1. 检查所有挂单成交情况，对成交的层触发配对止盈单；
//  2. 判断是否需要"重新居中"；
//  3. 补齐缺失的挂单。
func (e *Engine) OnTick(ctx context.Context, ex exchange.Exchange) ([]Event, error) {
	if !e.initialized {
		return e.Initialize(ctx, ex)
	}
	var events []Event

	ticker, err := ex.GetTicker(ctx, e.cfg.Symbol)
	if err != nil {
		return nil, fmt.Errorf("获取行情失败: %w", err)
	}

	openOrders, err := ex.GetOpenOrders(ctx, e.cfg.Symbol)
	if err != nil {
		return nil, fmt.Errorf("获取挂单失败: %w", err)
	}
	openByClientID := map[string]exchange.Order{}
	for _, o := range openOrders {
		openByClientID[o.ClientOrderID] = o
	}

	// 1. 检查曾经挂单、如今已不在"未成交列表"里的层：
	//    这只说明"这个单子不再是挂单状态了"，可能是成交，也可能是被手动撤销/被交易所拒绝——
	//    两者必须区分开来，否则被撤销的单子会被误判成"已成交"，进而错误地认为自己持有仓位，
	//    继续在错误的仓位假设上挂止盈单，导致仓位跟踪彻底错乱。
	//    因此这里额外查询一次订单的真实状态（GetOrder），而不是直接假定"消失=成交"。
	for _, lvl := range e.levels {
		if lvl.Status != LevelOrderOpen {
			continue
		}
		if _, stillOpen := openByClientID[lvl.OrderClientID]; stillOpen {
			continue
		}

		if lvl.ExchangeOrderID == "" {
			// 理论上不应发生（下单成功时必然会记录交易所订单ID）；
			// 为兼容极端情况，保守地按"已成交"处理，并记录一条警告方便排查。
			events = append(events, Event{Time: time.Now(), Type: "error",
				Message: fmt.Sprintf("level=%d 缺少交易所订单ID，按已成交处理（请检查是否为异常数据）", lvl.Index)})
			filledEvents := e.onLevelFilled(ctx, ex, lvl, ticker.Price)
			events = append(events, filledEvents...)
			continue
		}

		order, err := ex.GetOrder(ctx, e.cfg.Symbol, lvl.ExchangeOrderID)
		if err != nil {
			// 查询失败（网络抖动等）：保持原状态，下次tick重试，不做任何假设
			events = append(events, Event{Time: time.Now(), Type: "error",
				Message: fmt.Sprintf("查询订单状态失败 level=%d orderID=%s: %v", lvl.Index, lvl.ExchangeOrderID, err)})
			continue
		}

		switch order.Status {
		case exchange.OrderStatusFilled:
			filledEvents := e.onLevelFilled(ctx, ex, lvl, ticker.Price)
			events = append(events, filledEvents...)
		case exchange.OrderStatusCanceled, exchange.OrderStatusRejected:
			// 被撤销/被拒绝：这一层重新变回空层，等待下一轮 placeMissingOrders 重新挂单，
			// 绝不能当成交处理，否则会凭空产生一笔不存在的仓位记录。
			lvl.Status = LevelEmpty
			lvl.OrderClientID = ""
			lvl.ExchangeOrderID = ""
			events = append(events, Event{Time: time.Now(), Type: "info",
				Message: fmt.Sprintf("level=%d 挂单被撤销/拒绝（状态=%s），已重置为空层", lvl.Index, order.Status)})
		default:
			// 已不在挂单列表但状态既非成交也非撤销/拒绝（例如仍是NEW或PARTIALLY_FILLED），
			// 属于交易所侧数据短暂不一致，保持原状态观察，不做处理，避免误判。
		}
	}

	// 2. 判断是否需要重新居中
	if e.shouldRecenter(ticker.Price) {
		recenterEvents, err := e.recenter(ctx, ex, ticker.Price)
		if err != nil {
			events = append(events, Event{Time: time.Now(), Type: "error",
				Message: fmt.Sprintf("重新居中失败: %v", err)})
		} else {
			events = append(events, recenterEvents...)
		}
	}

	// 3. 补齐缺失挂单
	placeEvents, err := e.placeMissingOrders(ctx, ex, ticker.Price)
	if err != nil {
		return events, err
	}
	events = append(events, placeEvents...)

	return events, nil
}

// onLevelFilled 处理某一层成交后的动作：
//   - 买单成交（开多）：标记该层为 Filled，并在上一层（index+1）挂出对应数量的卖单作为止盈
//   - 卖单成交（开空，仅neutral模式）：标记该层为 Filled，并在下一层（index-1）挂出买单作为止盈（买回平空）
//   - 若某层原本是"止盈单"性质（即对侧持仓的平仓单），成交后应将两层都重置为 Empty，
//     并计入已实现盈亏。这里通过检查该层是否本就"配对"来简化判断。
func (e *Engine) onLevelFilled(ctx context.Context, ex exchange.Exchange, lvl *Level, currentPrice float64) []Event {
	var events []Event
	qty := e.cfg.PerGridQuoteAmount / lvl.Price

	lvl.Status = LevelFilled
	lvl.FilledQty = qty
	lvl.FilledPrice = lvl.Price
	lvl.FilledAt = time.Now()
	lvl.OrderClientID = ""
	lvl.ExchangeOrderID = ""

	events = append(events, Event{Time: time.Now(), Type: "grid_filled",
		Message: fmt.Sprintf("网格成交 level=%d price=%.4f qty=%.6f", lvl.Index, lvl.Price, qty)})

	var pairIndex int
	var side exchange.Side
	var posSide exchange.PositionSide
	if lvl.Index < 0 {
		// 买单成交 -> 上一层挂卖单止盈（平多）。若紧邻层正好是中心线(0)，
		// 跳过中心线取下一层，避免把配对层错误指向中心线（不存在的层）。
		pairIndex = lvl.Index + 1
		if pairIndex == 0 {
			pairIndex = lvl.Index + 2
		}
		side = exchange.SideSell
		posSide = exchange.PositionLong
	} else {
		pairIndex = lvl.Index - 1
		if pairIndex == 0 {
			pairIndex = lvl.Index - 2
		}
		side = exchange.SideBuy
		posSide = exchange.PositionShort
	}

	pairLvl, ok := e.levels[pairIndex]
	if !ok {
		// 极端情况：配对层超出当前网格范围（例如网格层数设置过小）。
		// 直接在"成交价 ± 一个间距"处挂平仓单，但目标状态记录在成交层自身以外的
		// 一个游离追踪结构中，而不是覆盖 lvl 本身的 Filled 状态——
		// 简化实现：由风控引擎的回撤保护兜底，此处仅记录警告事件。
		events = append(events, Event{Time: time.Now(), Type: "error",
			Message: fmt.Sprintf("level=%d 配对层超出网格范围，建议增大网格层数(GridCount)", lvl.Index)})
		return events
	}

	if pairLvl.Status == LevelFilled {
		// 配对层已经持有反向仓位（说明这是在给已有持仓做平仓单），
		// 直接对冲计入已实现盈亏，两层都清空
		pnl := (lvl.Price - pairLvl.FilledPrice) * qty
		if lvl.Index > 0 {
			pnl = -pnl
		}
		e.realizedPnL += pnl
		lvl.Status = LevelEmpty
		lvl.OrderClientID = ""
		lvl.ExchangeOrderID = ""
		pairLvl.Status = LevelEmpty
		pairLvl.OrderClientID = ""
		pairLvl.ExchangeOrderID = ""
		events = append(events, Event{Time: time.Now(), Type: "take_profit",
			Message: fmt.Sprintf("配对止盈 level=%d<->%d 已实现盈亏=%.4f", lvl.Index, pairIndex, pnl)})
		return events
	}

	e.placeTakeProfit(ctx, ex, side, posSide, pairLvl.Price, qty, pairLvl)
	return events
}

func (e *Engine) placeTakeProfit(ctx context.Context, ex exchange.Exchange, side exchange.Side, posSide exchange.PositionSide, price, qty float64, targetLvl *Level) {
	e.seq++
	clientID := fmt.Sprintf("%s-TP-%d-%d", e.cfg.Symbol, targetLvl.Index, e.seq)
	order, err := ex.PlaceOrder(ctx, exchange.OrderRequest{
		Symbol:        e.cfg.Symbol,
		Side:          side,
		PositionSide:  posSide,
		Type:          exchange.OrderTypeLimit,
		Price:         price,
		Quantity:      qty,
		ReduceOnly:    true,
		ClientOrderID: clientID,
	})
	if err != nil {
		return
	}
	targetLvl.Status = LevelOrderOpen
	targetLvl.OrderClientID = clientID
	targetLvl.ExchangeOrderID = order.ExchangeOrderID
}

// shouldRecenter 判断价格是否已经偏离网格中心足够远（视为趋势而非震荡），
// 且距离上次重新居中已经过了最小冷却时间
func (e *Engine) shouldRecenter(currentPrice float64) bool {
	if e.spacing <= 0 {
		return false
	}
	if time.Since(e.lastRecenter) < time.Duration(e.cfg.MinRecenterIntervalSec)*time.Second {
		return false
	}
	deviation := math.Abs(currentPrice-e.center) / e.spacing
	return deviation >= e.cfg.RecenterThresholdGrids
}

// recenter 撤销所有未成交挂单，围绕最新 EMA 中心与 ATR 间距重新铺设网格。
// 已成交（持仓中）的层不会被强制平仓——重新居中只影响挂单，不代表止损，
// 是否需要对旧仓位止损由风控引擎（risk包）的回撤保护规则单独负责。
func (e *Engine) recenter(ctx context.Context, ex exchange.Exchange, currentPrice float64) ([]Event, error) {
	var events []Event

	openOrders, err := ex.GetOpenOrders(ctx, e.cfg.Symbol)
	if err != nil {
		return nil, err
	}
	for _, o := range openOrders {
		_ = ex.CancelOrder(ctx, e.cfg.Symbol, o.ExchangeOrderID)
	}

	// 保留仍在持仓中的层，用于后续平仓单继续挂出
	heldPositions := map[int]*Level{}
	for idx, lvl := range e.levels {
		if lvl.Status == LevelFilled {
			cp := *lvl
			heldPositions[idx] = &cp
		}
	}

	klines, err := ex.GetKlines(ctx, e.cfg.Symbol, "3m", 200)
	if err != nil {
		return nil, err
	}
	center, spacing := e.computeCenterAndSpacing(klines, currentPrice)
	e.buildLevels(center, spacing)

	// 旧持仓层：按原成交价映射回新网格中最近的层，保持 Filled 状态，
	// 这样旧仓位依然会在合适的价位被挂出平仓单，而不会丢失追踪
	for _, held := range heldPositions {
		nearestIdx := int(math.Round((held.FilledPrice - center) / spacing))
		if nearestIdx == 0 {
			nearestIdx = 1
			if held.FilledPrice < center {
				nearestIdx = -1
			}
		}
		if lvl, ok := e.levels[nearestIdx]; ok && lvl.Status == LevelEmpty {
			lvl.Status = LevelFilled
			lvl.FilledQty = held.FilledQty
			lvl.FilledPrice = held.FilledPrice
			lvl.FilledAt = held.FilledAt
		}
	}

	e.lastRecenter = time.Now()
	e.recenterCount++

	events = append(events, Event{Time: time.Now(), Type: "recenter",
		Message: fmt.Sprintf("重新居中 #%d：新中心=%.4f 新间距=%.4f（原持仓层已迁移 %d 个）",
			e.recenterCount, center, spacing, len(heldPositions))})
	return events, nil
}

// ForceReset 用于外部（如风控引擎的强平保护）已经直接对交易所下单平仓、
// 绕过网格引擎自身的场景：此时引擎内部记录的持仓/挂单状态与交易所真实状态
// 已经不一致，与其尝试精细修补（哪一层该清空、哪些挂单该撤销很难可靠判断），
// 不如整体撤销该交易对所有挂单、清空内部状态，下一次 OnTick 会自动重新
// Initialize，相当于"推倒重来"，这是能保证状态一致性的最简单可靠的做法。
func (e *Engine) ForceReset(ctx context.Context, ex exchange.Exchange) error {
	openOrders, err := ex.GetOpenOrders(ctx, e.cfg.Symbol)
	if err == nil {
		for _, o := range openOrders {
			_ = ex.CancelOrder(ctx, e.cfg.Symbol, o.ExchangeOrderID)
		}
	}
	e.levels = map[int]*Level{}
	e.initialized = false
	return err
}

// TotalPositionQuote 估算当前网格持有的总名义仓位价值（USDT），供风控引擎读取
func (e *Engine) TotalPositionQuote() float64 {
	total := 0.0
	for _, lvl := range e.levels {
		if lvl.Status == LevelFilled {
			total += lvl.FilledQty * lvl.FilledPrice
		}
	}
	return total
}

// PositionSummary 汇总当前所有"多头"网格层（index<0，已成交未平仓）的持仓数量与
// 加权平均成本价。
//
// 现货交易所没有交易所原生的"持仓"概念可查（只有钱包余额，而且钱包里可能还有
// 与本网格无关的其他资产），所以现货模式下 manager 层不使用 exchange.GetPositions()，
// 改用这个方法从网格引擎自身的记账数据里合成一个"虚拟持仓"，喂给风控引擎做
// 仓位占比校验和回撤保护判断。
//
// 合约的 long_only 模式其实也可以用这个方法自查（结果应该和交易所返回的一致），
// 但目前只在 manager 判断为现货交易所时才会调用它，合约仍然以交易所返回的
// 真实持仓为准。
func (e *Engine) PositionSummary() (qty, avgEntryPrice float64) {
	var totalQty, totalCost float64
	for _, lvl := range e.levels {
		if lvl.Status == LevelFilled && lvl.Index < 0 {
			totalQty += lvl.FilledQty
			totalCost += lvl.FilledQty * lvl.FilledPrice
		}
	}
	if totalQty <= 0 {
		return 0, 0
	}
	return totalQty, totalCost / totalQty
}

// Snapshot 返回当前网格状态快照，供 API/Web 展示
func (e *Engine) Snapshot(currentPrice float64) Snapshot {
	levels := make([]Level, 0, len(e.levels))
	for _, lvl := range e.levels {
		levels = append(levels, *lvl)
	}
	spacingPct := 0.0
	if e.center > 0 {
		spacingPct = e.spacing / e.center * 100
	}
	return Snapshot{
		Symbol:             e.cfg.Symbol,
		Mode:               e.cfg.Mode,
		Center:             e.center,
		Spacing:            e.spacing,
		SpacingPct:         spacingPct,
		CurrentPrice:       currentPrice,
		Levels:             levels,
		LastRecenter:       e.lastRecenter,
		RecenterCount:      e.recenterCount,
		TotalPositionQuote: e.TotalPositionQuote(),
		RealizedPnL:        e.realizedPnL,
	}
}

// Config 返回引擎当前配置（只读）
func (e *Engine) Config() Config { return e.cfg }

// UpdateConfig 允许在运行中调整部分参数（不会立即触发重新铺网，
// 下一次 OnTick 判断 recenter 时才会用新参数生效）
func (e *Engine) UpdateConfig(cfg Config) { e.cfg = cfg }
