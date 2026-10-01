// 日内亏损保护：调用方以递增日号开启交易日，以现金余额加各合约持仓按
// 最新已生效报价计算的市值作为基准净值；日内亏损达到或超过上限时立即
// 限制增险（拒绝新买单、按订单编号从大到小撤销全部有效买单的未成交部分），
// 限制持续到下一交易日。
package book

import (
	"fmt"
	"math"
	"sort"
)

// RiskTriggerKind 描述日内亏损触线的引发方式。
type RiskTriggerKind int

const (
	// RiskTriggerDay 开日即触线（例如零上限）。
	RiskTriggerDay RiskTriggerKind = iota + 1
	// RiskTriggerLimit 调整上限触线。
	RiskTriggerLimit
	// RiskTriggerQuote 报价生效触线。
	RiskTriggerQuote
	// RiskTriggerFill 成交入账触线。
	RiskTriggerFill
)

// String 返回触线原因的中文描述。
func (k RiskTriggerKind) String() string {
	switch k {
	case RiskTriggerDay:
		return "开日"
	case RiskTriggerLimit:
		return "调限额"
	case RiskTriggerQuote:
		return "报价"
	case RiskTriggerFill:
		return "成交"
	default:
		return "未知"
	}
}

// PositionValuation 是触线时刻某个持仓合约的估值快照。
type PositionValuation struct {
	Symbol      string // 合约代码
	Qty         int64  // 持仓数量
	QuoteValid  bool   // 是否有有效报价
	QuoteSeq    int64  // 报价序号
	QuoteMoment int64  // 报价时刻
	QuotePrice  int64  // 报价价格
}

// RiskStatus 是日内风险状态的查询结果。尚未开始交易日时 Started 为 false。
type RiskStatus struct {
	Day        int64 // 当日日号
	Started    bool  // 是否已开始交易日
	Baseline   int64 // 基准净值（开日时）
	NetValue   int64 // 当前净值
	Loss       int64 // 日内亏损（盈利时为 0）
	Limit      int64 // 亏损上限
	Restricted bool  // 是否已触线限制增险
}

// riskState 是单个交易日的风险状态。
type riskState struct {
	day        int64
	limit      int64
	baseline   int64
	restricted bool
}

// StartDay 以递增的正整数日号和非负亏损上限开始交易日。
// 以现金余额加各合约持仓按最新已生效报价计算的市值作为当日基准净值，
// 订单占用资金不重复扣除；零上限在开日时立即触线。
//
// 日号必须为正整数且不小于已开始的最大日号：
//   - 同日号且上限相同：不产生任何变化；
//   - 同日号但上限不同：报错且不改状态；
//   - 更大日号：重新确定基准净值并清除上一日的限制，但不恢复上一日已撤销的订单；
//   - 倒退日号、非正日号或负上限：报错且不改状态。
//
// 日号不由行情时刻推导，跨日只由调用方明确发起。
func (e *Engine) StartDay(day int64, limit int64) error {
	if day <= 0 {
		return fmt.Errorf("日号必须为正整数: %d", day)
	}
	if limit < 0 {
		return fmt.Errorf("亏损上限不能为负: %d", limit)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.risk != nil {
		switch {
		case day == e.risk.day:
			if limit == e.risk.limit {
				return nil // 同日号同上限：不产生变化
			}
			return fmt.Errorf("日号 %d 已开始，亏损上限为 %d，不能改为 %d", day, e.risk.limit, limit)
		case day < e.risk.day:
			return fmt.Errorf("日号不能倒退: 当前日号 %d，请求日号 %d", e.risk.day, day)
		}
	}

	baseline, ok := e.valuationLocked()
	if !ok {
		return fmt.Errorf("开日净值超出整数范围，日号 %d 未生效", day)
	}

	if e.risk == nil {
		e.risk = &riskState{}
	}
	e.risk.day = day
	e.risk.limit = limit
	e.risk.baseline = baseline
	e.risk.restricted = false

	if limit == 0 {
		e.triggerRiskLocked(RiskTriggerDay, 0, 0)
	}
	return nil
}

// SetLimit 在当日调整亏损上限。尚未开始交易日时调用报错。
// 上限调高不解除已有的触线限制；上限下调立即按当前净值检查亏损，
// 需要时触发与行情触线相同的处理。调整导致净值超出整数范围时整体报错、
// 上限不改变。
func (e *Engine) SetLimit(limit int64) error {
	if limit < 0 {
		return fmt.Errorf("亏损上限不能为负: %d", limit)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.risk == nil {
		return fmt.Errorf("尚未开始交易日，不能调整亏损上限")
	}
	if limit == e.risk.limit {
		return nil
	}
	net, ok := e.valuationLocked()
	if !ok {
		return fmt.Errorf("净值超出整数范围，亏损上限未调整")
	}
	loss := lossLocked(e.risk.baseline, net)
	lower := limit < e.risk.limit
	e.risk.limit = limit
	if lower && loss >= limit && !e.risk.restricted {
		e.triggerRiskLocked(RiskTriggerLimit, 0, 0)
	}
	return nil
}

// RiskStatus 返回当前风险状态。尚未开始交易日时 Started 为 false，
// 其余字段为零值。
func (e *Engine) RiskStatus() RiskStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.risk == nil {
		return RiskStatus{Started: false}
	}
	net, ok := e.valuationLocked()
	if !ok {
		// 报价与成交路径已保证估值不溢出，此处为兜底。
		return RiskStatus{
			Day:        e.risk.day,
			Started:    true,
			Limit:      e.risk.limit,
			Restricted: e.risk.restricted,
		}
	}
	return RiskStatus{
		Day:        e.risk.day,
		Started:    true,
		Baseline:   e.risk.baseline,
		NetValue:   net,
		Loss:       lossLocked(e.risk.baseline, net),
		Limit:      e.risk.limit,
		Restricted: e.risk.restricted,
	}
}

// valuationLocked 计算当前净值：现金余额加各合约持仓按最新已生效报价
// 计算的市值。订单占用资金不重复扣除。返回 false 表示结果超出 int64 范围。
func (e *Engine) valuationLocked() (int64, bool) {
	total := e.cash
	for _, st := range e.symbols {
		if st.position == 0 {
			continue
		}
		price := int64(0)
		if st.hasQuote {
			price = st.latest.Price
		}
		mv, ok := mulPositive(st.position, price)
		if !ok {
			return 0, false
		}
		if total > 0 && mv > math.MaxInt64-total {
			return 0, false
		}
		total += mv
	}
	return total, true
}

// valuationsLocked 返回触线时刻各持仓合约的估值快照（按合约代码排序）。
func (e *Engine) valuationsLocked() []PositionValuation {
	out := make([]PositionValuation, 0)
	for sym, st := range e.symbols {
		if st.position == 0 {
			continue
		}
		v := PositionValuation{Symbol: sym, Qty: st.position}
		if st.hasQuote {
			v.QuoteValid = true
			v.QuoteSeq = st.latest.Seq
			v.QuoteMoment = st.latest.Moment
			v.QuotePrice = st.latest.Price
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// valuationAfterLocked 试算成交入账后的净值。cashBefore 为入账前现金余额，
// amount 为成交金额（正数）；返回 false 表示结果超出 int64 范围。
func (e *Engine) valuationAfterLocked(cashBefore int64, st *symbolState, o *Order, qty, amount int64) (int64, bool) {
	cashAfter := cashBefore
	if o.Side == Buy {
		cashAfter -= amount
	} else {
		if amount > math.MaxInt64-cashBefore {
			return 0, false
		}
		cashAfter += amount
	}
	posAfter := st.position
	if o.Side == Buy {
		posAfter += qty
	} else {
		posAfter -= qty
	}

	total := cashAfter
	for sym, symSt := range e.symbols {
		pos := symSt.position
		if sym == o.Symbol {
			pos = posAfter
		}
		if pos == 0 {
			continue
		}
		price := int64(0)
		if symSt.hasQuote {
			price = symSt.latest.Price
		}
		mv, ok := mulPositive(pos, price)
		if !ok {
			return 0, false
		}
		if total > 0 && mv > math.MaxInt64-total {
			return 0, false
		}
		total += mv
	}
	return total, true
}

// lossLocked 计算日内亏损：基准净值减当前净值；盈利时记零。
func lossLocked(baseline, net int64) int64 {
	if net >= baseline {
		return 0
	}
	return baseline - net
}

// riskSnapshotLocked 返回当日风险快照：日号、当前亏损、上限与是否已限制。
// ok 为 false 表示尚未开始交易日或估值溢出。
func (e *Engine) riskSnapshotLocked() (day, loss, limit int64, restricted, ok bool) {
	if e.risk == nil {
		return 0, 0, 0, false, false
	}
	net, ok := e.valuationLocked()
	if !ok {
		return e.risk.day, 0, e.risk.limit, e.risk.restricted, false
	}
	return e.risk.day, lossLocked(e.risk.baseline, net), e.risk.limit, e.risk.restricted, true
}

// triggerRiskLocked 执行触线处理：标记限制、记录触线事件（含引发方式、
// 关联报价/成交与各持仓合约估值快照），并按订单编号从大到小撤销全部
// 有效买单的未成交部分。调用时须持有锁；重复触线不产生新记录。
func (e *Engine) triggerRiskLocked(kind RiskTriggerKind, quoteSeq, tradeID int64) {
	r := e.risk
	if r == nil || r.restricted {
		return
	}
	r.restricted = true

	net, ok := e.valuationLocked()
	if !ok {
		net = 0 // 报价与成交路径已保证不溢出，此处兜底
	}
	loss := lossLocked(r.baseline, net)

	// 汇总全部有效买单，按订单编号从大到小撤销。
	ids := make([]int64, 0)
	for _, st := range e.symbols {
		ids = append(ids, st.buyOrderIDs...)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })

	e.appendRecord(Record{
		Kind:         RecordRiskTriggered,
		RiskDay:      r.day,
		RiskBaseline: r.baseline,
		RiskNetValue: net,
		RiskLoss:     loss,
		RiskLimit:    r.limit,
		RiskTrigger:  kind,
		RiskQuoteSeq: quoteSeq,
		RiskTradeID:  tradeID,
		Valuations:   e.valuationsLocked(),
	}, nil)

	for _, id := range ids {
		o := e.orders[id]
		if o == nil || o.Status == StatusFilled || o.Status == StatusCanceled || o.Remaining() <= 0 {
			continue
		}
		e.cancelLocked(o, "日内亏损触线，撤销剩余买单")
		rec := &e.records[len(e.records)-1]
		rec.RiskDay = r.day
		rec.RiskLoss = loss
		rec.RiskLimit = r.limit
	}
}
