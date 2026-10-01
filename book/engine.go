// Package book 在 Ready 基线之上提供由调用方驱动的本地模拟交易：
// 行情按连续序号推进，买卖单先占用资金/持仓，成交由调用方逐笔提交，
// 并支持每个合约的最大持仓限额与完整的事件记录。
//
// 所有金额、价格、数量均以整数最小单位表示；Engine 的方法会自行串行化，
// 但业务流程仍由调用方驱动（不会自动撮合）。
package book

import (
	"fmt"
	"sync"
)

// Side 表示买卖方向。
type Side int

const (
	// Buy 是买入方向（只允许先买后卖）。
	Buy Side = iota + 1
	// Sell 是卖出方向。
	Sell
)

// String 返回方向的中文描述。
func (s Side) String() string {
	switch s {
	case Buy:
		return "买入"
	case Sell:
		return "卖出"
	default:
		return "未知方向"
	}
}

// OrderStatus 表示订单生命周期状态。拒绝的委托不会生成订单，
// 拒绝只体现在事件记录中（见 RecordRejected）。
type OrderStatus int

const (
	// StatusPending 待成交：订单已接受，尚无成交。
	StatusPending OrderStatus = iota + 1
	// StatusPartial 部分成交。
	StatusPartial
	// StatusFilled 全部成交。
	StatusFilled
	// StatusCanceled 已撤销（可能已有部分成交）。
	StatusCanceled
)

// String 返回状态的中文描述。
func (s OrderStatus) String() string {
	switch s {
	case StatusPending:
		return "待成交"
	case StatusPartial:
		return "部分成交"
	case StatusFilled:
		return "全部成交"
	case StatusCanceled:
		return "已撤销"
	default:
		return "未知状态"
	}
}

// Quote 是某个合约的一条报价。序号从 1 开始且必须连续推进，
// Moment 为模拟时刻，Price 为正整数价格（最小单位）。
type Quote struct {
	Seq    int64
	Moment int64
	Price  int64
}

// Order 是一笔已被接受的委托。
type Order struct {
	ID     int64
	Symbol string
	Side   Side
	Limit  int64 // 限价（正整数）
	Qty    int64 // 原始委托数量
	Filled int64 // 已成交数量
	Status OrderStatus
	Reason string // 撤销原因（StatusCanceled 时）
}

// Remaining 返回订单尚未成交且仍有效的数量；已撤销订单的剩余量已取消，返回 0。
func (o Order) Remaining() int64 {
	if o.Status == StatusCanceled {
		return 0
	}
	return o.Qty - o.Filled
}

// Trade 是调用方提交的一笔成交。TradeID 由调用方给出且必须唯一，
// 同一编号重复提交时按幂等规则处理（见 Engine.Fill）。
type Trade struct {
	TradeID int64
	OrderID int64
	Symbol  string
	Side    Side
	Price   int64 // 成交价（正整数）
	Qty     int64 // 本次成交量（正整数）
}

// FillResult 是一笔成交被接受后返回的结果。
type FillResult struct {
	TradeID   int64
	OrderID   int64
	Price     int64
	Qty       int64 // 本次成交量
	Filled    int64 // 成交后订单累计成交量
	Remaining int64 // 成交后订单剩余量
	Status    OrderStatus
}

// RecordKind 区分事件记录类型。
type RecordKind int

const (
	// RecordAccepted 委托接受。
	RecordAccepted RecordKind = iota + 1
	// RecordRejected 委托或成交被拒绝。
	RecordRejected
	// RecordFilled 成交记账成功。
	RecordFilled
	// RecordCanceled 订单撤销（含限额调整或亏损触线触发的撤销）。
	RecordCanceled
	// RecordRiskTriggered 日内亏损触线限制增险。
	RecordRiskTriggered
)

// String 返回记录类型的中文描述。
func (k RecordKind) String() string {
	switch k {
	case RecordAccepted:
		return "接受"
	case RecordRejected:
		return "拒绝"
	case RecordFilled:
		return "成交"
	case RecordCanceled:
		return "撤销"
	case RecordRiskTriggered:
		return "风险触线"
	default:
		return "未知记录"
	}
}

// Record 是接受、拒绝、成交、撤销的不可变事件记录。
// 报价与持仓限额快照取自事件发生当时，后续变化不会改写记录。
type Record struct {
	Kind   RecordKind
	Symbol string
	Side   Side
	Reason string // 拒绝原因或撤销原因

	OrderID int64
	TradeID int64

	Limit     int64 // 委托限价（接受/拒绝委托时）
	Qty       int64 // 委托数量，或本次成交量
	Filled    int64 // 事件发生后订单累计成交量
	Remaining int64 // 事件发生后订单剩余量（撤销时为被取消的剩余量）

	TradePrice int64 // 成交价（成交记录）

	// 事件发生时可用的报价快照；QuoteValid 为 false 表示当时无有效报价。
	QuoteValid  bool
	QuoteSeq    int64
	QuoteMoment int64
	QuotePrice  int64

	// 事件发生时该合约的最大持仓量快照。
	MaxPositionValid bool
	MaxPosition      int64

	// 日内风险字段：风险触线记录及触线造成的拒绝/撤销记录填写，
	// 用于保留当日亏损与上限，解释触线原因。
	RiskDay      int64
	RiskBaseline int64
	RiskNetValue int64
	RiskLoss     int64
	RiskLimit    int64

	// 触线引发方式与关联引用（仅风险触线记录填写）。
	RiskTrigger  RiskTriggerKind
	RiskQuoteSeq int64 // 引发触线的报价序号
	RiskTradeID  int64 // 引发触线的成交编号

	// 触线时各持仓合约的估值快照（仅风险触线记录填写）。
	Valuations []PositionValuation
}

// Engine 是单账户、多合约的本地模拟交易引擎。
type Engine struct {
	mu sync.Mutex

	cash         int64 // 现金余额
	reservedCash int64 // 所有未成交买单占用的现金（限价 × 剩余量）

	symbols     map[string]*symbolState
	orders      map[int64]*Order
	nextOrderID int64

	// 已接受成交的幂等表：编号 -> 原始内容与结果。
	fills map[int64]seenFill

	// 日内风险状态；nil 表示尚未开始交易日。
	risk *riskState

	records []Record
}

type symbolState struct {
	maxSet      bool
	maxPosition int64

	position     int64 // 当前持仓
	reservedSell int64 // 未成交卖单占用的持仓
	reservedBuy  int64 // 未成交买单剩余量之和（用于持仓限额检查）

	hasQuote bool
	latest   Quote // 最新已生效报价

	applied map[int64]Quote // 已生效报价（按序号留存，用于重复内容比对）
	pending map[int64]Quote // 跨号等待中的报价

	buyOrderIDs []int64 // 买单接受顺序，限额下调时从末尾开始取消
}

type seenFill struct {
	orderID int64
	symbol  string
	side    Side
	price   int64
	qty     int64
	result  FillResult
}

// NewEngine 以非负初始现金创建引擎。
func NewEngine(cash int64) (*Engine, error) {
	if cash < 0 {
		return nil, fmt.Errorf("初始现金不能为负: %d", cash)
	}
	return &Engine{
		cash:    cash,
		symbols: make(map[string]*symbolState),
		orders:  make(map[int64]*Order),
		fills:   make(map[int64]seenFill),
	}, nil
}

func (e *Engine) state(symbol string) *symbolState {
	st := e.symbols[symbol]
	if st == nil {
		st = &symbolState{
			applied: make(map[int64]Quote),
			pending: make(map[int64]Quote),
		}
		e.symbols[symbol] = st
	}
	return st
}

// Cash 返回现金余额。
func (e *Engine) Cash() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cash
}

// ReservedCash 返回未成交买单占用的现金。
func (e *Engine) ReservedCash() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reservedCash
}

// AvailableCash 返回可用现金（余额减去占用现金）。
func (e *Engine) AvailableCash() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cash - e.reservedCash
}

// Position 返回合约持仓数量。
func (e *Engine) Position(symbol string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.symbols[symbol]; st != nil {
		return st.position
	}
	return 0
}

// Sellable 返回合约可卖数量（持仓减去未成交卖单占用）。
func (e *Engine) Sellable(symbol string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.symbols[symbol]; st != nil {
		return st.position - st.reservedSell
	}
	return 0
}

// MaxPosition 返回合约最大持仓量；尚未设置时 ok 为 false。
func (e *Engine) MaxPosition(symbol string) (max int64, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.symbols[symbol]; st != nil && st.maxSet {
		return st.maxPosition, true
	}
	return 0, false
}

// CurrentQuote 返回合约最新已生效报价；尚无有效报价时 ok 为 false。
func (e *Engine) CurrentQuote(symbol string) (Quote, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.symbols[symbol]; st != nil && st.hasQuote {
		return st.latest, true
	}
	return Quote{}, false
}

// HasGap 报告合约报价是否存在缺口（已有跨号报价等待补齐）。
func (e *Engine) HasGap(symbol string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.symbols[symbol]; st != nil {
		return len(st.pending) > 0
	}
	return false
}

// Order 返回订单快照；订单不存在（含被拒绝的委托）时 ok 为 false。
func (e *Engine) Order(id int64) (Order, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	o := e.orders[id]
	if o == nil {
		return Order{}, false
	}
	return *o, true
}

// Orders 返回全部已接受订单的快照，按订单编号排列。
func (e *Engine) Orders() []Order {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Order, 0, len(e.orders))
	for id := int64(1); id <= e.nextOrderID; id++ {
		if o := e.orders[id]; o != nil {
			out = append(out, *o)
		}
	}
	return out
}

// Records 返回全部事件记录的副本，按发生先后排列。
func (e *Engine) Records() []Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Record, len(e.records))
	copy(out, e.records)
	return out
}

// SetMaxPosition 设置合约的非负最大持仓量。每个合约下单前必须先设置。
// 下调（或首次设置导致超限）时，立即从最后接受的买单开始取消其全部剩余量，
// 直到满足新限额；若持仓本身已超限，则撤掉全部未成交买单，卖单不受影响。
// 返回本次被撤销的订单编号列表（按撤销顺序）。
func (e *Engine) SetMaxPosition(symbol string, max int64) ([]int64, error) {
	if symbol == "" {
		return nil, fmt.Errorf("合约代码不能为空")
	}
	if max < 0 {
		return nil, fmt.Errorf("最大持仓量不能为负: %d", max)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	st := e.state(symbol)
	st.maxSet = true
	st.maxPosition = max

	var canceled []int64

	if st.position > max {
		// 持仓本身已超限：撤掉全部未成交买单，不自动平仓。
		for i := len(st.buyOrderIDs) - 1; i >= 0 && st.reservedBuy > 0; i-- {
			o := e.orders[st.buyOrderIDs[i]]
			if o == nil || o.Status == StatusFilled || o.Status == StatusCanceled {
				continue
			}
			if o.Remaining() > 0 {
				e.cancelLocked(o, fmt.Sprintf("持仓 %d 已超过新限额 %d，撤销剩余买单", st.position, max))
				canceled = append(canceled, o.ID)
			}
		}
		return canceled, nil
	}

	// 从最后接受的买单开始撤销，直到 持仓 + 买单剩余量 <= 新限额。
	for st.position+st.reservedBuy > max {
		var target *Order
		for i := len(st.buyOrderIDs) - 1; i >= 0; i-- {
			o := e.orders[st.buyOrderIDs[i]]
			if o == nil || o.Status == StatusFilled || o.Status == StatusCanceled {
				continue
			}
			if o.Remaining() > 0 {
				target = o
				break
			}
		}
		if target == nil {
			break // 理论上不会发生：reservedBuy 与剩余买单不一致时兜底
		}
		e.cancelLocked(target, fmt.Sprintf("持仓限额下调至 %d，撤销最后接受的剩余买单", max))
		canceled = append(canceled, target.ID)
	}
	return canceled, nil
}

// UpdateQuote 提交合约报价。只有连续序号才能推进最新报价：
// 跨号报价进入等待，缺口补齐后按序号依次生效，返回本次新生效的报价条数；
// 重复序号且内容（时刻、价格）一致时不产生新事件（返回 0）；
// 重复序号但内容不同时报错并保留原值（返回 0 和错误）。
func (e *Engine) UpdateQuote(symbol string, q Quote) (int, error) {
	if symbol == "" {
		return 0, fmt.Errorf("合约代码不能为空")
	}
	if q.Seq <= 0 {
		return 0, fmt.Errorf("报价序号必须从 1 开始: %d", q.Seq)
	}
	if q.Price <= 0 {
		return 0, fmt.Errorf("报价价格必须为正: %d", q.Price)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	st := e.state(symbol)

	if old, ok := st.applied[q.Seq]; ok {
		if old.Moment != q.Moment || old.Price != q.Price {
			return 0, fmt.Errorf("报价序号 %d 内容冲突: 已有 (时刻=%d, 价格=%d)，新值 (时刻=%d, 价格=%d)",
				q.Seq, old.Moment, old.Price, q.Moment, q.Price)
		}
		return 0, nil // 重复序号且内容一致，无新事件
	}
	if old, ok := st.pending[q.Seq]; ok {
		if old.Moment != q.Moment || old.Price != q.Price {
			return 0, fmt.Errorf("等待中的报价序号 %d 内容冲突: 已有 (时刻=%d, 价格=%d)，新值 (时刻=%d, 价格=%d)",
				q.Seq, old.Moment, old.Price, q.Moment, q.Price)
		}
		return 0, nil // 已在等待，重复提交不改变任何状态
	}

	if q.Seq > st.latest.Seq+1 {
		st.pending[q.Seq] = q // 跨号：先等待
		return 0, nil
	}

	// 整理本次生效的报价序列：q 本身 + 补齐缺口后依次生效的跨号报价。
	steps := make([]Quote, 0, 1)
	cur := q
	for {
		steps = append(steps, cur)
		next, ok := st.pending[cur.Seq+1]
		if !ok {
			break
		}
		cur = next
	}

	// 日内风险：逐条试算估值，找到首次触线位置；任一报价导致净值超出
	// int64 范围则整批报错、不改变任何业务状态（报价不生效、不触线）。
	if e.risk != nil {
		baseline := e.risk.baseline
		savedLatest := st.latest
		savedHasQuote := st.hasQuote
		firstTrigger := -1
		overflowAt := int64(0)
		for i, qq := range steps {
			st.latest = qq // 仅用于试算估值，循环结束后恢复
			st.hasQuote = true
			net, ok := e.valuationLocked()
			if !ok {
				overflowAt = qq.Seq
				break
			}
			loss := lossLocked(baseline, net)
			if firstTrigger < 0 && !e.risk.restricted && loss >= e.risk.limit {
				firstTrigger = i
			}
		}
		st.latest = savedLatest
		st.hasQuote = savedHasQuote
		if overflowAt > 0 {
			return 0, fmt.Errorf("报价 %s 序号 %d 导致净值超出整数范围，报价未生效", symbol, overflowAt)
		}

		for _, qq := range steps {
			st.applied[qq.Seq] = qq
			delete(st.pending, qq.Seq)
			st.latest = qq
			st.hasQuote = true
		}
		if firstTrigger >= 0 {
			tq := steps[firstTrigger]
			// 触线记录按触线报价估值，触线后最新报价仍为最后生效的报价。
			last := st.latest
			st.latest = tq
			e.triggerRiskLocked(RiskTriggerQuote, tq.Seq, 0)
			st.latest = last
		}
		return len(steps), nil
	}

	for _, qq := range steps {
		st.applied[qq.Seq] = qq
		delete(st.pending, qq.Seq)
		st.latest = qq
		st.hasQuote = true
	}
	return len(steps), nil
}

// Buy 提交买单：按限价 × 数量占用现金，并计入最大持仓量占用。
func (e *Engine) Buy(symbol string, qty, limit int64) (int64, error) {
	return e.placeOrder(symbol, Buy, qty, limit)
}

// Sell 提交卖单：占用可卖持仓。
func (e *Engine) Sell(symbol string, qty, limit int64) (int64, error) {
	return e.placeOrder(symbol, Sell, qty, limit)
}

func (e *Engine) placeOrder(symbol string, side Side, qty, limit int64) (int64, error) {
	var reason string
	switch {
	case symbol == "":
		reason = "合约代码不能为空"
	case side != Buy && side != Sell:
		reason = fmt.Sprintf("未知委托方向: %d", side)
	case qty <= 0:
		reason = fmt.Sprintf("委托数量必须为正: %d", qty)
	case limit <= 0:
		reason = fmt.Sprintf("委托限价必须为正: %d", limit)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	st := e.state(symbol)

	if reason == "" && !st.maxSet {
		reason = fmt.Sprintf("合约 %s 尚未设置最大持仓量", symbol)
	}
	if reason == "" && !st.hasQuote {
		reason = fmt.Sprintf("合约 %s 尚无有效报价，拒绝下单", symbol)
	}
	if reason == "" && side == Buy && len(st.pending) > 0 {
		reason = fmt.Sprintf("合约 %s 报价存在缺口（最新序号 %d，等待序号 %d），拒绝新买单",
			symbol, st.latest.Seq, st.latest.Seq+1)
	}
	if reason == "" && side == Buy && e.risk != nil && e.risk.restricted {
		_, loss, lim, _, _ := e.riskSnapshotLocked()
		reason = fmt.Sprintf("日内亏损已触线（当前亏损 %d ≥ 上限 %d），拒绝新买单", loss, lim)
	}
	if reason == "" {
		cost, ok := mulPositive(limit, qty)
		if !ok {
			reason = fmt.Sprintf("限价 %d × 数量 %d 超出整数范围", limit, qty)
		} else if side == Buy {
			if cost > e.cash-e.reservedCash {
				reason = fmt.Sprintf("现金不足: 需要 %d，可用 %d", cost, e.cash-e.reservedCash)
			} else if st.position+st.reservedBuy+qty > st.maxPosition {
				reason = fmt.Sprintf("超过最大持仓量 %d: 已持仓 %d + 买单剩余 %d + 本次 %d",
					st.maxPosition, st.position, st.reservedBuy, qty)
			}
		} else {
			sellable := st.position - st.reservedSell
			if qty > sellable {
				reason = fmt.Sprintf("可卖数量不足: 需要 %d，可卖 %d（持仓 %d，卖单占用 %d）",
					qty, sellable, st.position, st.reservedSell)
			}
		}
	}

	if reason != "" {
		e.appendReject(symbol, side, qty, limit, 0, reason)
		if e.risk != nil && e.risk.restricted {
			if _, loss, lim, _, ok := e.riskSnapshotLocked(); ok {
				rec := &e.records[len(e.records)-1]
				rec.RiskDay = e.risk.day
				rec.RiskLoss = loss
				rec.RiskLimit = lim
			}
		}
		return 0, fmt.Errorf("%s %s 委托被拒绝: %s", side, symbol, reason)
	}

	id := e.nextOrderID + 1
	e.nextOrderID = id
	o := &Order{
		ID:     id,
		Symbol: symbol,
		Side:   side,
		Limit:  limit,
		Qty:    qty,
		Status: StatusPending,
	}
	e.orders[id] = o

	if side == Buy {
		e.reservedCash += limit * qty
		st.reservedBuy += qty
		st.buyOrderIDs = append(st.buyOrderIDs, id)
	} else {
		st.reservedSell += qty
	}

	e.appendRecord(Record{
		Kind:      RecordAccepted,
		Symbol:    symbol,
		Side:      side,
		OrderID:   id,
		Limit:     limit,
		Qty:       qty,
		Filled:    0,
		Remaining: qty,
	}, st)
	return id, nil
}

// Fill 由调用方提交一笔成交，引擎不自动撮合。允许部分成交：
// 买入价不得高于限价，卖出价不得低于限价；成交量超过订单剩余量、
// 订单不存在或已撤销时整笔拒绝，资金与持仓不变。
//
// 幂等性：同一成交编号以相同内容再次提交时返回原结果且不新增事件，
// 即使订单此后已撤销也不会再次记账；同一编号对应不同内容时报错且不改状态。
// 被拒绝的成交不占用编号，修正内容后仍可用同一编号提交。
func (e *Engine) Fill(t Trade) (FillResult, error) {
	if t.TradeID <= 0 {
		return FillResult{}, fmt.Errorf("成交编号必须为正: %d", t.TradeID)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if prev, ok := e.fills[t.TradeID]; ok {
		if prev.orderID != t.OrderID || prev.symbol != t.Symbol || prev.side != t.Side ||
			prev.price != t.Price || prev.qty != t.Qty {
			reason := fmt.Sprintf("成交编号 %d 已用于不同内容的成交: 原有 (订单=%d, 合约=%s, 方向=%s, 价格=%d, 数量=%d)",
				t.TradeID, prev.orderID, prev.symbol, prev.side, prev.price, prev.qty)
			e.appendRecord(Record{
				Kind:       RecordRejected,
				Symbol:     t.Symbol,
				Side:       t.Side,
				OrderID:    t.OrderID,
				TradeID:    t.TradeID,
				Qty:        t.Qty,
				TradePrice: t.Price,
				Reason:     reason,
			}, e.symbols[t.Symbol])
			return FillResult{}, fmt.Errorf("%s", reason)
		}
		return prev.result, nil // 幂等重放：返回原结果，不新增事件、不再次记账
	}

	reason := e.validateFill(t)
	if reason != "" {
		st := e.symbols[t.Symbol]
		e.appendRecord(Record{
			Kind:       RecordRejected,
			Symbol:     t.Symbol,
			Side:       t.Side,
			OrderID:    t.OrderID,
			TradeID:    t.TradeID,
			Qty:        t.Qty,
			TradePrice: t.Price,
			Reason:     reason,
		}, st)
		return FillResult{}, fmt.Errorf("成交 %d 被拒绝: %s", t.TradeID, reason)
	}

	o := e.orders[t.OrderID]
	st := e.symbols[o.Symbol]
	amount, _ := mulPositive(t.Price, t.Qty) // 接受委托时已校验过可乘性，此处必然安全

	// 日内风险：先按成交入账后的状态试算净值；超出 int64 范围则整笔拒绝，
	// 除拒绝记录外不改变任何业务状态（不记账、不占用成交编号）。
	if e.risk != nil {
		if _, ok := e.valuationAfterLocked(e.cash, st, o, t.Qty, amount); !ok {
			reason := "成交后净值超出整数范围，整笔拒绝"
			e.appendRecord(Record{
				Kind:       RecordRejected,
				Symbol:     t.Symbol,
				Side:       t.Side,
				OrderID:    t.OrderID,
				TradeID:    t.TradeID,
				Qty:        t.Qty,
				TradePrice: t.Price,
				Reason:     reason,
			}, st)
			return FillResult{}, fmt.Errorf("成交 %d 被拒绝: %s", t.TradeID, reason)
		}
	}

	if o.Side == Buy {
		// 释放限价占用，按实际成交金额扣现金；价差立即释放。
		e.reservedCash -= o.Limit * t.Qty
		e.cash -= amount
		st.position += t.Qty
		st.reservedBuy -= t.Qty
	} else {
		e.cash += amount
		st.position -= t.Qty
		st.reservedSell -= t.Qty
	}
	o.Filled += t.Qty
	if o.Filled == o.Qty {
		o.Status = StatusFilled
	} else {
		o.Status = StatusPartial
	}

	res := FillResult{
		TradeID:   t.TradeID,
		OrderID:   o.ID,
		Price:     t.Price,
		Qty:       t.Qty,
		Filled:    o.Filled,
		Remaining: o.Remaining(),
		Status:    o.Status,
	}
	e.fills[t.TradeID] = seenFill{
		orderID: t.OrderID, symbol: t.Symbol, side: t.Side,
		price: t.Price, qty: t.Qty, result: res,
	}
	e.appendRecord(Record{
		Kind:       RecordFilled,
		Symbol:     o.Symbol,
		Side:       o.Side,
		OrderID:    o.ID,
		TradeID:    t.TradeID,
		Qty:        t.Qty,
		Filled:     o.Filled,
		Remaining:  o.Remaining(),
		TradePrice: t.Price,
	}, st)

	// 成交本身先完整记账，再处理由它触发的撤单。
	if e.risk != nil {
		if net, ok := e.valuationLocked(); ok {
			if loss := lossLocked(e.risk.baseline, net); loss >= e.risk.limit && !e.risk.restricted {
				e.triggerRiskLocked(RiskTriggerFill, 0, t.TradeID)
			}
		}
	}
	return res, nil
}

func (e *Engine) validateFill(t Trade) string {
	switch {
	case t.Symbol == "":
		return "合约代码不能为空"
	case t.Side != Buy && t.Side != Sell:
		return fmt.Sprintf("未知成交方向: %d", t.Side)
	case t.Qty <= 0:
		return fmt.Sprintf("成交数量必须为正: %d", t.Qty)
	case t.Price <= 0:
		return fmt.Sprintf("成交价格必须为正: %d", t.Price)
	}
	o := e.orders[t.OrderID]
	if o == nil {
		return fmt.Sprintf("订单 %d 不存在", t.OrderID)
	}
	if o.Symbol != t.Symbol {
		return fmt.Sprintf("成交合约 %s 与订单合约 %s 不一致", t.Symbol, o.Symbol)
	}
	if o.Side != t.Side {
		return fmt.Sprintf("成交方向 %s 与订单方向 %s 不一致", t.Side, o.Side)
	}
	if o.Status == StatusCanceled {
		return fmt.Sprintf("订单 %d 已撤销，不能成交", t.OrderID)
	}
	if t.Qty > o.Remaining() {
		return fmt.Sprintf("成交量 %d 超过订单剩余量 %d", t.Qty, o.Remaining())
	}
	if o.Side == Buy && t.Price > o.Limit {
		return fmt.Sprintf("买入成交价 %d 高于限价 %d", t.Price, o.Limit)
	}
	if o.Side == Sell && t.Price < o.Limit {
		return fmt.Sprintf("卖出成交价 %d 低于限价 %d", t.Price, o.Limit)
	}
	if _, ok := mulPositive(t.Price, t.Qty); !ok {
		return fmt.Sprintf("成交价 %d × 数量 %d 超出整数范围", t.Price, t.Qty)
	}
	return ""
}

// Cancel 撤销订单：只释放未成交部分占用的现金或持仓，已成交部分保留。
// 全部成交、已撤销或不存在的订单不能撤销。
func (e *Engine) Cancel(orderID int64) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	o := e.orders[orderID]
	switch {
	case o == nil:
		return fmt.Errorf("订单 %d 不存在", orderID)
	case o.Status == StatusFilled:
		return fmt.Errorf("订单 %d 已全部成交，不能撤销", orderID)
	case o.Status == StatusCanceled:
		return fmt.Errorf("订单 %d 已撤销", orderID)
	}

	e.cancelLocked(o, "调用方撤销")
	return nil
}

// cancelLocked 执行撤销并追加记录。调用时须持有锁且订单确有剩余量。
func (e *Engine) cancelLocked(o *Order, reason string) {
	remaining := o.Remaining()
	if remaining <= 0 {
		return
	}
	st := e.state(o.Symbol)
	if o.Side == Buy {
		e.reservedCash -= o.Limit * remaining // 与买单剩余量同步释放
		st.reservedBuy -= remaining
	} else {
		st.reservedSell -= remaining
	}
	o.Status = StatusCanceled
	o.Reason = reason

	e.appendRecord(Record{
		Kind:      RecordCanceled,
		Symbol:    o.Symbol,
		Side:      o.Side,
		OrderID:   o.ID,
		Limit:     o.Limit,
		Qty:       o.Qty,
		Filled:    o.Filled,
		Remaining: remaining,
		Reason:    reason,
	}, st)
}

func (e *Engine) appendReject(symbol string, side Side, qty, limit, orderID int64, reason string) {
	e.appendRecord(Record{
		Kind:    RecordRejected,
		Symbol:  symbol,
		Side:    side,
		Qty:     qty,
		Limit:   limit,
		OrderID: orderID,
		Reason:  reason,
	}, e.symbols[symbol])
}

// appendRecord 在事件记录上固化当时的报价与持仓限额快照。
// st 可能为 nil（例如空合约的非法输入），此时快照明确为空。
func (e *Engine) appendRecord(r Record, st *symbolState) {
	if st != nil {
		if st.hasQuote {
			r.QuoteValid = true
			r.QuoteSeq = st.latest.Seq
			r.QuoteMoment = st.latest.Moment
			r.QuotePrice = st.latest.Price
		}
		if st.maxSet {
			r.MaxPositionValid = true
			r.MaxPosition = st.maxPosition
		}
	}
	e.records = append(e.records, r)
}

// mulPositive 返回 a*b；a、b 必须为正，溢出时 ok 为 false。
func mulPositive(a, b int64) (int64, bool) {
	r := a * b
	if r/a != b {
		return 0, false
	}
	return r, true
}
