package book

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Account 是一个本地模拟交易账户：一个账户、多个合约。
// 现金、持仓、价格与数量均以整数最小单位表示。
// 所有方法并发安全；返回的订单/报价均为副本，调用方修改不会影响账户状态。
type Account struct {
	mu sync.Mutex

	cash      int64
	contracts map[string]*contractBook
	orders    map[int64]*Order
	fills     map[int64]*Fill

	nextOrderID int64
	nextFillID  int64
}

// contractBook 是单个合约的行情、持仓与限额状态。
type contractBook struct {
	quote   *Quote           // 最新有效报价（序号从 1 连续推进）
	pending map[int64]*Quote // 跨号等待的报价：seq -> quote
	seen    map[int64]*Quote // 已收到的全部报价（用于重复序号判别）

	position int64 // 持仓数量
	limit    int64 // 最大持仓量
	limitSet bool

	orders []int64 // 已接受订单的 ID，按接受顺序排列
}

// NewAccount 创建账户。initialCash 为初始现金，必须非负。
func NewAccount(initialCash int64) (*Account, error) {
	if initialCash < 0 {
		return nil, errors.New("初始现金不能为负")
	}
	return &Account{
		cash:      initialCash,
		contracts: make(map[string]*contractBook),
		orders:    make(map[int64]*Order),
		fills:     make(map[int64]*Fill),
		nextOrderID: 1,
		nextFillID:  1,
	}, nil
}

// ---------- 行情 ----------

// AddQuote 提交一个合约报价。
//   - 序号必须为正整数，价格必须为正整数；
//   - 只有连续序号才能推进为最新报价，跨号报价先等待，补齐后依次生效；
//   - 重复序号内容一致不产生新事件，内容不同则报错并保留原值。
func (a *Account) AddQuote(contract string, seq, time, price int64) error {
	if contract == "" {
		return errors.New("合约不能为空")
	}
	if seq <= 0 {
		return errors.New("报价序号必须为正整数")
	}
	if price <= 0 {
		return errors.New("报价价格必须为正整数")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	cb := a.getOrCreateContract(contract)

	if existing, ok := cb.seen[seq]; ok {
		if existing.Time == time && existing.Price == price {
			return nil // 重复序号内容一致：不产生新事件
		}
		return fmt.Errorf("报价序号 %d 重复且内容不一致，保留原值", seq)
	}

	q := &Quote{Seq: seq, Time: time, Price: price}
	cb.seen[seq] = q

	if cb.quote == nil {
		if seq == 1 {
			cb.quote = q
			a.drainPending(cb)
		} else {
			cb.pending[seq] = q
		}
	} else if seq == cb.quote.Seq+1 {
		cb.quote = q
		a.drainPending(cb)
	} else {
		cb.pending[seq] = q
	}
	return nil
}

// drainPending 在最新报价推进后，把已补齐的跨号报价依次生效。
func (a *Account) drainPending(cb *contractBook) {
	for {
		next := cb.quote.Seq + 1
		q, ok := cb.pending[next]
		if !ok {
			return
		}
		delete(cb.pending, next)
		cb.quote = q
	}
}

// hasGap 表示存在跨号等待的报价（行情尚未补齐）。
func (cb *contractBook) hasGap() bool { return len(cb.pending) > 0 }

// LatestQuote 返回合约的最新有效报价副本；从未有过有效报价时返回 nil。
func (a *Account) LatestQuote(contract string) *Quote {
	a.mu.Lock()
	defer a.mu.Unlock()
	cb := a.contracts[contract]
	if cb == nil || cb.quote == nil {
		return nil
	}
	c := *cb.quote
	return &c
}

// ---------- 持仓限额 ----------

// SetPositionLimit 设置合约的最大持仓量（必须非负）。
// 降低限额时立即从最后接受的买单开始，取消该单全部剩余量，直到满足新限额；
// 若已有持仓本身超限，则撤掉全部未成交买单并拒绝后续买单，仍允许卖出，不自动平仓。
// 释放现金与持仓额度在同一操作内完成。
func (a *Account) SetPositionLimit(contract string, limit int64) error {
	if contract == "" {
		return errors.New("合约不能为空")
	}
	if limit < 0 {
		return errors.New("持仓限额不能为负")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	cb := a.getOrCreateContract(contract)
	oldLimit := cb.limit
	wasSet := cb.limitSet
	cb.limit = limit
	cb.limitSet = true

	if wasSet && limit >= oldLimit {
		return nil // 提高限额不触发撤销
	}

	if cb.position > limit {
		// 持仓本身超限：撤掉全部未成交买单，不自动平仓。
		for i := len(cb.orders) - 1; i >= 0; i-- {
			o := a.orders[cb.orders[i]]
			if o.Side != Buy || o.RemainingQty() == 0 {
				continue
			}
			a.forceCancel(o, fmt.Sprintf("持仓 %d 已超过限额 %d，撤销全部未成交买单", cb.position, limit))
		}
		return nil
	}

	// 持仓未超限：从最后接受的买单开始，逐单取消全部剩余量，直到满足新限额。
	for a.positionPlusBuyRemaining(cb) > limit {
		var target *Order
		for i := len(cb.orders) - 1; i >= 0; i-- {
			o := a.orders[cb.orders[i]]
			if o.Side == Buy && o.RemainingQty() > 0 &&
				(o.Status == OrderPending || o.Status == OrderPartialFilled) {
				target = o
				break
			}
		}
		if target == nil {
			break
		}
		a.forceCancel(target, fmt.Sprintf("持仓限额由 %d 降低至 %d", oldLimit, limit))
	}
	return nil
}

// PositionLimit 返回合约的持仓限额；第二个返回值为 false 表示尚未设置。
func (a *Account) PositionLimit(contract string) (int64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cb := a.contracts[contract]
	if cb == nil {
		return 0, false
	}
	return cb.limit, cb.limitSet
}

// ---------- 下单 ----------

// PlaceOrder 提交订单。价格与数量必须为正整数，方向必须合法。
// 业务条件不满足时订单被拒绝（状态为 OrderRejected 并说明原因），不占用资金或持仓；
// 输入非法时返回 error，且不产生任何记录。
func (a *Account) PlaceOrder(contract string, side Side, price, qty int64) (*Order, error) {
	if contract == "" {
		return nil, errors.New("合约不能为空")
	}
	if side != Buy && side != Sell {
		return nil, errors.New("无效的买卖方向")
	}
	if price <= 0 {
		return nil, errors.New("价格必须为正整数")
	}
	if qty <= 0 {
		return nil, errors.New("数量必须为正整数")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	cb := a.contracts[contract]

	// 即使被拒也分配订单编号并留痕，记录当时的行情与限额快照。
	o := &Order{
		ID:       a.nextOrderID,
		Contract: contract,
		Side:     side,
		Price:    price,
		Qty:      qty,
		Quote:    snapshotQuote(cb),
	}
	a.nextOrderID++
	if cb != nil {
		o.Limit = cb.limit
		o.LimitSet = cb.limitSet
	}
	a.orders[o.ID] = o

	reject := func(reason string) (*Order, error) {
		o.Status = OrderRejected
		o.RejectReason = reason
		return a.cloneOrder(o), nil
	}

	if cb == nil {
		return reject("合约尚未设置持仓限额")
	}
	if cb.quote == nil {
		return reject("尚无有效报价")
	}
	if side == Buy && cb.hasGap() {
		return reject("行情序号存在缺口，拒绝新买单")
	}
	if !cb.limitSet {
		return reject("尚未设置持仓限额")
	}

	if side == Buy {
		buyRemaining := a.buyRemainingLocked(cb)
		if cb.position+buyRemaining+qty > cb.limit {
			return reject(fmt.Sprintf(
				"超过持仓限额：持仓 %d + 买单剩余 %d + 本次 %d > 限额 %d",
				cb.position, buyRemaining, qty, cb.limit))
		}
		need := price * qty
		if need > a.cash-a.frozenCashLocked() {
			return reject(fmt.Sprintf("资金不足：需要 %d，可用 %d", need, a.cash-a.frozenCashLocked()))
		}
	} else {
		sellable := cb.position - a.sellRemainingLocked(cb)
		if qty > sellable {
			return reject(fmt.Sprintf("可卖持仓不足：可卖 %d，委托 %d", sellable, qty))
		}
	}

	o.Status = OrderPending
	cb.orders = append(cb.orders, o.ID)
	return a.cloneOrder(o), nil
}

// ---------- 成交 ----------

// SubmitFill 提交一笔成交，由调用方驱动，系统不自动撮合。
//   - 成交量与成交价必须为正整数；买单成交价不得高于限价，卖单不得低于限价；
//   - 成交量超过剩余量、订单不存在、已拒绝或已撤销时整笔拒绝，资金与持仓不变；
//   - 允许部分成交；买入按实际成交金额扣现金，限价与成交价的差额立即释放，
//     卖出增加现金并扣持仓；
//   - 同一成交编号相同内容重复提交返回原结果，不新增事件；不同内容明确报错；
//     被拒绝的成交不占用编号，修正后仍可提交。
func (a *Account) SubmitFill(fillID, orderID, qty, price int64) (*Fill, error) {
	if fillID <= 0 {
		return nil, errors.New("成交编号必须为正整数")
	}
	if orderID <= 0 {
		return nil, errors.New("订单编号必须为正整数")
	}
	if qty <= 0 {
		return nil, errors.New("成交量必须为正整数")
	}
	if price <= 0 {
		return nil, errors.New("成交价格必须为正整数")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// 成交编号幂等去重：先于一切订单状态判断。
	if existing, ok := a.fills[fillID]; ok {
		if existing.OrderID == orderID && existing.Qty == qty && existing.Price == price {
			c := *existing
			if existing.Quote != nil {
				q := *existing.Quote
				c.Quote = &q
			}
			return &c, nil
		}
		return nil, fmt.Errorf("成交编号 %d 已存在但内容不一致，状态不变", fillID)
	}

	o := a.orders[orderID]
	if o == nil {
		return nil, errors.New("订单不存在")
	}
	if o.Status == OrderRejected {
		return nil, errors.New("订单已被拒绝，不能成交")
	}
	if o.Status == OrderCancelled {
		return nil, errors.New("订单已撤销，不能成交")
	}
	if o.Status == OrderFilled {
		return nil, errors.New("订单已全部成交")
	}
	if qty > o.RemainingQty() {
		return nil, fmt.Errorf("成交量 %d 超过剩余量 %d，整笔拒绝", qty, o.RemainingQty())
	}
	if o.Side == Buy && price > o.Price {
		return nil, fmt.Errorf("买入成交价 %d 高于限价 %d，整笔拒绝", price, o.Price)
	}
	if o.Side == Sell && price < o.Price {
		return nil, fmt.Errorf("卖出成交价 %d 低于限价 %d，整笔拒绝", price, o.Price)
	}

	cb := a.contracts[o.Contract]

	// 记账：冻结资金/持仓随剩余量释放，成交只按实际金额收付。
	amount := qty * price
	if o.Side == Buy {
		a.cash -= amount
		cb.position += qty
	} else {
		a.cash += amount
		cb.position -= qty
	}
	o.FilledQty += qty
	if o.RemainingQty() == 0 {
		o.Status = OrderFilled
	} else {
		o.Status = OrderPartialFilled
	}

	f := &Fill{
		ID:       fillID,
		OrderID:  orderID,
		Qty:      qty,
		Price:    price,
		Quote:    snapshotQuote(cb),
		Limit:    cb.limit,
		LimitSet: cb.limitSet,
	}
	a.fills[fillID] = f
	o.fills = append(o.fills, f)

	c := *f
	if f.Quote != nil {
		qq := *f.Quote
		c.Quote = &qq
	}
	return &c, nil
}

// ---------- 撤单 ----------

// CancelOrder 撤销订单，只释放未成交部分，已成交部分保留。
// 已全部成交、已撤销或已拒绝的订单不能撤销。
func (a *Account) CancelOrder(orderID int64) (*Order, error) {
	if orderID <= 0 {
		return nil, errors.New("订单编号必须为正整数")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	o := a.orders[orderID]
	if o == nil {
		return nil, errors.New("订单不存在")
	}
	if o.Status == OrderRejected {
		return nil, errors.New("订单已被拒绝，不能撤销")
	}
	if o.Status == OrderCancelled {
		return nil, errors.New("订单已撤销，不能重复撤销")
	}
	if o.Status == OrderFilled {
		return nil, errors.New("订单已全部成交，不能撤销")
	}

	cb := a.contracts[o.Contract]
	o.Status = OrderCancelled
	o.CancelReason = "客户撤单"
	o.CancelQuote = snapshotQuote(cb)
	if cb != nil {
		o.CancelLimit = cb.limit
		o.CancelLimitSet = cb.limitSet
	}
	return a.cloneOrder(o), nil
}

// forceCancel 用于持仓限额触发的强制撤销，调用方已持有锁。
func (a *Account) forceCancel(o *Order, reason string) {
	o.Status = OrderCancelled
	o.CancelReason = reason
	cb := a.contracts[o.Contract]
	o.CancelQuote = snapshotQuote(cb)
	if cb != nil {
		o.CancelLimit = cb.limit
		o.CancelLimitSet = cb.limitSet
	}
}

// ---------- 查询 ----------

// CashBalance 返回现金余额。
func (a *Account) CashBalance() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cash
}

// FrozenCash 返回占用现金（所有未成交买单按限价乘剩余量冻结的金额）。
func (a *Account) FrozenCash() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.frozenCashLocked()
}

// AvailableCash 返回可用现金 = 现金余额 - 占用现金。
func (a *Account) AvailableCash() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cash - a.frozenCashLocked()
}

// Position 返回合约持仓数量。
func (a *Account) Position(contract string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	cb := a.contracts[contract]
	if cb == nil {
		return 0
	}
	return cb.position
}

// Sellable 返回可卖持仓数量 = 持仓 - 未成交卖单冻结量。
func (a *Account) Sellable(contract string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	cb := a.contracts[contract]
	if cb == nil {
		return 0
	}
	return cb.position - a.sellRemainingLocked(cb)
}

// Order 返回订单副本；不存在时返回 nil。
func (a *Account) Order(id int64) *Order {
	a.mu.Lock()
	defer a.mu.Unlock()
	o := a.orders[id]
	if o == nil {
		return nil
	}
	return a.cloneOrder(o)
}

// Orders 返回全部订单副本，按编号升序。
func (a *Account) Orders() []*Order {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]int64, 0, len(a.orders))
	for id := range a.orders {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]*Order, 0, len(ids))
	for _, id := range ids {
		out = append(out, a.cloneOrder(a.orders[id]))
	}
	return out
}

// Fills 返回某订单的成交记录副本，按提交顺序。
func (a *Account) Fills(orderID int64) []*Fill {
	a.mu.Lock()
	defer a.mu.Unlock()
	o := a.orders[orderID]
	if o == nil {
		return nil
	}
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

// ---------- 内部工具 ----------

func (a *Account) getOrCreateContract(contract string) *contractBook {
	cb := a.contracts[contract]
	if cb == nil {
		cb = &contractBook{
			pending: make(map[int64]*Quote),
			seen:    make(map[int64]*Quote),
		}
		a.contracts[contract] = cb
	}
	return cb
}

// snapshotQuote 返回合约当前报价的副本；cb 为空或无有效报价时返回 nil（明确为空）。
func snapshotQuote(cb *contractBook) *Quote {
	if cb == nil || cb.quote == nil {
		return nil
	}
	c := *cb.quote
	return &c
}

func (a *Account) frozenCashLocked() int64 {
	var total int64
	for _, o := range a.orders {
		if o.Side == Buy && (o.Status == OrderPending || o.Status == OrderPartialFilled) {
			total += o.Price * o.RemainingQty()
		}
	}
	return total
}

func (a *Account) buyRemainingLocked(cb *contractBook) int64 {
	var total int64
	for _, id := range cb.orders {
		o := a.orders[id]
		if o.Side == Buy && (o.Status == OrderPending || o.Status == OrderPartialFilled) {
			total += o.RemainingQty()
		}
	}
	return total
}

func (a *Account) sellRemainingLocked(cb *contractBook) int64 {
	var total int64
	for _, id := range cb.orders {
		o := a.orders[id]
		if o.Side == Sell && (o.Status == OrderPending || o.Status == OrderPartialFilled) {
			total += o.RemainingQty()
		}
	}
	return total
}

func (a *Account) positionPlusBuyRemaining(cb *contractBook) int64 {
	return cb.position + a.buyRemainingLocked(cb)
}

// cloneOrder 返回订单副本（不含成交明细，成交通过 Fills 查询）。
func (a *Account) cloneOrder(o *Order) *Order {
	c := *o
	c.fills = nil
	if o.Quote != nil {
		q := *o.Quote
		c.Quote = &q
	}
	if o.CancelQuote != nil {
		q := *o.CancelQuote
		c.CancelQuote = &q
	}
	return &c
}
