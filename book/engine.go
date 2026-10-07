// Package book 在 Ready 基线之上提供由调用方驱动的本地模拟交易：
// 行情按连续序号推进，买卖单先占用资金/持仓，成交由调用方逐笔提交，
// 并支持每个合约的最大持仓限额与完整的事件记录。
//
// 调用方还可用递增的正整数日号开启交易日并设置日内亏损上限（StartTradingDay、
// SetLossLimit、RiskStatus）：当日内亏损（基准净值减当前净值，现金加持仓市值口径）
// 达到或超过上限时，引擎拒绝一切新买单并按编号从大到小撤销全部有效买单，限制持续到
// 下一交易日；未开日时不启用该保护，交易行为与基线完全一致。
//
// 调用方也可显式设置账户总持仓金额上限（SetPositionAmountLimit、
// PositionAmountStatus）：以各合约持仓按最新已生效报价计算的市值，加上有效买单
// 剩余量按 max(限价, 最新报价) 计算的占用为合计，限制多个合约合计占用的额度。
// 该上限未设置时不改变任何基线行为，设置后跨交易日保留；与日内亏损保护不同，
// 报价回落、卖出或上调上限后即可恢复买入，不做整日锁定。
//
// 调用方还可用 Modify 按订单编号修改待成交或部分成交订单的委托总量与限价：
// 合约、方向、订单编号及已成交数量不变，旧占用（现金/可卖数量/持仓限额/金额
// 上限）由新占用替换，历史成交不重新计价；取消剩余量仍走 Cancel。缺口期间买单
// 只允许剩余量、现金与持仓金额占用均不增加的修改，被亏损保护撤销的订单不能
// 借修改恢复。每次真实修改追加一条 RecordModified，重复提交当前参数不新增记录。
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

// ErrInt64Overflow 表示风险保护或账户总持仓金额限额启用期间，净值、亏损或金额
// 合计计算超出 int64 范围；或卖出成交所得加回现金余额后超出 int64 范围（现金结算
// 检查始终生效，不依赖任何保护或限额是否开启）；或买单申请后
// “已持仓数量 + 该合约有效买单剩余量 + 本次申请数量”合计超出 int64 范围
// （合约持仓限额检查始终生效，三个数各自合法并不代表合计可表示）。
// 返回该错误的开日、调限额、设置金额上限、报价、下单或成交调用整体不生效：
// 除拒绝记录外不改变任何业务状态。
var ErrInt64Overflow = errors.New("净值、亏损或持仓金额超出 int64 范围")

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

// PositionAmountStatus 是账户总持仓金额上限的查询结果。
// Enabled 为 false 表示调用方尚未设置上限，此时其余字段均为零值，
// 交易行为与未设置时完全一致。该上限跨交易日保留，不随交易日重置。
type PositionAmountStatus struct {
	Enabled     bool  // 是否已设置上限
	Limit       int64 // 当前上限（非负）
	Holding     int64 // 已持仓金额：各合约数量 × 最新已生效报价之和
	BuyReserved int64 // 有效买单剩余量占用：Σ 剩余量 × max(限价, 最新报价)
	Total       int64 // Holding + BuyReserved
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
	// RecordModified 已接受订单被成功修改（Old* 字段保存修改前参数）。
	RecordModified
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
	case RecordModified:
		return "修改"
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

	Limit     int64 // 委托限价（接受/拒绝委托、修改及修改拒绝时；修改记录为新限价）
	Qty       int64 // 委托数量（修改记录为新总量），或本次成交量
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

	// 账户总持仓金额上限快照（上限设置后，与金额相关的拒绝、撤销携带）。
	AmtEnabled     bool
	AmtLimit       int64 // 判断时的上限
	AmtHolding     int64 // 判断时的已持仓金额
	AmtBuyReserved int64 // 判断时有效买单剩余量占用金额
	AmtTotal       int64 // 判断时两类金额合计
	AmtApplyTotal  int64 // 拒绝新买单时：申请加入后的合计；其余事件为零

	// 金额判断时参与计算的各合约报价定位（上限相关拒绝、撤销携带）。
	AmtQuoteRefs []RiskQuoteRef

	// 修改记录专有（RecordModified）：修改前的限价、总量与当时已成交量；
	// 修改后参数与成交量保存在 Limit/Qty/Filled 中。
	OldLimit  int64
	OldQty    int64
	OldFilled int64
}

// RiskQuoteRef 是一次持仓估值中真正参与计算的某个合约的最新报价定位
// （序号、时刻与价格）；日内亏损净值与账户总持仓金额合计共用同一估值规则，
// 故两类记录都用它固化当时的报价事实。
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

	// 账户总持仓金额上限；amtLimitSet 为 false 时不启用任何金额限制。
	// 与亏损保护不同，该设置跨交易日保留。
	amtLimitSet bool
	amtLimit    int64

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
		if out[i].AmtQuoteRefs != nil {
			refs := make([]RiskQuoteRef, len(out[i].AmtQuoteRefs))
			copy(refs, out[i].AmtQuoteRefs)
			out[i].AmtQuoteRefs = refs
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

	return e.enforcePositionLimitLocked(st, max), nil
}

// lastActiveBuyLocked 返回该合约按接受次序最后一张仍有未成交部分的买单；
// 其他合约的买单与该合约的卖单不在 st.buyOrderIDs 中，已全部成交或已撤销的
// 订单跳过。修改只改总量与限价、不改变接受顺序，故修改过的订单仍按其最初
// 接受的位置参与选择。没有可选订单时返回 nil。
func (e *Engine) lastActiveBuyLocked(st *symbolState) *Order {
	for i := len(st.buyOrderIDs) - 1; i >= 0; i-- {
		o := e.orders[st.buyOrderIDs[i]]
		if o == nil || o.Status == StatusFilled || o.Status == StatusCanceled {
			continue
		}
		if o.Remaining() > 0 {
			return o
		}
	}
	return nil
}

// enforcePositionLimitLocked 是设置新持仓限额后收敛超限的唯一撤单规则，
// “持仓本身已超过新限额”和“加上有效买单剩余量才超过新限额”两种情形共用同一套
// 有效买单判断与接受次序：只考虑该合约仍有未成交部分的买单，自最后接受的订单起
// 每次撤销其全部剩余量（取消数量按当前总量减去已成交量计，现金按当前限价释放）。
//   - 持仓本身已超过新限额时，撤掉该合约全部有效买单，但持仓继续保留、不自动卖出，
//     撤销原因沿用持仓超限文案；
//   - 否则在“已持仓数量 + 有效买单剩余量”不大于新限额后停止，恰好等于时保留
//     较早的订单，撤销原因沿用限额下调文案。
//
// 其他合约买单、该合约卖单、已全部成交或已撤销的订单均不会被选中；未被选中的
// 订单及其占用原样保留。返回按实际撤销顺序排列的订单编号，每张订单只撤销一次。
func (e *Engine) enforcePositionLimitLocked(st *symbolState, max int64) []int64 {
	holdingOver := st.position > max
	var canceled []int64
	for {
		if !holdingOver && st.position+st.reservedBuy <= max {
			return canceled // 普通超限已收敛；恰好用满额度时保留较早订单
		}
		target := e.lastActiveBuyLocked(st)
		if target == nil {
			return canceled // 无有效买单可撤（reservedBuy 与剩余买单不一致时兜底）
		}
		var reason string
		if holdingOver {
			reason = fmt.Sprintf("持仓 %d 已超过新限额 %d，撤销剩余买单", st.position, max)
		} else {
			reason = fmt.Sprintf("持仓限额下调至 %d，撤销最后接受的剩余买单", max)
		}
		e.cancelLocked(target, reason)
		canceled = append(canceled, target.ID)
	}
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

	// 先在影子状态上按“前一条处理后的状态”逐步预检整条链：任一步使净值/亏损
	// 或账户金额合计溢出 int64，整批不生效（不写报价、不撤单、不改占用，原先
	// 等待的报价保留），只追加一条拒绝记录。
	// simCanceled 模拟本批此前各条报价已触发的亏损保护/金额上限撤单。
	simRestricted := e.riskRestricted
	simCanceled := map[int64]bool{}
	if e.riskOpen || e.amtLimitSet {
		for _, cur := range chain {
			riskTrigger := false
			if e.riskOpen {
				val, _, ok := e.valuationHypoLocked(e.cash, symbol, st.position, false, cur, true)
				if !ok {
					e.appendRecord(Record{
						Kind:   RecordRejected,
						Symbol: symbol,
						Reason: fmt.Sprintf("报价序号 %d 生效将使净值或亏损超出 int64 范围，整批拒绝", cur.Seq),
					}, st)
					return 0, fmt.Errorf("报价序号 %d: %w", cur.Seq, ErrInt64Overflow)
				}
				riskTrigger = !simRestricted && val.Loss >= e.riskLossLimit
			}
			// 同条报价同时触发两项保护时，先按亏损规则模拟撤光全部有效买单。
			if riskTrigger {
				simRestricted = true
				for _, id := range e.activeBuyIDsShadowLocked(simCanceled) {
					simCanceled[id] = true
				}
			}
			if e.amtLimitSet {
				// 与实际撤单共用同一套选择规则：本条报价的撤单承接此前各条
				// 报价与亏损保护处理后的状态（simCanceled），选出的撤单再供
				// 链中后续报价继续判断。
				steps, ok := e.planAmountCancelLocked(symbol, cur, simCanceled)
				if !ok {
					e.appendRecord(Record{
						Kind:       RecordRejected,
						Symbol:     symbol,
						AmtEnabled: true,
						AmtLimit:   e.amtLimit,
						Reason:     fmt.Sprintf("报价序号 %d 生效将使持仓金额或买单占用合计超出 int64 范围，整批拒绝", cur.Seq),
					}, st)
					return 0, fmt.Errorf("报价序号 %d: %w", cur.Seq, ErrInt64Overflow)
				}
				for _, step := range steps {
					simCanceled[step.id] = true
				}
			}
		}
	}

	// 逐条生效；预检已保证无溢出，处理顺序与预检一致。
	// 风险开启时每条真正生效的报价都重新计算风险：即使中间价格触线、最后价格
	// 恢复，限制也已保留，不会解除。金额上限紧接其后按新报价收敛。
	applied := 0
	for _, cur := range chain {
		st.applied[cur.Seq] = cur
		delete(st.pending, cur.Seq)
		st.latest = cur
		st.hasQuote = true
		applied++

		// 同一条报价同时触发两项保护时，先按原有亏损规则处理并保留其撤单原因；
		// 亏损保护已撤光全部买单，金额收敛不会再产生撤单记录。
		if e.riskOpen && !e.riskRestricted {
			if val, refs, ok := e.valuationLocked(); ok && val.Loss >= e.riskLossLimit {
				e.triggerRiskLocked(RiskTriggerQuote, cur.Seq, cur.Moment, val, refs)
			}
		}
		if e.amtLimitSet {
			e.enforceAmountLimitLocked(fmt.Sprintf("合约 %s 报价序号 %d（时刻 %d，价格 %d）生效",
				symbol, cur.Seq, cur.Moment, cur.Price))
		}
	}
	return applied, nil
}

// Buy 提交买单：按限价 × 数量占用现金，并计入最大持仓量占用。
// 最大持仓量约束 已持仓数量 + 该合约其他有效买单剩余量 + 本次申请数量；
// 合计超出 int64 可表示范围时整笔拒绝（返回零订单编号与包装 ErrInt64Overflow
// 的错误，只追加一条拒绝记录，不消耗订单编号），不会因数值回绕接受超限委托。
func (e *Engine) Buy(symbol string, qty, limit int64) (int64, error) {
	return e.placeOrder(symbol, Buy, qty, limit)
}

// Sell 提交卖单：只占用可卖持仓（委托数量不得超过 持仓减去其他有效卖单占用）。
// 接受前要求合约已有有效报价与持仓限额设置、数量与限价均为正且可卖数量充足；
// 卖单在成交前不占用现金、不产生金额，因此不要求“限价 × 委托总量”可用 int64
// 表示——整笔委托金额溢出并不阻止接受，这与 Modify 把有效卖单改到相同参数的
// 口径一致。接受后订单保留提交的总量与限价并占用相应可卖数量，不预先增加现金、
// 扣减持仓或改变买单现金占用；现金与持仓只在每笔成交回报时按该笔数量与价格结算，
// 单笔成交金额溢出或卖出所得加回现金溢出时由 Fill 拒绝（见 Fill 与 ErrInt64Overflow）。
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
	if reason == "" && side == Buy {
		// 买单按整笔限价金额（限价 × 委托总量）占用现金，乘积必须可用 int64 表示。
		cost, ok := mulPositive(limit, qty)
		if !ok {
			reason = fmt.Sprintf("限价 %d × 数量 %d 超出整数范围", limit, qty)
		} else if cost > e.cash-e.reservedCash {
			reason = fmt.Sprintf("现金不足: 需要 %d，可用 %d", cost, e.cash-e.reservedCash)
		}
	}
	if reason == "" && side == Sell {
		// 卖单在成交前不占用现金、不产生任何金额，只按可卖持仓决定能否接受：
		// 整笔限价金额（限价 × 委托总量）即使无法用 int64 表示也不拒绝，
		// 与把有效卖单修改到相同参数的口径保持一致。现金与持仓只在每笔实际
		// 成交回报时按该笔数量与价格结算（见 Fill 的单笔金额与现金加回检查）。
		sellable := st.position - st.reservedSell
		if qty > sellable {
			reason = fmt.Sprintf("可卖数量不足: 需要 %d，可卖 %d（持仓 %d，卖单占用 %d）",
				qty, sellable, st.position, st.reservedSell)
		}
	}

	// 合约持仓限额：已持仓数量 + 该合约其他有效买单剩余量 + 本次申请数量。
	// 三个数本身合法并不保证合计仍可用 int64 表示：合计溢出时不能让数值回绕后
	// 绕过限额检查，否则会等到成交后才暴露负持仓。溢出时整笔拒绝并返回
	// ErrInt64Overflow；合计可表示但超过限额时仍按原有超限原因拒绝。
	// （卖单不占用持仓额度；卖单在真正成交前也不能提前抵减持仓。）
	// 数量合计、超限与溢出判断与 Modify 共用 checkPositionLimit，规则只维护一处；
	// 新买单此前没有本单占用，旧剩余量按 0 处理。
	positionOverflow := false
	if reason == "" && side == Buy {
		check := checkPositionLimit(st, 0, qty)
		if check.overflow {
			positionOverflow = true
			reason = fmt.Sprintf("持仓数量合计超出 int64 范围: 本次申请 %d + 已持仓 %d + 有效买单剩余 %d 超过最大持仓量 %d，买单整体拒绝",
				qty, st.position, st.reservedBuy, st.maxPosition)
		} else if check.over {
			reason = fmt.Sprintf("超过最大持仓量 %d: 已持仓 %d + 买单剩余 %d + 本次 %d",
				st.maxPosition, st.position, check.used, qty)
		}
	}

	// 账户总持仓金额上限：新买单加入后合计不超过上限才可接受（恰好等于允许）。
	// 原有现金、持仓限额与行情缺口规则均已通过后才检查此项。
	if reason == "" && side == Buy && e.amtLimitSet {
		t, ok := e.amountTotalsLocked()
		if !ok {
			r := fmt.Sprintf("账户总持仓金额计算超出 int64 范围，买单 %s %d 股限价 %d 整体拒绝",
				symbol, qty, limit)
			e.appendAmountRejectLocked(symbol, qty, limit, r, nil, 0, false)
			return 0, fmt.Errorf("%s %s 委托被拒绝: %w", side, symbol, ErrInt64Overflow)
		}
		// 与修改买单、账户金额汇总共用同一单价口径 buyReservedUnit。
		unit := buyReservedUnit(st.hasQuote, st.latest.Price, limit)
		need, ok := mulPosInt64(qty, unit)
		var apply int64
		if ok {
			apply, ok = addInt64(t.total, need)
		}
		if !ok {
			r := fmt.Sprintf("账户总持仓金额申请后合计超出 int64 范围，买单 %s %d 股限价 %d 整体拒绝",
				symbol, qty, limit)
			e.appendAmountRejectLocked(symbol, qty, limit, r, &t, 0, false)
			return 0, fmt.Errorf("%s %s 委托被拒绝: %w", side, symbol, ErrInt64Overflow)
		}
		if apply > e.amtLimit {
			r := fmt.Sprintf("超过账户总持仓金额上限 %d: 已持仓金额 %d + 有效买单剩余占用 %d + 本次按 max(限价,报价)=%d 计 %d，申请后合计 %d",
				e.amtLimit, t.holding, t.reserved, unit, need, apply)
			e.appendAmountRejectLocked(symbol, qty, limit, r, &t, apply, true)
			return 0, fmt.Errorf("%s %s 委托被拒绝: %s", side, symbol, r)
		}
	}

	if reason != "" {
		e.appendReject(symbol, side, qty, limit, 0, reason)
		if positionOverflow {
			return 0, fmt.Errorf("%s %s 委托被拒绝: %w", side, symbol, ErrInt64Overflow)
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
//
// 卖出成交的现金结算始终检查可表示性：卖出所得加回现金余额后超出 int64 范围时
// 整笔拒绝（返回包装 ErrInt64Overflow 的错误），不依赖日内亏损保护是否开启，
// 也不以设置账户总持仓金额上限为前提；现金增加后恰好等于 int64 最大值允许。
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
			e.appendFillRejectLocked(t, reason)
			return FillResult{}, fmt.Errorf("%s", reason)
		}
		return prev.result, nil // 幂等重放：返回原结果，不新增事件、不再次记账
	}

	reason := e.validateFill(t)
	if reason != "" {
		e.appendFillRejectLocked(t, reason)
		return FillResult{}, fmt.Errorf("成交 %d 被拒绝: %s", t.TradeID, reason)
	}

	o := e.orders[t.OrderID]
	st := e.symbols[o.Symbol]

	// 同一笔成交的现金与持仓结算只在此试算一次：成交前的现金/净值检查与
	// 成功后的实际入账共用同一份结算结果，不会检查一套、记账另一套。
	// 卖出所得加回现金越界时 ok 为 false：该检查始终生效，不依赖日内亏损
	// 保护是否开启，也不以设置账户总持仓金额上限为前提。越界时整笔拒绝，
	// 除一条拒绝记录外不改变任何业务状态（成交编号不被占用，持仓与卖单
	// 占用保留）。
	settle, ok := e.settleFillLocked(o, st, t)
	if !ok {
		e.appendFillRejectLocked(t, fmt.Sprintf("卖出所得 %d 加入现金余额 %d 后超出 int64 范围，整笔拒绝",
			settle.amount, e.cash))
		return FillResult{}, fmt.Errorf("成交 %d: %w", t.TradeID, ErrInt64Overflow)
	}

	// 启用风险保护时：先在成交后的假设状态上试算净值与亏损——现金与持仓取
	// 同一份结算结果，报价仍用最新已生效报价（等待补齐的报价不参与）。
	// 越界则整笔拒绝，除拒绝记录外不改变任何业务状态（成交编号也不被占用）。
	if e.riskOpen {
		if _, _, ok := e.valuationHypoLocked(settle.cash, o.Symbol, settle.pos, true, Quote{}, false); !ok {
			e.appendFillRejectLocked(t, fmt.Sprintf("成交 %d 入账将使净值或亏损超出 int64 范围，整笔拒绝", t.TradeID))
			return FillResult{}, fmt.Errorf("成交 %d: %w", t.TradeID, ErrInt64Overflow)
		}
	}

	// 实际入账：现金与持仓直接采用成交前检查用过的同一份结算结果。
	// 买单按本次成交部分释放限价占用（价差立即释放），未成交部分继续占用；
	// 卖单只减少本次成交部分的可卖占用，不提前处理未成交部分。
	if o.Side == Buy {
		e.reservedCash -= o.Limit * t.Qty
		st.reservedBuy -= t.Qty
	} else {
		st.reservedSell -= t.Qty
	}
	e.cash = settle.cash
	st.position = settle.pos
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

// appendFillRejectLocked 追加一条成交拒绝记录，是 Fill 全部拒绝路径（成交编号
// 内容冲突、普通校验失败、卖出所得加回现金越界、入账后净值或亏损越界）共用的
// 唯一记录入口，保证回报内容与当时快照的保留规则只在此维护：
//
//   - 记录原样保存本次提交的订单编号、成交编号、合约、方向、成交价与数量；
//     合约或方向填错、或另一订单借用已入账编号时，绝不用原订单或第一次成交的
//     内容覆盖这次回报；
//   - 报价快照取本次回报合约当时的最新已生效报价，持仓限额取当时设置；尚未
//     具备的快照保持无效，等待补齐的报价不参与；
//   - 已开启交易日时同时固化提交前的日号、基准净值、当前净值、亏损、上限与
//     限制状态，未开日保持零值（均由 appendRecord 统一完成）。
//
// 每次拒绝只增加这一条事件：现金、持仓、买卖单占用、累计成交量与订单状态维持
// 提交前值，既有记录不被改写，未入账的成交编号也不被占用。调用时须持有引擎锁。
func (e *Engine) appendFillRejectLocked(t Trade, reason string) {
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
}

// ---------------------------------------------------------------------------
// 成交结算
//
// 同一笔成交的现金与持仓结算规则只在 settleFillLocked 维护，成交前的现金/
// 净值检查与成功后的实际入账共用同一份 fillSettlement，保证两个时点口径一致：
//
//   - 买入：现金只扣本次成交金额（成交价 × 成交量），持仓增加本次成交量；
//     限价 × 成交量的现金占用按本次成交部分释放，未成交部分继续占用；
//   - 卖出：本次所得加回现金，持仓与卖单占用减少本次成交量，不提前处理
//     未成交部分。
//
// 订单累计成交量与状态随本次成交更新，其他订单的占用保持不变。
// ---------------------------------------------------------------------------

// fillSettlement 是一笔成交按唯一结算规则试算出的入账结果。
type fillSettlement struct {
	amount int64 // 本次成交金额：成交价 × 成交量
	cash   int64 // 入账后的现金余额
	pos    int64 // 入账后该合约的持仓
}

// settleFillLocked 按上述规则计算一笔成交的结算结果。成交金额的可乘性由
// validateFill 保证；买单现金扣减（成交金额不超过该单现金占用）、买单持仓
// 增加（持仓限额约束）与卖单持仓扣减（可卖校验）都不会越界。卖出所得加回
// 现金余额可能越界，此时 ok 为 false 且 amount 已填好供拒绝原因使用；
// 该检查始终生效，不依赖任何保护或限额是否开启，余额恰好达到 int64 最大值
// 允许。调用时须持有引擎锁。
func (e *Engine) settleFillLocked(o *Order, st *symbolState, t Trade) (fillSettlement, bool) {
	amount, _ := mulPositive(t.Price, t.Qty) // validateFill 已校验单笔成交金额可乘
	s := fillSettlement{amount: amount}
	if o.Side == Buy {
		s.cash = e.cash - amount
		s.pos = st.position + t.Qty
	} else {
		cash, ok := addInt64(e.cash, amount)
		if !ok {
			return s, false
		}
		s.cash = cash
		s.pos = st.position - t.Qty
	}
	return s, true
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
	e.cancelLockedImpl(o, reason, nil)
}

// cancelLockedImpl 执行撤销；amt 非 nil 时（金额上限引发的撤销）在记录上固化
// 判断所用的两类金额、合计、上限与参与计算的各合约报价定位。
func (e *Engine) cancelLockedImpl(o *Order, reason string, amt *amtTotals) {
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

	r := Record{
		Kind:      RecordCanceled,
		Symbol:    o.Symbol,
		Side:      o.Side,
		OrderID:   o.ID,
		Limit:     o.Limit,
		Qty:       o.Qty,
		Filled:    o.Filled,
		Remaining: remaining,
		Reason:    reason,
	}
	if amt != nil {
		r.AmtEnabled = true
		r.AmtLimit = e.amtLimit
		r.AmtHolding = amt.holding
		r.AmtBuyReserved = amt.reserved
		r.AmtTotal = amt.total
		if amt.refs != nil {
			r.AmtQuoteRefs = append([]RiskQuoteRef(nil), amt.refs...)
		}
	}
	e.appendRecord(r, st)
}

// Modify 按订单编号修改一笔已接受订单的委托总量与限价。
//
// 只能修改待成交或部分成交订单；合约、方向、订单编号及已成交数量保持不变。
// 新总量必须大于已成交量，新限价必须为正；不存在、已撤销或全部成交的订单以及
// 不合法参数都会报错并追加一条拒绝记录，资金、持仓与其他订单占用保持原值。
// 取消剩余量继续使用 Cancel。
//
// 修改时以新占用替换原订单的旧占用（现金/可卖数量/合约持仓限额/账户总持仓金额
// 上限均按此口径，恰好等于额度允许；额度不足则拒绝，绝不为腾额度撤销其他订单）。
// 历史成交不重新计价：例如买入总量 10、已成交 4 改为总量 8、限价 12 后，剩余 4
// 只占用 48 现金，现金余额与已入账持仓不变；卖单只调整未成交部分占用的可卖数量。
//
// 有行情缺口时，买单修改只有在剩余量、现金占用、按现有口径计算的持仓金额占用均不
// 增加时才允许，任一项增加即拒绝（等待补齐的报价不参与判断）；卖单修改仍可办理，
// 但不得占用超过持仓的数量。日内亏损保护继续生效，被保护撤销的订单不能借修改恢复。
// 修改不改变订单原先的接受顺序：后续限额下调或报价触发撤单仍沿用已有次序。
//
// 每次真实修改只追加一条 RecordModified，保存修改前后参数、已成交量、当时的报价
// 与限额；金额上限拒绝时还保存修改前与申请后合计及参与计算的报价定位。对仍有效
// 订单重复提交当前参数时成功返回但不新增记录。修改失败只追加一条拒绝记录。
// 计算超出 int64 范围时返回包装 ErrInt64Overflow 的错误。
func (e *Engine) Modify(orderID, newQty, newLimit int64) error {
	var paramReason string
	switch {
	case newQty <= 0:
		paramReason = fmt.Sprintf("修改后委托总量必须为正: %d", newQty)
	case newLimit <= 0:
		paramReason = fmt.Sprintf("修改后委托限价必须为正: %d", newLimit)
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	o := e.orders[orderID]
	var reason string
	switch {
	case o == nil:
		reason = fmt.Sprintf("订单 %d 不存在", orderID)
	case o.Status == StatusCanceled:
		reason = fmt.Sprintf("订单 %d 已撤销，不能修改", orderID)
	case o.Status == StatusFilled:
		reason = fmt.Sprintf("订单 %d 已全部成交，不能修改", orderID)
	case paramReason != "":
		reason = paramReason
	case newQty <= o.Filled:
		reason = fmt.Sprintf("修改后总量 %d 必须大于已成交数量 %d", newQty, o.Filled)
	}
	if reason != "" {
		e.appendModifyRejectLocked(orderID, o, newQty, newLimit, reason, false, false, amtTotals{}, amtTotals{})
		return fmt.Errorf("修改订单 %d 被拒绝: %s", orderID, reason)
	}

	st := e.state(o.Symbol)
	oldLimit, oldQty := o.Limit, o.Qty
	oldRemaining := oldQty - o.Filled
	newRemaining := newQty - o.Filled

	// 对仍有效订单重复提交当前参数：成功返回且不新增记录、不动任何占用。
	if oldQty == newQty && oldLimit == newLimit {
		return nil
	}

	overflow := false
	var newCash, oldCash int64

	// 金额口径占用（剩余量 × max(限价, 最新已生效报价)）只在缺口规则与金额上限
	// 需要时才试算：未启用金额上限且无缺口时，报价再高也不影响修改。
	// 口径与接受新买单、账户金额汇总共用 buyReservedAmount/buyReservedUnit，统一维护。
	var newAmtNeed, oldAmtNeed int64
	needsComputed := false
	computeNeeds := func() bool {
		a, ok1 := buyReservedAmount(st, newRemaining, newLimit)
		b, ok2 := buyReservedAmount(st, oldRemaining, oldLimit)
		if !ok1 || !ok2 {
			return false
		}
		newAmtNeed, oldAmtNeed, needsComputed = a, b, true
		return true
	}

	if o.Side == Buy {
		// 买单现金占用：限价 × 剩余量。
		var ok bool
		newCash, ok = mulPositive(newLimit, newRemaining)
		if !ok {
			overflow = true
			reason = fmt.Sprintf("修改后限价 %d × 剩余量 %d 超出整数范围", newLimit, newRemaining)
		}
		oldCash = oldLimit * oldRemaining // 接受与成交时已校验过可乘性，必然安全
		if reason == "" {
			// 现金：原订单旧占用先释放、再由新占用替代；其他订单占用保留。
			usedByOthers, ok1 := subInt64(e.reservedCash, oldCash)
			available, ok2 := subInt64(e.cash, usedByOthers)
			if !ok1 || !ok2 {
				overflow = true
				reason = "修改后现金占用计算超出 int64 范围，修改拒绝"
			} else if newCash > available {
				reason = fmt.Sprintf("修改后现金不足: 需要 %d，可用 %d（原占用 %d 由新占用替代）",
					newCash, available, oldCash)
			}
		}
		if reason == "" {
			// 合约持仓限额：本单旧剩余量先从占用中扣除，再由新剩余量替代，其他
			// 买单的占用保留；新总量中已成交部分只计入持仓，不能再作为未成交占用。
			// 数量合计、超限与溢出判断与接受新买单共用 checkPositionLimit，同一条
			// 数量限额规则只维护一处。
			check := checkPositionLimit(st, oldRemaining, newRemaining)
			if check.overflow {
				overflow = true
				reason = "修改后持仓限额占用计算超出 int64 范围，修改拒绝"
			} else if check.over {
				reason = fmt.Sprintf("修改后超过最大持仓量 %d: 已持仓 %d + 买单剩余 %d",
					st.maxPosition, st.position, check.used+newRemaining)
			}
		}
		if reason == "" && len(st.pending) > 0 {
			// 行情缺口：剩余量、现金占用、按现有口径的持仓金额占用任一项增加即拒绝。
			if !computeNeeds() {
				overflow = true
				reason = fmt.Sprintf("订单 %d 修改后持仓金额占用计算超出 int64 范围，修改拒绝", orderID)
			} else if newRemaining > oldRemaining || newCash > oldCash || newAmtNeed > oldAmtNeed {
				reason = fmt.Sprintf("合约 %s 报价存在缺口（最新序号 %d，等待序号 %d），买单修改不得增加剩余量、现金或持仓金额占用",
					o.Symbol, st.latest.Seq, st.latest.Seq+1)
			}
		}
	} else {
		// 卖单只调整未成交部分占用的可卖数量；其他卖单占用保留。
		usedByOthers, ok1 := subInt64(st.reservedSell, oldRemaining)
		sellable, ok2 := subInt64(st.position, usedByOthers)
		if !ok1 || !ok2 {
			overflow = true
			reason = "修改后可卖数量计算超出 int64 范围，修改拒绝"
		} else if newRemaining > sellable {
			reason = fmt.Sprintf("修改后可卖数量不足: 需要 %d，可卖 %d（持仓 %d，其他卖单占用 %d）",
				newRemaining, sellable, st.position, usedByOthers)
		}
	}

	// 账户总持仓金额上限：修改前合计减去本订单旧占用、加上新占用后不得超过上限
	// （恰好等于允许）；额度不足只拒绝本次修改，不撤销其他订单。
	var before, apply amtTotals
	amtOverLimit := false
	if reason == "" && o.Side == Buy && e.amtLimitSet {
		b, okb := e.amountTotalsLocked()
		if !okb || (!needsComputed && !computeNeeds()) {
			overflow = true
			reason = fmt.Sprintf("订单 %d 修改后账户总持仓金额计算超出 int64 范围，修改拒绝", orderID)
		} else {
			base, ok1 := subInt64(b.total, oldAmtNeed)
			aTotal, ok2 := addInt64(base, newAmtNeed)
			if !ok1 || !ok2 {
				overflow = true
				reason = fmt.Sprintf("订单 %d 修改后账户总持仓金额申请后合计超出 int64 范围，修改拒绝", orderID)
			} else {
				before = b
				apply.total = aTotal
				if aTotal > e.amtLimit {
					amtOverLimit = true
					reason = fmt.Sprintf("修改后超过账户总持仓金额上限 %d: 修改前合计 %d，申请后合计 %d（原占用由新占用替代，不撤销其他订单）",
						e.amtLimit, b.total, aTotal)
				}
			}
		}
	}

	if reason != "" {
		// 金额上限“超额”拒绝需要修改前/申请后合计快照；溢出拒绝只固化启用状态与上限。
		e.appendModifyRejectLocked(orderID, o, newQty, newLimit, reason, overflow,
			amtOverLimit, before, apply)
		if overflow {
			return fmt.Errorf("修改订单 %d: %w", orderID, ErrInt64Overflow)
		}
		return fmt.Errorf("修改订单 %d 被拒绝: %s", orderID, reason)
	}

	// 真实修改：旧占用由新占用替代（各项校验已保证结果有界）。
	if o.Side == Buy {
		e.reservedCash += newCash - oldCash
		st.reservedBuy += newRemaining - oldRemaining
	} else {
		st.reservedSell += newRemaining - oldRemaining
	}
	o.Limit = newLimit
	o.Qty = newQty
	if o.Filled > 0 {
		o.Status = StatusPartial // newQty > Filled 保证仍有剩余量
	}

	e.appendRecord(Record{
		Kind:      RecordModified,
		Symbol:    o.Symbol,
		Side:      o.Side,
		OrderID:   o.ID,
		Limit:     newLimit,
		Qty:       newQty,
		Filled:    o.Filled,
		Remaining: o.Remaining(),
		OldLimit:  oldLimit,
		OldQty:    oldQty,
		OldFilled: o.Filled,
	}, st)
	return nil
}

// buyReservedUnit 是买单未成交部分“持仓金额占用”唯一的单价口径：
//
//	max(限价, 最新已生效报价)
//
// 接受新买单、修改买单与账户金额汇总都经此取值，保证“该用限价还是报价”这条
// 业务规则只在此维护；无有效报价时按限价计（买单接受后必有报价，此为兜底）。
// 它与现金占用单价（恒为限价）明确区分：报价高于限价时持仓金额占用按报价计，
// 报价低于限价时仍按限价计。
func buyReservedUnit(hasQuote bool, quotePrice, limit int64) int64 {
	if hasQuote && quotePrice > limit {
		return quotePrice
	}
	return limit
}

// buyReservedAmount 是买单未成交部分持仓金额占用的完整口径：
//
//	剩余量 × buyReservedUnit(限价, 最新已生效报价)
//
// 修改买单在缺口规则与金额上限试算时使用；乘积溢出 int64 时 ok 为 false。
// 调用时须持有引擎锁。
func buyReservedAmount(st *symbolState, remaining, limit int64) (int64, bool) {
	return mulPosInt64(remaining, buyReservedUnit(st.hasQuote, st.latest.Price, limit))
}

// ---------------------------------------------------------------------------
// 合约最大持仓数量限额
//
// 同一条数量限额规则在接受新买单（Buy）与修改有效买单（Modify）两处共用，
// 数量合计、超限与溢出判断只在 checkPositionLimit 维护：
//
//	申请后数量占用 = 该合约已持仓数量
//	             + 该合约其他有效买单的未成交部分
//	             + 本次申请买单的未成交数量
//
//   - 只计算当前合约：其他合约的订单不占用本合约的数量额度；
//   - 新买单以申请总量作为“本次申请未成交数量”，此前没有本单占用
//     （replaceRemaining 传 0）；
//   - 修改买单以“新总量 − 已成交量”替换本单旧剩余量（replaceRemaining 传旧
//     剩余量）：新总量中已经成交的部分只计入持仓，不能再作为未成交占用；
//     其他买单的占用保留；
//   - 已撤销与全部成交的买单没有有效剩余占用（不在 reservedBuy 内）；卖单成交
//     前也不能提前抵减已持仓数量；
//   - 合计恰好等于最大持仓量时允许，超过时按普通超限拒绝；数量本身合法而合计
//     超出 int64 范围时整体拒绝（ErrInt64Overflow），不能让数值回绕后绕过限额。
// ---------------------------------------------------------------------------

// positionLimitCheck 是一次合约数量限额试算的结果。
type positionLimitCheck struct {
	used     int64 // 除本单外的有效买单未成交占用（reservedBuy 扣除本单旧剩余量）
	overflow bool  // 申请后合计计算超出 int64 范围；此时 over 无意义
	over     bool  // 申请后合计（已持仓 + used + 申请未成交数量）超过最大持仓量；恰好等于不算超限
}

// checkPositionLimit 按上述唯一的数量限额规则试算。replaceRemaining 为本单旧
// 剩余量（接受新买单时传 0），applyRemaining 为本次申请买单的未成交数量（新买单
// 为申请总量；修改买单为新总量减去已成交量）。合计溢出 int64 时只置 overflow，
// 不回绕出一个伪合计；可表示时再与 st.maxPosition 比较，恰好等于不算超限。
func checkPositionLimit(st *symbolState, replaceRemaining, applyRemaining int64) positionLimitCheck {
	used, ok1 := subInt64(st.reservedBuy, replaceRemaining)
	apply, ok2 := addInt64(st.position, used)
	apply, ok3 := addInt64(apply, applyRemaining)
	if !ok1 || !ok2 || !ok3 {
		return positionLimitCheck{used: used, overflow: true}
	}
	return positionLimitCheck{used: used, over: apply > st.maxPosition}
}

// amtRejectDetail 是账户总持仓金额上限拒绝记录要固化的金额快照，新买单与修改买单
// 两个入口共用同一份描述，保证金额含义、报价来源与历史保留规则只在此维护。
type amtRejectDetail struct {
	totals      *amtTotals // 判断时的两类金额与合计；nil 表示金额计算本身溢出，只留启用状态与上限
	applyTotal  int64      // withApply 时的申请后合计
	withApply   bool       // 是否固化申请后合计
	applySymbol string     // 决定本次申请金额的合约：其报价须并入参与报价
}

// fillAmtRejectLocked 把金额上限拒绝快照固化到记录上：启用状态与判断时的上限
// 总是携带；detail.totals 非 nil 时再固化两类金额、合计与参与报价定位。
// 记录保存的是拒绝发生时的事实，此后报价、调限额或成功修改订单都不会改写它。
func (e *Engine) fillAmtRejectLocked(r *Record, detail amtRejectDetail) {
	r.AmtEnabled = true
	r.AmtLimit = e.amtLimit
	if detail.totals == nil {
		return
	}
	r.AmtHolding = detail.totals.holding
	r.AmtBuyReserved = detail.totals.reserved
	r.AmtTotal = detail.totals.total
	if detail.withApply {
		r.AmtApplyTotal = detail.applyTotal
	}
	r.AmtQuoteRefs = e.amountRejectRefsLocked(detail.totals.refs, detail.applySymbol)
}

// amountRejectRefsLocked 汇总金额判断的参与报价：当时贡献了持仓或买单占用的合约
// （refs），加上决定本次申请金额的合约 applySymbol——即使它此前没有持仓或有效买单。
// 每个合约只出现一次并按合约代码排列；等待补齐的报价不是已生效报价，不会进入快照。
// 返回的是新切片，调用方对结果的改动不影响引擎内留存的内容。
func (e *Engine) amountRejectRefsLocked(refs []RiskQuoteRef, applySymbol string) []RiskQuoteRef {
	out := append([]RiskQuoteRef(nil), refs...)
	// 申请合约的报价也参与了申请金额（max(限价, 报价)），须一并留存。
	if st := e.symbols[applySymbol]; st != nil && st.hasQuote {
		found := false
		for _, rf := range out {
			if rf.Symbol == applySymbol {
				found = true
				break
			}
		}
		if !found {
			out = append(out, RiskQuoteRef{
				Symbol: applySymbol, Seq: st.latest.Seq, Moment: st.latest.Moment, Price: st.latest.Price,
			})
			sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
		}
	}
	return out
}

// appendModifyRejectLocked 追加修改拒绝记录；o 可能为 nil（订单不存在），
// 此时以 reqOrderID 固化调用方提交的订单编号。金额上限超额拒绝（withAmt）固化
// 修改前合计、申请后合计与参与计算的报价定位；溢出或未启用金额上限时不带金额明细。
func (e *Engine) appendModifyRejectLocked(reqOrderID int64, o *Order, newQty, newLimit int64,
	reason string, overflow, withAmt bool, before, apply amtTotals) {

	var symbol string
	var side Side
	if o != nil {
		symbol = o.Symbol
		side = o.Side
	}
	r := Record{
		Kind:    RecordRejected,
		Symbol:  symbol,
		Side:    side,
		OrderID: reqOrderID,
		Limit:   newLimit,
		Qty:     newQty,
		Reason:  reason,
	}
	if o != nil && e.amtLimitSet && (overflow || withAmt) {
		detail := amtRejectDetail{applySymbol: symbol}
		if withAmt {
			detail.totals = &before
			detail.applyTotal = apply.total
			detail.withApply = true
		}
		e.fillAmtRejectLocked(&r, detail)
	}
	e.appendRecord(r, e.symbols[symbol])
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

// appendAmountRejectLocked 追加新买单因账户总持仓金额上限产生的拒绝记录。
// t 为 nil 表示申请前金额汇总本身溢出，此时只固化启用状态与上限；
// withApply 为 true 时同时保存申请后合计。
func (e *Engine) appendAmountRejectLocked(symbol string, qty, limit int64, reason string,
	t *amtTotals, applyTotal int64, withApply bool) {
	r := Record{
		Kind:   RecordRejected,
		Symbol: symbol,
		Side:   Buy,
		Qty:    qty,
		Limit:  limit,
		Reason: reason,
	}
	e.fillAmtRejectLocked(&r, amtRejectDetail{
		totals:      t,
		applyTotal:  applyTotal,
		withApply:   withApply,
		applySymbol: symbol,
	})
	e.appendRecord(r, e.symbols[symbol])
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
// 共用持仓估值（日内亏损保护与账户总持仓金额上限）
//
// 两项保护共用同一条持仓估值规则，只在此维护：
//   - 持仓市值始终为“持仓数量 × 最新已生效报价”；等待补齐的报价不是已生效报价，
//     不能提前参与；卖单占用的数量在成交前仍属于持仓，不提前抵减；
//   - 没有持仓的合约不贡献持仓市值，也不进入仅用于解释净值的持仓报价定位；
//   - 报价定位按合约代码排列，同一合约在一次计算中只出现一次，固化当时真正
//     参与计算的合约、序号、时刻与价格。
//
// 两项保护的差异只发生在估值核心之外：
//   - 日内亏损净值 = 现金余额 + 持仓市值。未成交买单冻结的现金仍是现金余额的一部分
//     （占用只是冻结，并未扣除），不重复扣减，买单剩余量也不按限价另计占用；
//   - 账户总持仓金额合计 = 持仓市值 + 有效买单剩余量 × max(限价, 最新报价)，
//     买单占用只属于金额上限口径，绝不带进净值。
// ---------------------------------------------------------------------------

// valuationScope 描述一次持仓估值所用的状态；零值即按当前实际状态估值。
// 风险侧试算可覆盖现金、某合约的持仓与/或报价（报价生效前、成交入账前的判断与
// 实际查询使用同一估值规则，但尚未接受的改动不会写回现金、持仓或当前报价）；
// 金额侧的报价链预检通过 quoteSym/quote 与 canceled 表达假设状态。
type valuationScope struct {
	cash     int64 // 估值起点现金（风险净值口径使用）
	hypoSym  string
	hypoPos  int64
	usePos   bool
	quoteSym string
	quote    Quote
	useQuote bool

	// withOrders 为 true 时额外计入有效买单剩余量占用（仅账户金额上限口径）：
	// 每笔剩余买单按 剩余量 × max(限价, 最新报价) 计；canceled 中的编号视为已撤销。
	// 风险净值口径恒为 false——买单冻结的现金属于现金余额，不能从净值再次扣除。
	withOrders bool
	canceled   map[int64]bool
}

// positionValuation 是一次持仓估值的结果。holding 为各合约持仓市值之和；
// reserved 为有效买单剩余量占用之和（仅 scope.withOrders 时非零）；refs 为真正
// 参与本次计算的合约报价定位，按合约代码排列、每合约一条。
type positionValuation struct {
	holding  int64
	reserved int64
	refs     []RiskQuoteRef
}

// positionValuationLocked 是两项保护共用的唯一持仓估值入口：负责逐合约选择最新
// 已生效报价（含假设覆盖）、计算持仓市值、可选计入有效买单剩余量占用、排列报价
// 定位，并对每个乘积与合计做 int64 溢出检查；任一步溢出时 ok 为 false。
// 调用时须持有引擎锁。
func (e *Engine) positionValuationLocked(scope valuationScope) (positionValuation, bool) {
	var out positionValuation

	syms := make([]string, 0, len(e.symbols))
	for s := range e.symbols {
		syms = append(syms, s)
	}
	sort.Strings(syms)

	for _, s := range syms {
		st := e.symbols[s]

		pos := st.position
		if scope.usePos && s == scope.hypoSym {
			pos = scope.hypoPos
		}

		hasQuote := st.hasQuote
		q := st.latest
		if scope.useQuote && s == scope.quoteSym {
			hasQuote = true
			q = scope.quote
		}

		contributed := false

		// 持仓市值：持仓数量 × 最新已生效报价。卖单占用的持仓不抵减；
		// 无持仓或无已生效报价的合约不贡献市值，也不进入报价定位。
		if pos != 0 && hasQuote {
			marketValue, ok := mulPosInt64(pos, q.Price)
			if !ok {
				return positionValuation{}, false
			}
			out.holding, ok = addInt64(out.holding, marketValue)
			if !ok {
				return positionValuation{}, false
			}
			contributed = true
		}

		// 有效买单剩余量占用（仅账户金额上限口径）：
		// Σ 剩余量 × buyReservedUnit(限价, 最新已生效报价)。同一合约同时有持仓和
		// 买单时只贡献一次报价定位；只有有效买单的合约进入金额定位，但不会进入
		// 日内净值的持仓报价定位（风险口径 withOrders=false）。
		if scope.withOrders && hasQuote {
			var symReserved int64
			for _, id := range st.buyOrderIDs {
				if scope.canceled != nil && scope.canceled[id] {
					continue
				}
				o := e.orders[id]
				if o == nil || o.Status == StatusFilled || o.Status == StatusCanceled {
					continue
				}
				rem := o.Remaining()
				if rem <= 0 {
					continue
				}
				need, ok := mulPosInt64(rem, buyReservedUnit(hasQuote, q.Price, o.Limit))
				if !ok {
					return positionValuation{}, false
				}
				symReserved, ok = addInt64(symReserved, need)
				if !ok {
					return positionValuation{}, false
				}
				contributed = true
			}
			var ok bool
			out.reserved, ok = addInt64(out.reserved, symReserved)
			if !ok {
				return positionValuation{}, false
			}
		}

		if contributed {
			out.refs = append(out.refs, RiskQuoteRef{Symbol: s, Seq: q.Seq, Moment: q.Moment, Price: q.Price})
		}
	}
	return out, true
}

// valuationLocked 按当前状态试算净值与日内亏损。
func (e *Engine) valuationLocked() (RiskValuation, []RiskQuoteRef, bool) {
	return e.valuationWithLocked(valuationScope{cash: e.cash})
}

// valuationHypoLocked 在假设状态上试算：可覆盖现金、某合约持仓与某合约报价，
// 供成交后、报价逐条生效前在不改业务状态的前提下做溢出与触线检查。
func (e *Engine) valuationHypoLocked(cash int64, hypoSym string, hypoPos int64, usePos bool,
	hypoQuote Quote, useQuote bool) (RiskValuation, []RiskQuoteRef, bool) {
	return e.valuationWithLocked(valuationScope{
		cash:     cash,
		hypoSym:  hypoSym,
		hypoPos:  hypoPos,
		usePos:   usePos,
		quoteSym: hypoSym,
		quote:    hypoQuote,
		useQuote: useQuote,
	})
}

// valuationWithLocked 在共用持仓估值之上加回现金得到净值，再以基准净值减当前净值
// 得到日内亏损（浮盈记零）；现金加市值或亏损差值溢出 int64 时 ok 为 false。
func (e *Engine) valuationWithLocked(scope valuationScope) (RiskValuation, []RiskQuoteRef, bool) {
	pv, ok := e.positionValuationLocked(scope)
	if !ok {
		return RiskValuation{}, nil, false
	}
	equity, ok := addInt64(scope.cash, pv.holding)
	if !ok {
		return RiskValuation{}, nil, false
	}
	loss, ok := subLoss(e.riskBaseline, equity)
	if !ok {
		return RiskValuation{}, nil, false
	}
	return RiskValuation{Equity: equity, Loss: loss}, pv.refs, true
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

// ---------------------------------------------------------------------------
// 账户总持仓金额上限
//
// 口径（跨全部合约合计）：
//   - 已持仓金额：各合约持仓数量 × 最新已生效报价；卖单占用的持仓在成交前不抵减。
//   - 买单剩余量占用：每笔有效买单 剩余数量 × max(限价, 最新报价)。
//   - 合计为两者之和；等待补齐的报价不参与，无有效报价的持仓/买单不贡献金额。
//
// 新买单以申请后的合计不超过上限才可接受（恰好等于允许）。首次设置、调整上限以及
// 每条报价真正生效后立即收敛超限：按订单编号从大到小撤销买单全部剩余量；若仅持仓
// 金额本身已超限，则撤掉全部剩余买单但不自动卖出。与日内亏损保护不同，该限制不做
// 整日锁定：报价回落、卖出或上调上限后即可再次买入，也不恢复已撤订单。
// ---------------------------------------------------------------------------

// amtTotals 是一次金额试算结果。
type amtTotals struct {
	holding  int64 // 已持仓金额
	reserved int64 // 有效买单剩余量占用金额
	total    int64 // 两者合计
	refs     []RiskQuoteRef
}

// amountTotalsLocked 按当前状态计算两类金额、合计与参与计算的报价定位。
// 任何乘积或求和溢出 int64 时 ok 为 false。
func (e *Engine) amountTotalsLocked() (amtTotals, bool) {
	return e.amountTotalsShadowLocked("", Quote{}, nil)
}

// amountTotalsShadowLocked 在影子状态上试算：
// quoteSym 非空时以 quote 作为该合约的最新已生效报价（报价链预检）；
// canceled 中的订单编号视为已撤销，不贡献买单占用。
// 持仓市值、报价选择、排序、报价定位与溢出检查全部复用与日内亏损净值相同的
// positionValuationLocked；这里只在其之上合计 持仓市值 + 买单剩余量占用。
func (e *Engine) amountTotalsShadowLocked(quoteSym string, quote Quote, canceled map[int64]bool) (amtTotals, bool) {
	pv, ok := e.positionValuationLocked(valuationScope{
		quoteSym:   quoteSym,
		quote:      quote,
		useQuote:   quoteSym != "",
		withOrders: true,
		canceled:   canceled,
	})
	if !ok {
		return amtTotals{}, false
	}
	total, ok := addInt64(pv.holding, pv.reserved)
	if !ok {
		return amtTotals{}, false
	}
	return amtTotals{
		holding:  pv.holding,
		reserved: pv.reserved,
		total:    total,
		refs:     pv.refs,
	}, true
}

// SetPositionAmountLimit 显式设置账户总持仓金额上限（非负 int64），跨交易日保留。
// 上限只约束多个合约合计的“持仓市值 + 有效买单剩余量占用”。
// 首次设置与每次调整后都立即按当前状态收敛超限（见包注释），返回本次被撤销的
// 订单编号（按撤销顺序）；负值报错并保留原设置；同值重复设置为无操作。
// 金额乘积或合计溢出 int64 时整体报错（包装 ErrInt64Overflow）：本次设置不生效、
// 原设置保留，只增加一条拒绝记录。
func (e *Engine) SetPositionAmountLimit(limit int64) ([]int64, error) {
	if limit < 0 {
		return nil, fmt.Errorf("账户总持仓金额上限不能为负: %d", limit)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.amtLimitSet && limit == e.amtLimit {
		return nil, nil // 同值设置：幂等无操作
	}

	// 先在当前状态上试算：溢出则设置不生效，保留原设置。
	if _, ok := e.amountTotalsLocked(); !ok {
		e.records = append(e.records, Record{
			Kind:       RecordRejected,
			Reason:     fmt.Sprintf("设置账户总持仓金额上限为 %d 失败: %v", limit, ErrInt64Overflow),
			AmtEnabled: e.amtLimitSet,
			AmtLimit:   e.amtLimit,
		})
		return nil, fmt.Errorf("设置账户总持仓金额上限 %d: %w", limit, ErrInt64Overflow)
	}

	e.amtLimitSet = true
	e.amtLimit = limit
	canceled := e.enforceAmountLimitLocked("设置账户总持仓金额上限")
	return canceled, nil
}

// PositionAmountStatus 返回账户总持仓金额上限状态；未设置时 Enabled 为 false，
// 其余字段均为零值。
func (e *Engine) PositionAmountStatus() PositionAmountStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.amtLimitSet {
		return PositionAmountStatus{Enabled: false}
	}
	st := PositionAmountStatus{Enabled: true, Limit: e.amtLimit}
	if t, ok := e.amountTotalsLocked(); ok {
		st.Holding = t.holding
		st.BuyReserved = t.reserved
		st.Total = t.total
	}
	return st
}

// activeBuyIDsShadowLocked 返回全部仍有剩余量的买单编号，按编号从大到小排列；
// simCanceled 中的编号视为已撤销（报价链预检时模拟本批此前各条报价造成的撤单）。
func (e *Engine) activeBuyIDsShadowLocked(simCanceled map[int64]bool) []int64 {
	var ids []int64
	for id, o := range e.orders {
		if simCanceled != nil && simCanceled[id] {
			continue
		}
		if o.Side == Buy && o.Status != StatusCanceled && o.Status != StatusFilled && o.Remaining() > 0 {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	return ids
}

// amtCancelStep 是金额上限收敛计划中的一步：撤销 id 订单的全部剩余量。
// totals 为撤销该单之前（仍超限时）判断所用的金额快照，供撤单记录原样固化；
// holdingOnly 表示该步属于“仅持仓金额超限、撤光全部剩余买单”的情形。
type amtCancelStep struct {
	id          int64
	totals      amtTotals
	holdingOnly bool
}

// planAmountCancelLocked 是账户总持仓金额上限撤单的唯一选择规则，供设置上限、
// 连续报价与补齐缺口的整批预检和实际撤单共同使用，保证相同持仓与订单状态下
// 得出一致的撤单结果。
//
// quoteSym 非空时以 quote 作为该合约的最新已生效报价（报价链预检）；base 中的
// 订单编号视为已撤销（本批此前各条报价或亏损保护已撤）。规则：若仅持仓金额
// 超限，则选出全部有效买单（编号从大到小），不自动卖出；否则自编号最大的买单
// 起逐个选出并撤销全部剩余量，直到合计不再超限（恰好等于上限时保留订单）。
// 返回的每一步都携带撤销前判断所用的金额快照；任一步金额乘积或合计溢出
// int64 时 ok 为 false。
func (e *Engine) planAmountCancelLocked(quoteSym string, quote Quote, base map[int64]bool) ([]amtCancelStep, bool) {
	t, ok := e.amountTotalsShadowLocked(quoteSym, quote, base)
	if !ok {
		return nil, false
	}

	// 在 base 之上叠加本计划已选出的撤单，逐步试算后续状态。
	sim := make(map[int64]bool, len(base))
	for id := range base {
		sim[id] = true
	}

	var steps []amtCancelStep

	if t.holding > e.amtLimit {
		// 仅持仓金额已超限：撤掉全部剩余买单，不自动卖出。
		for _, id := range e.activeBuyIDsShadowLocked(sim) {
			steps = append(steps, amtCancelStep{id: id, totals: t, holdingOnly: true})
			sim[id] = true
			// 撤单只减不增，重算必然可表示；快照供下一步记录固化。
			t, _ = e.amountTotalsShadowLocked(quoteSym, quote, sim)
		}
		return steps, true
	}

	for t.total > e.amtLimit {
		ids := e.activeBuyIDsShadowLocked(sim)
		if len(ids) == 0 {
			break // 兜底：无单可撤
		}
		id := ids[0] // 编号最大者先撤
		steps = append(steps, amtCancelStep{id: id, totals: t})
		sim[id] = true
		t, ok = e.amountTotalsShadowLocked(quoteSym, quote, sim)
		if !ok {
			return nil, false
		}
	}
	return steps, true
}

// enforceAmountLimitLocked 按 planAmountCancelLocked 选出的计划实际撤单，
// 返回按撤销顺序排列的订单编号。cause 用作文案中的引发来源（“设置账户总持仓
// 金额上限”或“报价生效”）。调用前须确认 amtLimitSet 且调用方已在变更生效前
// 完成溢出预检。
func (e *Engine) enforceAmountLimitLocked(cause string) []int64 {
	if !e.amtLimitSet {
		return nil
	}
	steps, ok := e.planAmountCancelLocked("", Quote{}, nil)
	if !ok {
		return nil // 调用方负责在变更生效前完成溢出预检
	}

	var canceled []int64
	for _, step := range steps {
		o := e.orders[step.id]
		var reason string
		if step.holdingOnly {
			reason = fmt.Sprintf("账户总持仓金额上限 %d：%s后仅持仓金额 %d 已超过上限，撤销买单全部剩余量",
				e.amtLimit, cause, step.totals.holding)
		} else {
			reason = fmt.Sprintf("账户总持仓金额上限 %d：%s后持仓金额 %d 与买单剩余占用 %d 合计 %d 超限，按订单编号从大到小撤销买单全部剩余量",
				e.amtLimit, cause, step.totals.holding, step.totals.reserved, step.totals.total)
		}
		e.cancelAmountLocked(o, reason, step.totals)
		canceled = append(canceled, step.id)
	}
	return canceled
}

// cancelAmountLocked 与 cancelLocked 做同样的撤销，但撤销记录额外携带撤销前
// （仍超限时）判断所用的两类金额、合计、上限与参与报价定位（取自 t）。
func (e *Engine) cancelAmountLocked(o *Order, reason string, t amtTotals) {
	e.cancelLockedImpl(o, reason, &t)
}
