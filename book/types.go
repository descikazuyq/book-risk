package book

// Side 表示买卖方向。
type Side int

const (
	// Buy 买入：先买后卖，不做空。
	Buy Side = iota + 1
	// Sell 卖出：只允许卖出已有持仓。
	Sell
)

// OrderStatus 表示订单状态。
type OrderStatus int

const (
	// OrderRejected 订单被拒绝，不占用任何资金或持仓。
	OrderRejected OrderStatus = iota + 1
	// OrderPending 已接受、尚未成交。
	OrderPending
	// OrderPartialFilled 部分成交，仍有剩余量。
	OrderPartialFilled
	// OrderFilled 全部成交。
	OrderFilled
	// OrderCancelled 已撤销，仅释放未成交部分。
	OrderCancelled
)

// Quote 是某个合约的行情报价。
type Quote struct {
	Seq   int64 // 序号，从 1 开始，必须连续才能推进为最新报价
	Time  int64 // 模拟时刻
	Price int64 // 价格（最小单位，正整数）
}

// Order 是订单记录。接受、拒绝、成交和撤销都会留痕。
type Order struct {
	ID        int64
	Contract  string
	Side      Side
	Price     int64 // 限价：买单成交价不得高于它，卖单不得低于它
	Qty       int64 // 委托数量（正整数）
	FilledQty int64 // 已成交数量
	Status    OrderStatus

	// Quote 是接受/拒绝时的报价快照；无有效报价时为空（nil）。
	Quote *Quote
	// Limit 是接受/拒绝时的持仓限额；LimitSet 区分“未设置”与“限额为 0”。
	Limit    int64
	LimitSet bool

	// RejectReason 是拒绝原因，接受时为空。
	RejectReason string
	// CancelReason 是撤销原因，未撤销时为空。
	CancelReason string
	// CancelQuote / CancelLimit 是撤销时的快照。
	CancelQuote    *Quote
	CancelLimit    int64
	CancelLimitSet bool

	fills []*Fill
}

// RemainingQty 返回未成交剩余量。
func (o *Order) RemainingQty() int64 { return o.Qty - o.FilledQty }

// Fills 返回该订单的成交记录副本（按提交顺序）。
func (o *Order) Fills() []*Fill {
	out := make([]*Fill, len(o.fills))
	for i, f := range o.fills {
		c := *f
		if f.Quote != nil {
			q := *f.Quote
			c.Quote = &q
		}
		out[i] = &c
	}
	return out
}

// Fill 是成交记录。成交由调用方提交，每笔有唯一编号。
type Fill struct {
	ID      int64 // 成交编号（调用方提供，正整数，全局唯一）
	OrderID int64
	Qty     int64
	Price   int64

	// Quote / Limit 是成交记账时的行情与持仓限额快照。
	Quote    *Quote
	Limit    int64
	LimitSet bool
}
