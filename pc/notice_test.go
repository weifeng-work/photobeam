package main

// 收图通知批量合并的回归测试
//
// 为什么值得测：这块是「用户被刷屏」的直接修复点，
// 逻辑涉及定时器 + 计数复位，靠肉眼和真机都很难确认「一阵发期只发一条」。

import (
	"sync"
	"testing"
	"time"
)

type noticeRecorder struct {
	mu    sync.Mutex
	calls []struct {
		N     int
		Bytes int64
	}
}

func (r *noticeRecorder) sink(_ noticeItem, n int, bytes int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, struct {
		N     int
		Bytes int64
	}{n, bytes})
}

func (r *noticeRecorder) snapshot() [][2]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][2]int64, 0, len(r.calls))
	for _, c := range r.calls {
		out = append(out, [2]int64{int64(c.N), c.Bytes})
	}
	return out
}

// setupNoticeTest：把时间窗调小、换上记录器
func setupNoticeTest(t *testing.T) *noticeRecorder {
	t.Helper()
	oldQuiet, oldHold, oldSink := noticeQuiet, noticeHold, noticeSink
	noticeQuiet = 60 * time.Millisecond
	noticeHold = 300 * time.Millisecond
	rec := &noticeRecorder{}
	noticeSink = rec.sink
	recvNotices = &noticeBatch{}
	t.Cleanup(func() {
		noticeQuiet, noticeHold, noticeSink = oldQuiet, oldHold, oldSink
		recvNotices = &noticeBatch{}
	})
	return rec
}

// 同一阵发期（间隔 < quiet）的 N 张 → 合并成一条，计数与字节数都对
func TestNoticeBatchesBurst(t *testing.T) {
	rec := setupNoticeTest(t)
	for i := 0; i < 5; i++ {
		pushRecvNotice(noticeItem{Title: "已同步到剪贴板", Dev: "PhoneA", Size: 1000})
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // 等静默期过去

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("5 张同批应只发 1 条通知，实际 %d 条：%v", len(got), got)
	}
	if got[0][0] != 5 || got[0][1] != 5000 {
		t.Fatalf("合并结果应为 n=5 bytes=5000，实际 n=%d bytes=%d", got[0][0], got[0][1])
	}
}

// 单张 → 一条通知，n=1（保持原单张样式）
func TestNoticeSingle(t *testing.T) {
	rec := setupNoticeTest(t)
	pushRecvNotice(noticeItem{Title: "已同步到剪贴板", Size: 123})
	time.Sleep(200 * time.Millisecond)

	got := rec.snapshot()
	if len(got) != 1 || got[0][0] != 1 {
		t.Fatalf("单张应发 1 条 n=1，实际 %v", got)
	}
}

// 两批之间隔着静默期 → 必须是两条，不能合并（用户需要知道"又来了新图"）
func TestNoticeSeparateBursts(t *testing.T) {
	rec := setupNoticeTest(t)
	pushRecvNotice(noticeItem{Size: 10})
	time.Sleep(200 * time.Millisecond)
	pushRecvNotice(noticeItem{Size: 20})
	time.Sleep(200 * time.Millisecond)

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("间隔超过静默期应发 2 条，实际 %d 条：%v", len(got), got)
	}
}

// 连续不断（间隔恒 < quiet）→ 由 hold 兜底，至少发一条，且不能退化成每张一条
func TestNoticeHoldFlushesDuringEndlessStream(t *testing.T) {
	rec := setupNoticeTest(t)
	// 持续 900ms，每 30ms 一张 → 30 张
	deadline := time.Now().Add(900 * time.Millisecond)
	for time.Now().Before(deadline) {
		pushRecvNotice(noticeItem{Size: 100})
		time.Sleep(30 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	got := rec.snapshot()
	if len(got) == 0 {
		t.Fatal("持续连拍必须有兜底通知，否则用户看不到任何进度")
	}
	if len(got) > 6 {
		t.Fatalf("兜底过密（%d 条），仍像刷屏", len(got))
	}
	var total int64
	for _, c := range got {
		total += c[0]
	}
	if total != 30 {
		t.Fatalf("计数丢失：合并后总数应为 30，实际 %d", total)
	}
}
