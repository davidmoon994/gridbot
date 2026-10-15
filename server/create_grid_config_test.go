package server

import (
	"encoding/json"
	"testing"
)

// 创建网格的请求解析：止损线没传（或传0）用默认 15；传了就原样带进配置；
// 前端提交的是 JSON 数字，必须能被正确解析。
func TestCreateGridRequestStopLossPct(t *testing.T) {
	cfg := func(body string) float64 {
		var req createGridRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("解析 %s 失败: %v", body, err)
		}
		return req.toConfig().StopLossPct
	}
	if got := cfg(`{"symbol":"ETHUSDC"}`); got != 15 {
		t.Errorf("没传止损线应默认 15，实际 %.2f", got)
	}
	if got := cfg(`{"symbol":"ETHUSDC","stop_loss_pct":0}`); got != 15 {
		t.Errorf("传 0 应按默认 15 处理，实际 %.2f", got)
	}
	if got := cfg(`{"symbol":"ETHUSDC","stop_loss_pct":45}`); got != 45 {
		t.Errorf("传 45 应原样带入，实际 %.2f", got)
	}
	if got := cfg(`{"symbol":"ETHUSDC","stop_loss_pct":12.5}`); got != 12.5 {
		t.Errorf("传 12.5 应原样带入，实际 %.2f", got)
	}
}
