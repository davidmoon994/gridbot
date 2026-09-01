// Package risk 实现独立于策略的风控引擎。
//
// 设计原则（参考 NOFX）：策略（网格引擎）只负责"提议"要挂什么单，
// 真正下单前必须经过这里的硬约束校验；任何策略参数都不能绕过这一层。
// 这样即使网格参数配置错误或策略逻辑有bug，也有最后一道防线。
package risk

import (
	"fmt"
	"time"
)

// Limits 是硬约束配置，建议由系统管理员/配置文件设置，
// 不应暴露给"策略自定义"层随意修改。
type Limits struct {
	// MaxLeverage 允许的最大杠杆倍数
	MaxLeverage float64

	// MaxPositionQuoteRatio 单个交易对最大持仓名义价值 / 账户净值 的比例
	// 例如 0.5 表示最多用净值的50%去持有该币种的仓位
	MaxPositionQuoteRatio float64

	// MaxTotalMarginRatio 总保证金使用率上限（0~1），预留部分给清算保护和手续费
	MaxTotalMarginRatio float64

	// MinOrderQuoteAmount 单笔订单最小名义金额（USDT），低于交易所最小名义价值会被拒
	MinOrderQuoteAmount float64

	// MaxDrawdownFromPeakPct 单个持仓从最高浮盈回撤超过该比例（0~1）时，
	// 触发强制平仓保护（对应 NOFX 的"回撤自动平仓"机制）
	MaxDrawdownFromPeakPct float64

	// MinPeakPctForProtection 浮盈峰值至少达到该百分比（如 1.0 表示1%）才启用回撤保护判断。
	// 避免峰值本身就很小（比如0.5%）时，微小波动被放大成"回撤过半"的假信号。
	MinPeakPctForProtection float64

	// MaxDailyLossQuote 当日最大允许亏损（USDT），触发后暂停新开仓（熔断）
	MaxDailyLossQuote float64

	// CircuitBreakerPriceMovePct 单次行情检测到价格跳动超过该百分比（如插针、极端行情）
	// 时，暂停新开仓一段时间，只允许平仓操作
	CircuitBreakerPriceMovePct float64
	CircuitBreakerCooldownSec  int
}

// DefaultLimits 提供一组保守的默认硬约束，参考 NOFX 文档中的约束设定
func DefaultLimits() Limits {
	return Limits{
		MaxLeverage:                20,
		MaxPositionQuoteRatio:      0.5,
		MaxTotalMarginRatio:        0.9,
		MinOrderQuoteAmount:        12,
		MaxDrawdownFromPeakPct:     0.5,
		MinPeakPctForProtection:    1.0, // 浮盈峰值需达到1%才启用回撤保护，过滤微小波动噪音
		MaxDailyLossQuote:          0,   // 0 表示不启用，需显式配置
		CircuitBreakerPriceMovePct: 8.0, // 单tick 8% 视为极端行情
		CircuitBreakerCooldownSec:  600,
	}
}

// AccountState 是校验时需要的账户与市场上下文，由调用方（调度器）填充
type AccountState struct {
	EquityQuote             float64 // 账户净值（USDT）
	UsedMarginQuote         float64 // 已用保证金
	SymbolPositionQuote     float64 // 该交易对当前持仓名义价值
	DailyRealizedPnL        float64 // 当日已实现盈亏（负数表示亏损）
	LastPrice               float64
	PrevPrice               float64
	PeakUnrealizedPnLPct    float64 // 该持仓历史最高浮盈百分比
	CurrentUnrealizedPnLPct float64
}

// OrderIntent 是策略提议的一笔订单，等待风控校验
type OrderIntent struct {
	Symbol       string
	QuoteAmount  float64 // 名义金额
	Leverage     float64
	IsReduceOnly bool // 平仓/减仓单通常应放宽限制（风险在降低而非增加）
}

// Decision 风控裁决结果
type Decision struct {
	Approved bool
	Reason   string
}

// Engine 风控引擎
type Engine struct {
	limits Limits

	circuitBreakerUntil time.Time
}

// NewEngine 创建风控引擎
func NewEngine(limits Limits) *Engine {
	return &Engine{limits: limits}
}

// CheckPriceMove 独立检测一次价格跳动是否触发极端行情熔断。
//
// 之所以要单独提供这个方法、不能只依赖 Evaluate 内部的检测：Evaluate 只在
// 网格引擎"恰好有新订单要下"的时候才会被调用；如果价格发生剧烈跳动的那个
// tick 周期里网格引擎没有触发任何新挂单（比如所有该挂的单子都已经挂着），
// 熔断检测就完全不会被执行到，极端行情熔断形同虚设。调用方（manager）应该
// 在每个tick周期都主动调用这个方法一次，不依赖是否有下单动作。
func (e *Engine) CheckPriceMove(lastPrice, prevPrice float64) {
	if prevPrice <= 0 {
		return
	}
	movePct := absF(lastPrice-prevPrice) / prevPrice * 100
	if movePct >= e.limits.CircuitBreakerPriceMovePct {
		e.circuitBreakerUntil = time.Now().Add(time.Duration(e.limits.CircuitBreakerCooldownSec) * time.Second)
	}
}

// Evaluate 对一笔订单意图进行硬约束校验。任何一条不通过都会拒绝，
// 且不提供"策略自定义覆盖"的旁路——这是与 prompt-guide 中"模式3需自行负责风控"
// 完全不同的设计：这里的风控在代码层面强制执行，网格参数无法绕开。
func (e *Engine) Evaluate(intent OrderIntent, state AccountState) Decision {
	// 下单时也顺带检测一次（双重保险：manager 每个tick已经会主动调用
	// CheckPriceMove，这里是防止调用方遗漏时的兜底）
	e.CheckPriceMove(state.LastPrice, state.PrevPrice)

	// 0. 熔断期间只允许减仓单
	if !intent.IsReduceOnly && time.Now().Before(e.circuitBreakerUntil) {
		return Decision{Approved: false, Reason: fmt.Sprintf(
			"熔断保护生效中（至 %s），暂停新开仓", e.circuitBreakerUntil.Format("15:04:05"))}
	}

	if intent.IsReduceOnly {
		// 减仓单只做最基本的合法性检查，不做仓位/杠杆限制（风险在降低）
		return Decision{Approved: true}
	}

	// 2. 最小名义金额
	if intent.QuoteAmount < e.limits.MinOrderQuoteAmount {
		return Decision{Approved: false, Reason: fmt.Sprintf(
			"订单名义金额 %.2f 低于最小限制 %.2f", intent.QuoteAmount, e.limits.MinOrderQuoteAmount)}
	}

	// 3. 杠杆限制
	if intent.Leverage > e.limits.MaxLeverage {
		return Decision{Approved: false, Reason: fmt.Sprintf(
			"杠杆 %.1fx 超过最大限制 %.1fx", intent.Leverage, e.limits.MaxLeverage)}
	}

	// 4. 单交易对仓位占净值比例
	if state.EquityQuote > 0 {
		projected := state.SymbolPositionQuote + intent.QuoteAmount
		ratio := projected / state.EquityQuote
		if ratio > e.limits.MaxPositionQuoteRatio {
			return Decision{Approved: false, Reason: fmt.Sprintf(
				"该交易对预计仓位占净值 %.1f%%，超过最大限制 %.1f%%",
				ratio*100, e.limits.MaxPositionQuoteRatio*100)}
		}
	}

	// 5. 总保证金使用率
	if state.EquityQuote > 0 {
		projectedMargin := state.UsedMarginQuote + intent.QuoteAmount/maxF(intent.Leverage, 1)
		marginRatio := projectedMargin / state.EquityQuote
		if marginRatio > e.limits.MaxTotalMarginRatio {
			return Decision{Approved: false, Reason: fmt.Sprintf(
				"预计总保证金使用率 %.1f%%，超过最大限制 %.1f%%",
				marginRatio*100, e.limits.MaxTotalMarginRatio*100)}
		}
	}

	// 6. 当日最大亏损熔断
	if e.limits.MaxDailyLossQuote > 0 && -state.DailyRealizedPnL >= e.limits.MaxDailyLossQuote {
		return Decision{Approved: false, Reason: fmt.Sprintf(
			"当日已实现亏损 %.2f 达到熔断线 %.2f，停止新开仓",
			-state.DailyRealizedPnL, e.limits.MaxDailyLossQuote)}
	}

	return Decision{Approved: true}
}

// ShouldForceClose 判断某个持仓是否触发"回撤保护"强制平仓：
// 浮盈从历史峰值回撤超过设定比例。对应 prompt-guide 中提到的
// "最高收益率"与"盈亏回撤"概念。
func (e *Engine) ShouldForceClose(state AccountState) (bool, string) {
	if state.PeakUnrealizedPnLPct < e.limits.MinPeakPctForProtection {
		return false, ""
	}
	drawdown := (state.PeakUnrealizedPnLPct - state.CurrentUnrealizedPnLPct) / state.PeakUnrealizedPnLPct
	if drawdown >= e.limits.MaxDrawdownFromPeakPct {
		return true, fmt.Sprintf("浮盈从峰值 %.2f%% 回撤 %.1f%%，触发保护性平仓",
			state.PeakUnrealizedPnLPct, drawdown*100)
	}
	return false, ""
}

// InCircuitBreaker 当前是否处于熔断期
func (e *Engine) InCircuitBreaker() (bool, time.Time) {
	return time.Now().Before(e.circuitBreakerUntil), e.circuitBreakerUntil
}

func absF(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func maxF(v, floor float64) float64 {
	if v < floor {
		return floor
	}
	return v
}
