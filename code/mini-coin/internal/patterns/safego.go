package patterns

import "runtime/debug"

// PanicHandler 处理子协程 panic。生产环境应在这里记日志、打点并报警。
type PanicHandler func(recovered any, stack []byte)

// SafeGo 启动一个带 panic 保护的 goroutine。
//
// gRPC 的 recovery interceptor 只包住 handler 主调用链，包不住 handler
// 内部新开的 goroutine，所以子协程必须自己 recover。
func SafeGo(handler PanicHandler, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if handler != nil {
					handler(r, debug.Stack())
				}
			}
		}()
		fn()
	}()
}
