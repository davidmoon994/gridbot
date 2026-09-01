package exchange

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BinanceSpotExchange 是 Binance 现货的完整适配器实现。
//
// 与 BinanceFuturesExchange 的关键差异：
//  1. 域名不同：现货是 api.binance.com，合约是 fapi.binance.com；
//  2. 没有杠杆、保证金、持仓方向的概念——买入就是拿USDC换成币，卖出就是换回来，
//     SetLeverage 在这里是空操作；
//  3. 没有交易所原生的"持仓"可查——现货只有钱包余额，不像合约那样每个symbol
//     有独立跟踪的仓位。GetPositions 在这里固定返回空列表，持仓统计改由
//     网格引擎自己记账（见 strategy.Engine.PositionSummary），在 manager 层
//     合成一个虚拟持仓喂给风控引擎，避免和用户钱包里其他资产的余额混在一起算错。
type BinanceSpotExchange struct {
	apiKey     string
	apiSecret  string
	baseURL    string
	httpClient *http.Client

	mu           sync.Mutex
	timeOffsetMs int64
	offsetSynced bool
}

// NewBinanceSpotExchange 创建 Binance 现货适配器
func NewBinanceSpotExchange(apiKey, apiSecret string, testnet bool) *BinanceSpotExchange {
	base := "https://api.binance.com"
	if testnet {
		base = "https://testnet.binance.vision"
	}
	return &BinanceSpotExchange{
		apiKey:     apiKey,
		apiSecret:  apiSecret,
		baseURL:    base,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (b *BinanceSpotExchange) Name() string { return "binance_spot" }

type binanceSpotAPIError struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func (b *BinanceSpotExchange) syncTimeOffset(ctx context.Context) error {
	b.mu.Lock()
	if b.offsetSynced {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	body, err := b.rawRequest(ctx, http.MethodGet, "/api/v3/time", nil, false)
	if err != nil {
		return err
	}
	var resp struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("解析服务器时间失败: %w", err)
	}
	localMs := time.Now().UnixMilli()
	b.mu.Lock()
	b.timeOffsetMs = resp.ServerTime - localMs
	b.offsetSynced = true
	b.mu.Unlock()
	return nil
}

func (b *BinanceSpotExchange) nowMs() int64 {
	b.mu.Lock()
	offset := b.timeOffsetMs
	b.mu.Unlock()
	return time.Now().UnixMilli() + offset
}

func (b *BinanceSpotExchange) rawRequest(ctx context.Context, method, path string, params url.Values, needAuth bool) ([]byte, error) {
	if params == nil {
		params = url.Values{}
	}
	fullURL := b.baseURL + path
	if len(params) > 0 {
		fullURL += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return nil, err
	}
	if needAuth || b.apiKey != "" {
		req.Header.Set("X-MBX-APIKEY", b.apiKey)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr binanceSpotAPIError
		if jsonErr := json.Unmarshal(body, &apiErr); jsonErr == nil && apiErr.Code != 0 {
			return nil, fmt.Errorf("binance API 错误 [%d]: %s", apiErr.Code, apiErr.Msg)
		}
		return nil, fmt.Errorf("binance HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func (b *BinanceSpotExchange) signedRequest(ctx context.Context, method, path string, params url.Values) ([]byte, error) {
	if err := b.syncTimeOffset(ctx); err != nil {
		fmt.Printf("[binance_spot] 时间同步失败，使用本地时间: %v\n", err)
	}
	if params == nil {
		params = url.Values{}
	}
	params.Set("timestamp", strconv.FormatInt(b.nowMs(), 10))
	if params.Get("recvWindow") == "" {
		params.Set("recvWindow", "5000")
	}

	query := params.Encode()
	mac := hmac.New(sha256.New, []byte(b.apiSecret))
	mac.Write([]byte(query))
	signature := hex.EncodeToString(mac.Sum(nil))
	query += "&signature=" + signature

	fullURL := b.baseURL + path + "?" + query
	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-MBX-APIKEY", b.apiKey)

	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr binanceSpotAPIError
		if jsonErr := json.Unmarshal(body, &apiErr); jsonErr == nil && apiErr.Code != 0 {
			return nil, fmt.Errorf("binance API 错误 [%d]: %s", apiErr.Code, apiErr.Msg)
		}
		return nil, fmt.Errorf("binance HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

func spotParseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func (b *BinanceSpotExchange) GetTicker(ctx context.Context, symbol string) (*Ticker, error) {
	params := url.Values{"symbol": {symbol}}
	body, err := b.rawRequest(ctx, http.MethodGet, "/api/v3/ticker/bookTicker", params, false)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Symbol   string `json:"symbol"`
		BidPrice string `json:"bidPrice"`
		AskPrice string `json:"askPrice"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析 ticker 失败: %w", err)
	}
	bid := spotParseFloat(raw.BidPrice)
	ask := spotParseFloat(raw.AskPrice)
	return &Ticker{
		Symbol:    symbol,
		Price:     (bid + ask) / 2,
		Bid:       bid,
		Ask:       ask,
		Volume24h: 0,
		Timestamp: time.Now(),
	}, nil
}

func (b *BinanceSpotExchange) GetKlines(ctx context.Context, symbol string, interval string, limit int) ([]Kline, error) {
	params := url.Values{
		"symbol":   {symbol},
		"interval": {interval},
		"limit":    {strconv.Itoa(limit)},
	}
	body, err := b.rawRequest(ctx, http.MethodGet, "/api/v3/klines", params, false)
	if err != nil {
		return nil, err
	}
	var raw [][]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析K线失败: %w", err)
	}
	out := make([]Kline, 0, len(raw))
	for _, row := range raw {
		if len(row) < 6 {
			continue
		}
		openTimeMs, _ := row[0].(float64)
		out = append(out, Kline{
			OpenTime: time.UnixMilli(int64(openTimeMs)),
			Open:     spotParseFloat(fmt.Sprint(row[1])),
			High:     spotParseFloat(fmt.Sprint(row[2])),
			Low:      spotParseFloat(fmt.Sprint(row[3])),
			Close:    spotParseFloat(fmt.Sprint(row[4])),
			Volume:   spotParseFloat(fmt.Sprint(row[5])),
		})
	}
	return out, nil
}

func (b *BinanceSpotExchange) GetSymbolInfo(ctx context.Context, symbol string) (*SymbolInfo, error) {
	params := url.Values{"symbol": {symbol}}
	body, err := b.rawRequest(ctx, http.MethodGet, "/api/v3/exchangeInfo", params, false)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Symbols []struct {
			Symbol              string `json:"symbol"`
			BaseAssetPrecision  int    `json:"baseAssetPrecision"`
			QuoteAssetPrecision int    `json:"quoteAssetPrecision"`
			Filters             []struct {
				FilterType  string `json:"filterType"`
				TickSize    string `json:"tickSize"`
				StepSize    string `json:"stepSize"`
				MinQty      string `json:"minQty"`
				MinNotional string `json:"minNotional"` // "NOTIONAL" 和旧版 "MIN_NOTIONAL" 这两种filterType用的都是这个JSON字段名
			} `json:"filters"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析 exchangeInfo 失败: %w", err)
	}
	for _, s := range raw.Symbols {
		if s.Symbol != symbol {
			continue
		}
		info := &SymbolInfo{
			Symbol:            s.Symbol,
			PricePrecision:    s.QuoteAssetPrecision,
			QuantityPrecision: s.BaseAssetPrecision,
		}
		for _, f := range s.Filters {
			switch f.FilterType {
			case "PRICE_FILTER":
				info.TickSize = spotParseFloat(f.TickSize)
			case "LOT_SIZE":
				info.StepSize = spotParseFloat(f.StepSize)
				info.MinQuantity = spotParseFloat(f.MinQty)
			case "NOTIONAL", "MIN_NOTIONAL":
				if f.MinNotional != "" {
					info.MinNotional = spotParseFloat(f.MinNotional)
				}
			}
		}
		return info, nil
	}
	return nil, fmt.Errorf("未找到交易对 %s", symbol)
}

func (b *BinanceSpotExchange) PlaceOrder(ctx context.Context, req OrderRequest) (*Order, error) {
	symbolInfo, err := b.GetSymbolInfo(ctx, req.Symbol)
	if err != nil {
		return nil, fmt.Errorf("下单前获取精度信息失败: %w", err)
	}
	price := roundToStep(req.Price, symbolInfo.TickSize, symbolInfo.PricePrecision)
	qty := roundToStep(req.Quantity, symbolInfo.StepSize, symbolInfo.QuantityPrecision)

	params := url.Values{
		"symbol":   {req.Symbol},
		"side":     {strings.ToUpper(string(req.Side))},
		"type":     {strings.ToUpper(string(req.Type))},
		"quantity": {trimFloat(qty, symbolInfo.QuantityPrecision)},
	}
	if req.ClientOrderID != "" {
		params.Set("newClientOrderId", req.ClientOrderID)
	}
	if req.Type == OrderTypeLimit {
		params.Set("price", trimFloat(price, symbolInfo.PricePrecision))
		params.Set("timeInForce", "GTC")
	}
	// 注意：现货没有 positionSide / reduceOnly 概念，卖单天然受限于实际可用余额，
	// 交易所会在余额不足时直接拒绝，不需要也不支持这两个参数。

	body, err := b.signedRequest(ctx, http.MethodPost, "/api/v3/order", params)
	if err != nil {
		return nil, err
	}
	return parseBinanceSpotOrder(body)
}

func (b *BinanceSpotExchange) CancelOrder(ctx context.Context, symbol string, exchangeOrderID string) error {
	params := url.Values{"symbol": {symbol}, "orderId": {exchangeOrderID}}
	_, err := b.signedRequest(ctx, http.MethodDelete, "/api/v3/order", params)
	return err
}

func (b *BinanceSpotExchange) GetOpenOrders(ctx context.Context, symbol string) ([]Order, error) {
	params := url.Values{"symbol": {symbol}}
	body, err := b.signedRequest(ctx, http.MethodGet, "/api/v3/openOrders", params)
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析挂单列表失败: %w", err)
	}
	out := make([]Order, 0, len(raw))
	for _, r := range raw {
		o, err := parseBinanceSpotOrder(r)
		if err != nil {
			continue
		}
		out = append(out, *o)
	}
	return out, nil
}

func (b *BinanceSpotExchange) GetOrder(ctx context.Context, symbol string, exchangeOrderID string) (*Order, error) {
	params := url.Values{"symbol": {symbol}, "orderId": {exchangeOrderID}}
	body, err := b.signedRequest(ctx, http.MethodGet, "/api/v3/order", params)
	if err != nil {
		return nil, err
	}
	return parseBinanceSpotOrder(body)
}

// GetPositions 现货没有交易所原生"持仓"概念，固定返回空列表。
// 持仓统计由 manager 层调用 strategy.Engine.PositionSummary() 自己合成，
// 不使用这个方法的返回值（详见本文件顶部注释）。
func (b *BinanceSpotExchange) GetPositions(ctx context.Context, symbol string) ([]Position, error) {
	return nil, nil
}

func (b *BinanceSpotExchange) GetBalances(ctx context.Context) ([]Balance, error) {
	body, err := b.signedRequest(ctx, http.MethodGet, "/api/v3/account", nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Balances []struct {
			Asset  string `json:"asset"`
			Free   string `json:"free"`
			Locked string `json:"locked"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析余额失败: %w", err)
	}
	out := make([]Balance, 0, len(raw.Balances))
	for _, a := range raw.Balances {
		free := spotParseFloat(a.Free)
		locked := spotParseFloat(a.Locked)
		total := free + locked
		if total == 0 {
			continue
		}
		out = append(out, Balance{Asset: a.Asset, Available: free, Total: total})
	}
	return out, nil
}

// SetLeverage 现货没有杠杆概念，空操作
func (b *BinanceSpotExchange) SetLeverage(ctx context.Context, symbol string, leverage float64) error {
	return nil
}

func parseBinanceSpotOrder(body []byte) (*Order, error) {
	var raw struct {
		OrderId       int64  `json:"orderId"`
		ClientOrderId string `json:"clientOrderId"`
		Symbol        string `json:"symbol"`
		Side          string `json:"side"`
		Type          string `json:"type"`
		Price         string `json:"price"`
		OrigQty       string `json:"origQty"`
		ExecutedQty   string `json:"executedQty"`
		Status        string `json:"status"`
		TransactTime  int64  `json:"transactTime"`
		Time          int64  `json:"time"`
		UpdateTime    int64  `json:"updateTime"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析订单失败: %w", err)
	}
	status := OrderStatusNew
	switch raw.Status {
	case "FILLED":
		status = OrderStatusFilled
	case "PARTIALLY_FILLED":
		status = OrderStatusPartial
	case "CANCELED", "EXPIRED":
		status = OrderStatusCanceled
	case "REJECTED":
		status = OrderStatusRejected
	}
	createdMs := raw.Time
	if createdMs == 0 {
		createdMs = raw.TransactTime
	}
	updatedMs := raw.UpdateTime
	if updatedMs == 0 {
		updatedMs = createdMs
	}
	return &Order{
		ExchangeOrderID: strconv.FormatInt(raw.OrderId, 10),
		ClientOrderID:   raw.ClientOrderId,
		Symbol:          raw.Symbol,
		Side:            Side(strings.ToLower(raw.Side)),
		Type:            OrderType(strings.ToLower(raw.Type)),
		Price:           spotParseFloat(raw.Price),
		Quantity:        spotParseFloat(raw.OrigQty),
		FilledQuantity:  spotParseFloat(raw.ExecutedQty),
		Status:          status,
		CreatedAt:       time.UnixMilli(createdMs),
		UpdatedAt:       time.UnixMilli(updatedMs),
	}, nil
}
