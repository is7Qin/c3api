#!/usr/bin/env python3
"""独立复算 payloadDigest 的期望值（P3a）。

本脚本是 **KAT 的独立实现**：编码规则从规格抄下来，不从 Go 实现推导——
`internal/scheduler/payload_digest.go` 的注释与这里的 docstring 是同一份规格的两份
表述，任何一侧改编码（换段序、改前缀宽度、换哈希、把 expires 挪出前像）都会让两边
的输出对不上。

编码（段序固定，段数恒为 3）：

    段 0  token      seg(s)   = u64be(len(s)) || UTF-8 内容
    段 1  refresh    seg(s)
    段 2  expires    seg64(v) = u64be(8) || u64be(uint64(v))   # 共 16B

前像 = seg(token) || seg(refresh) || seg64(expiresUnixNano)，长度 = 24 + |token| +
|refresh| + 8，sha256 取 32 字节。

为什么要 seg64 而不是把整数写成十进制串：十进制不是单射（"+1" 与 "1"、"01" 与 "1"
都是两条不同的串而同一个数），会凭空多出可区分的键；定宽大端则把 int64 双射到
8 字节，负数按二补数（与 Go 的 uint64(v) 一致）。

用法：python internal/scheduler/testdata/payload_digest_vectors.py

输出：每组向量的分段前像（每段一行，可直接作为 Go KAT 的字符串字面量）、前像总长
与最终摘要。报告里贴这份输出，Go 侧的钉住值逐字节相同。
"""

import hashlib

SEGMENTS = ("token", "refresh", "expires")

EXPIRES_TYPICAL = 1893456000000000000  # 2030-01-01T00:00:00Z，UnixNano


def seg(text):
    """8 字节大端长度前缀 + UTF-8 内容。"""
    raw = text.encode("utf-8")
    return len(raw).to_bytes(8, "big") + raw


def seg64(value):
    """8 字节大端长度前缀（恒 8）+ 8 字节大端二补数。"""
    raw = (value % (1 << 64)).to_bytes(8, "big")
    return (8).to_bytes(8, "big") + raw


def preimage(parts):
    """三段前像。parts 顺序必须是 SEGMENTS。"""
    assert len(parts) == len(SEGMENTS), parts
    token, refresh, expires = parts
    return seg(token) + seg(refresh) + seg64(expires)


def digest(parts):
    return hashlib.sha256(preimage(parts)).hexdigest()


# 6 组向量：典型 / 只变 token / 只变 refresh / 只变 expires / 空三元组 / 长度前缀对
# （第 6 组是**两条**，它们的裸拼接同为 "abc1"，长度前缀必须把前像分开）。
VECTORS = [
    (
        "typical",
        "典型：at/rt/2030-01-01Z 过期时刻",
        [("at", "rt", EXPIRES_TYPICAL)],
    ),
    (
        "token_only",
        "只变 token（refresh/expires 恒定）",
        [("at2", "rt", EXPIRES_TYPICAL)],
    ),
    (
        "refresh_only",
        "只变 refresh（token/expires 恒定）",
        [("at", "rt2", EXPIRES_TYPICAL)],
    ),
    (
        "expires_only",
        "只变 expires（token/refresh 恒定）——摘要漏掉 expires 的唯一机械抓点",
        [("at", "rt", EXPIRES_TYPICAL + 1)],
    ),
    (
        "empty",
        "空三元组（nil ext / 全空字段都落在这里：derefString 抹平 nil 与 \"\"）",
        [("", "", 0)],
    ),
    (
        "length_prefix_pair",
        "长度前缀对：(\"ab\",\"c\",1) 与 (\"a\",\"bc\",1)，裸拼接同为 ab c 1",
        [("ab", "c", 1), ("a", "bc", 1)],
    ),
]


def go_literal(parts):
    """把前像渲染成 Go 侧 KAT 用的「一段一行」字面量。"""
    token, refresh, expires = parts
    lines = []
    for index, (name, chunk) in enumerate(
        (("token", seg(token)), ("refresh", seg(refresh)), ("expires", seg64(expires)))
    ):
        lines.append(
            '\t\t\t\t"%s" + // 段 %d %s（len=%d）'
            % (chunk.hex(), index, name, len(chunk))
        )
    return "\n".join(lines)


def main():
    print("payloadDigest KAT —— 独立实现复算（6 组向量，含「只变 expires」）")
    print("编码：seg(token) || seg(refresh) || seg64(expires)，各段 8B 大端长度前缀，sha256\n")
    seen = {}
    for name, why, members in VECTORS:
        print("==" * 36)
        print("[%s] %s" % (name, why))
        for parts in members:
            pre = preimage(parts)
            d = digest(parts)
            seen[(name, tuple(str(p) for p in parts))] = d
            print(
                "输入 = token=%r refresh=%r expires=%d"
                % (parts[0], parts[1], parts[2])
            )
            print(
                "前像长度 = %d 字节（%s 4096B 栈上上界）"
                % (len(pre), "<=" if len(pre) <= 4096 else ">")
            )
            print("前像（hex，分段）：")
            for i, (segname, chunk) in enumerate(
                (
                    ("token", seg(parts[0])),
                    ("refresh", seg(parts[1])),
                    ("expires", seg64(parts[2])),
                )
            ):
                print("  段 %d %-8s len=%4d  %s" % (i, segname, len(chunk), chunk.hex()))
            print("前像（hex，整串）= %s" % pre.hex())
            print("sha256            = %s" % d)
            print("Go 字面量：")
            print(go_literal(parts))
    # 结构性自证（Go 侧另有等价断言，这里是独立实现的对照）。
    pair = VECTORS[5][2]
    assert preimage(pair[0]) != preimage(pair[1]), "长度前缀失效：前像被折成同一串"
    assert digest(pair[0]) != digest(pair[1]), "长度前缀失效：摘要被折成同一枚"
    print("==" * 36)
    print("只变 expires：%s != %s" % (digest(VECTORS[0][2][0]), digest(VECTORS[3][2][0])))
    print("长度前缀对：  %s != %s" % (digest(pair[0]), digest(pair[1])))
    print(
        "负值单射：    %s / %s / %s"
        % (digest(("a", "b", -1)), digest(("a", "b", 0)), digest(("a", "b", 1)))
    )


if __name__ == "__main__":
    main()
