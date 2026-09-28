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
	"sort"
	"strings"
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

	// IsShort 明确记录这一层持有/挂着的是不是空头仓位（仅 Neutral 模式的
	// index>0 建仓单会是 true）。不能再用"层号的正负"去推断多空方向：
	// 重新居中之后旧持仓会被搬到新网格里，层号的正负和它真实的多空方向
	// 已经没有任何关系了。
	IsShort bool `json:"is_short"`
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
	// UnrealizedPnL 是按当前市价实时估算的浮动盈亏（USDT/USDC），跟
	// RealizedPnL 不是一回事：RealizedPnL 只在止盈单真正成交、完成一次
	// 完整平仓时才会变化，价格波动本身不会让它跳动；这个字段才是"现在
	// 账面上浮盈/浮亏多少"，会随行情实时变化。
	UnrealizedPnL float64 `json:"unrealized_pnl"`
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

	// tpRetryAfter / tpErrLoggedAt 用来给"挂止盈单失败"做退避和日志限流：
	// 失败后至少间隔一段时间才会重试同一笔持仓，错误日志也不会每个tick刷一条。
	tpRetryAfter  map[int]time.Time
	tpErrLoggedAt map[int]time.Time
}

// exitIndexBase 止盈单在 e.levels 里占用的层号从这里开始往上排。
// 止盈单的价格取决于对应持仓的成本价，而不是网格层位，所以不能再占用
// 网格自己的层位（否则重新居中后价格和层号脱钩，止盈单就会找不到位置或被放错位置）。
const exitIndexBase = 1000

const (
	tpRetryBackoff = 15 * time.Second
	tpErrLogEvery  = 60 * time.Second
)

// NewEngine 创建一个新的移动网格引擎
func NewEngine(cfg Config) *Engine {
	return &Engine{
		cfg:           cfg,
		levels:        map[int]*Level{},
		instanceID:    randomInstanceID(),
		tpRetryAfter:  map[int]time.Time{},
		tpErrLoggedAt: map[int]time.Time{},
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
	inheritEvents, ad := e.inheritExistingOrders(ctx, ex)
	events = append(events, inheritEvents...)
	events = append(events, e.reconcileExistingPositions(ctx, ex, ad)...)

	placeEvents, err := e.placeMissingOrders(ctx, ex, ticker.Price)
	if err != nil {
		return events, err
	}
	return append(events, placeEvents...), nil
}

// adoption 记录"启动时从遗留止盈单里认领回来的持仓"总量和成本，
// 供后面核对交易所净持仓时扣除，剩下的才是真正需要新挂止盈单的部分。
type adoption struct {
	longQty, longCost   float64
	shortQty, shortCost float64
}

// inheritExistingOrders 处理上一次运行遗留在交易所的本策略挂单（程序重启/重新启动网格时）：
//   - 基础（建仓）单：撤销。新网格会按新的中心/间距重新挂，不撤会叠出重复的买单；
//   - 止盈单：不撤，原样认领。每张止盈单对应一笔持仓，按"止盈价 ∓ 一个间距"还原它的
//     大致成本价，重建"持仓 + 止盈单"这一对——这样重启之后多笔持仓不会被合并成一笔，
//     旧的止盈单也不会变成脱离追踪的孤儿单、更不会和新挂的止盈单叠加超出持仓
//     （币安会按"所有挂着的减仓单合计数量 ≤ 持仓数量"校验，超了就是 -2022 拒单）。
func (e *Engine) inheritExistingOrders(ctx context.Context, ex exchange.Exchange) ([]Event, adoption) {
	var events []Event
	var ad adoption

	orders, err := ex.GetOpenOrders(ctx, e.cfg.Symbol)
	if err != nil {
		events = append(events, Event{Time: time.Now(), Type: "error",
			Message: fmt.Sprintf("启动时读取交易所遗留挂单失败，本次不会清理/认领旧挂单，请留意是否有重复挂单: %v", err)})
		return events, ad
	}

	var exits []exchange.Order
	cancelled := 0
	for _, o := range orders {
		switch {
		case e.isOurEntryOrder(o.ClientOrderID):
			if err := ex.CancelOrder(ctx, e.cfg.Symbol, o.ExchangeOrderID); err != nil {
				events = append(events, Event{Time: time.Now(), Type: "error",
					Message: fmt.Sprintf("启动时撤销遗留基础单失败 orderID=%s: %v", o.ExchangeOrderID, err)})
			} else {
				cancelled++
			}
		case e.isOurExitOrder(o.ClientOrderID):
			exits = append(exits, o)
		}
	}
	if cancelled > 0 {
		events = append(events, Event{Time: time.Now(), Type: "info",
			Message: fmt.Sprintf("启动时撤销了上一次遗留的基础挂单 %d 张（新网格会重新挂）", cancelled)})
	}
	if len(exits) == 0 {
		return events, ad
	}

	// 合约：认领数量不能超过交易所真实持仓；现货没有持仓接口，全部认领。
	capLong, capShort := math.Inf(1), math.Inf(1)
	if !e.cfg.IsSpot() {
		positions, err := ex.GetPositions(ctx, e.cfg.Symbol)
		if err != nil {
			events = append(events, Event{Time: time.Now(), Type: "error",
				Message: fmt.Sprintf("启动时核对持仓失败，遗留止盈单本次不认领（保持原样不撤），请留意: %v", err)})
			return events, ad
		}
		capLong, capShort = 0, 0
		for _, p := range positions {
			if p.PositionSide == exchange.PositionShort {
				capShort += p.Quantity
			} else {
				capLong += p.Quantity
			}
		}
	}

	// 止盈价离成本近的先认领
	sort.SliceStable(exits, func(i, j int) bool {
		a, b := exits[i], exits[j]
		if (a.Side == exchange.SideBuy) != (b.Side == exchange.SideBuy) {
			return a.Side != exchange.SideBuy
		}
		if a.Side == exchange.SideBuy {
			return a.Price > b.Price
		}
		return a.Price < b.Price
	})

	adopted := 0
	for _, o := range exits {
		qty := o.Quantity - o.FilledQuantity
		if qty <= 0 {
			continue
		}
		isShort := o.Side == exchange.SideBuy
		entryPx := o.Price - e.spacing
		if isShort {
			entryPx = o.Price + e.spacing
		}
		if isShort {
			if ad.shortQty+qty > capShort+1e-9 {
				events = append(events, Event{Time: time.Now(), Type: "error",
					Message: fmt.Sprintf("遗留止盈单 %s（价格=%.4f 数量=%.6f）超出当前空头持仓，未认领也未撤销，请人工核对", o.ExchangeOrderID, o.Price, qty)})
				continue
			}
			ad.shortQty += qty
			ad.shortCost += qty * entryPx
		} else {
			if ad.longQty+qty > capLong+1e-9 {
				events = append(events, Event{Time: time.Now(), Type: "error",
					Message: fmt.Sprintf("遗留止盈单 %s（价格=%.4f 数量=%.6f）超出当前多头持仓，未认领也未撤销，请人工核对", o.ExchangeOrderID, o.Price, qty)})
				continue
			}
			ad.longQty += qty
			ad.longCost += qty * entryPx
		}
		idx := e.seatEntry(entryPx, qty, time.Now(), isShort)
		exitIdx := e.nextExitIndex()
		e.levels[exitIdx] = &Level{
			Index: exitIdx, Price: o.Price, Status: LevelOrderOpen,
			OrderClientID: o.ClientOrderID, ExchangeOrderID: o.ExchangeOrderID,
			OrderQty: qty, IsExitOrder: true, PairWithIndex: idx, IsShort: isShort,
		}
		adopted++
	}
	if adopted > 0 {
		events = append(events, Event{Time: time.Now(), Type: "info",
			Message: fmt.Sprintf("启动时认领了上一次遗留的止盈单 %d 张（未撤销），并还原出对应的持仓记录", adopted)})
	}
	return events, ad
}

// reconcileExistingPositions 核对交易所这个symbol当前的真实净持仓，扣除已经通过遗留止盈单
// 认领回来的部分（见 inheritExistingOrders），剩下的部分认领成一笔持仓并挂出止盈单——
// 而不是放任这笔仓位在软件的记账里凭空消失。
//
// 现货没有交易所原生"持仓"概念可查（见 exchange/binance_spot.go 顶部注释），
// 这里对现货直接跳过。
func (e *Engine) reconcileExistingPositions(ctx context.Context, ex exchange.Exchange, ad adoption) []Event {
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

	minQty, minNotional := 0.0, 0.0
	if info, err := ex.GetSymbolInfo(ctx, e.cfg.Symbol); err == nil && info != nil {
		minQty, minNotional = info.MinQuantity, info.MinNotional
	}

	for _, p := range positions {
		if p.Quantity <= 0 || p.EntryPrice <= 0 {
			continue
		}
		isShort := p.PositionSide == exchange.PositionShort
		qty, entryPx := p.Quantity, p.EntryPrice

		adoptedQty, adoptedCost := ad.longQty, ad.longCost
		if isShort {
			adoptedQty, adoptedCost = ad.shortQty, ad.shortCost
		}
		if adoptedQty > 0 {
			rem := p.Quantity - adoptedQty
			if rem <= 1e-9 {
				continue // 已经被遗留止盈单完全覆盖
			}
			remCost := p.Quantity*p.EntryPrice - adoptedCost
			if px := remCost / rem; px > 0 {
				entryPx = px
			}
			qty = rem
			if rem < minQty || rem*entryPx < minNotional {
				events = append(events, Event{Time: time.Now(), Type: "info",
					Message: fmt.Sprintf("启动核对：扣除已认领的止盈单后，剩余持仓 %.6f 太小，不足以挂止盈单，已忽略", rem)})
				continue
			}
		}

		idx := e.seatEntry(entryPx, qty, time.Now(), isShort)
		events = append(events, Event{Time: time.Now(), Type: "info",
			Message: fmt.Sprintf("启动时发现交易所遗留持仓：%s 数量=%.6f 成本=%.4f，已接管到 level=%d，将挂出对应止盈单",
				p.PositionSide, qty, entryPx, idx)})
		events = append(events, e.placeExitFor(ctx, ex, e.levels[idx], qty)...)
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
			lvl.IsShort = false
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
			lvl.IsShort = true
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
	pending := make([]*Level, 0, len(e.levels))
	for _, lvl := range e.levels {
		if lvl.Status == LevelOrderOpen {
			pending = append(pending, lvl)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Index < pending[j].Index })
	for _, lvl := range pending {
		if _, stillOpen := openByClientID[lvl.OrderClientID]; stillOpen {
			continue
		}

		if lvl.ExchangeOrderID == "" {
			// 理论上不应发生（下单成功时必然会记录交易所订单ID）；
			// 为兼容极端情况，保守地按"已成交"处理，并记录一条警告方便排查。
			events = append(events, Event{Time: time.Now(), Type: "error",
				Message: fmt.Sprintf("level=%d 缺少交易所订单ID，按已成交处理（请检查是否为异常数据）", lvl.Index)})
			events = append(events, e.onLevelFilled(ctx, ex, lvl, nil)...)
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
			events = append(events, e.onLevelFilled(ctx, ex, lvl, order)...)
		case exchange.OrderStatusCanceled, exchange.OrderStatusRejected:
			// 被撤销/被拒绝，绝不能当成交处理，否则会凭空产生一笔不存在的仓位记录。
			if lvl.IsExitOrder {
				// 止盈单被撤了：删掉这条记录，对应的持仓会被下面的兜底扫描重新挂上止盈单。
				delete(e.levels, lvl.Index)
				events = append(events, Event{Time: time.Now(), Type: "info",
					Message: fmt.Sprintf("止盈单被撤销/拒绝（价格=%.4f 状态=%s），将自动补挂", lvl.Price, order.Status)})
			} else {
				lvl.Status = LevelEmpty
				lvl.OrderClientID = ""
				lvl.ExchangeOrderID = ""
				lvl.OrderQty = 0
				lvl.IsShort = false
				events = append(events, Event{Time: time.Now(), Type: "info",
					Message: fmt.Sprintf("level=%d 挂单被撤销/拒绝（状态=%s），已重置为空层", lvl.Index, order.Status)})
			}
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

// onLevelFilled 处理某一层的订单成交后的动作：
//   - 建仓单成交（买入开多；Neutral 模式下卖出开空）：记录持仓，并立刻为它挂出止盈单；
//   - 止盈单成交：按记录的配对关系结算已实现盈亏，释放对应的持仓层。
//
// order 是这笔单子的交易所真实状态（来自 OnTick 里的 GetOrder），优先使用它返回的
// 真实成交数量/均价做记账；只有极端情况下查不到订单详情（order == nil）时，才退化使用
// 下单时记录的 lvl.OrderQty / 挂单价 lvl.Price 兜底。
func (e *Engine) onLevelFilled(ctx context.Context, ex exchange.Exchange, lvl *Level, order *exchange.Order) []Event {
	var events []Event
	qty := lvl.OrderQty
	price := lvl.Price
	if qty <= 0 {
		// 兜底：连 OrderQty 都没有记录到的极端情况（理论不应发生）。
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

	if lvl.IsExitOrder {
		return e.settleExit(lvl, qty)
	}

	events = append(events, Event{Time: time.Now(), Type: "grid_filled",
		Message: fmt.Sprintf("网格成交 level=%d price=%.4f qty=%.6f", lvl.Index, price, qty)})
	events = append(events, e.placeExitFor(ctx, ex, lvl, qty)...)
	return events
}

// pairAndPlaceTakeProfit 是"给一笔刚成交/刚认领的订单做后续处理"的统一入口：
//   - lvl 是一张止盈单（IsExitOrder）：结算平仓；
//   - lvl 是一笔持仓：确保它有一张止盈单挂着（已有则什么都不做）。
//
// 从两处调用：正常成交检测走 onLevelFilled；程序重启后核对交易所真实遗留持仓走
// reconcileExistingPositions。
func (e *Engine) pairAndPlaceTakeProfit(ctx context.Context, ex exchange.Exchange, lvl *Level, qty float64) []Event {
	if lvl.IsExitOrder {
		return e.settleExit(lvl, qty)
	}
	return e.placeExitFor(ctx, ex, lvl, qty)
}

// settleExit 结算一张已经成交的止盈单：按配对关系找到对应持仓，计入已实现盈亏，
// 释放持仓层，并把这张止盈单从层表里移除。
func (e *Engine) settleExit(exitLvl *Level, qty float64) []Event {
	var events []Event
	exitIdx := exitLvl.Index
	entry, ok := e.levels[exitLvl.PairWithIndex]
	delete(e.levels, exitIdx) // 这张止盈单已经成交，无论如何都要移除
	if !ok || entry.Status != LevelFilled || entry.IsExitOrder {
		events = append(events, Event{Time: time.Now(), Type: "error",
			Message: fmt.Sprintf("止盈单成交（价格=%.4f 数量=%.6f），但找不到对应的持仓记录（level=%d），已实现盈亏无法准确计算，请人工核对交易所实际持仓",
				exitLvl.FilledPrice, qty, exitLvl.PairWithIndex)})
		return events
	}
	entryIdx, entryPx, isShort := entry.Index, entry.FilledPrice, entry.IsShort
	pnl := (exitLvl.FilledPrice - entryPx) * qty
	if isShort {
		// 空头：开空价更高、买回价更低才赚钱，方向和多头相反
		pnl = -pnl
	}
	e.realizedPnL += pnl
	e.releaseEntry(entryIdx)
	events = append(events, Event{Time: time.Now(), Type: "take_profit",
		Message: fmt.Sprintf("配对止盈 建仓level=%d 成本=%.4f 平仓价=%.4f 数量=%.6f 已实现盈亏=%.4f",
			entryIdx, entryPx, exitLvl.FilledPrice, qty, pnl)})
	return events
}

// placeExitFor 给一笔持仓挂出止盈单。
//
// 止盈价 = 这笔持仓自己的成本价 ± 一个网格间距（多头加、空头减），和网格层位、层号、
// 重新居中之后的网格坐标都没有关系——止盈单不再占用网格层位，所以不存在
// "找不到空闲层"或者"止盈价比成本还低"这类问题。
//
// 已经有止盈单挂着就直接跳过（每个 tick 的兜底扫描会反复调用这里，必须幂等）；
// 挂单失败会退避一段时间再重试，错误日志也做了限流，不会每个 tick 刷一条。
func (e *Engine) placeExitFor(ctx context.Context, ex exchange.Exchange, entry *Level, qty float64) []Event {
	var events []Event
	if e.findExitFor(entry.Index) != nil {
		return events
	}
	now := time.Now()
	if t, ok := e.tpRetryAfter[entry.Index]; ok && now.Before(t) {
		return events
	}

	side, posSide := exchange.SideSell, exchange.PositionLong
	tpPrice := entry.FilledPrice + e.spacing
	if entry.IsShort {
		side, posSide = exchange.SideBuy, exchange.PositionShort
		tpPrice = entry.FilledPrice - e.spacing
	}
	if tpPrice <= 0 {
		return events
	}

	exitLvl := &Level{Index: e.nextExitIndex(), Price: tpPrice, Status: LevelEmpty, IsShort: entry.IsShort}
	if err := e.placeTakeProfit(ctx, ex, side, posSide, tpPrice, qty, exitLvl, entry.Index); err != nil {
		e.tpRetryAfter[entry.Index] = now.Add(tpRetryBackoff)
		if last, ok := e.tpErrLoggedAt[entry.Index]; !ok || now.Sub(last) >= tpErrLogEvery {
			e.tpErrLoggedAt[entry.Index] = now
			events = append(events, Event{Time: now, Type: "error",
				Message: fmt.Sprintf("持仓(level=%d 成本=%.4f 数量=%.6f)挂止盈单失败（目标价=%.4f），将自动重试: %v",
					entry.Index, entry.FilledPrice, qty, tpPrice, err)})
		}
		return events
	}
	e.levels[exitLvl.Index] = exitLvl
	delete(e.tpRetryAfter, entry.Index)
	delete(e.tpErrLoggedAt, entry.Index)
	return events
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

// ensureTakeProfits 兜底扫描：所有处于"持仓中"（LevelFilled）的层，都应该有一张止盈单
// 正挂着。没有的（止盈单从未挂成功、被人手动撤了、被交易所拒了）就补挂。
// placeExitFor 是幂等的，可以每个 tick 都调用。
func (e *Engine) ensureTakeProfits(ctx context.Context, ex exchange.Exchange) []Event {
	var events []Event
	var entries []*Level
	for _, lvl := range e.levels {
		if lvl.Status == LevelFilled && !lvl.IsExitOrder && lvl.FilledQty > 0 {
			entries = append(entries, lvl)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Index < entries[j].Index })
	for _, lvl := range entries {
		events = append(events, e.placeExitFor(ctx, ex, lvl, lvl.FilledQty)...)
	}
	return events
}

// nextExitIndex 返回一个当前没被占用的止盈单层号（从 exitIndexBase 起找第一个空位，
// 已平仓释放掉的号会被复用，所以号码不会无限增长）。
func (e *Engine) nextExitIndex() int {
	for i := exitIndexBase; ; i++ {
		if _, used := e.levels[i]; !used {
			return i
		}
	}
}

// findExitFor 找到某个持仓层当前正挂着的止盈单（没有则返回 nil）。
// 直接按 PairWithIndex 精确匹配，不依赖任何层号大小/正负的推断。
func (e *Engine) findExitFor(entryIndex int) *Level {
	for _, l := range e.levels {
		if l.IsExitOrder && l.Status == LevelOrderOpen && l.PairWithIndex == entryIndex {
			return l
		}
	}
	return nil
}

// seatEntry 把一笔已经存在的持仓（重新居中前的旧持仓、或重启后从交易所认领回来的持仓）
// 安放进当前网格，返回它占用的层号。
//
// 规则：
//   - 多头只会落在负数层、空头只会落在正数层（层号的正负只是"这一层在中心的哪一侧"，
//     多空方向由 Level.IsShort 明确记录，不再靠层号推断）；
//   - 优先落在成本价对应的最近一层，被占用就往更远处找空层；
//   - 成本价不在建仓区（多头高于中心/空头低于中心），或建仓区已经没有空层时，
//     落到网格范围之外的"溢出层"：它不占用任何一个真实的挂单价位，但同样被
//     记账、统计浮盈、并配有止盈单——绝不能因为放不下就把这笔持仓丢掉。
func (e *Engine) seatEntry(price, qty float64, filledAt time.Time, isShort bool) int {
	n := e.cfg.GridCount
	idx := 0
	found := false
	if e.spacing > 0 {
		desired := int(math.Round((price - e.center) / e.spacing))
		if !isShort && price < e.center {
			if desired > -1 {
				desired = -1
			}
			if desired < -n {
				desired = -n
			}
			for i := desired; i >= -n; i-- {
				if l, ok := e.levels[i]; ok && l.Status == LevelEmpty {
					idx, found = i, true
					break
				}
			}
		} else if isShort && price > e.center {
			if desired < 1 {
				desired = 1
			}
			if desired > n {
				desired = n
			}
			for i := desired; i <= n; i++ {
				if l, ok := e.levels[i]; ok && l.Status == LevelEmpty {
					idx, found = i, true
					break
				}
			}
		}
	}
	if !found {
		for k := 1; ; k++ {
			cand := -(n + k)
			if isShort {
				cand = n + k
			}
			if _, used := e.levels[cand]; !used {
				e.levels[cand] = &Level{Index: cand, Price: price, Status: LevelEmpty}
				idx = cand
				break
			}
		}
	}
	lvl := e.levels[idx]
	lvl.Status = LevelFilled
	lvl.FilledQty = qty
	lvl.FilledPrice = price
	lvl.FilledAt = filledAt
	lvl.OrderQty = qty
	lvl.OrderClientID = ""
	lvl.ExchangeOrderID = ""
	lvl.IsExitOrder = false
	lvl.PairWithIndex = 0
	lvl.IsShort = isShort
	return idx
}

// releaseEntry 释放一个持仓层：网格范围内的层恢复成空层（可以重新挂买单），
// 溢出层直接删除。
func (e *Engine) releaseEntry(idx int) {
	delete(e.tpRetryAfter, idx)
	delete(e.tpErrLoggedAt, idx)
	n := e.cfg.GridCount
	if idx < -n || idx > n {
		delete(e.levels, idx)
		return
	}
	lvl, ok := e.levels[idx]
	if !ok {
		return
	}
	lvl.Status = LevelEmpty
	lvl.OrderClientID = ""
	lvl.ExchangeOrderID = ""
	lvl.OrderQty = 0
	lvl.FilledQty = 0
	lvl.FilledPrice = 0
	lvl.IsExitOrder = false
	lvl.PairWithIndex = 0
	lvl.IsShort = false
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

// recenter 围绕最新 EMA 中心与 ATR 间距重新铺设网格。
//
// 只处理"以后新仓位挂在哪"：
//   - 只撤销还没成交的基础（建仓）挂单；已经挂出的止盈单一律不碰，
//     它们跟对应持仓的成本价绑定，跟网格坐标系无关，会一直挂着直到自然成交；
//   - 已经成交的持仓一笔都不能丢：按各自的成本价重新安放进新网格（见 seatEntry），
//     放不下的进溢出层，仍然记账、仍然有止盈单；
//   - 为了不留下"撤了一半、新网格没建好"的中间状态，先算好新网格，再动手撤单。
func (e *Engine) recenter(ctx context.Context, ex exchange.Exchange, currentPrice float64) ([]Event, error) {
	var events []Event

	klines, err := ex.GetKlines(ctx, e.cfg.Symbol, "3m", 200)
	if err != nil {
		return nil, err
	}
	center, spacing := e.computeCenterAndSpacing(klines, currentPrice)

	var heldEntries, heldExits []*Level
	for _, lvl := range e.levels {
		switch {
		case lvl.Status == LevelFilled && !lvl.IsExitOrder:
			cp := *lvl
			heldEntries = append(heldEntries, &cp)
		case lvl.Status == LevelOrderOpen && lvl.IsExitOrder:
			cp := *lvl
			heldExits = append(heldExits, &cp)
		}
	}

	openOrders, err := ex.GetOpenOrders(ctx, e.cfg.Symbol)
	if err != nil {
		return nil, err
	}
	for _, o := range openOrders {
		if !e.isOurEntryOrder(o.ClientOrderID) {
			continue // 止盈单、以及不属于本策略的订单，一律不动
		}
		if err := ex.CancelOrder(ctx, e.cfg.Symbol, o.ExchangeOrderID); err != nil {
			events = append(events, Event{Time: time.Now(), Type: "error",
				Message: fmt.Sprintf("重新居中时撤销基础挂单失败 orderID=%s（价格=%.4f），该单可能仍挂在交易所上，请留意是否与新挂单重复: %v",
					o.ExchangeOrderID, o.Price, err)})
		}
	}

	e.buildLevels(center, spacing)
	e.tpRetryAfter = map[int]time.Time{}
	e.tpErrLoggedAt = map[int]time.Time{}

	// 多头从成本高的开始安放（优先拿到靠近成本价的层），空头反过来。
	sort.SliceStable(heldEntries, func(i, j int) bool {
		a, b := heldEntries[i], heldEntries[j]
		if a.IsShort != b.IsShort {
			return !a.IsShort
		}
		if a.IsShort {
			return a.FilledPrice < b.FilledPrice
		}
		return a.FilledPrice > b.FilledPrice
	})
	oldToNew := map[int]int{}
	for _, h := range heldEntries {
		oldToNew[h.Index] = e.seatEntry(h.FilledPrice, h.FilledQty, h.FilledAt, h.IsShort)
	}
	for _, x := range heldExits {
		if ni, ok := oldToNew[x.PairWithIndex]; ok {
			x.PairWithIndex = ni
		}
		e.levels[x.Index] = x // 止盈单层号 >= exitIndexBase，不会和网格层位冲突
	}

	e.lastRecenter = time.Now()
	e.recenterCount++
	events = append(events, Event{Time: time.Now(), Type: "recenter",
		Message: fmt.Sprintf("重新居中 #%d：新中心=%.4f 新间距=%.4f（保留持仓 %d 笔、原有止盈单 %d 张，止盈单均未被撤销）",
			e.recenterCount, center, spacing, len(heldEntries), len(heldExits))})
	return events, nil
}

// isOurEntryOrder 判断一个交易所挂单是不是本策略挂的"基础（建仓）单"。
// 靠 ClientOrderID 前缀识别：买单 "<SYMBOL>-B-"、Neutral 模式的开空卖单 "<SYMBOL>-S-"。
// 止盈单是 "<SYMBOL>-TP-"，不在此列。
func (e *Engine) isOurEntryOrder(clientID string) bool {
	return strings.HasPrefix(clientID, e.cfg.Symbol+"-B-") || strings.HasPrefix(clientID, e.cfg.Symbol+"-S-")
}

func (e *Engine) isOurExitOrder(clientID string) bool {
	return strings.HasPrefix(clientID, e.cfg.Symbol+"-TP-")
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
	e.tpRetryAfter = map[int]time.Time{}
	e.tpErrLoggedAt = map[int]time.Time{}
	e.initialized = false
	return err
}

// ResetPositionSide 只清理指定持仓方向（Long / Short）相关的记录和止盈单，不影响该网格上
// 其它方向、其它未成交层的正常挂单。
//
// 用于风控"强制平仓"之后的收尾：该方向的持仓已经被市价单平掉了，所以对应的持仓记录要清空，
// 它们各自的止盈单也已经没有库存可减，必须撤掉（否则会变成孤儿减仓单）。
// 这是唯一会主动撤销止盈单的场景——其它任何情况（重新居中、重启、停止网格）都不碰止盈单。
func (e *Engine) ResetPositionSide(ctx context.Context, ex exchange.Exchange, posSide exchange.PositionSide) []Event {
	var events []Event
	wantShort := posSide == exchange.PositionShort
	var entries []*Level
	for _, l := range e.levels {
		if l.Status == LevelFilled && !l.IsExitOrder && l.IsShort == wantShort {
			entries = append(entries, l)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Index < entries[j].Index })
	for _, entry := range entries {
		idx := entry.Index
		if x := e.findExitFor(idx); x != nil {
			if x.ExchangeOrderID != "" {
				if err := ex.CancelOrder(ctx, e.cfg.Symbol, x.ExchangeOrderID); err != nil {
					events = append(events, Event{Time: time.Now(), Type: "error",
						Message: fmt.Sprintf("撤销遗留止盈单失败（持仓已被强平，该单已无对应库存，请手动核对交易所挂单）orderID=%s: %v", x.ExchangeOrderID, err)})
				}
			}
			delete(e.levels, x.Index)
		}
		e.releaseEntry(idx)
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

// UnrealizedPnL 按当前市价估算所有持仓的浮动盈亏（USDT/USDC）。
// 多空方向由 Level.IsShort 决定（多头现价越高浮盈越多，空头现价越低浮盈越多），
// 不能再用层号的正负推断——重新居中/重启认领之后，层号和多空方向已经没有关系了。
// 只统计已经成交的持仓；挂着还没成交的建仓单/止盈单不计入。
func (e *Engine) UnrealizedPnL(currentPrice float64) float64 {
	total := 0.0
	for _, lvl := range e.levels {
		if lvl.Status != LevelFilled || lvl.IsExitOrder {
			continue
		}
		if lvl.IsShort {
			total += (lvl.FilledPrice - currentPrice) * lvl.FilledQty
		} else {
			total += (currentPrice - lvl.FilledPrice) * lvl.FilledQty
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
		if lvl.Status == LevelFilled && !lvl.IsExitOrder && !lvl.IsShort {
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
		UnrealizedPnL:      e.UnrealizedPnL(currentPrice),
	}
}

// Config 返回引擎当前配置（只读）
func (e *Engine) Config() Config { return e.cfg }

// UpdateConfig 允许在运行中调整部分参数（不会立即触发重新铺网，
// 下一次 OnTick 判断 recenter 时才会用新参数生效）
func (e *Engine) UpdateConfig(cfg Config) { e.cfg = cfg }
