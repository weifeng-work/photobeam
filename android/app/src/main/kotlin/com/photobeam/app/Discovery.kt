package com.photobeam.app

// 三通道发现（协议 §2）：直连 → UDP 广播 → 单播扫描
// 网络指纹前置判定 [N-06][S-01]

import android.content.Context
import android.net.wifi.WifiManager
import java.net.DatagramPacket
import java.net.DatagramSocket
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.NetworkInterface
import java.net.Socket
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit

data class FoundPC(val name: String, val ip: String, val port: Int, val fp: String, val paired: Boolean)

object Discovery {
    const val UDP_PORT = 5416
    const val TCP_BASE = 5416
    const val TCP_MAX = 5436

    private fun discoverFrame(): ByteArray =
        org.json.JSONObject()
            .put("pb", 1).put("type", "discover")
            .put("dev", android.os.Build.MODEL).put("ver", PROTO_VER)
            .toString().toByteArray(Charsets.UTF_8)

    private fun parseFound(b: ByteArray): FoundPC? = try {
        val o = org.json.JSONObject(String(b, Charsets.UTF_8))
        if (o.optString("type") == "found" && o.optInt("pb") == 1)
            FoundPC(o.optString("name", ""), o.optString("ip", ""), o.optInt("port", 0),
                o.optString("fp", ""), o.optBoolean("paired", false))
        else null
    } catch (e: Exception) {
        null
    }

    private fun activeInterfaces(): List<NetworkInterface> =
        NetworkInterface.getNetworkInterfaces().asSequence()
            .filter { it.isUp && !it.isLoopback && it.interfaceAddresses.isNotEmpty() }
            .toList()

    // 按“真实子网掩码”推导定向广播地址 [P1-18]
    private fun broadcastAddrs(): List<InetAddress> {
        val out = ArrayList<InetAddress>()
        for (nif in activeInterfaces()) {
            for (ia in nif.interfaceAddresses) {
                val addr = ia.address
                if (addr.address.size != 4) continue // 仅 IPv4
                val prefix = ia.networkPrefixLength.toInt()
                if (prefix <= 0 || prefix >= 32) continue
                val ipInt = ((addr.address[0].toInt() and 0xFF) shl 24) or
                    ((addr.address[1].toInt() and 0xFF) shl 16) or
                    ((addr.address[2].toInt() and 0xFF) shl 8) or
                    (addr.address[3].toInt() and 0xFF)
                val bInt = ipInt or ((1u shl (32 - prefix)) - 1u).toInt()
                out.add(InetAddress.getByAddress(byteArrayOf(
                    ((bInt shr 24) and 0xFF).toByte(), ((bInt shr 16) and 0xFF).toByte(),
                    ((bInt shr 8) and 0xFF).toByte(), (bInt and 0xFF).toByte())))
                return out // 正常家庭网一个接口即够；多接口 M1 取首个非回环
            }
        }
        if (out.isEmpty()) out.add(InetAddress.getByName("255.255.255.255"))
        return out
    }

    /** 网络指纹（配对成功时记录，发送前比对）[N-06][S-01] */
    fun currentFingerprint(ctx: Context): String {
        // 判据降级：SSID 需要定位权限 [S-01]——不用 SSID，用网关 IP + 子网（无需危险权限）
        val gw = gatewayIP(ctx) ?: return "unknown"
        val nif = activeInterfaces().firstOrNull { n ->
            n.interfaceAddresses.any { it.address.hostAddress == ownIP() }
        }
        val prefix = nif?.interfaceAddresses?.firstOrNull { it.address.address.size == 4 }?.networkPrefixLength ?: 0
        return "$gw/$prefix"
    }

    fun ownIP(): String? {
        for (nif in activeInterfaces()) {
            for (ia in nif.interfaceAddresses) {
                val a = ia.address.hostAddress
                if (a != null && !a.contains(":") && !a.startsWith("127.")) return a
            }
        }
        return null
    }

    fun gatewayIP(ctx: Context): String? = try {
        val wm = ctx.applicationContext.getSystemService(Context.WIFI_SERVICE) as WifiManager
        val dhcp = wm.dhcpInfo
        if (dhcp == null) null
        else {
            val g = dhcp.gateway
            if (g == 0) null
            else String.format("%d.%d.%d.%d", g and 0xFF, (g shr 8) and 0xFF, (g shr 16) and 0xFF, (g shr 24) and 0xFF)
        }
    } catch (e: Exception) {
        null
    }

    /**
     * 三通道解析（协议 §2）。knownIP/knownPort 来自已配对记录（可为 null/0）。
     * knownFP = 已配对 PC 的指纹（可为 null）。
     * 返回第一个命中的 PC，全失败返回 null。
     */
    fun resolve(ctx: Context, knownIP: String?, knownPort: Int, knownFP: String? = null,
                timeoutBudgetMs: Long = 9000): FoundPC? {
        val start = System.currentTimeMillis()
        // ① 直连上次 IP（TCP connect 有即时失败反馈 → 不等满 1.5s [N-06]）
        if (!knownIP.isNullOrEmpty() && knownPort in TCP_BASE..TCP_MAX) {
            try {
                val s = Socket()
                s.connect(InetSocketAddress(knownIP, knownPort), 1500)
                s.close()
                return FoundPC("", knownIP, knownPort, "", true)
            } catch (e: Exception) {
                // 继续下一通道
            }
        }
        if (System.currentTimeMillis() - start > timeoutBudgetMs) return null

        // ② UDP 广播（等满 1.5s；UDP 无失败信号 [N-06]）
        broadcastAddrs().forEach { bcast ->
            hitViaUDP(bcast, knownIP, knownFP)?.let { return it }
        }
        // 受限广播补充
        hitViaUDP(InetAddress.getByName("255.255.255.255"), knownIP, knownFP)?.let { return it }
        if (System.currentTimeMillis() - start > timeoutBudgetMs) return null

        // ③ 单播扫描 /24（等满 6s；不做 TCP 端口扫描 [P0-11]）
        val pool = Executors.newFixedThreadPool(50)
        val hits = java.util.Collections.synchronizedList(ArrayList<FoundPC>())
        val own = ownIP() ?: return null
        val prefix24 = own.substringBeforeLast('.') + "."
        for (i in 1..254) {
            if (System.currentTimeMillis() - start > timeoutBudgetMs) break
            val ip = prefix24 + i
            if (ip == own) continue
            pool.execute {
                hitViaUDP(InetAddress.getByName(ip), knownIP, knownFP, perAddrTimeoutMs = 400)?.let { hits.add(it) }
            }
        }
        pool.shutdown()
        pool.awaitTermination(6, TimeUnit.SECONDS)
        if (hits.isNotEmpty()) {
            // 指纹相同的那台优先（同一局域网可能有多台 PhotoBeam PC）
            return hits.firstOrNull { !knownFP.isNullOrEmpty() && it.fp == knownFP }
                ?: hits.firstOrNull { it.fp.isNotEmpty() && knownIP == null }
                ?: hits.first()
        }
        return null
    }

    /**
     * 往 target 发一次 discover，然后收应答。
     *
     * [Stage 5 修正] 同一局域网里可能**同时有多台 PhotoBeam PC**（同事的、另一台机器上的）。
     * 已知 PC 的指纹（knownFP）时：
     *   · 应答来自别的 IP 且指纹不同 → **是别人的机器 → 忽略这条应答，继续等自己那台的**；
     *   · 应答来自别的 IP 但指纹相同 → 同一台 PC 换了 IP → 算命中。
     * 没有 knownFP（首次配对）→ 首个应答即走（此时没有可比对的指纹；真伪由握手签名把关）。
     */
    private fun hitViaUDP(target: InetAddress, knownIP: String?, knownFP: String?,
                          perAddrTimeoutMs: Int = 1500): FoundPC? {
        return try {
            val s = DatagramSocket().apply {
                soTimeout = perAddrTimeoutMs
                broadcast = true
            }
            val frame = discoverFrame()
            s.send(DatagramPacket(frame, frame.size, InetSocketAddress(target, UDP_PORT)))
            val buf = ByteArray(2048)
            val deadline = System.currentTimeMillis() + perAddrTimeoutMs
            var fallback: FoundPC? = null
            while (true) {
                val left = (deadline - System.currentTimeMillis()).toInt()
                if (left <= 0) break
                try {
                    s.soTimeout = left
                    val pkt = DatagramPacket(buf, buf.size)
                    s.receive(pkt)
                    val found = parseFound(pkt.data) ?: continue
                    // 手机端手工 IP 兜底时 ip 字段可能为空 → 补 [R-19]
                    val ip = if (found.ip.isNotEmpty()) found.ip else pkt.address.hostAddress
                    if (knownFP.isNullOrEmpty()) {
                        // 没有可比对的指纹 → 首个应答即走
                        s.close()
                        return FoundPC(found.name, ip, found.port, found.fp, found.paired)
                    }
                    if (found.fp == knownFP) {
                        // 指纹一致 = 同一台 PC（哪怕 IP 变了）→ 精确命中
                        s.close()
                        return FoundPC(found.name, ip, found.port, found.fp, found.paired)
                    }
                    if (knownIP != null && knownIP != ip) {
                        // 别人的 PhotoBeam PC → 忽略，继续等自己那台的应答
                        continue
                    }
                    if (fallback == null) {
                        fallback = FoundPC(found.name, ip, found.port, found.fp, found.paired)
                    }
                } catch (e: Exception) {
                    break // 收超时（UDP 无失败信号，只能等满）
                }
            }
            s.close()
            fallback
        } catch (e: Exception) {
            null
        }
    }
}
