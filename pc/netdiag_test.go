package main

// 网络自检的运行时自检（PC-7）
// 只验证解析结果是否合理（结构体布局 / IfType 偏移 / 地址解析），不做任何写操作。

import (
	"testing"
	"unsafe"
)

func TestNetDiagProbe(t *testing.T) {
	if got := unsafe.Sizeof(ipAdapterAddresses{}); got != 216 {
		t.Fatalf("IP_ADAPTER_ADDRESSES 布局不符: size=%d（期望 216）", got)
	}
	all := queryNetIfaces()
	t.Logf("枚举到 %d 块网卡", len(all))
	for _, ni := range all {
		t.Logf("name=%q desc=%q ifIndex=%d ifType=%d ipv4=%v ipv6=%v gw=%v",
			ni.Name, ni.Desc, ni.IfIndex, ni.IfType, ni.IPv4, ni.IPv6, ni.Gateway)
	}
	real := pickRealAdapters(all)
	t.Logf("过滤后真实网卡 %d 块", len(real))
	t.Logf("对外通告 IP: %v", netAdvertisedIPs())
	t.Logf("默认网关: %q", netDefaultGateway())
	t.Logf("网络 profile: %q", netProfile())
	t.Logf("防火墙路径比对: %q", firewallPathVerdict())
	t.Logf("SSID: %q", wlanSSID())
}
