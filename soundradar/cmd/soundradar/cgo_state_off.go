//go:build !cgo

package main

// cgoState 记录本次构建是否启用了 cgo，由 version 子命令打印出来，
// 方便确认产物确实是"纯 Go、无 cgo"的那一份。
const cgoState = "disabled (pure Go)"
