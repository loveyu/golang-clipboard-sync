package main

import "time"

const clipboardEventDebounce = 200 * time.Millisecond

// notifyClipboardEvent 非阻塞地投递剪贴板变更信号。
// 通道满时说明已有事件等待处理，可以安全合并当前事件。
func notifyClipboardEvent(events chan<- struct{}) {
	select {
	case events <- struct{}{}:
	default:
	}
}

// consumeClipboardEvents 在单个 goroutine 中串行处理剪贴板事件。
// 连续事件会等待 quietPeriod 后合并处理，handle 永远不会并发执行。
func consumeClipboardEvents(
	events <-chan struct{},
	stop <-chan struct{},
	quietPeriod time.Duration,
	handle func(),
) {
	for {
		select {
		case <-events:
		case <-stop:
			return
		}

		timer := time.NewTimer(quietPeriod)
	debounce:
		for {
			select {
			case <-events:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(quietPeriod)
			case <-timer.C:
				break debounce
			case <-stop:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			}
		}

		handle()
	}
}
