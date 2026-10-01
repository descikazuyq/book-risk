// Package book 在 Ready 基线之上提供由调用方驱动的本地模拟交易：
// 行情按连续序号推进，买卖单先占用资金/持仓，成交由调用方逐笔提交，
// 并支持每个合约的最大持仓限额与完整的事件记录。
//
// 调用方还可用递增的正整数日号开启交易日并设置日内亏损上限（StartTradingDay、
// SetLossLimit、RiskStatus）：当日内亏损（基准净值减当前净值，现金加持仓市值口径）
// 达到或超过上限时，引擎拒绝一切新买单并按编号从大到小撤销全部有效买单，限制持续到
// 下一交易日；未开日时不启用该保护，交易行为与基线完全一致。
//
// 所有金额、价格、数量均以整数最小单位表示；Engine 的方法会自行串行化，
// 但业务流程仍由调用方驱动（不会自动撮合）。
package book

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
)

// ErrInt64Overflow 表示风险保护启用期间，净值或亏损计算超出 int64 范围。
// 返回该错误的开日、调限额、报价、成交调用整体不生效：除拒绝记录外不改变任何业务状态。
var ErrInt64Overflow = errors.New("净值或亏损超出 int64 范围")

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

// RiskTrigger 区分日内亏损保护首次触线的引发来源。
type RiskTrigger int

const (
	// RiskTriggerStartDay 由开始交易日（含零上限）引发。
	RiskTriggerStartDay RiskTrigger = iota + 1
	// RiskTriggerAdjustLimit 由下调亏损上限（含任何使当前亏损达限的调整）引发。
	RiskTriggerAdjustLimit
	// RiskTriggerQuote 由报价生效引发。
	RiskTriggerQuote
	// RiskTriggerTrade 由成交入账引发。
	RiskTriggerTrade
)

// String 返回触发来源的中文描述。
func (t RiskTrigger) String() string {
	switch t {
	case RiskTriggerStartDay:
		return "开日"
	case RiskTriggerAdjustLimit:
		return "调限额"
	case RiskTriggerQuote:
		return "报价"
	case RiskTriggerTrade:
		return "成交"
	default:
		return "未知触发"
	}
}

// RiskValuation 是一次净值/亏损试算结果。亏损盈利时记零。
type RiskValuation struct {
	Equity int64 // 当前净值：现金 + 各合约持仓按最新已生效报价计算的市值
	Loss   int64 // 日内亏损：基准净值 - 当前净值（小于 0 时记 0）
}

// RiskStatus 是日内亏损保护的查询结果。Open 为 false 表示尚未开始交易日，
// 其余字段在未开日时均为零值。
type RiskStatus struct {
	Open       bool // 是否已开始交易日
	Day        int64
	Baseline   int64 // 当日基准净值
	Equity     int64 // 当前净值
	Loss       int64 // 当前日内亏损（盈利时为 0）
	LossLimit  int64 // 当前亏损上限
	Restricted bool  // 是否已限制增险
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
	// RecordCanceled 订单撤销（含限额调整触发的撤销）。
	RecordCanceled
	// RecordRiskTriggered 日内亏损保护首次触线：一个交易日只产生一条。
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

	// 日内亏损保护快照（开日后的拒绝、撤销、成交与触线记录携带）。
	RiskDay      int64
	RiskBaseline int64
	RiskEquity   int64
	RiskLoss     int64
	RiskLimit    int64
	RiskRestrict bool

	// 触线记录专有：触发来源与关联编号（报价序号或成交编号）。
	RiskTrigger   RiskTrigger
	RiskRefSeq    int64 // 报价引发时为报价序号；成交引发时为成交编号
	RiskRefMoment int64 // 报价引发时为报价时刻

	// 触线时计算所用的各持仓合约报价快照（仅 RecordRiskTriggered 携带）。
	RiskQuoteRefs []RiskQuoteRef
}

// RiskQuoteRef 是触线净值计算所用的某个持仓合约最新报价定位。
type RiskQuoteRef struct {
	Symbol string
	Seq    int64
	Moment int64
	Price  int64
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

	// 日内亏损保护状态；riskOpen 为 false 时其余字段无意义，交易行为与基线一致。
	riskOpen       bool
	riskDay        int64
	riskBaseline   int64 // 当日基准净值
	riskLossLimit  int64 // 当前亏损上限
	riskRestricted bool  // 触线后持续到下一交易日

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
	for i := range out {
		if out[i].RiskQuoteRefs != nil {
			refs := make([]RiskQuoteRef, len(out[i].RiskQuoteRefs))
			copy(refs, out[i].RiskQuoteRefs)
			out[i].RiskQuoteRefs = refs
		}
	}
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

	// q.Seq == latest+1：本次将生效的报价链（含连续的等待报价），先收集再处理。
	chain := []Quote{q}
	for {
		next, ok := st.pending[chain[len(chain)-1].Seq+1]
		if !ok {
			break
		}
		chain = append(chain, next)
	}

	if e.riskOpen {
		// 先逐条试算：任何一步使净值或亏损超出 int64，整批报价不生效。
		for _, cur := range chain {
			if _, _, ok := e.valuationHypoLocked(e.cash, symbol, st.position, false, cur, true); !ok {
				e.appendRecord(Record{
					Kind:   RecordRejected,
					Symbol: symbol,
					Reason: fmt.Sprintf("报价序号 %d 生效将使净值或亏损超出 int64 范围，整批拒绝", cur.Seq),
				}, st)
				return 0, fmt.Errorf("报价序号 %d: %w", cur.Seq, ErrInt64Overflow)
			}
		}
	}

	// 逐条生效；风险开启时每条真正生效的报价都重新计算风险。
	// 即使中间价格触线、最后价格恢复，限制也已保留，不会解除。
	applied := 0
	for _, cur := range chain {
		st.applied[cur.Seq] = cur
		delete(st.pending, cur.Seq)
		st.latest = cur
		st.hasQuote = true
		applied++

		if e.riskOpen && !e.riskRestricted {
			if val, refs, ok := e.valuationLocked(); ok && val.Loss >= e.riskLossLimit {
				e.triggerRiskLocked(RiskTriggerQuote, cur.Seq, cur.Moment, val, refs)
			}
		}
	}
	return applied, nil
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

	if reason == "" && side == Buy && e.riskOpen && e.riskRestricted {
		reason = fmt.Sprintf("交易日 %d 日内亏损保护已触发（亏损上限 %d），拒绝所有新买单",
			e.riskDay, e.riskLossLimit)
	}
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

	// 启用风险保护时：先在假设成交后的状态上试算净值。越界则整笔拒绝，
	// 除拒绝记录外不改变任何业务状态（成交编号也不被占用）。
	if e.riskOpen {
		var hypoCash, hypoPos int64
		arithOK := true
		if o.Side == Buy {
			hypoCash, arithOK = subInt64(e.cash, amount) // 成交金额不超过买单占用，余额不会为负
			if arithOK {
				hypoPos, arithOK = addInt64(st.position, t.Qty)
			}
		} else {
			hypoCash, arithOK = addInt64(e.cash, amount)
			if arithOK {
				hypoPos, arithOK = subInt64(st.position, t.Qty) // 可卖校验保证持仓足量
			}
		}
		if arithOK {
			if _, _, ok := e.valuationHypoLocked(hypoCash, o.Symbol, hypoPos, true, Quote{}, false); !ok {
				arithOK = false
			}
		}
		if !arithOK {
			e.appendRecord(Record{
				Kind:       RecordRejected,
				Symbol:     o.Symbol,
				Side:       o.Side,
				OrderID:    o.ID,
				TradeID:    t.TradeID,
				Qty:        t.Qty,
				TradePrice: t.Price,
				Reason:     fmt.Sprintf("成交 %d 入账将使净值或亏损超出 int64 范围，整笔拒绝", t.TradeID),
			}, st)
			return FillResult{}, fmt.Errorf("成交 %d: %w", t.TradeID, ErrInt64Overflow)
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

	// 成交先完整记账，再处理由它触发的风险触线撤单。
	// 卖单成交也会重算风险（例如卖出亏损兑现），但限制一旦触发便不会因卖出盈利而解除。
	if e.riskOpen && !e.riskRestricted {
		if val, refs, ok := e.valuationLocked(); ok && val.Loss >= e.riskLossLimit {
			e.triggerRiskLocked(RiskTriggerTrade, t.TradeID, 0, val, refs)
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
// 交易日已开启时，还会固化当日风险快照（日号、基准、当前净值与亏损、上限、是否已限制），
// 触线造成的拒绝与撤销据此可解释；后续行情、跨日与限额调整都不会改写这些记录。
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
	if e.riskOpen {
		r.RiskDay = e.riskDay
		r.RiskBaseline = e.riskBaseline
		r.RiskLimit = e.riskLossLimit
		r.RiskRestrict = e.riskRestricted
		// 启用期间净值恒在 int64 范围内（越界变更此前已整体拒绝），溢出时仅留零值。
		if val, _, ok := e.valuationLocked(); ok {
			r.RiskEquity = val.Equity
			r.RiskLoss = val.Loss
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

// ---------------------------------------------------------------------------
// 日内亏损保护
//
// 净值口径：现金余额 + 各合约持仓按最新已生效报价计算的市值。
// 未成交买单占用的现金是现金余额的一部分（占用只是冻结，并未扣除），因此不重复扣减；
// 跨号等待的报价不是“已生效报价”，不参与估值；无有效报价的持仓同样不计市值。
// ---------------------------------------------------------------------------

// valuationLocked 按当前状态试算净值与日内亏损。
func (e *Engine) valuationLocked() (RiskValuation, []RiskQuoteRef, bool) {
	return e.valuationHypoLocked(e.cash, "", 0, false, Quote{}, false)
}

// valuationHypoLocked 在假设状态上试算：可覆盖现金、某合约持仓与某合约报价，
// 供成交后、报价逐条生效前在不改业务状态的前提下做溢出与触线检查。
func (e *Engine) valuationHypoLocked(cash int64, hypoSym string, hypoPos int64, usePos bool,
	hypoQuote Quote, useQuote bool) (RiskValuation, []RiskQuoteRef, bool) {

	equity := cash
	var refs []RiskQuoteRef

	syms := make([]string, 0, len(e.symbols))
	for s := range e.symbols {
		syms = append(syms, s)
	}
	sort.Strings(syms)

	for _, s := range syms {
		st := e.symbols[s]
		pos := st.position
		if usePos && s == hypoSym {
			pos = hypoPos
		}
		if pos == 0 {
			continue // 空仓合约不参与估值，也无需留存报价
		}
		hasQuote := st.hasQuote
		q := st.latest
		if useQuote && s == hypoSym {
			hasQuote = true
			q = hypoQuote
		}
		if !hasQuote {
			continue // 无已生效报价：该持仓暂不计市值
		}
		marketValue, ok := mulPosInt64(pos, q.Price)
		if !ok {
			return RiskValuation{}, nil, false
		}
		equity, ok = addInt64(equity, marketValue)
		if !ok {
			return RiskValuation{}, nil, false
		}
		refs = append(refs, RiskQuoteRef{Symbol: s, Seq: q.Seq, Moment: q.Moment, Price: q.Price})
	}

	loss, ok := subLoss(e.riskBaseline, equity)
	if !ok {
		return RiskValuation{}, nil, false
	}
	return RiskValuation{Equity: equity, Loss: loss}, refs, true
}

// subLoss 返回基准净值减当前净值；浮盈（差值为负）记零。差值溢出时 ok 为 false。
func subLoss(baseline, equity int64) (int64, bool) {
	diff, ok := subInt64(baseline, equity)
	if !ok {
		return 0, false
	}
	if diff < 0 {
		return 0, true
	}
	return diff, true
}

// addInt64 做带溢出检查的有符号加法。
func addInt64(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

// subInt64 做带溢出检查的有符号减法。
func subInt64(a, b int64) (int64, bool) {
	if b > 0 && a < math.MinInt64+b {
		return 0, false
	}
	if b < 0 && a > math.MaxInt64+b {
		return 0, false
	}
	return a - b, true
}

// mulPosInt64 返回 a*b；a 非负、b 为正，乘积溢出 int64 时 ok 为 false。
func mulPosInt64(a, b int64) (int64, bool) {
	if a < 0 || b <= 0 {
		return 0, false
	}
	if a == 0 {
		return 0, true
	}
	if a > math.MaxInt64/b {
		return 0, false
	}
	return a * b, true
}

// triggerRiskLocked 首次触线处理：记录触线事件并按订单编号从大到小撤销全部有效买单。
// 调用前须确认 riskOpen 且当前尚未限制；val/refs 为引发触线的那次试算结果
// （报价批量生效时可能对应中间价格，必须原样固化）。
func (e *Engine) triggerRiskLocked(trigger RiskTrigger, refID, refMoment int64,
	val RiskValuation, refs []RiskQuoteRef) {

	if e.riskRestricted {
		return
	}
	e.riskRestricted = true

	var refsCopy []RiskQuoteRef
	if refs != nil {
		refsCopy = make([]RiskQuoteRef, len(refs))
		copy(refsCopy, refs)
	}

	e.records = append(e.records, Record{
		Kind: RecordRiskTriggered,
		Reason: fmt.Sprintf("日内亏损触线（%s）：基准净值 %d，当前净值 %d，亏损 %d，亏损上限 %d",
			trigger, e.riskBaseline, val.Equity, val.Loss, e.riskLossLimit),
		RiskDay:       e.riskDay,
		RiskBaseline:  e.riskBaseline,
		RiskEquity:    val.Equity,
		RiskLoss:      val.Loss,
		RiskLimit:     e.riskLossLimit,
		RiskRestrict:  true,
		RiskTrigger:   trigger,
		RiskRefSeq:    refID,
		RiskRefMoment: refMoment,
		RiskQuoteRefs: refsCopy,
	})

	// 限制增险：撤销所有合约仍有未成交部分的买单，编号从大到小。
	var ids []int64
	for id, o := range e.orders {
		if o.Side == Buy && o.Status != StatusCanceled && o.Status != StatusFilled && o.Remaining() > 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	for _, id := range ids {
		e.cancelLocked(e.orders[id],
			fmt.Sprintf("日内亏损 %d 达到或超过上限 %d，风险保护撤销全部未成交买单", val.Loss, e.riskLossLimit))
	}
}

// StartTradingDay 以递增的正整数日号和非负亏损上限开始交易日。
// 跨日只由调用方明确发起，日号绝不从行情时刻推导。
//
// 开日时以现金余额加各合约持仓按最新已生效报价计算的市值作为当日基准净值；
// 零上限在开日时立即触线。更大日号会重新确定基准并清除上一日的限制，
// 但不恢复任何旧订单。同日号同上限的重复请求不产生变化；同日号不同上限、
// 倒退日号、非正日号、负上限均报错且不改状态。
func (e *Engine) StartTradingDay(day, lossLimit int64) error {
	if day <= 0 {
		return fmt.Errorf("交易日日号必须为正整数: %d", day)
	}
	if lossLimit < 0 {
		return fmt.Errorf("亏损上限不能为负: %d", lossLimit)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.riskOpen {
		switch {
		case day < e.riskDay:
			return fmt.Errorf("交易日日号不能倒退: 当前 %d，请求 %d", e.riskDay, day)
		case day == e.riskDay && lossLimit != e.riskLossLimit:
			return fmt.Errorf("交易日 %d 已开启（当前上限 %d），同日用不同上限 %d 重开被拒绝",
				day, e.riskLossLimit, lossLimit)
		case day == e.riskDay:
			return nil // 同日同上限：幂等无操作
		}
	}

	val, refs, ok := e.valuationLocked()
	if !ok {
		e.appendRecord(Record{
			Kind:   RecordRejected,
			Reason: fmt.Sprintf("开始交易日 %d 失败: %v", day, ErrInt64Overflow),
		}, nil)
		return fmt.Errorf("开始交易日 %d: %w", day, ErrInt64Overflow)
	}

	e.riskOpen = true
	e.riskDay = day
	e.riskBaseline = val.Equity
	e.riskLossLimit = lossLimit
	e.riskRestricted = false

	if lossLimit == 0 {
		// 零上限立即触线：当前亏损记 0，0 >= 0；保留基准计算所用的各持仓报价。
		e.triggerRiskLocked(RiskTriggerStartDay, 0, 0, RiskValuation{Equity: val.Equity, Loss: 0}, refs)
	}
	return nil
}

// SetLossLimit 在当前交易日调整亏损上限（非负）。尚未开始交易日时调用报错。
// 下调（或任何调整）使当前亏损达到或超过新上限时，立即触发与开日触线相同的限制处理；
// 已限制时上调上限也不能解除限制。同值调整不产生变化。
// 净值试算溢出时整体报错，除拒绝记录外不改变任何业务状态（含本次限额修改）。
func (e *Engine) SetLossLimit(lossLimit int64) error {
	if lossLimit < 0 {
		return fmt.Errorf("亏损上限不能为负: %d", lossLimit)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.riskOpen {
		return fmt.Errorf("尚未开始交易日，不能调整亏损上限")
	}
	if lossLimit == e.riskLossLimit {
		return nil
	}

	val, refs, ok := e.valuationLocked()
	if !ok {
		e.appendRecord(Record{
			Kind:   RecordRejected,
			Reason: fmt.Sprintf("调整亏损上限为 %d 失败: %v", lossLimit, ErrInt64Overflow),
		}, nil)
		return fmt.Errorf("调整亏损上限: %w", ErrInt64Overflow)
	}

	e.riskLossLimit = lossLimit
	if !e.riskRestricted && val.Loss >= lossLimit {
		e.triggerRiskLocked(RiskTriggerAdjustLimit, 0, 0, val, refs)
	}
	return nil
}

// RiskStatus 返回日内风险状态；未开始交易日时 Open 为 false，其余字段为零值，
// 交易行为与未启用保护时完全一致。
func (e *Engine) RiskStatus() RiskStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.riskOpen {
		return RiskStatus{Open: false}
	}
	st := RiskStatus{
		Open:       true,
		Day:        e.riskDay,
		Baseline:   e.riskBaseline,
		LossLimit:  e.riskLossLimit,
		Restricted: e.riskRestricted,
	}
	if val, _, ok := e.valuationLocked(); ok {
		st.Equity = val.Equity
		st.Loss = val.Loss
	}
	return st
}
