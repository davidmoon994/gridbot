package manager

import (
	"errors"
	"testing"
)

func TestIsTransientError(t *testing.T) {
	transient := []string{
		"获取挂单失败: binance API 错误 [-1000]: An unknown error occurred while processing the request.",
		`获取K线失败：请求 /fapi/v1/klines 失败: Get "https://demo-fapi.binance.com/fapi/v1/klines": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`,
		"风控验证前获取账户状态失败: 请求 /fapi/v2/balance 失败: Get \"...\": context canceled",
		"binance API 错误 [-1003]: Too many requests",
		"dial tcp: i/o timeout",
		"read tcp: connection reset by peer",
		"Get \"...\": EOF",
		"binance API 错误 [-1016]: Service is shutting down",
		"binance API 错误 [-1001]: DISCONNECTED",
		"Get \"...\": 503 Service Unavailable",
	}
	for _, msg := range transient {
		if !isTransientError(errors.New(msg)) {
			t.Errorf("应判定为临时性错误，但没有: %q", msg)
		}
	}

	fatal := []string{
		"binance API 错误 [-2015]: Invalid API-key, IP, or permissions for action.",
		"binance API 错误 [-1022]: Signature for this request is not valid.",
		"binance API 错误 [-1121]: Invalid symbol.",
		"binance API 错误 [-2014]: API-key format invalid.",
		"binance API 错误 [-2019]: Margin is insufficient.",
		"交易对拼写错误",
		"未知错误",
	}
	for _, msg := range fatal {
		if isTransientError(errors.New(msg)) {
			t.Errorf("应判定为非临时性（致命）错误，但被判成了临时性: %q", msg)
		}
	}

	if isTransientError(nil) {
		t.Error("nil 不应判定为临时性错误")
	}
}
