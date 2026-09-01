package risk

import (
	"context"
	"fmt"
	"sync"

	"gridbot/exchange"
)

// AccountStateProvider 由外部（调度器）提供实时账户状态，
// 供风控引擎在每次下单前做校验。
type AccountStateProvider func(ctx context.Context, symbol string, orderQuoteAmount float64) (AccountState, error)

// GuardedExchange 用装饰器模式包装任意 exchange.Exchange 实现，
// 在真正调用底层 PlaceOrder 之前，强制经过风控引擎校验。
//
// 这是本项目风控体系的核心：无论上层策略（网格引擎）传入什么参数，
// 只要不满足硬约束，订单在到达交易所之前就会被拦截，策略代码没有
// 任何旁路能绕过这一层。
type GuardedExchange struct {
	exchange.Exchange
	riskEngine *Engine
	stateFn    AccountStateProvider
	onReject   func(symbol, reason string)

	mu               sync.Mutex
	leverageBySymbol map[string]float64
}

// NewGuardedExchange 创建风控包装
func NewGuardedExchange(inner exchange.Exchange, riskEngine *Engine, stateFn AccountStateProvider, onReject func(symbol, reason string)) *GuardedExchange {
	return &GuardedExchange{
		Exchange:         inner,
		riskEngine:       riskEngine,
		stateFn:          stateFn,
		onReject:         onReject,
		leverageBySymbol: map[string]float64{},
	}
}

// SetLeverage 记录每个交易对当前设置的杠杆，供下单时风控校验使用，
// 然后再委托给底层交易所真正执行设置。
func (g *GuardedExchange) SetLeverage(ctx context.Context, symbol string, leverage float64) error {
	g.mu.Lock()
	g.leverageBySymbol[symbol] = leverage
	g.mu.Unlock()
	return g.Exchange.SetLeverage(ctx, symbol, leverage)
}

func (g *GuardedExchange) currentLeverage(symbol string) float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if lv, ok := g.leverageBySymbol[symbol]; ok && lv > 0 {
		return lv
	}
	return 1
}

// PlaceOrder 覆盖底层方法，加入风控校验
func (g *GuardedExchange) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (*exchange.Order, error) {
	quoteAmount := req.Quantity * req.Price

	state, err := g.stateFn(ctx, req.Symbol, quoteAmount)
	if err != nil {
		return nil, fmt.Errorf("风控校验前获取账户状态失败: %w", err)
	}

	intent := OrderIntent{
		Symbol:       req.Symbol,
		QuoteAmount:  quoteAmount,
		Leverage:     g.currentLeverage(req.Symbol),
		IsReduceOnly: req.ReduceOnly,
	}

	decision := g.riskEngine.Evaluate(intent, state)
	if !decision.Approved {
		if g.onReject != nil {
			g.onReject(req.Symbol, decision.Reason)
		}
		return nil, fmt.Errorf("风控拒绝: %s", decision.Reason)
	}

	return g.Exchange.PlaceOrder(ctx, req)
}
