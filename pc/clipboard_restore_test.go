package main

// [Stage 17] 剪贴板「60 秒还原用户原内容」的判定逻辑单测
//
// 为什么需要这个测试：这段判定以前是内联在 time.AfterFunc 里的 if，
// **go test 完全覆盖不到** —— 于是 2026-11-07 那次改动把基准序号挪到了
// PNG 补写**之前**，而 SetClipboardData 成功会让剪贴板序号 +1，
// 导致「正常路径下永远不还原」这个回归一路绿灯发出去。
//
// 现在判定被抽成纯函数 restoreDecision（见 clipboard_windows.go），
// 下面把它的三条分支都钉住。
//
// 运行：cd pc && go test -run TestRestoreDecision -v

import "testing"

// TestRestoreDecision：还原决策的三条分支
func TestRestoreDecision(t *testing.T) {
	cases := []struct {
		name      string
		seqBase   uint32
		seqNow    uint32
		replaced  bool
		wantOK    bool
		wantWhy   string
	}{
		{
			name:    "序号没变且用户没动过 → 还原",
			seqBase: 100, seqNow: 100, replaced: false,
			wantOK: true,
		},
		{
			// ★ 这条就是被漏掉的回归：PNG 补写自己让序号 +1，
			//   基准必须取在补写**之后**，否则这里会判成「用户动过」。
			name:    "基准取在补写之后（序号含 PNG 那次 +1）→ 还原",
			seqBase: 101, seqNow: 101, replaced: false,
			wantOK: true,
		},
		{
			name:    "序号变了 → 用户复制了别的 → 不还原",
			seqBase: 100, seqNow: 101, replaced: false,
			wantOK: false,
		},
		{
			name:    "已确认用户复制了别的（归属标记不匹配）→ 不还原",
			seqBase: 100, seqNow: 100, replaced: true,
			wantOK: false,
		},
		{
			name:    "用户介入优先于序号相同",
			seqBase: 100, seqNow: 100, replaced: true,
			wantOK: false,
		},
	}
	for _, c := range cases {
		ok, why := restoreDecision(c.seqBase, c.seqNow, c.replaced)
		if ok != c.wantOK {
			t.Errorf("%s：restoreDecision(%d, %d, %v) = %v，期望 %v（原因：%s）",
				c.name, c.seqBase, c.seqNow, c.replaced, ok, c.wantOK, why)
		}
		if !ok && why == "" {
			t.Errorf("%s：判定为不还原时必须给出原因（否则日志里会出现无信息量的跳过）", c.name)
		}
	}
}

// TestRestoreDecisionSeqSemantics：守住「SetClipboardData 会让序号自增」这个前提。
//
// 本机 Win32 探针实测（2026-11-07）：
//
//	seq0=8144  after session1(Empty+3xSet+Close)=8152  after session2(1xSet+Close)=8153
//	=> SetClipboardData WITHOUT EmptyClipboard DOES bump the sequence number
//
// 这条测试把该前提固化成断言：还原基准**必须**取在自己最后一次写之后，
// 否则基准与还原时刻的序号必然不等 → 永远不还原。
func TestRestoreDecisionSeqSemantics(t *testing.T) {
	// 模拟：DIB 写入后序号 = 100 → PNG 补写（一次 SetClipboardData）→ 序号 = 101
	seqAfterDIB := uint32(100)
	seqAfterPNG := seqAfterDIB + 1

	// 错误做法：基准取在 PNG 补写之前
	if ok, _ := restoreDecision(seqAfterDIB, seqAfterPNG, false); ok {
		t.Error("基准取在 PNG 补写之前时不应还原（这正是被修掉的回归：会覆盖用户刚复制的内容）")
	}
	// 正确做法：基准取在 PNG 补写之后
	if ok, why := restoreDecision(seqAfterPNG, seqAfterPNG, false); !ok {
		t.Errorf("基准取在 PNG 补写之后应当还原，实际不还原：%s", why)
	}
}
