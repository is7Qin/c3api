#!/usr/bin/env python3
"""独立复算 factsDigest 的期望值（U40）。

本脚本是 **KAT 的独立实现**：编码规则从规格抄下来，不从 Go 实现推导——
`internal/scheduler/static_facts.go` 的注释与这里的 docstring 是同一份规格的两份
表述，任何一侧改编码（换段序、改前缀宽度、换哈希）都会让两边的输出对不上。

编码（段序固定）：

    段 0  channel            标签  chan:static / chan:codex / chan:bps / chan:<n>
    段 1  codexAccountID
    段 2  installationID
    段 3  bpsProfileDigest
    段 4  upstreamKey
    段 5  pat

每段 = 8 字节大端长度前缀 + UTF-8 内容；六段串联后取 sha256（32 字节）。

用法：python internal/scheduler/testdata/facts_digest_vectors.py

输出：每组向量的分段前像（每段一行，可直接作为 Go KAT 的字符串字面量）、
前像总长与最终摘要。报告里贴这份输出，Go 侧的钉住值逐字节相同。
"""

import hashlib

SEGMENTS = (
    "channel",
    "codexAccountID",
    "installationID",
    "bpsProfileDigest",
    "upstreamKey",
    "pat",
)

# 通道标签：与 static_facts.go 的 factsChannelLabel 一一对应。
CHANNEL_STATIC = "chan:static"
CHANNEL_CODEX = "chan:codex"
CHANNEL_BPS = "chan:bps"

BPS_PROFILE_DIGEST = (
    "9f2d3b1c0a7e5648d1c2b3a495f60718293a4b5c6d7e8f90a1b2c3d4e5f60718"
)


def seg(text):
    """8 字节大端长度前缀 + UTF-8 内容。"""
    raw = text.encode("utf-8")
    return len(raw).to_bytes(8, "big") + raw


def preimage(parts):
    """六段前像。parts 顺序必须是 SEGMENTS。"""
    assert len(parts) == len(SEGMENTS), parts
    return b"".join(seg(p) for p in parts)


def digest(parts):
    return hashlib.sha256(preimage(parts)).hexdigest()


# 向量集：codex 典型 / BPS 典型 / 空 pat / >512B 溢出 / 槽位歧义对。
VECTORS = [
    (
        "codex_typical",
        "codex 真实形状：channel + 账号 + UUID installation + 上游键 + pat（无 BPS profile）",
        [
            CHANNEL_CODEX,
            "acct_9f2c41d8e7b3",
            "3f1c9a24-8b6d-4e07-9c1a-2d5b8e7f0a13",
            "",
            "sk-live-codex-0001",
            "pat-codex-0001",
        ],
    ),
    (
        "bps_typical",
        "BPS 典型：installation 恒空、pat 恒空（BPS 分支从来不读 PAT）、profile 摘要有值",
        [
            CHANNEL_BPS,
            "acct_7c1d5e93b204",
            "",
            BPS_PROFILE_DIGEST,
            "sk-live-bps-0001",
            "",
        ],
    ),
    (
        "codex_empty_pat",
        "空 pat：codex 通道但凭据没有 PAT（缺失 = \"\"，与改前的空 string 字段同义）",
        [
            CHANNEL_CODEX,
            "acct_9f2c41d8e7b3",
            "3f1c9a24-8b6d-4e07-9c1a-2d5b8e7f0a13",
            "",
            "sk-live-codex-0001",
            "",
        ],
    ),
    (
        "overflow_547",
        ">512B 溢出：四段合计 547B ⇒ Go 侧退化为一次 make（前像仍逐字节可复算）",
        [
            CHANNEL_CODEX,
            "acct_9f2c41d8e7b3",
            "3f1c9a24-8b6d-4e07-9c1a-2d5b8e7f0a13",
            "",
            # 547B 前像：跨过 512B 的栈上上界（>512 才走 make 分支），但仍留在
            # momus 实测的 534B 形状区间（465–561 ns/op）——溢出基准用同一形状。
            "sk-live-" + "0123456789abcdef" * 26,
            "pat-" + "f" * 8,
        ],
    ),
    (
        "slot_ab_c",
        "槽位歧义对 A：(\"ab\",\"c\") —— 裸拼接与下一组同为 \"abc\"，长度前缀必须分开",
        [CHANNEL_CODEX, "acct_9f2c41d8e7b3", "", "", "ab", "c"],
    ),
    (
        "slot_a_bc",
        "槽位歧义对 B：(\"a\",\"bc\")",
        [CHANNEL_CODEX, "acct_9f2c41d8e7b3", "", "", "a", "bc"],
    ),
]


def go_literal(parts):
    """把前像渲染成 Go 侧 KAT 用的「一段一行」字面量。"""
    lines = []
    for i, (name, value) in enumerate(zip(SEGMENTS, parts)):
        chunk = seg(value).hex()
        lines.append(
            '\t\t"%s" + // 段 %d %s = %r（len=%d）' % (chunk, i, name, value, len(value.encode("utf-8")))
        )
    return "\n".join(lines)


def main():
    print("factsDigest KAT —— 独立实现复算（%d 组向量）" % len(VECTORS))
    print("编码：6 段 × (8B 大端长度前缀 + UTF-8)，串联后 sha256\n")
    for name, why, parts in VECTORS:
        pre = preimage(parts)
        d = digest(parts)
        print("==" * 36)
        print("[%s] %s" % (name, why))
        print("前像长度 = %d 字节（%s 512B 栈上上界）" % (len(pre), "<=" if len(pre) <= 512 else ">"))
        print("前像（hex，分段）：")
        for i, (segname, value) in enumerate(zip(SEGMENTS, parts)):
            print(
                "  段 %d %-17s len=%4d  %s"
                % (i, segname, len(value.encode("utf-8")), seg(value).hex())
            )
        print("前像（hex，整串）= %s" % pre.hex())
        print("sha256            = %s" % d)
        print("Go 字面量：")
        print(go_literal(parts))
    # 槽位歧义对的非碰撞自证。
    a = digest(VECTORS[4][2])
    b = digest(VECTORS[5][2])
    assert a != b, "槽位歧义对被折成同一摘要"
    print("==" * 36)
    print("槽位歧义对：%s != %s" % (a[:16], b[:16]))


if __name__ == "__main__":
    main()
