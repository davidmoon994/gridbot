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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"time"

	"gridbot/exchange"
)

// randomInstanceID 生成一个6位十六进制的随机短串，用于区分不同次启动的
// Engine 实例，混入 ClientOrderID 防止重启后订单ID撞车（见 Engine.instanceID 注释）。
// 用 crypto/rand 而不是 math/rand：不需要密码学强度，只是图个不依赖手动播种、
// 不会因为两次启动时间太近而生成相同序列的方便。
func randomInstanceID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		// 极小概率的兜底：读随机数失败就退化用当前时间纳秒的低位，
		// 依然能起到"跟上次大概率不一样"的效果。
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(b)
}

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
	// OrderQty 是挂这笔单时实际提交给交易所的数量（下单请求里的 Quantity）。
	// 成交后记账/挂止盈单时应优先使用交易所真实返回的成交数量（order.FilledQuantity），
	// 但那需要一次额外的 GetOrder 查询成功才能拿到；这个字段作为"退化兜底"——
	// 即便查询失败也能保证止盈单挂出的数量与这笔单子实际提交的数量一致，
	// 而不是用下单时刻已经不适用的另一个价格重新拍脑袋算一个数字出来。
	OrderQty    float64   `json:"order_qty"`
	FilledQty   float64   `json:"filled_qty"`
	FilledPrice float64   `json:"filled_price"`
	FilledAt    time.Time `json:"filled_at"`

	// IsExitOrder / PairWithIndex 明确记录这一层此刻的"角色"和"配对对象"，
	// 不再靠"配对层状态是否恰好是Filled"这种间接推断来判断一笔成交
	// 到底是"新开仓需要挂止盈"还是"止盈单成交需要平仓结算"。
	//
	// 修复说明：只做多模式下 index<0 的层永远只应该是建仓买单，绝不可能是
	// 平仓单；但价格一根K线跌穿两层（比如 -1 和 -2 相继成交）是网格策略
	// 的正常操作，出现频率并不低。以前的代码遇到这种情况会把"-2 全新买入"
	// 误判成"给 -1 那笔仓位平仓"（因为它只看"-1 现在状态是不是Filled"，
	// 不管 -1 是被谁、以什么方式占着的），编出一个不存在的"已实现盈亏"，
	// 然后把两层的记录都清空——但交易所上这两笔仓位其实都还在，一个止盈单
	// 都没挂出去，变成完全脱离监管的裸仓位，内部账目也跟着错。
	// 有了这两个字段后，一笔成交是"该结算平仓"还是"该新挂止盈"，只看这一层
	// 自己的 IsExitOrder 就行，不再依赖旁边那层凑巧处于什么状态。
	IsExitOrder   bool `json:"is_exit_order"`
	PairWithIndex int  `json:"pair_with_index"` // 0 表示未配对（0本身不是合法层号，可安全当作"无"）
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

	// instanceID 是每次创建 Engine 时生成的一个随机短串，混入订单的
	// ClientOrderID 里，确保"重启程序"不会导致订单ID撞车。
	//
	// 背景：seq 只存在内存里，每次程序重启（或重新启动这个网格）都会
	// 从0重新计数；但交易所（尤其币安）会在一段时间内记住用过的
	// ClientOrderID，不允许重复。如果没有这个随机成分，频繁重启会导致
	// "ClientOrderId is duplicated"这类挂单失败，且很难排查，因为
	// 每次生成的ID字符串在数值上确实和之前用过的一模一样。
	instanceID string

	initialized bool
}

// NewEngine 创建一个新的移动网格引擎
func NewEngine(cfg Config) *Engine {
	return &Engine{
		cfg:        cfg,
		levels:     map[int]*Level{},
		instanceID: randomInstanceID(),
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

	// 核对交易所侧是否存在这个symbol的真实遗留持仓——网格引擎自己的成交记账
	// 只存在内存里，程序每次重启都会从空白状态开始；如果重启前有仓位还没
	// 平掉（比如上一次强平下单失败、或者进程被意外杀掉），不主动核对的话，
	// 软件会误以为自己一无所有，那笔真实仓位就变成了脱离监管的"孤儿仓位"，
	// 既不会被继续追踪浮盈回撤，也不会有对应的止盈单在等着它。
	events = append(events, e.reconcileExistingPositions(ctx, ex)...)

	placeEvents, err := e.placeMissingOrders(ctx, ex, ticker.Price)
	if err != nil {
		return events, err
	}
	return append(events, placeEvents...), nil
}

// reconcileExistingPositions 在网格刚初始化、所有层都还是空白状态时，
// 主动查询交易所这个symbol当前的真实持仓，如果发现有遗留仓位，
// 把它"认领"到最接近的网格层上（多头只会认领到index<0的层，
// 空头只会认领到index>0的层），并立刻为它挂出对应的止盈单——
// 而不是放任这笔仓位在软件的记账里凭空消失。
//
// 现货没有交易所原生"持仓"概念可查（见 exchange/binance_spot.go 顶部注释），
// 这里对现货直接跳过；现货重启后的持仓核对是一个更深层的已知限制
// （需要把网格引擎的运行时状态持久化到数据库才能彻底解决，目前还没做，
// 见架构文档"后续可扩展方向"）。
func (e *Engine) reconcileExistingPositions(ctx context.Context, ex exchange.Exchange) []Event {
	var events []Event
	if e.cfg.IsSpot() {
		return events
	}

	positions, err := ex.GetPositions(ctx, e.cfg.Symbol)
	if err != nil {
		events = append(events, Event{Time: time.Now(), Type: "error",
			Message: fmt.Sprintf("启动时核对交易所真实持仓失败，可能遗漏此前未平的仓位，请自行去交易所确认: %v", err)})
		return events
	}

	for _, p := range positions {
		if p.Quantity <= 0 || p.EntryPrice <= 0 {
			continue
		}

		nearestIdx := int(math.Round((p.EntryPrice - e.center) / e.spacing))
		// 多头持仓必须落在负数层，空头必须落在正数层（网格的方向约定），
		// 就算按价格算出来的最近层落在了错误的一侧或者中心线上，也要强制扭正。
		if p.PositionSide == exchange.PositionLong && nearestIdx >= 0 {
			nearestIdx = -1
		}
		if p.PositionSide == exchange.PositionShort && nearestIdx <= 0 {
			nearestIdx = 1
		}
		if nearestIdx < -e.cfg.GridCount {
			nearestIdx = -e.cfg.GridCount
		}
		if nearestIdx > e.cfg.GridCount {
			nearestIdx = e.cfg.GridCount
		}

		lvl, ok := e.levels[nearestIdx]
		if !ok {
			events = append(events, Event{Time: time.Now(), Type: "error",
				Message: fmt.Sprintf("发现交易所遗留持仓（%s 数量=%.6f 成本=%.4f），但网格范围内找不到合适的层容纳，请人工确认该仓位",
					p.PositionSide, p.Quantity, p.EntryPrice)})
			continue
		}

		lvl.Status = LevelFilled
		lvl.FilledQty = p.Quantity
		lvl.FilledPrice = p.EntryPrice
		lvl.FilledAt = time.Now()

		events = append(events, Event{Time: time.Now(), Type: "info",
			Message: fmt.Sprintf("启动时发现交易所遗留持仓：%s 数量=%.6f 成本=%.4f，已接管到 level=%d，将挂出对应止盈单",
				p.PositionSide, p.Quantity, p.EntryPrice, nearestIdx)})

		events = append(events, e.pairAndPlaceTakeProfit(ctx, ex, lvl, p.Quantity)...)
	}
	return events
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

	for i := -e.cfg.GridCount; i <= e.cfg.GridCount; i++ {
		if i == 0 {
			continue
		}
		lvl := e.levels[i]
		if lvl.Status != LevelEmpty {
			continue
		}
		// 按这一层自己的挂单价计算数量，而不是按调用时刻的市价计算——
		// 否则同一个 PerGridQuoteAmount 在不同层上买到的名义金额会不一致
		// （层价格离当前价越远，用市价算出的数量偏差越大），"每格固定名义金额"
		// 这个参数的语义就名不副实了。
		qty := e.cfg.PerGridQuoteAmount / lvl.Price

		if i < 0 {
			// 买入层：只在价格上方时才有意义挂限价买单（价格低于当前价）
			if lvl.Price >= currentPrice {
				continue
			}
			e.seq++
			clientID := fmt.Sprintf("%s-B-%d-%s-%d", e.cfg.Symbol, i, e.instanceID, e.seq)
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
			lvl.OrderQty = qty
		} else {
			if e.cfg.Mode != ModeNeutral {
				continue // long_only 模式下的裸空层不主动开仓
			}
			if lvl.Price <= currentPrice {
				continue
			}
			e.seq++
			clientID := fmt.Sprintf("%s-S-%d-%s-%d", e.cfg.Symbol, i, e.instanceID, e.seq)
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
			lvl.OrderQty = qty
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
			filledEvents := e.onLevelFilled(ctx, ex, lvl, nil)
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
			filledEvents := e.onLevelFilled(ctx, ex, lvl, order)
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

	// 3. 兜底检查：所有"已成交/持仓中"的层是否都有一个正在挂着的止盈单。
	// 覆盖两种此前会导致仓位"裸奔"的场景：
	//   a) recenter 撤销了旧止盈单、把持仓迁移到新网格后，没有为其重新挂出止盈单；
	//   b) 上一次 placeTakeProfit 因为网络错误/数量或精度不合法等原因下单失败，
	//      之前是静默丢弃、没有任何重试路径。
	// 每个 tick 都跑一遍，天然具备重试能力；已有正在挂着的止盈单的层会被跳过，不会重复下单。
	events = append(events, e.ensureTakeProfits(ctx, ex)...)

	// 4. 补齐缺失挂单
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
//
// order 是本层这笔单子的交易所真实状态（来自 OnTick 里的 GetOrder），优先使用它
// 返回的真实成交数量/均价做记账；只有在极端情况下查不到订单详情（order == nil，
// 见 OnTick 中"缺少交易所订单ID"的兜底分支）时，才退化使用下单时记录的
// lvl.OrderQty / 挂单价 lvl.Price 兜底。
//
// 修复说明：这里以前是无论如何都用 PerGridQuoteAmount/lvl.Price 重新估算一遍数量，
// 而实际下单时（placeMissingOrders/placeTakeProfit）用的是另一个价格基准算出来的
// 数量，两者对不上，导致这里记的"持仓数量"和交易所真实成交的数量不一致，后续挂出
// 的止盈单数量也跟着错，可能被交易所拒单（现货余额不够/合约减仓单超过持仓）或
// 留下永远平不掉的残余仓位。
func (e *Engine) onLevelFilled(ctx context.Context, ex exchange.Exchange, lvl *Level, order *exchange.Order) []Event {
	var events []Event
	qty := lvl.OrderQty
	price := lvl.Price
	if qty <= 0 {
		// 兜底：连 OrderQty 都没有记录到的极端情况（理论不应发生），退回旧的估算方式。
		qty = e.cfg.PerGridQuoteAmount / lvl.Price
	}
	if order != nil {
		if order.FilledQuantity > 0 {
			qty = order.FilledQuantity
		}
		if order.AvgFillPrice > 0 {
			price = order.AvgFillPrice
		}
	}

	lvl.Status = LevelFilled
	lvl.FilledQty = qty
	lvl.FilledPrice = price
	lvl.FilledAt = time.Now()
	lvl.OrderClientID = ""
	lvl.ExchangeOrderID = ""

	events = append(events, Event{Time: time.Now(), Type: "grid_filled",
		Message: fmt.Sprintf("网格成交 level=%d price=%.4f qty=%.6f", lvl.Index, price, qty)})

	events = append(events, e.pairAndPlaceTakeProfit(ctx, ex, lvl, qty)...)
	return events
}

// pairAndPlaceTakeProfit 处理一笔刚成交的订单的后续动作，分两种情况：
//   - 如果这一层本身就是一张止盈单（lvl.IsExitOrder==true），说明是在给
//     lvl.PairWithIndex 指向的建仓层平仓，直接按记录的配对关系结算已实现盈亏，
//     两层都清空；
//   - 如果这一层是一笔全新的建仓成交，就找一个空闲层挂出对应的止盈单
//     （优先挂相邻层，被占用则往同方向继续找下一个空闲层）。
//
// 从两处调用：正常成交检测走 onLevelFilled；程序重启后核对交易所真实
// 遗留持仓走 reconcileExistingPositions——两种场景都需要"给持仓层配一个
// 止盈出口"，逻辑完全一样，抽成一个函数避免重复代码。
func (e *Engine) pairAndPlaceTakeProfit(ctx context.Context, ex exchange.Exchange, lvl *Level, qty float64) []Event {
	var events []Event

	if lvl.IsExitOrder {
		// 这一层本身就是一张止盈单，现在成交了：说明是在给 lvl.PairWithIndex
		// 指向的那个建仓层平仓。直接按记录的配对关系结算，不用再靠"配对层
		// 状态是不是Filled"去猜——这正是修复前的做法，会把"另一笔完全不
		// 相关、恰好也是Filled状态的建仓层"误判成平仓对象（比如价格连续
		// 跌穿多层，-1和-2先后成交，-2成交时如果去看"-1是不是Filled"，
		// -1当然是Filled的，但那只是因为-1自己是笔独立的建仓，不代表-2在
		// 给它平仓）。
		entryLvl, ok := e.levels[lvl.PairWithIndex]
		if !ok || entryLvl.Status != LevelFilled {
			// 理论不应该出现：配对的建仓层已经不在网格范围内，或者状态不对
			// （比如被 recenter/强平 提前清空了）。这笔止盈单在交易所那边
			// 已经真实成交、持仓已经被平掉，所以这一层的状态无论如何都要
			// 清空；只是这种情况下算不出准确的已实现盈亏，记录下来方便人工核对。
			events = append(events, Event{Time: time.Now(), Type: "error",
				Message: fmt.Sprintf("level=%d 止盈单成交，但配对建仓层level=%d状态异常，已实现盈亏无法准确计算，仅清空本层状态，请人工核对交易所实际持仓", lvl.Index, lvl.PairWithIndex)})
			lvl.Status = LevelEmpty
			lvl.OrderClientID = ""
			lvl.ExchangeOrderID = ""
			lvl.OrderQty = 0
			lvl.IsExitOrder = false
			lvl.PairWithIndex = 0
			return events
		}

		pnl := (lvl.FilledPrice - entryLvl.FilledPrice) * qty
		if entryLvl.Index > 0 {
			// 配对的建仓层是开空仓（Neutral模式下 index>0 的层）：
			// 买入平空时，赚的是"开空价更高、买回补仓价更低"，方向和多头相反。
			pnl = -pnl
		}
		e.realizedPnL += pnl
		lvl.Status = LevelEmpty
		lvl.OrderClientID = ""
		lvl.ExchangeOrderID = ""
		lvl.OrderQty = 0
		lvl.IsExitOrder = false
		lvl.PairWithIndex = 0
		entryLvl.Status = LevelEmpty
		entryLvl.OrderClientID = ""
		entryLvl.ExchangeOrderID = ""
		entryLvl.OrderQty = 0
		entryLvl.FilledQty = 0
		entryLvl.FilledPrice = 0
		events = append(events, Event{Time: time.Now(), Type: "take_profit",
			Message: fmt.Sprintf("配对止盈 level=%d<->%d 已实现盈亏=%.4f", entryLvl.Index, lvl.Index, pnl)})
		return events
	}

	// 走到这里，说明 lvl 是一笔全新的建仓成交（买入开多，或 Neutral 模式下
	// 卖出开空），需要挂一张止盈单。优先挂在紧邻的那一层；如果紧邻层已经被
	// 占用（比如价格连续跌穿多层，相邻层还留着另一笔尚未平仓的独立仓位或
	// 挂单），就继续往同一方向找下一个空闲层，而不是像修复前那样直接把
	// 别人的仓位记录当成平仓对象冲掉。
	var step int
	var side exchange.Side
	var posSide exchange.PositionSide
	if lvl.Index < 0 {
		step = 1
		side = exchange.SideSell
		posSide = exchange.PositionLong
	} else {
		step = -1
		side = exchange.SideBuy
		posSide = exchange.PositionShort
	}

	pairIndex := lvl.Index + step
	if pairIndex == 0 {
		pairIndex += step
	}
	for {
		pairLvl, ok := e.levels[pairIndex]
		if !ok {
			events = append(events, Event{Time: time.Now(), Type: "error",
				Message: fmt.Sprintf("level=%d 找不到空闲的止盈挂靠层（已尝试到level=%d），建议增大网格层数(GridCount)，本次将在下个tick自动重试", lvl.Index, pairIndex-step)})
			return events
		}
		if pairLvl.Status == LevelEmpty {
			if err := e.placeTakeProfit(ctx, ex, side, posSide, pairLvl.Price, qty, pairLvl, lvl.Index); err != nil {
				events = append(events, Event{Time: time.Now(), Type: "error",
					Message: fmt.Sprintf("level=%d 挂止盈单失败（数量=%.6f 目标层=%d 价格=%.4f），将在下个tick自动重试: %v",
						lvl.Index, qty, pairIndex, pairLvl.Price, err)})
			}
			return events
		}
		if pairLvl.Status == LevelOrderOpen && pairLvl.IsExitOrder && pairLvl.PairWithIndex == lvl.Index {
			// 已经是这一层自己的止盈单了（比如 ensureTakeProfits 兜底重扫时
			// 发现的），不重复下单。
			return events
		}
		// 这一层被别的仓位/挂单占用了，继续往同一方向找下一层。
		pairIndex += step
		if pairIndex == 0 {
			pairIndex += step
		}
	}
}

func (e *Engine) placeTakeProfit(ctx context.Context, ex exchange.Exchange, side exchange.Side, posSide exchange.PositionSide, price, qty float64, targetLvl *Level, entryIndex int) error {
	e.seq++
	clientID := fmt.Sprintf("%s-TP-%d-%s-%d", e.cfg.Symbol, targetLvl.Index, e.instanceID, e.seq)
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
		return err
	}
	targetLvl.Status = LevelOrderOpen
	targetLvl.OrderClientID = clientID
	targetLvl.ExchangeOrderID = order.ExchangeOrderID
	targetLvl.OrderQty = qty
	// 明确标记"这一层现在是止盈单，配对的建仓层是 entryIndex"——
	// 之后这笔止盈单成交时，直接按这两个字段判断该怎么结算，不再去猜。
	targetLvl.IsExitOrder = true
	targetLvl.PairWithIndex = entryIndex
	return nil
}

// ensureTakeProfits 兜底扫描：任何处于"已成交/持仓中"（LevelFilled）状态的层，
// 理论上都应该有一个配对层正挂着止盈单（LevelOrderOpen）在等它成交。
// 如果配对层是 Empty（说明止盈单从未成功挂出，或者被 recenter 撤销后没再挂回去），
// 就重新尝试挂一次。pairAndPlaceTakeProfit 内部已经对"配对层已经是 OrderOpen/Filled"
// 的情况做了跳过处理，这里可以安全地每个 tick 都调用，不会产生重复止盈单。
func (e *Engine) ensureTakeProfits(ctx context.Context, ex exchange.Exchange) []Event {
	var events []Event
	for _, lvl := range e.levels {
		if lvl.Status != LevelFilled || lvl.FilledQty <= 0 {
			continue
		}
		events = append(events, e.pairAndPlaceTakeProfit(ctx, ex, lvl, lvl.FilledQty)...)
	}
	return events
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
//
// 已不再被"回撤保护性强平"路径调用（见 ResetPositionSide），仅保留作为
// 需要整体推倒重来时（比如手动排障）的兜底工具。
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

// ResetPositionSide 只清理指定持仓方向（Long 对应 index<0 的买入层，Short 对应
// index>0 的卖出层，仅 Neutral 模式会用到 Short）相关的层状态和挂单，不影响该
// 网格上其它方向、其它未成交层的正常挂单。
//
// 背景：这里以前统一调用 ForceReset——"回撤保护性强平"只是把某一个方向的持仓
// 用市价单平掉了，但 ForceReset 会把整个网格所有层的挂单全部撤销、状态全部
// 清空重建。代价是只要有一层触发了保护线，其它运行正常、完全没问题的层
// （包括同方向还没成交的建仓挂单、Neutral 模式下反方向的层）也会被一起打断
// 重来。震荡行情下这道保护线很容易被频繁触发（比如浮盈刚过1%又回撤过半），
// 于是"网格被腰斩重建"变成了家常便饭：不仅额外产生撤单/重挂成本、市价平仓
// 的吃单手续费，还会打断其它本来快要正常走到止盈价的层，让它们也提前跟着
// 陪葬。改成只精确处理被强平方向涉及到的层：已经建仓（Filled）的层，因为
// 对应的持仓已经被外部市价单平掉了，标记清空；它们各自配对的止盈挂单
// （如果已经挂出、状态是 OrderOpen）也一并撤销清空，因为止盈单对应的库存
// 已经不存在了，继续挂着只会变成一个减仓方向对不上任何真实持仓的孤儿单。
// 其它层完全不动，继续按各自计划运行。
//
// 直接按 PairWithIndex 精确匹配对应的止盈层，不再需要公式反推，见下方实现。
func (e *Engine) ResetPositionSide(ctx context.Context, ex exchange.Exchange, posSide exchange.PositionSide) []Event {
	var events []Event
	for idx, lvl := range e.levels {
		isEntryLevel := (posSide == exchange.PositionLong && idx < 0) || (posSide == exchange.PositionShort && idx > 0)
		if !isEntryLevel || lvl.Status != LevelFilled {
			continue
		}

		// 直接按 PairWithIndex 精确找到这个建仓层对应的止盈层，不再用
		// "相邻层"公式反推——止盈单如果因为相邻层被占用而挂到了更远一层
		// （见 pairAndPlaceTakeProfit 的向外搜索逻辑），公式反推会找错层。
		for _, pairLvl := range e.levels {
			if pairLvl.Status == LevelOrderOpen && pairLvl.IsExitOrder && pairLvl.PairWithIndex == idx {
				if pairLvl.ExchangeOrderID != "" {
					if err := ex.CancelOrder(ctx, e.cfg.Symbol, pairLvl.ExchangeOrderID); err != nil {
						events = append(events, Event{Time: time.Now(), Type: "error",
							Message: fmt.Sprintf("撤销level=%d遗留止盈单失败（持仓已被强平，该单已无对应库存，请手动核对交易所挂单）: %v", pairLvl.Index, err)})
					}
				}
				pairLvl.Status = LevelEmpty
				pairLvl.OrderClientID = ""
				pairLvl.ExchangeOrderID = ""
				pairLvl.OrderQty = 0
				pairLvl.IsExitOrder = false
				pairLvl.PairWithIndex = 0
				break
			}
		}

		lvl.Status = LevelEmpty
		lvl.OrderClientID = ""
		lvl.ExchangeOrderID = ""
		lvl.OrderQty = 0
		lvl.FilledQty = 0
		lvl.FilledPrice = 0
	}
	return events
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
