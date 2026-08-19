package event

import "context"

// Handler 消费一条 RuntimeEvent。处理器必须可并发调用。
type Handler func(context.Context, RuntimeEvent)

// Bus 把归一化事件同步分发给所有 Handler。
type Bus struct {
	handlers []Handler
}

// NewBus 复制 handlers；忽略 nil。
func NewBus(handlers ...Handler) *Bus {
	out := make([]Handler, 0, len(handlers))
	for _, handler := range handlers {
		if handler != nil {
			out = append(out, handler)
		}
	}
	return &Bus{handlers: out}
}

// Publish 按注册顺序调用 Handler。nil Bus 是空操作。
// ponytail: 同步分发；实时链路若要异步，自己在 Handler 里排队。
func (b *Bus) Publish(ctx context.Context, ev RuntimeEvent) {
	if b == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for _, handler := range b.handlers {
		handler(ctx, ev)
	}
}
