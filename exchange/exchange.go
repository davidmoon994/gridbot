// Package exchange 定义了所有交易所必须实现的通用接口。
//
// 设计原则（参考 NOFX）：策略层和风控层只依赖这个接口，不关心
// 具体是 Binance、OKX 还是模拟盘。新增交易所时只需实现该接口，
// 不需要改动策略引擎和风控引擎一行代码。
package exchange

import (
	"context"
	"errors"
	"time"
)

// Side 订单方向
type Side string

const (
	SideBuy  Side = "buy"
	SideSell Side = "sell"
)

// PositionSide 持仓方向（用于合约的多空区分）
type PositionSide string

const (
	PositionLong  PositionSide = "long"
	PositionShort PositionSide = "short"
)

// OrderType 订单类型
type OrderType string

const (
	OrderTypeLimit  OrderType = "limit"
	OrderTypeMarket OrderType = "market"
)

// OrderStatus 订单状态
type OrderStatus string

const (
	OrderStatusNew      OrderStatus = "new"
	OrderStatusFilled   OrderStatus = "filled"
	OrderStatusPartial  OrderStatus = "partial"
	OrderStatusCanceled OrderStatus = "canceled"
	OrderStatusRejected OrderStatus = "rejected"
)

// ErrOrderNotFound 订单不存在
var ErrOrderNotFound = errors.New("order not found")

// ErrInsufficientBalance 余额不足
var ErrInsufficientBalance = errors.New("insufficient balance")

// Ticker 最新行情快照
type Ticker struct {
	Symbol    string    `json:"symbol"`
	Price     float64   `json:"price"`
	Bid       float64   `json:"bid"`
	Ask       float64   `json:"ask"`
	Volume24h float64   `json:"volume_24h"`
	Timestamp time.Time `json:"timestamp"`
}

// Kline K线
type Kline struct {
	OpenTime time.Time
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   float64
}

// OrderRequest 下单请求
type OrderRequest struct {
	Symbol        string
	Side          Side
	PositionSide  PositionSide // 合约用；现货可忽略
	Type          OrderType
	Price         float64 // 限价单必填
	Quantity      float64 // 基础货币数量
	ReduceOnly    bool    // 是否只减仓（用于网格止盈单）
	ClientOrderID string  // 客户端自定义ID，便于网格引擎追踪某一格
}

// Order 订单信息（下单后返回 & 查询用）
type Order struct {
	ExchangeOrderID string       `json:"exchange_order_id"`
	ClientOrderID   string       `json:"client_order_id"`
	Symbol          string       `json:"symbol"`
	Side            Side         `json:"side"`
	PositionSide    PositionSide `json:"position_side"`
	Type            OrderType    `json:"type"`
	Price           float64      `json:"price"`
	Quantity        float64      `json:"quantity"`
	FilledQuantity  float64      `json:"filled_quantity"`
	AvgFillPrice    float64      `json:"avg_fill_price"`
	Status          OrderStatus  `json:"status"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

// Position 持仓信息
type Position struct {
	Symbol           string       `json:"symbol"`
	PositionSide     PositionSide `json:"position_side"`
	Quantity         float64      `json:"quantity"`
	EntryPrice       float64      `json:"entry_price"`
	MarkPrice        float64      `json:"mark_price"`
	Leverage         float64      `json:"leverage"`
	MarginUsed       float64      `json:"margin_used"`
	UnrealizedPnL    float64      `json:"unrealized_pnl"`
	LiquidationPrice float64      `json:"liquidation_price"`
}

// Balance 账户余额
type Balance struct {
	Asset     string  `json:"asset"`
	Available float64 `json:"available"`
	Total     float64 `json:"total"`
}

// SymbolInfo 交易对的精度与限制信息，网格引擎需要按此对齐价格/数量精度
type SymbolInfo struct {
	Symbol            string
	PricePrecision    int     // 价格小数位数
	QuantityPrecision int     // 数量小数位数
	MinNotional       float64 // 最小名义价值（价格*数量）
	MinQuantity       float64
	TickSize          float64 // 最小价格变动单位
	StepSize          float64 // 最小数量变动单位
	MaxLeverage       float64
}

// Exchange 是所有交易所适配器必须实现的统一接口。
//
// 实现者需要保证：
//  1. 所有价格/数量在下单前已按 SymbolInfo 对齐精度（或在 PlaceOrder 内部对齐）；
//  2. 网络错误、限频错误应包装后返回，不要 panic；
//  3. ClientOrderID 要能在 GetOpenOrders / GetOrder 中原样返回，网格引擎依赖它
//     判断"这是哪一格的单子"。
type Exchange interface {
	// Name 返回交易所标识，如 "binance"、"okx"、"paper"
	Name() string

	// GetTicker 获取最新行情
	GetTicker(ctx context.Context, symbol string) (*Ticker, error)

	// GetKlines 获取K线，用于计算 EMA/ATR 等指标
	GetKlines(ctx context.Context, symbol string, interval string, limit int) ([]Kline, error)

	// GetSymbolInfo 获取交易对精度与限制
	GetSymbolInfo(ctx context.Context, symbol string) (*SymbolInfo, error)

	// PlaceOrder 下单
	PlaceOrder(ctx context.Context, req OrderRequest) (*Order, error)

	// CancelOrder 撤单
	CancelOrder(ctx context.Context, symbol string, exchangeOrderID string) error

	// GetOpenOrders 获取当前挂单
	GetOpenOrders(ctx context.Context, symbol string) ([]Order, error)

	// GetOrder 查询单个订单状态
	GetOrder(ctx context.Context, symbol string, exchangeOrderID string) (*Order, error)

	// GetPositions 获取当前持仓（合约）
	GetPositions(ctx context.Context, symbol string) ([]Position, error)

	// GetBalances 获取账户余额
	GetBalances(ctx context.Context) ([]Balance, error)

	// SetLeverage 设置杠杆（合约）；现货实现可直接返回 nil
	SetLeverage(ctx context.Context, symbol string, leverage float64) error
}
