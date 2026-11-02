package main

// UDP 发现应答器 — 协议 §2（5416/udp 固定端口）

import (
	"net"
)

func startDiscoveryResponder() error {
	addr := &net.UDPAddr{Port: cfg.UDPPort}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return err
	}
	go goroutineSafe("discovery", func() {
		buf := make([]byte, 2048)
		for {
			n, raddr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			var m struct {
				PB   int    `json:"pb"`
				Type string `json:"type"`
			}
			if parseJSONInto(buf[:n], &m) != nil || m.Type != "discover" {
				continue
			}
			resp, _ := jsonMarshalIndent(map[string]any{
				"pb":     1,
				"type":   "found",
				"name":   hostname(),
				"port":   cfg.TCPPort, // 实际生效的 TCP 端口（可能已漂移）[P1-新2]
				"fp":     identity.FP(),
				"paired": len(cfg.Devices) > 0, // 仅 UI 提示，不构成安全判断 [P1-17]
			})
			conn.WriteToUDP(resp, raddr)
		}
	})
	logf("UDP 发现应答 :%d", cfg.UDPPort)
	return nil
}
