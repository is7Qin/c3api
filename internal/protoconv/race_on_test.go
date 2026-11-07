//go:build race

package protoconv

// raceEnabled -race 下 bench 的 ns/op 会放大数倍 ⇒ 只断言 allocs，不断言 ns。
const raceEnabled = true
