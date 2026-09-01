// Package exfactory 根据已保存的交易所凭证构造对应的 exchange.Exchange 实例。
// 单独成包是为了让 main（启动时恢复连接）和 server（Web 界面绑定/切换时）
// 复用同一份构造逻辑，避免出现两处不一致的实现。
package exfactory

import (
	"fmt"

	"gridbot/exchange"
	"gridbot/store"
)

// SupportedExchangeTypes 是当前支持通过 Web 界面绑定的真实交易所类型
var SupportedExchangeTypes = []string{"binance_futures", "binance_spot", "okx", "okx_spot"}

// Build 根据凭证构造交易所实例
func Build(cred store.ExchangeCredential) (exchange.Exchange, error) {
	switch cred.ExchangeType {
	case "binance_futures":
		ex := exchange.NewBinanceFuturesExchange(cred.APIKey, cred.APISecret, cred.Testnet)
		ex.HedgeMode = cred.HedgeMode
		return ex, nil
	case "binance_spot":
		return exchange.NewBinanceSpotExchange(cred.APIKey, cred.APISecret, cred.Testnet), nil
	case "okx":
		ex := exchange.NewOKXExchange(cred.APIKey, cred.APISecret, cred.Passphrase, cred.Testnet)
		if cred.HedgeMode {
			ex.PositionMode = "long_short"
		} else {
			ex.PositionMode = "net"
		}
		return ex, nil
	case "okx_spot":
		return exchange.NewOKXSpotExchange(cred.APIKey, cred.APISecret, cred.Passphrase, cred.Testnet), nil
	default:
		return nil, fmt.Errorf("不支持的交易所类型: %s（当前支持: %v）", cred.ExchangeType, SupportedExchangeTypes)
	}
}

// IsSupported 判断某个交易所类型字符串是否受支持
func IsSupported(exchangeType string) bool {
	for _, t := range SupportedExchangeTypes {
		if t == exchangeType {
			return true
		}
	}
	return false
}
