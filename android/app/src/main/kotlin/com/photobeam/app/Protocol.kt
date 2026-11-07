package com.photobeam.app

// PhotoBeam 协议实现（Kotlin 端）— 依据 docs/protocol-v1.md v1.9
// 平台要求：Android 13 (API 33)+。Ed25519/X25519 均为 BouncyCastle rfc8032/rfc7748 裸字节实现（
// 避免 Conscrypt XDH generateSecret 的端序歧义与 Go 端不对拍的风险）

import java.io.InputStream
import java.io.OutputStream
import java.net.Socket
import java.math.BigInteger
import java.security.MessageDigest
import java.security.SecureRandom
import javax.crypto.Cipher
import javax.crypto.spec.GCMParameterSpec
import javax.crypto.spec.SecretKeySpec

// ---------- X25519（纯 Kotlin，RFC 7748） ----------

object X25519 {
    // BouncyCastle rfc7748 裸字节实现（与 Go crypto/ecdh 直接对拍，无端序歧义）
    fun generatePrivateKey(): ByteArray = ByteArray(32).also { java.security.SecureRandom().nextBytes(it) }

    fun publicKey(secret: ByteArray): ByteArray {
        val pub = ByteArray(32)
        org.bouncycastle.math.ec.rfc7748.X25519.generatePublicKey(secret, 0, pub, 0)
        return pub
    }

    fun shared(secret: ByteArray, peerPubRaw: ByteArray): ByteArray {
        val out = ByteArray(32)
        org.bouncycastle.math.ec.rfc7748.X25519.scalarMult(secret, 0, peerPubRaw, 0, out, 0)
        return out
    }
}

// ---------- 编解码 ----------

const val PROTO_VER = 1
const val MIN_PROTO_VER = 1

fun b64e(b: ByteArray): String = java.util.Base64.getEncoder().encodeToString(b)
fun b64d(s: String): ByteArray = java.util.Base64.getDecoder().decode(s)
fun intToBytes(v: Int): ByteArray = byteArrayOf(
    ((v shr 24) and 0xFF).toByte(), ((v shr 16) and 0xFF).toByte(),
    ((v shr 8) and 0xFF).toByte(), (v and 0xFF).toByte())

fun sha256(b: ByteArray): ByteArray = MessageDigest.getInstance("SHA-256").digest(b)

// 设备指纹 fp = hex(SHA-256(ed_pub_raw32 || x25519_id_pub_raw32))[:16]（协议 §1 [R-25]）
fun fingerprint(edPubRaw: ByteArray, x25519IdPubRaw: ByteArray): String {
    val md = MessageDigest.getInstance("SHA-256")
    md.update(edPubRaw)
    md.update(x25519IdPubRaw)
    return md.digest().joinToString("") { "%02x".format(it) }.take(16)
}

fun sha256Stream(input: java.io.InputStream, body: (ByteArray, Int) -> Unit): ByteArray {
    val md = MessageDigest.getInstance("SHA-256")
    val buf = ByteArray(256 * 1024)
    while (true) {
        val n = input.read(buf)
        if (n < 0) break
        md.update(buf, 0, n)
        body(buf, n)
    }
    return md.digest()
}

fun hmacSha256(key: ByteArray, data: ByteArray): ByteArray {
    val mac = javax.crypto.Mac.getInstance("HmacSHA256")
    mac.init(SecretKeySpec(key, "HmacSHA256"))
    return mac.doFinal(data)
}

fun hkdf(ikm: ByteArray, salt: ByteArray, info: ByteArray, len: Int): ByteArray {
    val prk = hmacSha256(if (salt.isEmpty()) ByteArray(32) else salt, ikm)
    val out = ByteArray(len)
    var t = ByteArray(0)
    var pos = 0
    var counter = 1
    while (pos < len) {
        t = hmacSha256(prk, t + info + byteArrayOf(counter.toByte()))
        val n = minOf(t.size, len - pos)
        System.arraycopy(t, 0, out, pos, n)
        pos += n
        counter++
    }
    return out
}

// ---------- 设备身份 ----------

// Ed25519 PKCS8 v1（seed 为最后 32B）：30 2e 02 01 00 30 05 06 03 2b 65 70 04 22 04 20 || seed
fun ed25519Pkcs8FromSeed(seed: ByteArray): ByteArray {
    require(seed.size == 32)
    val prefix = byteArrayOf(0x30, 0x2e, 0x02, 0x01, 0x00, 0x30, 0x05, 0x06,
        0x03, 0x2b, 0x65, 0x70, 0x04, 0x22, 0x04, 0x20)
    return prefix + seed
}

class IdentityKeys(
    val edPrivRaw: ByteArray, // 32B Ed25519 seed（私钥，BC rfc8032 签名用）
    val edPubRaw: ByteArray,  // 32B raw 公钥
    val xPriv: ByteArray,     // 32B 长期 X25519 私钥（仅指纹与设备绑定证明）
    val xPub: ByteArray      // 32B
) {
    fun fingerprint(): String {
        val md = MessageDigest.getInstance("SHA-256")
        md.update(edPubRaw)
        md.update(xPub)
        return md.digest().joinToString("") { "%02x".format(it) }.take(16)
    }

    fun sign(transcriptHash: ByteArray): ByteArray {
        val sig = ByteArray(64)
        org.bouncycastle.math.ec.rfc8032.Ed25519.sign(edPrivRaw, 0, edPubRaw, 0,
            transcriptHash, 0, transcriptHash.size, sig, 0)
        return sig
    }

    companion object {
        fun verify(edPubRaw: ByteArray, msg: ByteArray, sig: ByteArray): Boolean =
            org.bouncycastle.math.ec.rfc8032.Ed25519.verify(sig, 0, edPubRaw, 0,
                msg, 0, msg.size)

        fun loadOrCreate(prefs: android.content.SharedPreferences): IdentityKeys {
            val pk = prefs.getString("ed_seed", null)
            val pub = prefs.getString("ed_pub_raw", null)
            val xp = prefs.getString("x_priv", null)
            if (pk != null && pub != null && xp != null) {
                val xPriv = b64d(xp)
                return IdentityKeys(b64d(pk), b64d(pub), xPriv, X25519.publicKey(xPriv))
            }
            val edSeed = ByteArray(32).also { SecureRandom().nextBytes(it) }
            val edPubRaw = ByteArray(32)
            org.bouncycastle.math.ec.rfc8032.Ed25519.generatePublicKey(edSeed, 0, edPubRaw, 0)
            val xPriv = X25519.generatePrivateKey()
            val xPub = X25519.publicKey(xPriv)
            val keys = IdentityKeys(edSeed, edPubRaw, xPriv, xPub)
            prefs.edit()
                .putString("ed_seed", b64e(keys.edPrivRaw))
                .putString("ed_pub_raw", b64e(keys.edPubRaw))
                .putString("x_priv", b64e(keys.xPriv))
                .commit() // 同步落盘：安装器重装杀进程时 apply 的异步写会丢
            return keys
        }
    }
}

// ---------- 加密帧（协议 §4） ----------

const val FRAME_CTRL: Byte = 0x01
const val FRAME_FILE: Byte = 0x02
const val MAX_FRAME_LEN = 1 shl 20

class SecureChannel(val sessionKey: ByteArray) {
    fun seal(frameType: Byte, payload: ByteArray): ByteArray {
        require(payload.size <= MAX_FRAME_LEN) { "帧超限" }
        val nonce = ByteArray(12).also { SecureRandom().nextBytes(it) }
        val lenB = intToBytes(payload.size)
        val aad = byteArrayOf(frameType) + lenB + nonce
        val c = Cipher.getInstance("AES/GCM/NoPadding")
        c.init(Cipher.ENCRYPT_MODE, SecretKeySpec(sessionKey, "AES"), GCMParameterSpec(128, nonce))
        c.updateAAD(aad)
        val ct = c.doFinal(payload)
        return byteArrayOf(frameType) + lenB + nonce + ct
    }

    fun open(frame: ByteArray): Pair<Byte, ByteArray> {
        require(frame.size >= 5 + 12 + 16) { "帧过短" }
        val ft = frame[0]
        val plen = ((frame[1].toInt() and 0xFF) shl 24) or ((frame[2].toInt() and 0xFF) shl 16) or
            ((frame[3].toInt() and 0xFF) shl 8) or (frame[4].toInt() and 0xFF)
        require(plen in 1..MAX_FRAME_LEN) { "帧长度非法: $plen" }
        require(frame.size == 5 + 12 + plen + 16) { "帧长度不一致" }
        val nonce = frame.copyOfRange(5, 17)
        val aad = frame.copyOfRange(0, 17)
        val ct = frame.copyOfRange(17, frame.size)
        val c = Cipher.getInstance("AES/GCM/NoPadding")
        c.init(Cipher.DECRYPT_MODE, SecretKeySpec(sessionKey, "AES"), GCMParameterSpec(128, nonce))
        c.updateAAD(aad)
        return Pair(ft, c.doFinal(ct))
    }

    fun writeFrame(out: OutputStream, frameType: Byte, payload: ByteArray) {
        out.write(seal(frameType, payload))
        out.flush()
    }

    fun readFrame(inp: InputStream): Pair<Byte, ByteArray> {
        val f = inp.readFrame()
        return open(f)
    }
}

fun OutputStream.writePlainFrame(body: ByteArray) {
    val len = body.size
    write(byteArrayOf(((len shr 24) and 0xFF).toByte(), ((len shr 16) and 0xFF).toByte(),
        ((len shr 8) and 0xFF).toByte(), (len and 0xFF).toByte()))
    write(body)
    flush()
}

fun InputStream.readPlainFrame(max: Int = 4096): ByteArray {
    val h = readNBytes(4)
    require(h.size == 4) { "连接关闭" }
    val len = ((h[0].toInt() and 0xFF) shl 24) or ((h[1].toInt() and 0xFF) shl 16) or
        ((h[2].toInt() and 0xFF) shl 8) or (h[3].toInt() and 0xFF)
    require(len in 1..max) { "明文帧长度非法: $len" }
    val b = readNBytes(len)
    require(b.size == len) { "连接关闭" }
    return b
}

fun InputStream.readFrame(max: Int = MAX_FRAME_LEN + 64): ByteArray {
    val h = readNBytes(5)
    require(h.size == 5) { "连接关闭" }
    val plen = ((h[1].toInt() and 0xFF) shl 24) or ((h[2].toInt() and 0xFF) shl 16) or
        ((h[3].toInt() and 0xFF) shl 8) or (h[4].toInt() and 0xFF)
    require(plen in 1..MAX_FRAME_LEN) { "帧长度非法: $plen" }
    val rest = readNBytes(12 + plen + 16)
    require(rest.size == 12 + plen + 16) { "连接关闭" }
    return h + rest
}

// ---------- transcript（协议 §3.1 [P0-01][N-03]） ----------

private fun helloField(name: String, value: ByteArray): ByteArray {
    val nb = name.toByteArray(Charsets.UTF_8)
    val out = ByteArray(4 + nb.size + 4 + value.size)
    var p = 0
    fun putU32(v: Int) {
        out[p++] = ((v shr 24) and 0xFF).toByte()
        out[p++] = ((v shr 16) and 0xFF).toByte()
        out[p++] = ((v shr 8) and 0xFF).toByte()
        out[p++] = (v and 0xFF).toByte()
    }
    putU32(nb.size); System.arraycopy(nb, 0, out, p, nb.size); p += nb.size
    putU32(value.size); System.arraycopy(value, 0, out, p, value.size)
    return out
}

private fun helloItems(
    devName: String, edPubRaw: ByteArray, nonce: ByteArray, role: String,
    ver: Int, ephRaw: ByteArray, idRaw: ByteArray
): ByteArray {
    val roleByte: Byte = if (role == "sender") 0x00 else 0x01
    return helloField("dev_name", devName.toByteArray(Charsets.UTF_8)) +
        helloField("ed_pub", edPubRaw) +
        helloField("nonce", nonce) +
        helloField("role", byteArrayOf(roleByte)) +
        helloField("ver", intToBytes(ver)) +
        helloField("x25519_eph_pub", ephRaw) +
        helloField("x25519_id_pub", idRaw)
}

// transcript = SHA-256( [0x01][Sender 字段项][0x00][Receiver 字段项] ) [N-03]
fun buildTranscript(
    sDevName: String, sEdPubRaw: ByteArray, sNonce: ByteArray, sVer: Int, sEph: ByteArray, sId: ByteArray,
    rDevName: String, rEdPubRaw: ByteArray, rNonce: ByteArray, rVer: Int, rEph: ByteArray, rId: ByteArray
): ByteArray =
    sha256(byteArrayOf(0x01) +
        helloItems(sDevName, sEdPubRaw, sNonce, "sender", sVer, sEph, sId) +
        byteArrayOf(0x00) +
        helloItems(rDevName, rEdPubRaw, rNonce, "receiver", rVer, rEph, rId))

// ---------- 握手（协议 §3） ----------

class HandshakeResult(
    val channel: SecureChannel,
    val mode: String,            // pairing | reconnect
    val receiverName: String,
    val receiverFP: String,
    val receiverEdPubRaw: ByteArray, // 保存设备记录用（协议 §3.3 持久化字段）
    val sas: String,
    val transcript: ByteArray,
    val versionErr: String?      // 非 null = 对端报告版本不支持
)

/**
 * SAS 核对结果（三态，Stage 5 新增）
 *
 * 此前只有「一致 / 不一致」两态，**等待超时也被当成「不一致」**发 `pair_reject` →
 * PC 端会把一次「用户没来得及看」记成「手机端拒绝配对」，日志与提示全错。
 * 现在超时单独成一态：手机端**不发任何帧**直接断开，由 PC 自己的 60 秒定时器收尾并提示。
 */
const val SAS_CONFIRM = 0 // 用户点「一致」
const val SAS_REJECT = 1  // 用户点「不一致」→ 发 pair_reject
const val SAS_TIMEOUT = 2 // 等待超时 → 静默断开

class PairingEngine(
    private val prefs: android.content.SharedPreferences,
    private val keys: IdentityKeys,
    private val myName: String
) {
    // 已配对设备记录（从 prefs 读）
    // [Stage 10] 新增 devDir：PC 端为本机分配的落盘目录名（<设备名>-<fp8>），
    // 用于在界面上告诉用户「我这台手机的照片在电脑的哪个目录」。
    data class PairedPC(val fp: String, val edPubRaw: ByteArray, val name: String,
        val lastIP: String, val lastPort: Int, val devDir: String = "")

    fun loadPairedPC(): PairedPC? {
        val fp = prefs.getString("pc_fp", null)
        android.util.Log.d("PhotoBeam", "loadPairedPC: fp=${fp ?: "null"}")
        if (fp == null) return null
        return PairedPC(fp, b64d(prefs.getString("pc_edpub", "")!!),
            prefs.getString("pc_name", "") ?: "", prefs.getString("pc_ip", "") ?: "",
            prefs.getInt("pc_port", 0), prefs.getString("pc_dev_dir", "") ?: "")
    }

    fun savePairedPC(pc: PairedPC) {
        // [Stage 17 · 修复 S-2] **绝不能**把 prefs 整个打进日志。
        //
        // 原实现末尾带 `prefs=$prefs`：SharedPreferencesImpl.toString() 会打印
        // map 里**全部** key=value —— 其中含 `ed_seed`（Ed25519 私钥种子）与
        // `x_priv`（X25519 私钥）。而 savePairedPC 在**每次重连**都会调用
        // （:527），正常同步时等于**每 30 秒把两把长期身份私钥原文写进 logcat**。
        //
        // 后果：私钥进日志环形缓冲 → adb logcat、厂商自动上传的日志收集、
        // 任何崩溃上报 SDK 都能拿到 → 可冒充本机。这与本项目
        // 「数据不出局域网、不上云」的隐私承诺直接冲突。
        android.util.Log.d("PhotoBeam", "savePairedPC: fp=${pc.fp} name=${pc.name} devDir=${pc.devDir}")
        // [A4] 记录配对时间（诊断信息用）：只在**首次**配对时写入，
        // 之后 updateLastConnection 的重写不能覆盖它。
        val pairedAt = try {
            val old = prefs.getLong("pc_paired_at", 0L)
            if (old > 0L) old else System.currentTimeMillis()
        } catch (e: Exception) { System.currentTimeMillis() }
        prefs.edit()
            .putString("pc_fp", pc.fp).putString("pc_edpub", b64e(pc.edPubRaw))
            .putString("pc_name", pc.name).putString("pc_ip", pc.lastIP)
            .putInt("pc_port", pc.lastPort).putLong("pc_paired_at", pairedAt)
            // [Stage 10] PC 端分配给本机的落盘目录名（老版本 PC 不给 → 空 → 界面不显示）
            .putString("pc_dev_dir", pc.devDir)
            .commit() // [B-09] 同步落盘
    }

    fun pairedFingerprint(): String? = prefs.getString("pc_fp", null)
    fun pairedNetworkFingerprint(): String? = prefs.getString("net_fp", null)
    fun saveNetworkFingerprint(fp: String) {
        prefs.edit().putString("net_fp", fp).commit()
    }

    /** [P1-01] found 应答后更新 PC 的 ip/监听端口（保留 fp/名称/公钥） */
    fun updateLastConnection(ip: String, port: Int) {
        val known = loadPairedPC() ?: return
        savePairedPC(known.copy(lastIP = ip, lastPort = port))
    }

    fun clearPairedPC() {
        // [修复] 清干净**全部**配对相关键：pc_dev_dir / pc_paired_at / net_fp 一并清，
        // 否则一旦接线（解绑 / 重新配对）会在 prefs 里留下脏数据（旧落盘目录名、
        // 旧配对时间、旧网络指纹都会误导 UI 与「换网判定」）。
        prefs.edit().remove("pc_fp").remove("pc_edpub").remove("pc_name")
            .remove("pc_ip").remove("pc_port").remove("pc_dev_dir")
            .remove("pc_paired_at").remove("net_fp").commit()
    }

    /**
     * 执行握手（HELLO → HELLO → MODE → ECDH/SAS → 认证帧）。
     * pairing 模式下，本函数发送 pair_confirm 前会回调 [sasConfirm]
     * （返回 SAS_CONFIRM / SAS_REJECT / SAS_TIMEOUT 三态）。
     *
     * [协议 §3.2 [R-13][R-14]] [sasConfirm] 的第三个参数 `wasPinned`：
     * true = 本端**已 pin 过这台 PC**（prefs 里有 pc_fp），但 PC 回的 MODE 是
     * `pairing` —— 即「PC 的配对记录已被重置」（在 PC 上点过「重新配对」/清了
     * devices.json）。此时对话框必须追加提示，用户确认后才覆盖旧信任密钥。
     * 首次配对（wasPinned=false）不需要这句。
     *
     * [Stage 5 第五轮] 回调多带一个 **peerName**（对端电脑名，来自对端 HELLO 的 dev_name）：
     * 用户要能核对「对面那台电脑是不是我的电脑」——这在同一个局域网里跑着多对
     * PhotoBeam 用户时是唯一可靠的锚点（C1）。
     */
    fun handshake(
        sock: Socket,
        sasConfirm: (sas: String, peerName: String, wasPinned: Boolean) -> Int,
        allowPairing: Boolean = true
    ): HandshakeResult {
        val inp = sock.getInputStream()
        val out = sock.getOutputStream()
        val known = loadPairedPC()

        fun dbg(m: String) = android.util.Log.d("PhotoBeam", "握手: $m")
        dbg("开始")
        // 1) 写自己的 HELLO（不等待对方）
        val eph = ByteArray(32).also { SecureRandom().nextBytes(it) }
        val ephPub = X25519.publicKey(eph)
        val myNonce = ByteArray(16).also { SecureRandom().nextBytes(it) }
        out.writePlainFrame(org.json.JSONObject()
            .put("pb", 1).put("type", "hello").put("ver", PROTO_VER)
            .put("ed_pub", b64e(keys.edPubRaw))
            .put("x25519_id_pub", b64e(keys.xPub))
            .put("x25519_eph_pub", b64e(ephPub))
            .put("nonce", b64e(myNonce))
            .put("dev_name", myName)
            .put("role", "sender")
            // [协议 §3.1 增补 · v1.5] pair_intent：本次连接是否由**用户主动发起配对**。
            //   allowPairing=true  → 交互路径（MainActivity 点「② 配对电脑」）→ PC 必须弹向导窗，
            //                         用户才能做人眼核对 6 位 SAS。
            //   allowPairing=false → 后台自动连接/重连（SenderService 每 30 秒一次）
            //                         → PC 不得弹向导窗（否则用户正在核对的窗口被反复顶掉）。
            // 注意：该字段**不进入 transcript**（§3.1 字段表未增补），密码学中性。
            .put("pair_intent", allowPairing)
            .toString().toByteArray(Charsets.UTF_8))

        dbg("HELLO 已发送, 本机 fp=${keys.fingerprint()}")
        // 2) 读对端 HELLO
        val peerHello = org.json.JSONObject(String(inp.readPlainFrame(), Charsets.UTF_8))
        val peerEdRaw = b64d(peerHello.getString("ed_pub"))
        val peerIdRaw = b64d(peerHello.getString("x25519_id_pub"))
        val peerEphRaw = b64d(peerHello.getString("x25519_eph_pub"))
        val peerNonce = b64d(peerHello.getString("nonce"))
        val peerFP = fingerprint(peerEdRaw, peerIdRaw)

        dbg("对端 HELLO 已收, fp=$peerFP")
        // 3) 读 MODE（明文）
        val modeObj = org.json.JSONObject(String(inp.readPlainFrame(), Charsets.UTF_8))
        val mode = modeObj.optString("mode", "pairing")
        val errObj = modeObj.optJSONObject("err")
        if (errObj != null) {
            return HandshakeResult(SecureChannel(ByteArray(32)), mode,
                peerHello.optString("dev_name", ""), peerFP, peerEdRaw,
                "", ByteArray(32), errObj.optString("reason"))
        }

        dbg("MODE 已收: mode=$mode err=${modeObj.optJSONObject("err")}")
        // [协议 §3.2 [R-13]] reset=true = PC 侧曾清除过本机的配对记录（点了「重新配对」/
        // 清了 devices.json）。协议只要求「记日志 + 触发确认提示」，故这里只打日志；
        // 触发条件用 known!=null（无论 reset）——见下面 pairing 分支的 wasPinned。
        if (modeObj.optBoolean("reset", false)) {
            dbg("PC 报告 reset=true：本机在电脑端的配对记录曾被清除")
        }
        // 4) 密钥协商 [N-01]：shared = X25519(本端临时私钥, 对端临时公钥)
        val shared = X25519.shared(eph, peerEphRaw)
        // [协议 §3 低阶点防护] 对端临时公钥若是低阶点（如全零），X25519 会算出**全零**共享密钥。
        // 正常对端不可能给出这种结果 → 按协议违规处理：断开 + 日志（调用方会 close socket）。
        if (shared.all { it == 0.toByte() }) {
            dbg("X25519 shared 全零（对端临时公钥为低阶点）→ 协议违规，断开")
            throw java.io.IOException("密钥协商失败：对端临时公钥无效（共享密钥全零）")
        }
        val sessionKey = hkdf(shared, myNonce + peerNonce,
            "photobeam-session-v1".toByteArray(), 32)
        // SAS salt（发送方视角）：sender_eph‖receiver_eph‖sender_nonce‖receiver_nonce
        val sasKey = hkdf(shared, ephPub + peerEphRaw + myNonce + peerNonce,
            "photobeam-sas-v1".toByteArray(), 4)
        val sasInt = ((sasKey[0].toInt() and 0xFF) shl 24) or
            ((sasKey[1].toInt() and 0xFF) shl 16) or
            ((sasKey[2].toInt() and 0xFF) shl 8) or
            (sasKey[3].toInt() and 0xFF)
        val sas = "%06d".format((sasInt.toLong() and 0xFFFFFFFFL) % 1000000L)

        val transcript = buildTranscript(
            myName, keys.edPubRaw, myNonce, PROTO_VER, ephPub, keys.xPub,
            peerHello.getString("dev_name"), peerEdRaw, peerNonce,
            peerHello.getInt("ver"), peerEphRaw, peerIdRaw)

        val ch = SecureChannel(sessionKey)
        dbg("SAS=$sas 会话就绪")

        // 5) 认证
        if (mode == "pairing") {
            if (!allowPairing) {
                throw java.io.IOException("对端要求重新配对（本端在后台发送路径，已断开）")
            }
            // [Stage 5] 三态：一致 / 用户拒绝 / 等待超时
            // 超时**不发 pair_reject**——pair_reject 的语义是「用户明确拒绝」，
            // 把「用户没来得及看」也当成拒绝，会让 PC 端记成「手机端拒绝配对」并弹错提示。
            // 超时只静默断开，由 PC 自己的 60 秒定时器收尾并提示「配对超时」。
            //
            // [协议 §3.2 [R-13][R-14]] 第三个参数 wasPinned = 本端**已 pin 过这台 PC**，
            // 但 PC 却回 pairing（= 它的配对记录被重置了）→ 对话框要追加确认文案。
            //
            // [Stage 17 · 修复 M-6] 判定必须带上 fp 比对：`known != null` 只能说明
            // 「本机存过**某一台**电脑」，不能说明「存的就是**这次握手对面这台**」。
            // 协议原文（docs/protocol-v1.md:210）写的是「Sender 已 pin **该 PC** 但
            // mode=pairing」。用 known != null 的后果：先配了 PC-A，之后改配同网段的
            // PC-B → PC-B 回 pairing → wasPinned=true → 弹「该电脑的配对记录已被重置」，
            // 而 PC-B 从来没配过；用户确认后还会静默覆盖 PC-A 的信任记录。
            when (sasConfirm(sas, peerHello.optString("dev_name", ""),
                known != null && known.fp == peerFP)) {
                SAS_CONFIRM -> {
                    ch.writeFrame(out, FRAME_CTRL, org.json.JSONObject()
                        .put("type", "pair_confirm")
                        .put("sas", sas)
                        .put("sig", b64e(keys.sign(transcript)))
                        .put("dev_name", myName)
                        .toString().toByteArray(Charsets.UTF_8))
                    dbg("pair_confirm 已发送 (agreed=true)")
                }
                SAS_REJECT -> {
                    ch.writeFrame(out, FRAME_CTRL, org.json.JSONObject()
                        .put("type", "pair_reject")
                        .put("sas", sas)
                        .put("sig", b64e(keys.sign(transcript)))
                        .put("dev_name", myName)
                        .toString().toByteArray(Charsets.UTF_8))
                    dbg("pair_reject 已发送 (用户点了『不一致』)")
                    throw java.io.IOException("用户拒绝配对（两端数字不一致）")
                }
                else -> {
                    dbg("SAS 核对超时，静默断开（不发 pair_reject）")
                    throw java.io.IOException("配对核对超时（未在窗口内确认）")
                }
            }
            // 读 pair_ok 并校验 Receiver 签名 [P0-03]
            dbg("读 pair_ok...")
            val (ft, pt) = ch.readFrame(inp)
            require(ft == FRAME_CTRL) { "配对阶段收到非控制帧" }
            val ok = org.json.JSONObject(String(pt, Charsets.UTF_8))
            require(ok.optString("type") == "pair_ok") { "配对流程异常" }
            val peerSig = b64d(ok.getString("sig"))
            require(IdentityKeys.verify(peerEdRaw, transcript, peerSig)) { "电脑签名校验失败" }
            val name = ok.optString("dev_name").ifEmpty { peerHello.getString("dev_name") }
            // [Stage 10] PC 端回传本机的落盘目录名（老版本 PC 没有这个字段 → 空）
            val devDir = ok.optString("dev_dir", "")
            dbg("pair_ok dev_dir=$devDir")
            // 配对成功持久化（协议 §3.3）：双方各自保存
            savePairedPC(PairedPC(peerFP, peerEdRaw, name,
                sock.inetAddress.hostAddress ?: "", sock.port, devDir))
            return HandshakeResult(ch, mode, name, peerFP, peerEdRaw, sas, transcript, null)
        } else {
            // 重连：PC 已 pin 本端。本端 pin 丢失（重装/清数据）→ 按协议 §3.2
            // 断开并提示，由用户在 PC 端点『重新配对』后走 SAS 流程重建双向 pin
            if (known == null) {
                throw java.io.IOException("电脑的身份记录与手机不匹配（电脑端有本机记录但本机已重置）。" +
                    "请在电脑托盘点『重新配对』后重试")
            }
            dbg("重连模式（双向 pin 均存在）")
            ch.writeFrame(out, FRAME_CTRL, org.json.JSONObject()
                .put("type", "auth").put("sig", b64e(keys.sign(transcript)))
                .toString().toByteArray(Charsets.UTF_8))
            val (ft, pt) = ch.readFrame(inp)
            require(ft == FRAME_CTRL) { "认证阶段收到非控制帧" }
            val ok = org.json.JSONObject(String(pt, Charsets.UTF_8))
            require(ok.optString("type") == "auth_ok") { "认证流程异常" }
            val peerSig = b64d(ok.getString("sig"))
            require(IdentityKeys.verify(known.edPubRaw, transcript, peerSig)) { "电脑签名校验失败" }
            // [Stage 10] 重连时 PC 也会回传 dev_dir（老版本 PC 没有 → 空，保留旧值）
            val devDir = ok.optString("dev_dir", "")
            if (devDir.isNotEmpty()) {
                dbg("auth_ok dev_dir=$devDir")
            }
            savePairedPC(known.copy(lastIP = sock.inetAddress.hostAddress ?: known.lastIP,
                lastPort = sock.port,
                devDir = devDir.ifEmpty { known.devDir }))
            return HandshakeResult(ch, mode,
                peerHello.optString("dev_name", ""), peerFP, peerEdRaw, sas, transcript, null)
        }
    }
}
