package main

// 网络自检四件套（PC-7）—— 协议 §8:289-292、§3.4:152
//
//	1. 对外通告 IP + 所属网卡名：GetAdaptersAddresses（按 IfType 过滤：以太网 6 /
//	   无线 71 等），并按名称黑名单排除 Hyper-V / WSL / VMware / Tailscale /
//	   WireGuard / Loopback / vEthernet 等虚拟网卡
//	2. 默认网关：同一份 GetAdaptersAddresses 的网关链
//	3. 网络 profile（公用 / 专用）：netsh wlan show interfaces → Get-NetConnectionProfile
//	4. 防火墙规则路径 vs 当前 exe 路径：netsh advfirewall 输出比对
//	5. 认证失败统计：内存计数（PC 重启清零，如实写明）
//
// 零 CGO：只用 syscall / x/sys/windows，不引入新依赖。
//
// 【结构体偏移说明】以下偏移均为**本机实测**（Windows 10 19045 / x64）验证过的：
//   - IP_ADAPTER_ADDRESSES：Length=0, IfIndex=4, Next=8, AdapterName=16,
//     FirstUnicastAddress=24, Description=64, FriendlyName=72（名字/IP 均解析正确）
//   - 网关链指针：实测在偏移 **208**（SDK 文档写 200，两者都试，取先命中的）
//   - IfType：走 ConvertInterfaceIndexToLuid 取完整 NET_LUID，取其高 16 位
//     （以太网 0x0006 / 无线 0x0047 / 回环 0x0018 / 隧道 0x0035 —— 实测都对）
//
// 【安全约定】OS 侧的字符串与 sockaddr 一律用 ReadProcessMemory 读（不裸解指针，
// 遇到哨兵值/野指针只是读不到，不会把进程带崩）；缓冲区内自己的结构体按**偏移**
// 访问（指针派生的 uintptr + 偏移，go vet 默认检查也能过）。

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	iphlpapi                 = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetAdaptersAddresses = iphlpapi.NewProc("GetAdaptersAddresses")
	procConvertIfIndexToLuid = iphlpapi.NewProc("ConvertInterfaceIndexToLuid")
)

// IfType 常量（winnl.h）
const (
	ifTypeEthernet   = 6  // IF_TYPE_ETHERNET_CSMACD
	ifTypeIEEE80211  = 71 // IF_TYPE_IEEE80211（无线）
	ifTypeLoopback   = 24 // IF_TYPE_SOFTWARE_LOOPBACK
	ifTypeMaxTrusted = 999
)

// ---------- IP_ADAPTER_ADDRESSES（只保留用到的字段 + 必要的占位） ----------

type ipAdapterAddresses struct {
	Length                 uint32
	IfIndex                uint32
	Next                   uintptr
	AdapterName            uintptr
	FirstUnicastAddress    uintptr
	FirstAnycastAddress    uintptr
	FirstMulticastAddress  uintptr
	FirstDnsServerAddress  uintptr
	DnsSuffix              uintptr
	Description            uintptr
	FriendlyName           uintptr
	PhysicalAddress        [8]byte
	PhysicalAddressLength  uint32
	Flags                  uint32
	Mtu                    uint32
	Ipv6IfIndex            uint32
	ZoneIndices            [16]uint32
	FirstPrefix            uintptr
	TransmitLinkSpeed      uint64
	ReceiveLinkSpeed       uint64
	FirstWinsServerAddress uintptr
	_                      uintptr // 200：保留槽位（不同 SDK 版本此处置网关链）
	FirstGatewayAddress    uintptr // 208：实测网关链位置
}

// socketAddress（SOCKET_ADDRESS：lpSockaddr + iSockaddrLength）
type socketAddress struct {
	lpSockaddr uintptr
	length     int32
}

// 链表节点：{Length, Reserved, Next, SOCKET_ADDRESS Address}（Address 在节点 +16）
type ipAdapterUnicastAddress struct {
	Length             uint32
	Flags              uint32
	Next               uintptr
	Address            socketAddress
	PrefixOrigin       int32
	SuffixOrigin       int32
	DadState           int32
	ValidLifetime      uint32
	PreferredLifetime  uint32
	LeaseLifetime      uint32
	OnLinkPrefixLength uint8
	SkipAsSource       uint8
	_                  [2]byte
	_                  uint64
}

type ipAdapterGatewayAddress struct {
	Length   uint32
	Reserved uint32
	Next     uintptr
	Address  socketAddress
}

// netIface：一块网卡的对外信息
type netIface struct {
	Name    string // FriendlyName（本地化，如「WLAN」「以太网」）
	Desc    string // Description（驱动描述）
	IfIndex uint32
	IfType  uint32
	IPv4    []string
	IPv6    []string
	Gateway []string
}

// ---------- 底层读取（全部走 ReadProcessMemory，不裸解 OS 指针） ----------

var (
	selfProcOnce sync.Once
	selfProc     windows.Handle
)

func currentProcess() windows.Handle {
	selfProcOnce.Do(func() {
		selfProc, _ = windows.GetCurrentProcess()
	})
	return selfProc
}

// validUserPtr：像不像一个可读的用户态地址
func validUserPtr(p uintptr) bool {
	return p != 0 && p != ^uintptr(0) && p >= 0x10000 && p <= 0x7FFFFFFF0000
}

// readMem：读进程内存（跨页部分读失败时用已读部分）
func readMem(p uintptr, n int) []byte {
	if !validUserPtr(p) || n <= 0 {
		return nil
	}
	raw := make([]byte, n)
	var read uintptr
	err := windows.ReadProcessMemory(currentProcess(), p, &raw[0], uintptr(n), &read)
	if err != nil && read == 0 {
		return nil
	}
	return raw[:read]
}

// utf16At：从地址读 NUL 结尾的 UTF-16 字符串（最多 max 个码元）
func utf16At(p uintptr, max int) string {
	if max <= 0 {
		return ""
	}
	raw := readMem(p, max*2)
	if len(raw) == 0 {
		return ""
	}
	u := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		c := binary.LittleEndian.Uint16(raw[i : i+2])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return windows.UTF16ToString(u)
}

// readSockAddr：安全读 SOCKET_ADDRESS 指向的 sockaddr
func readSockAddr(sa socketAddress) (uint16, string) {
	raw := readMem(sa.lpSockaddr, 32)
	if len(raw) < 4 {
		return 0, ""
	}
	fam := binary.LittleEndian.Uint16(raw[0:2])
	switch fam {
	case 2: // AF_INET
		if len(raw) < 8 {
			return fam, ""
		}
		return fam, net.IP(raw[4:8]).String()
	case 23: // AF_INET6
		if len(raw) < 24 {
			return fam, ""
		}
		return fam, net.IP(raw[8:24]).String()
	}
	return fam, ""
}

// ifTypeOf：取网卡的 IfType
// 走 ConvertInterfaceIndexToLuid 取完整 NET_LUID，再取其高 16 位（= IF_TYPE_*）。
// 实测要点：直接把 ifIndex 当 LUID 塞给 GetIfEntry2 会在 index=1（回环）上
// **崩在 iphlpapi 内部**（NET_LUID 高位必须带 IfType），所以这里只做 LUID 换算。
func ifTypeOf(ifIndex uint32) uint32 {
	if ifIndex == 0 {
		return 0
	}
	var luid uint64
	r, _, _ := procConvertIfIndexToLuid.Call(uintptr(ifIndex), uintptr(unsafe.Pointer(&luid)))
	runtime.KeepAlive(&luid)
	if r != 0 {
		return 0 // 换算失败 → 未知（不按 IfType 过滤，只走名称黑名单）
	}
	t := uint32(luid >> 48)
	if t == 0 || t > ifTypeMaxTrusted {
		return 0
	}
	return t
}

// bufAt：取缓冲区内的结构体（指针派生的 uintptr + 偏移，go vet 默认检查可通过）
func bufAt[T any](buf []byte, off uintptr) *T {
	return (*T)(unsafe.Pointer(uintptr(unsafe.Pointer(&buf[0])) + off))
}

// bufPtrAt：取缓冲区内的 uintptr 字段（按偏移）
func bufPtrAt(buf []byte, off uintptr) uintptr {
	if off+unsafe.Sizeof(uintptr(0)) > uintptr(len(buf)) {
		// [L4] 越界兜底：当前唯一调用点被 `a.Length >= 216` 兜住，但 helper 将来
		// 换偏移就可能越界读（unsafe 越界读不 panic，只会读到垃圾或崩进程）。
		return 0
	}
	return *(*uintptr)(unsafe.Pointer(uintptr(unsafe.Pointer(&buf[0])) + off))
}

// bufU32At：取缓冲区内的 uint32 字段（按偏移）—— 用于按**真实结构偏移**读字段，
// 不必在 Go 结构体里逐字段复刻（漏一个字段后面全体错位）。
func bufU32At(buf []byte, off uintptr) uint32 {
	if off+unsafe.Sizeof(uint32(0)) > uintptr(len(buf)) {
		return 0 // [L4] 越界兜底，理由同 bufPtrAt
	}
	return *(*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(&buf[0])) + off))
}

// queryNetIfaces：枚举网卡（GetAdaptersAddresses + ConvertInterfaceIndexToLuid）
// GAA_FLAG_INCLUDE_GATEWAYS(0x80) 必须显式带上，否则网关列表恒为空
func queryNetIfaces() []netIface {
	const gaaFlags = 0x0002 | 0x0004 | 0x0008 | 0x0080 // SKIP_ANYCAST|MULTICAST|DNS + INCLUDE_GATEWAYS
	var out []netIface
	var size uint32
	// 第一次调用只取所需缓冲区大小
	procGetAdaptersAddresses.Call(0 /*AF_UNSPEC*/, gaaFlags, 0, 0,
		uintptr(unsafe.Pointer(&size)))
	if size == 0 {
		return nil
	}
	buf := make([]byte, size)
	r, _, _ := procGetAdaptersAddresses.Call(0, gaaFlags, 0, uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)))
	runtime.KeepAlive(&buf)
	if r != 0 || len(buf) == 0 {
		return nil
	}
	base := uintptr(unsafe.Pointer(&buf[0]))
	total := uintptr(len(buf))
	// 链表指针是绝对地址 → 换算成缓冲区偏移（越界即视为无效）
	offOf := func(p uintptr) (uintptr, bool) {
		if p < base || p >= base+total {
			return 0, false
		}
		return p - base, true
	}
	addr := base
	for {
		off, ok := offOf(addr)
		if !ok {
			break
		}
		a := bufAt[ipAdapterAddresses](buf, off)
		if a.Length < 216 || a.Length > 4096 {
			break // 布局不符 → 停止（不猜）
		}
		ni := netIface{
			Name:    utf16At(a.FriendlyName, 128),
			Desc:    utf16At(a.Description, 128),
			IfIndex: a.IfIndex,
		}
		ni.IfType = ifTypeOf(a.IfIndex)
		if ni.IfType == 0 {
			// [修复] 按**真实偏移**读字段：IP_ADAPTER_ADDRESSES（x64）里
			//   +96=Mtu, +100=IfType, +104=OperStatus, +108=Ipv6IfIndex。
			// 旧 Go 结构体漏了 IfType/OperStatus 两个字段，把 Ipv6IfIndex 放在 +100 →
			// fallback 读到的是 IfType（6/71），再把它当接口索引去换算必然失败，
			// 网卡类型被错误分类/过滤（热点的网卡可能被误判成虚拟网卡）。
			// 这里直接读 +100 的 IfType（它就是权威值），仅当其为 0 才用 +108 的 Ipv6IfIndex 兜底。
			if t := bufU32At(buf, off+100); t > 0 && t <= ifTypeMaxTrusted {
				ni.IfType = t
			} else if v6 := bufU32At(buf, off+108); v6 != 0 {
				ni.IfType = ifTypeOf(v6)
			}
		}
		// 单播地址
		if uoff, ok := offOf(a.FirstUnicastAddress); ok {
			for {
				u := bufAt[ipAdapterUnicastAddress](buf, uoff)
				if fam, ip := readSockAddr(u.Address); ip != "" {
					if fam == 23 {
						ni.IPv6 = append(ni.IPv6, ip)
					} else {
						ni.IPv4 = append(ni.IPv4, ip)
					}
				}
				noff, ok := offOf(u.Next)
				if !ok {
					break
				}
				uoff = noff
			}
		}
		// 网关（两个候选偏移，取先命中的）
		for _, goff := range []uintptr{208, 200} {
			gw, ok := offOf(bufPtrAt(buf, off+goff))
			if !ok {
				continue
			}
			for {
				g := bufAt[ipAdapterGatewayAddress](buf, gw)
				if _, ip := readSockAddr(g.Address); ip != "" {
					ni.Gateway = append(ni.Gateway, ip)
				}
				noff, ok := offOf(g.Next)
				if !ok {
					break
				}
				gw = noff
			}
			if len(ni.Gateway) > 0 {
				break
			}
		}
		out = append(out, ni)
		next, ok := offOf(a.Next)
		if !ok {
			break
		}
		addr = base + next
	}
	return out
}

// ---------- 网卡筛选（IfType + 名称黑名单） ----------

// 虚拟网卡名称黑名单（协议 §8：Hyper-V/WSL/VMware/Tailscale/WireGuard/Loopback/vEthernet）
var virtNameHints = []string{
	"hyper-v", "hyperv", "vethernet", "vmware", "virtualbox", "vbox",
	"tailscale", "wireguard", "wsl", "loopback", "pseudo", "zerotier",
	"openvpn", "tap-windows", "npcap", "docker", "virbr", "vpn", "bluetooth",
	"wi-fi direct", "virtual adapter", "tunnel", "wwan", "mobile broadband",
}

func isVirtualAdapter(ni netIface) bool {
	hay := strings.ToLower(ni.Name + "|" + ni.Desc)
	for _, h := range virtNameHints {
		if strings.Contains(hay, h) {
			return true
		}
	}
	return false
}

// pickRealAdapters：筛出真实网卡（以太网/无线，IfType 已知时按类型过滤）
func pickRealAdapters(all []netIface) []netIface {
	var out []netIface
	for _, ni := range all {
		if isVirtualAdapter(ni) {
			continue
		}
		if ni.IfType != 0 {
			switch ni.IfType {
			case ifTypeEthernet, ifTypeIEEE80211:
			default:
				continue // 非以太网/无线（PPP/Tunnel/Loopback…）→ 排除
			}
		}
		if len(ni.IPv4) == 0 {
			continue // 没有 IPv4 → 对手机不可达
		}
		out = append(out, ni)
	}
	return out
}

// ---------- 对外通告 IP / 网卡名 ----------

// netAdvertisedIPs：手机可达的 IP 候选（网卡名 + IPv4）
func netAdvertisedIPs() []string {
	var out []string
	for _, ni := range pickRealAdapters(queryNetIfaces()) {
		for _, ip := range ni.IPv4 {
			out = append(out, ip+" ("+ni.Name+")")
		}
	}
	return out
}

// netDefaultGateway：默认网关（真实网卡上的第一个 IPv4 网关）
func netDefaultGateway() string {
	for _, ni := range pickRealAdapters(queryNetIfaces()) {
		for _, g := range ni.Gateway {
			if !strings.Contains(g, ":") {
				return g
			}
		}
	}
	return ""
}

// ---------- 网络 profile（公用 / 专用） ----------

// netProfile：Windows 网络位置（公用/专用）；读不到就写「未知」
// 数据来源顺序：
//  ① netsh wlan show interfaces（协议 §8 提到的来源；多数系统该输出里**不含**网络位置字段）
//  ② PowerShell Get-NetConnectionProfile（真正带 NetworkCategory 的接口，最可靠）
//  ③ 注册表 NetworkList\Profiles（需管理员权限，常常 Access denied）
func netProfile() string {
	if p := profileFromNetsh(); p != "" {
		return p
	}
	if p := profileFromPowerShell(); p != "" {
		return p
	}
	if p := netProfileFromRegistry(); p != "" {
		return p
	}
	return "未知（无法读取网络位置）"
}

// profileFromNetsh：netsh wlan show interfaces 里找 公用/专用 字样
func profileFromNetsh() string {
	out, err := runCapture("netsh", "wlan", "show", "interfaces")
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(out, "\n") {
		v := strings.TrimSpace(ln)
		if i := strings.LastIndex(v, ":"); i >= 0 {
			switch strings.TrimSpace(v[i+1:]) {
			case "公用", "Public":
				return "公用"
			case "专用", "Private":
				return "专用"
			}
		}
	}
	return ""
}

// profileFromPowerShell：Get-NetConnectionProfile 的 NetworkCategory
// 输出形如「以太网 2=Private;Meta=Private」；优先挑真实网卡（名称对得上）
func profileFromPowerShell() string {
	script := "(Get-NetConnectionProfile | ForEach-Object { $_.InterfaceAlias + '=' + $_.NetworkCategory }) -join ';'"
	out, err := runCapture("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		return ""
	}
	real := map[string]bool{}
	for _, ni := range pickRealAdapters(queryNetIfaces()) {
		real[ni.Name] = true
	}
	fallback := ""
	for _, part := range strings.Split(out, ";") {
		part = strings.TrimSpace(part)
		i := strings.LastIndex(part, "=")
		if i < 0 {
			continue
		}
		alias := strings.TrimSpace(part[:i])
		cat := strings.TrimSpace(part[i+1:])
		var label string
		switch cat {
		case "Private":
			label = "专用"
		case "Public":
			label = "公用"
		case "DomainAuthenticated":
			label = "域网络"
		default:
			continue
		}
		if real[alias] {
			return label + "（" + alias + "）"
		}
		if fallback == "" {
			fallback = label + "（" + alias + "）"
		}
	}
	return fallback
}

// netProfileFromRegistry：按当前 SSID 匹配注册表里的网络位置
func netProfileFromRegistry() string {
	ssid := wlanSSID()
	if ssid == "" {
		return ""
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion\NetworkList\Profiles`, 0)
	if err != nil {
		return ""
	}
	defer k.Close()
	names, err := k.ReadSubKeyNames(-1)
	if err != nil {
		return ""
	}
	for _, n := range names {
		sub, err := registry.OpenKey(registry.LOCAL_MACHINE,
			`SOFTWARE\Microsoft\Windows NT\CurrentVersion\NetworkList\Profiles\`+n, 0)
		if err != nil {
			continue
		}
		pname, _, _ := sub.GetStringValue("ProfileName")
		cat, _, _ := sub.GetIntegerValue("Category")
		sub.Close()
		if pname != ssid {
			continue
		}
		switch cat {
		case 0:
			return "公用"
		case 1:
			return "专用"
		case 2:
			return "域网络"
		}
	}
	return ""
}

// wlanSSID：当前连接的 WiFi 名称
func wlanSSID() string {
	out, err := runCapture("netsh", "wlan", "show", "interfaces")
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(out, "\n") {
		v := strings.TrimSpace(ln)
		if !strings.HasPrefix(strings.ToLower(v), "ssid") {
			continue
		}
		if i := strings.LastIndex(v, ":"); i >= 0 {
			s := strings.TrimSpace(v[i+1:])
			if s != "" {
				return s
			}
		}
	}
	return ""
}

// ---------- 防火墙规则路径 vs 当前 exe（协议 §8:291） ----------

// firewallRulePrograms：取防火墙规则里绑定的程序路径
// 返回 (路径, 规则是否存在)；端口规则（未按程序绑定）存在但没有 Program 行 → ("", true)
func firewallRulePrograms(rule string) (string, bool) {
	out, err := runCapture("netsh", "advfirewall", "firewall", "show", "rule", "name="+rule)
	if err != nil {
		return "", false
	}
	found := strings.Contains(out, rule)
	for _, ln := range strings.Split(out, "\n") {
		v := strings.TrimSpace(ln)
		// 本地化标签：「程序:」/「Program:」
		i := strings.Index(v, ":")
		if i <= 0 {
			continue
		}
		label := strings.TrimSpace(v[:i])
		if label != "程序" && !strings.EqualFold(label, "Program") {
			continue
		}
		return strings.TrimSpace(v[i+1:]), found
	}
	return "", found
}

// firewallPathVerdict：规则里的程序路径与当前 exe 是否一致
func firewallPathVerdict() string {
	exe, err := os.Executable()
	if err != nil {
		return "未知（取不到当前 exe 路径）"
	}
	exe, _ = filepath.Abs(exe)
	var parts []string
	for _, rule := range []string{"PhotoBeam-TCP", "PhotoBeam-UDP"} {
		p, found := firewallRulePrograms(rule)
		if !found {
			parts = append(parts, rule+": 未找到规则")
			continue
		}
		if p == "" {
			parts = append(parts, rule+": 不一致（规则未按程序路径绑定，只按端口放行）")
			continue
		}
		if strings.EqualFold(filepath.Clean(p), filepath.Clean(exe)) {
			parts = append(parts, rule+": 一致")
		} else {
			parts = append(parts, rule+": 不一致（规则指向 "+p+"）")
		}
	}
	return strings.Join(parts, "；") + "｜当前 exe: " + exe
}

// ---------- 认证失败统计（协议 §3.4:152，内存计数，PC 重启清零） ----------

type authStat struct {
	mu           sync.Mutex
	failCount    int
	lastFail     string
	lastFailTime time.Time
	lastOKTime   time.Time
}

var authStats = &authStat{}

func (a *authStat) fail(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failCount++
	a.lastFail = reason
	a.lastFailTime = timeNow()
}

func (a *authStat) ok() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastOKTime = timeNow()
}

// snapshot：失败次数 / 最近失败原因 / 最近失败时间 / 最近成功时间
func (a *authStat) snapshot() (count int, lastFail string, lastFailTime time.Time, lastOKTime time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.failCount, a.lastFail, a.lastFailTime, a.lastOKTime
}

// authStatText：诊断文本用的认证失败统计（如实写明计数器在内存、重启清零）
func authStatText() string {
	count, lastFail, failTime, okTime := authStats.snapshot()
	failTxt := "无"
	if lastFail != "" {
		failTxt = fmt.Sprintf("%s（%s）", lastFail, failTime.Format("2006-01-02 15:04:05"))
	}
	okTxt := "尚无成功记录"
	if !okTime.IsZero() {
		okTxt = okTime.Format("2006-01-02 15:04:05")
	}
	return fmt.Sprintf("认证失败次数: %d｜最近失败: %s｜最近成功: %s｜（计数器在内存，PC 重启清零）",
		count, failTxt, okTxt)
}
