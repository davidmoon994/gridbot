package exchange

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"
)

// PaperExchange 是一个内存中的模拟盘实现，不连接任何真实交易所。
//
// 用途：
//  1. 本地验证网格策略与风控引擎的逻辑是否正确；
//  2. 作为接入真实交易所前的回归测试环境；
//  3. 演示 Web 界面功能。
//
// 行情模拟：以随机游走 + 可注入的价格序列驱动，价格变动会触发挂单撮合。
type PaperExchange struct {
	mu sync.Mutex

	symbolPrice map[string]float64
	klines      map[string][]Kline
	orders      map[string]*Order // key: exchangeOrderID
	positions   map[string]*Position
	balances    map[string]*Balance
	leverage    map[string]float64
	orderSeq    int64

	// 波动率参数，用于随机游走模拟；可通过 SetVolatility 调整
	volatility float64
	rng        *rand.Rand
}

// NewPaperExchange 创建模拟盘，initialPrice 为各交易对初始价格
func NewPaperExchange(initialPrices map[string]float64, quoteAsset string, initialBalance float64) *PaperExchange {
	p := &PaperExchange{
		symbolPrice: map[string]float64{},
		klines:      map[string][]Kline{},
		orders:      map[string]*Order{},
		positions:   map[string]*Position{},
		balances:    map[string]*Balance{},
		leverage:    map[string]float64{},
		volatility:  0.0015, // 每个tick约0.15%波动，可调整
		rng:         rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	for sym, px := range initialPrices {
		p.symbolPrice[sym] = px
		p.klines[sym] = seedKlines(px, 200, p.rng)
	}
	p.balances[quoteAsset] = &Balance{Asset: quoteAsset, Available: initialBalance, Total: initialBalance}
	return p
}

// StartAutoTick 启动一个后台协程，按给定间隔持续推进所有交易对的模拟行情。
// 返回一个 stop 函数，调用它可以停止该协程（比如切换到真实交易所时不再需要模拟盘走字）。
func (p *PaperExchange) StartAutoTick(interval time.Duration) (stop func()) {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		p.mu.Lock()
		symbols := make([]string, 0, len(p.symbolPrice))
		for sym := range p.symbolPrice {
			symbols = append(symbols, sym)
		}
		p.mu.Unlock()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				for _, sym := range symbols {
					p.Tick(sym)
				}
			}
		}
	}()
	return func() { close(done) }
}

func seedKlines(startPrice float64, n int, rng *rand.Rand) []Kline {
	ks := make([]Kline, 0, n)
	px := startPrice
	now := time.Now().Add(-time.Duration(n) * 3 * time.Minute)
	for i := 0; i < n; i++ {
		change := (rng.Float64() - 0.5) * 0.004
		open := px
		px = px * (1 + change)
		high := math.Max(open, px) * (1 + rng.Float64()*0.001)
		low := math.Min(open, px) * (1 - rng.Float64()*0.001)
		ks = append(ks, Kline{
			OpenTime: now.Add(time.Duration(i) * 3 * time.Minute),
			Open:     open,
			High:     high,
			Low:      low,
			Close:    px,
			Volume:   1000 + rng.Float64()*500,
		})
	}
	return ks
}

// Tick 推进一次模拟行情（由外部调度器周期调用），返回最新价格。
// 内部会检查所有挂单是否触发成交。
func (p *PaperExchange) Tick(symbol string) float64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	px, ok := p.symbolPrice[symbol]
	if !ok {
		return 0
	}
	change := (p.rng.Float64() - 0.5) * 2 * p.volatility
	newPx := px * (1 + change)
	if newPx <= 0 {
		newPx = px
	}
	p.symbolPrice[symbol] = newPx

	// 追加K线
	ks := p.klines[symbol]
	last := ks[len(ks)-1]
	high := math.Max(px, newPx)
	low := math.Min(px, newPx)
	ks = append(ks, Kline{
		OpenTime: last.OpenTime.Add(3 * time.Minute),
		Open:     px,
		High:     high,
		Low:      low,
		Close:    newPx,
		Volume:   1000 + p.rng.Float64()*500,
	})
	if len(ks) > 1000 {
		ks = ks[len(ks)-1000:]
	}
	p.klines[symbol] = ks

	p.matchOrders(symbol, low, high, newPx)
	p.updatePositionMark(symbol, newPx)
	return newPx
}

// SetVolatility 调整模拟波动率（每tick的标准差近似值）
func (p *PaperExchange) SetVolatility(v float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.volatility = v
}

// InjectPrice 强制设置价格（用于测试特定场景，如模拟单边急跌）
func (p *PaperExchange) InjectPrice(symbol string, price float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.symbolPrice[symbol]
	p.symbolPrice[symbol] = price
	low, high := math.Min(old, price), math.Max(old, price)
	p.matchOrders(symbol, low, high, price)
	p.updatePositionMark(symbol, price)
}

func (p *PaperExchange) matchOrders(symbol string, low, high, last float64) {
	for _, o := range p.orders {
		if o.Symbol != symbol || o.Status != OrderStatusNew {
			continue
		}
		if o.Type == OrderTypeMarket {
			p.fillOrder(o, last)
			continue
		}
		// 限价单：价格落入本次波动区间即视为成交
		if o.Side == SideBuy && o.Price >= low {
			p.fillOrder(o, math.Min(o.Price, high))
		} else if o.Side == SideSell && o.Price <= high {
			p.fillOrder(o, math.Max(o.Price, low))
		}
	}
}

func (p *PaperExchange) fillOrder(o *Order, fillPrice float64) {
	o.Status = OrderStatusFilled
	o.FilledQuantity = o.Quantity
	o.AvgFillPrice = fillPrice
	o.UpdatedAt = time.Now()

	key := o.Symbol + ":" + string(o.PositionSide)
	pos, ok := p.positions[key]
	if !ok {
		pos = &Position{Symbol: o.Symbol, PositionSide: o.PositionSide, Leverage: p.leverage[o.Symbol]}
		if pos.Leverage == 0 {
			pos.Leverage = 1
		}
		p.positions[key] = pos
	}

	signedQty := o.Quantity
	if o.Side == SideSell {
		signedQty = -signedQty
	}
	if o.PositionSide == PositionShort {
		signedQty = -signedQty // short: sell增加空头，buy平空
	}

	newQty := pos.Quantity + signedQty
	if pos.Quantity == 0 || sameSign(pos.Quantity, newQty) {
		// 加仓或开仓：更新均价
		if newQty != 0 {
			pos.EntryPrice = (pos.EntryPrice*math.Abs(pos.Quantity) + fillPrice*math.Abs(signedQty)) / math.Abs(newQty)
		}
	}
	pos.Quantity = newQty
	pos.MarginUsed = math.Abs(pos.Quantity) * pos.EntryPrice / pos.Leverage
}

func sameSign(a, b float64) bool {
	return (a >= 0 && b >= 0) || (a <= 0 && b <= 0)
}

func (p *PaperExchange) updatePositionMark(symbol string, price float64) {
	for _, pos := range p.positions {
		if pos.Symbol != symbol || pos.Quantity == 0 {
			continue
		}
		pos.MarkPrice = price
		direction := 1.0
		if pos.PositionSide == PositionShort {
			direction = -1.0
		}
		pos.UnrealizedPnL = direction * pos.Quantity * (price - pos.EntryPrice)
		if pos.PositionSide == PositionShort {
			pos.UnrealizedPnL = -pos.Quantity * (price - pos.EntryPrice)
		}
		// 简化强平价估算
		if pos.Leverage > 0 {
			maintMarginRatio := 0.005
			if pos.PositionSide == PositionShort {
				pos.LiquidationPrice = pos.EntryPrice * (1 + 1/pos.Leverage - maintMarginRatio)
			} else {
				pos.LiquidationPrice = pos.EntryPrice * (1 - 1/pos.Leverage + maintMarginRatio)
			}
		}
	}
}

// ---- Exchange 接口实现 ----

func (p *PaperExchange) Name() string { return "paper" }

func (p *PaperExchange) GetTicker(ctx context.Context, symbol string) (*Ticker, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	px, ok := p.symbolPrice[symbol]
	if !ok {
		return nil, fmt.Errorf("symbol %s not found in paper exchange", symbol)
	}
	spread := px * 0.0002
	return &Ticker{
		Symbol:    symbol,
		Price:     px,
		Bid:       px - spread,
		Ask:       px + spread,
		Volume24h: 1_000_000,
		Timestamp: time.Now(),
	}, nil
}

func (p *PaperExchange) GetKlines(ctx context.Context, symbol string, interval string, limit int) ([]Kline, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ks, ok := p.klines[symbol]
	if !ok {
		return nil, fmt.Errorf("symbol %s not found", symbol)
	}
	if limit > 0 && limit < len(ks) {
		ks = ks[len(ks)-limit:]
	}
	out := make([]Kline, len(ks))
	copy(out, ks)
	return out, nil
}

func (p *PaperExchange) GetSymbolInfo(ctx context.Context, symbol string) (*SymbolInfo, error) {
	return &SymbolInfo{
		Symbol:            symbol,
		PricePrecision:    2,
		QuantityPrecision: 4,
		MinNotional:       12,
		MinQuantity:       0.0001,
		TickSize:          0.01,
		StepSize:          0.0001,
		MaxLeverage:       20,
	}, nil
}

func (p *PaperExchange) PlaceOrder(ctx context.Context, req OrderRequest) (*Order, error) {
	p.mu.Lock()
	p.orderSeq++
	id := fmt.Sprintf("PAPER-%d", p.orderSeq)
	px, ok := p.symbolPrice[req.Symbol]
	if !ok {
		p.mu.Unlock()
		return nil, fmt.Errorf("symbol %s not found", req.Symbol)
	}
	order := &Order{
		ExchangeOrderID: id,
		ClientOrderID:   req.ClientOrderID,
		Symbol:          req.Symbol,
		Side:            req.Side,
		PositionSide:    req.PositionSide,
		Type:            req.Type,
		Price:           req.Price,
		Quantity:        req.Quantity,
		Status:          OrderStatusNew,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	p.orders[id] = order
	p.mu.Unlock()

	if req.Type == OrderTypeMarket {
		p.mu.Lock()
		p.fillOrder(order, px)
		p.mu.Unlock()
	}
	return order, nil
}

func (p *PaperExchange) CancelOrder(ctx context.Context, symbol string, exchangeOrderID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.orders[exchangeOrderID]
	if !ok {
		return ErrOrderNotFound
	}
	if o.Status == OrderStatusNew {
		o.Status = OrderStatusCanceled
		o.UpdatedAt = time.Now()
	}
	return nil
}

func (p *PaperExchange) GetOpenOrders(ctx context.Context, symbol string) ([]Order, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Order
	for _, o := range p.orders {
		if o.Symbol == symbol && o.Status == OrderStatusNew {
			out = append(out, *o)
		}
	}
	return out, nil
}

func (p *PaperExchange) GetOrder(ctx context.Context, symbol string, exchangeOrderID string) (*Order, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	o, ok := p.orders[exchangeOrderID]
	if !ok {
		return nil, ErrOrderNotFound
	}
	cp := *o
	return &cp, nil
}

func (p *PaperExchange) GetPositions(ctx context.Context, symbol string) ([]Position, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Position
	for _, pos := range p.positions {
		if pos.Symbol == symbol && pos.Quantity != 0 {
			out = append(out, *pos)
		}
	}
	return out, nil
}

func (p *PaperExchange) GetBalances(ctx context.Context) ([]Balance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []Balance
	for _, b := range p.balances {
		out = append(out, *b)
	}
	return out, nil
}

func (p *PaperExchange) SetLeverage(ctx context.Context, symbol string, leverage float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.leverage[symbol] = leverage
	return nil
}
