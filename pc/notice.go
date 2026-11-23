package main

// 收图通知的「批量合并」
//
// 背景：手机一次性补传几十张时，旧实现每张都弹一个右下角窗口 → 屏幕被刷满，
// 用户根本看不清（实测用户反馈「疯狂刷屏」）。
// 语义：同一阵发期（连续到达）的图片合并成**一条**通知：
//   · 只有 1 张 → 保持原来的单张样式（带缩略图，信息最全）
//   · 2 张以上 → 「收到 N 张照片 · 共 X MB · 来自 <设备> · 已保存到 <目录>」
// 合并窗口：最后一张到达后静默 noticeQuiet 就发；但如果一直有图进来，
// 最迟 noticeHold 也会先发一条（避免长连拍时通知永远憋着）。
// 注意：这里只影响**通知**；剪贴板仍按每张写入（最终停留在最后一张，与原来一致）。

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// 变量而非常量：单元测试会把它们调小，避免测试里真的等 15 秒
var (
	noticeQuiet = 900 * time.Millisecond
	noticeHold  = 15 * time.Second // 连续不断时也最多每 15 秒一条，避免长补传变成刷屏
)

// noticeSink：真正弹窗的出口（测试替换成记录器，不碰 Win32）
var noticeSink = showNotice

type noticeItem struct {
	Title string
	Line1 string
	Line2 string
	Saved string // 单张时作为第三行；批量时不再逐条显示
	Dib   []byte
	ImgW  int
	ImgH  int
	Dev   string
	Size  int64
}

type noticeBatch struct {
	mu      sync.Mutex
	n       int
	bytes   int64
	firstAt time.Time
	lastAt  time.Time
	last    noticeItem
	timer   *time.Timer
}

var recvNotices = &noticeBatch{}

// pushRecvNotice：收图后调用（任意 goroutine 安全）
func pushRecvNotice(it noticeItem) {
	b := recvNotices
	b.mu.Lock()
	b.n++
	b.bytes += it.Size
	b.last = it
	now := timeNow()
	if b.n == 1 {
		b.firstAt = now
	}
	b.lastAt = now
	if b.timer == nil {
		b.scheduleLocked()
	}
	// 长连拍：从第一张起超过 noticeHold 就先发一条，别一直憋着
	if now.Sub(b.firstAt) >= noticeHold {
		item, n, bytes := b.takeLocked()
		b.mu.Unlock()
		noticeSink(item, n, bytes)
		return
	}
	b.mu.Unlock()
}

// scheduleLocked：安排下一次检查（调用方需持锁）
func (b *noticeBatch) scheduleLocked() {
	d := noticeQuiet
	if rem := noticeHold - timeNow().Sub(b.firstAt); rem < d {
		d = rem
	}
	if d < 100*time.Millisecond {
		d = 100 * time.Millisecond
	}
	b.timer = time.AfterFunc(d, b.onTimer)
}

func (b *noticeBatch) onTimer() {
	b.mu.Lock()
	if b.n == 0 {
		b.timer = nil
		b.mu.Unlock()
		return
	}
	quiet := timeNow().Sub(b.lastAt) >= noticeQuiet
	hold := timeNow().Sub(b.firstAt) >= noticeHold
	if !quiet && !hold {
		b.scheduleLocked()
		b.mu.Unlock()
		return
	}
	item, n, bytes := b.takeLocked()
	b.mu.Unlock()
	noticeSink(item, n, bytes)
}

// takeLocked：取走当前批次并复位（调用方需持锁）
func (b *noticeBatch) takeLocked() (noticeItem, int, int64) {
	item, n, bytes := b.last, b.n, b.bytes
	b.n, b.bytes = 0, 0
	b.timer = nil
	return item, n, bytes
}

func showNotice(it noticeItem, n int, bytes int64) {
	if n <= 0 {
		return
	}
	if n == 1 {
		// 单张：完全保持原样（带缩略图时信息最全）
		if it.Dib != nil && it.ImgW > 0 && it.ImgH > 0 {
			showPopupThumb4(it.Title, it.Line1, it.Line2, it.Saved, 5000, it.Dib, it.ImgW, it.ImgH)
		} else {
			showPopup4(it.Title, it.Line1, it.Line2, it.Saved, 5000)
		}
		return
	}
	title := fmt.Sprintf("收到 %d 张照片", n)
	line1 := fmt.Sprintf("共 %s", humanBytes(bytes))
	if it.Dev != "" {
		line1 += " · 来自 " + it.Dev
	}
	showPopup4(title, line1, it.Saved, "点击托盘图标可查看状态面板", 4000)
	logf("批量通知: %d 张（%s）来自 %s", n, humanBytes(bytes), it.Dev)
}

// fileSize：落盘文件大小（取不到就 0，只影响通知里的"共 X MB"）
func fileSize(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
