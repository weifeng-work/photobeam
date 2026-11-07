# PII 守门人的规则模板（不含真实值 —— 真实规则放在 GitHub Secret `PII_PATTERNS` 里）

`.github/workflows/pii-guard.yml` 会读仓库 Secret `PII_PATTERNS`（JSON 数组），
对**新增的每一行**与**每条提交信息**做扫描。Secret 不回读，所以本文件只负责：

1. 说明 secret 应有的**形状**，避免下次配错；
2. 列出本项目历史上踩过的漏网类型，供维护者核对 secret 是否覆盖。

## Secret 格式

```json
[
  {"name": "机型",     "regex": "<真实机型名的正则，多个用 | >"},
  {"name": "设备指纹", "regex": "<真实指纹值，多个用 | >"},
  {"name": "PC 主机名", "regex": "<真实主机名>"},
  {"name": "用户名",   "regex": "<Windows 用户名>"},
  {"name": "真实内网IP", "regex": "192\\.168\\.0\\.\\d+"},
  {"name": "测试目录", "regex": "<USER-DIR>|<USER-DIR>A|<USER-DIR>B"}
]
```

> 注意 JSON 里的反斜杠要写两次（`\\.`），否则 JSON 解析会吃掉一层。

## 必须覆盖的类别（都是本项目真实漏过的）

| 类别 | 为什么会漏 | 该抓的形状 |
|---|---|---|
| **机型名** | 2026-11-07 那次 6 个提交里有 4 个把真机型号写进源码注释，`PII_PATTERNS` 里没有机型规则 → 全部漏过 | 具体型号字符串 |
| **设备指纹** | Initial commit 起就带着真实指纹，两次 `filter-repo` 都没清到（规则集里没有指纹） | 16/64 位十六进制串 |
| **用户相册目录** | 实测记录里写了真机的相册目录名（如用户的网盘/微信目录） | 目录名 |
| **真实内网 IP** | 证据文档里贴了 `adb connect <IP>` | 局域网段的具体点分地址；文档里统一使用的中性示例地址应加白名单排除 |

## 维护要求

- **改了 secret 就要验证**：推一个只改注释的提交，确认 PII Guard 变绿（说明规则加载成功）；
  再临时推一个含假标识（如 `<PHONE-MODEL>`）的测试提交，确认它**变红**——两头都测才算可用。
- workflow 现在是 **fail-closed**：secret 缺失直接红，不再静默放行。
- 新分支 / force-push 时 `github.event.before` 为全 0，workflow 会退化为
  「扫最近 50 个提交」并把范围打印出来；想更严就手动 `workflow_dispatch` 全量复扫。
