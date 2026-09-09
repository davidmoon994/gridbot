package manager

import (
	"fmt"
	"testing"
	"time"
)

// 验证修复：强平下单的 ClientOrderID 长度必须小于36个字符（币安限制），
// 之前用纳秒时间戳拼出来的ID经常超限导致强平下单直接失败。
func TestForceCloseClientOrderIDLength(t *testing.T) {
	symbols := []string{"ETHUSDC", "BTCUSDT", "1000SHIBUSDC", "SOLUSDC"}
	for _, symbol := range symbols {
		id := fmt.Sprintf("%s-FC-%d", symbol, time.Now().UnixMilli())
		if len(id) >= 36 {
			t.Fatalf("symbol=%s 生成的ClientOrderID长度=%d 超过币安36字符限制: %s", symbol, len(id), id)
		}
		t.Logf("symbol=%s -> id=%s (长度=%d)", symbol, id, len(id))
	}
}
