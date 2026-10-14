package app

import "testing"

// temporaryChatEligible 的不变量：temporary_chat 开着才走临时对话，且 multi_turn 一律压制
// （两者冲突——临时对话不进会话列表，多轮要靠服务端留会话，见 temporaryChatEligible 注释）。
func TestTemporaryChatEligible(t *testing.T) {
	old := rtCfg()
	defer func() { rtMu.Lock(); rtVal = old; rtMu.Unlock() }()

	cases := []struct {
		temp, multi, want bool
	}{
		{false, false, false}, // 都关
		{true, false, true},   // 只开临时 → 生效
		{true, true, false},   // 临时+多轮 → 多轮压制
		{false, true, false},  // 只开多轮
	}
	for _, c := range cases {
		rtMu.Lock()
		rtVal.TemporaryChat = c.temp
		rtVal.MultiTurn = c.multi
		rtMu.Unlock()
		if got := temporaryChatEligible(); got != c.want {
			t.Errorf("temp=%v multi=%v => %v, 期望 %v", c.temp, c.multi, got, c.want)
		}
	}
}
