# internal/identityfence — 候选身份指纹（I）与身份围栏哨兵的权威定义

> 根 `AGENTS.md` 覆盖项目级约定。P5-1 / R2 的 D5。

## 职责

`identityfence.go` 的 `CandidateFingerprint`：**本轴**（scheduler / rulepersist / 后续其它控制面组件）
的唯一一份定义。

## 为什么单独成包

`candidateFingerprint` 与三个哨兵原先住在 `internal/scheduler`，而 `internal/rulepersist`（规则判定
落库）要判它们。留在 scheduler 会让"谁是权威"变成"谁刚好 import 了谁"；搬进一个只依赖 `domain`
的叶子包之后，这条轴上的权威只有一处。

**第二套已消除（P3c I-A）**：`internal/sdkbridge/failure.go` 原先自持一套同名三哨兵（`sdkbridge:`
前缀）——那会让跨包 `errors.Is` 恒假，上游已分类成"缺身份"的判决落到别处会被误判成可重试。
现在只留**变量别名**（同一 error 值）。

## 不变量

- **叶子包**：只依赖 `internal/domain`（实测：`errors` + `fmt` + `domain`）。
- **哨兵必须是同一 error 值**：消费方用 `var ErrX = identityfence.ErrX` 别名，**不要**各自
  `errors.New` 新实例。
- 指纹字段的**声明顺序无关哈希顺序**（`hashFields` 实参顺序在函数内固定）。

## 验证

```bash
go test ./internal/identityfence/ -count=1
```
