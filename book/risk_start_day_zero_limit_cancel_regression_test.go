package book

import (
	"strings"
	"testing"
)

// 本文件为日内亏损保护补充回归保障：调用方“首次开启交易日”且把亏损上限设为零时，
// 即使行情没有下跌、当前亏损为零，也必须立即限制买入并处理开日前已经接受的有效
// 买单——重点覆盖账户已有部分成交买单、其他合约挂单买单与有效卖单同时存在的情况。
//
// 关键构造：初始现金 1000；A、B 两个合约最大持仓量均为 100，最新已生效报价均为
// 序号 1（A：时刻 10、价格 10；B：时刻 20、价格 20）。先接受 A 买单（编号 1，
// 总量 10、限价 10），其中 4 份按价格 9 成交（cash=964，A 持仓 4，剩余 6 份占用
// 60 现金）；再接受 B 买单（编号 2，总量 3、限价 20，占用 60 现金，B 无持仓）；
// 最后接受 A 卖单（编号 3，数量 2、限价 9，占用 2 股可卖数量，A 可卖 2）。
// 此时现金 964、两张买单各占用 60 现金（合计 120），账户尚未开启交易日，账户总
// 持仓金额上限也未启用。
//
// 以日号 1、亏损上限 0 开日：基准净值与当前净值均为 964 现金 + A 持仓 4×10 =
// 1004，亏损为 0；0 >= 0 立即触线，买入限制即刻生效。先追加一条来源为“开日”的
// 触线记录（估值报价只包含有持仓的 A，只有挂单的 B 不得进入持仓估值），再按买单
// 编号从大到小撤销：先撤编号 2 的 B 买单剩余 3 份，再撤编号 1 的 A 买单剩余 6 份；
// 编号更大的 A 卖单继续有效。现金仍为 964、A 持仓仍为 4、A 买单累计成交仍为 4，
// 占用现金降为 0、可用现金变为 964，A 可卖数量仍为 2，已经入账的成交不重新计价。
//
// 边界：同样的开日前状态使用正数上限 1 时零亏损不触线，不新增触线或撤销记录，
// 订单与占用保持原值；负亏损上限必须报错，账户仍未开日，不撤单、不释放占用、
// 不新增记录。

// startDayZeroLimitSetup 构造“首次开日且零上限”的共用前置状态，返回引擎与
// A 买单、B 买单、A 卖单的编号（依次为 1、2、3）。
func startDayZeroLimitSetup(t *testing.T) (e *Engine, buyA, buyB, sellA int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 20}, 1)

	buyA = mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: buyA, Symbol: "A", Side: Buy, Price: 9, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	buyB = mustBuy(t, e, "B", 3, 20)
	sellA = mustSell(t, e, "A", 2, 9)

	// 现金 964；A 买单剩余 6×10 与 B 买单 3×20 各占用 60；A 持仓 4，卖单占用 2。
	if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 ||
		e.Position("A") != 4 || e.Position("B") != 0 || e.Sellable("A") != 2 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d available=%d posA=%d posB=%d sellableA=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(),
			e.Position("A"), e.Position("B"), e.Sellable("A"))
	}
	o, _ := e.Order(buyA)
	if o.Status != StatusPartial || o.Qty != 10 || o.Filled != 4 || o.Remaining() != 6 {
		t.Fatalf("A 买单应为部分成交、累计成交 4、剩余 6: %+v", o)
	}
	o, _ = e.Order(buyB)
	if o.Status != StatusPending || o.Qty != 3 || o.Filled != 0 || o.Remaining() != 3 {
		t.Fatalf("B 买单应待成交、剩余 3: %+v", o)
	}
	o, _ = e.Order(sellA)
	if o.Status != StatusPending || o.Qty != 2 || o.Limit != 9 || o.Remaining() != 2 {
		t.Fatalf("A 卖单应保持有效、占用 2 股: %+v", o)
	}
	return e, buyA, buyB, sellA
}

// TestRiskStartDayZeroLimitRestrictsAndCancelsPendingBuys 覆盖主路径：零上限在首次
// 开日时立即触线（亏损为 0），先固化开日触线记录（持仓估值只含 A，不含只有挂单的
// B），再按买单编号从大到小撤销 B 买单与 A 买单剩余部分，卖单与已入账成交保留；
// 开日后新买单即使其他条件全部满足也被风险保护拒绝。
func TestRiskStartDayZeroLimitRestrictsAndCancelsPendingBuys(t *testing.T) {
	e, buyA, buyB, sellA := startDayZeroLimitSetup(t)

	recsBefore := len(e.Records())
	if err := e.StartTradingDay(1, 0); err != nil {
		t.Fatalf("零上限合法开日必须成功: %v", err)
	}

	// 基准净值与当前净值都是 1004：现金 964 + A 持仓 4×seq1 报价 10；买单冻结的
	// 120 现金属于余额不重复扣除；B 只有挂单没有持仓，不贡献持仓市值。
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 {
		t.Fatalf("交易日应已开启: %+v", st)
	}
	if st.Baseline != 1004 || st.Equity != 1004 || st.Loss != 0 ||
		st.LossLimit != 0 || !st.Restricted {
		t.Fatalf("零上限开日应得到基准与净值 1004、亏损 0 并立即限制: %+v", st)
	}

	// 本次开日只新增三条记录：触线、撤 B 买单、撤 A 买单（按编号从大到小）。
	newRecs := e.Records()[recsBefore:]
	if len(newRecs) != 3 {
		t.Fatalf("开日应只新增 3 条记录（1 触线 + 2 撤单），实际 %d 条", len(newRecs))
	}

	trig := newRecs[0]
	if trig.Kind != RecordRiskTriggered {
		t.Fatalf("第一条新增记录必须是触线记录: %+v", trig)
	}
	if trig.RiskTrigger != RiskTriggerStartDay || trig.RiskRefSeq != 0 || trig.RiskRefMoment != 0 {
		t.Fatalf("触线来源必须是开日且无报价/成交关联编号: trigger=%s ref=%d moment=%d",
			trig.RiskTrigger, trig.RiskRefSeq, trig.RiskRefMoment)
	}
	if trig.RiskDay != 1 || trig.RiskBaseline != 1004 || trig.RiskEquity != 1004 ||
		trig.RiskLoss != 0 || trig.RiskLimit != 0 || !trig.RiskRestrict {
		t.Fatalf("触线记录必须保存日号 1、净值 1004、亏损 0、上限 0 与已限制状态: %+v", trig)
	}
	// 估值报价只包含有持仓的 A；只有挂单的 B 不能算进持仓估值。
	if len(trig.RiskQuoteRefs) != 1 ||
		trig.RiskQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 1, Moment: 10, Price: 10}) {
		t.Fatalf("触线估值报价只能包含持仓的 A@seq1(10,10)，不得包含只有挂单的 B: %+v",
			trig.RiskQuoteRefs)
	}

	// 先撤编号更大的 B 买单（编号 2）剩余 3 份。
	crB := newRecs[1]
	if crB.Kind != RecordCanceled || crB.OrderID != buyB || crB.Symbol != "B" ||
		crB.Side != Buy || crB.Limit != 20 || crB.Qty != 3 || crB.Filled != 0 || crB.Remaining != 3 {
		t.Fatalf("第二条记录必须是撤销 B 买单剩余 3 份: %+v", crB)
	}
	if !crB.QuoteValid || crB.QuoteSeq != 1 || crB.QuoteMoment != 20 || crB.QuotePrice != 20 {
		t.Fatalf("B 撤单记录报价快照应锚定 B seq1(20,20): %+v", crB)
	}
	if crB.RiskDay != 1 || crB.RiskBaseline != 1004 || crB.RiskEquity != 1004 ||
		crB.RiskLoss != 0 || crB.RiskLimit != 0 || !crB.RiskRestrict {
		t.Fatalf("B 撤单记录必须保存开日时的风险快照（已限制）: %+v", crB)
	}

	// 再撤编号 1 的 A 买单剩余 6 份，已成交 4 份保留。
	crA := newRecs[2]
	if crA.Kind != RecordCanceled || crA.OrderID != buyA || crA.Symbol != "A" ||
		crA.Side != Buy || crA.Limit != 10 || crA.Qty != 10 || crA.Filled != 4 || crA.Remaining != 6 {
		t.Fatalf("第三条记录必须是撤销 A 买单剩余 6 份、已成交 4 份保留: %+v", crA)
	}
	if !crA.QuoteValid || crA.QuoteSeq != 1 || crA.QuoteMoment != 10 || crA.QuotePrice != 10 {
		t.Fatalf("A 撤单记录报价快照应锚定 A seq1(10,10): %+v", crA)
	}
	if crA.RiskDay != 1 || crA.RiskBaseline != 1004 || crA.RiskEquity != 1004 ||
		crA.RiskLoss != 0 || crA.RiskLimit != 0 || !crA.RiskRestrict {
		t.Fatalf("A 撤单记录必须保存开日时的风险快照（已限制）: %+v", crA)
	}

	if countRiskRecords(e) != 1 {
		t.Fatal("一个交易日只能有一条触线记录")
	}

	// 两张买单均已撤销；编号更大的 A 卖单继续有效。
	o, _ := e.Order(buyA)
	if o.Status != StatusCanceled || o.Qty != 10 || o.Filled != 4 || o.Remaining() != 0 {
		t.Fatalf("A 买单应已撤销、累计成交保留 4: %+v", o)
	}
	if !strings.Contains(o.Reason, "亏损 0") || !strings.Contains(o.Reason, "上限 0") {
		t.Fatalf("A 买单撤单原因必须固化亏损 0 与零上限: %q", o.Reason)
	}
	o, _ = e.Order(buyB)
	if o.Status != StatusCanceled || o.Qty != 3 || o.Filled != 0 || o.Remaining() != 0 {
		t.Fatalf("B 买单应已撤销、无成交: %+v", o)
	}
	o, _ = e.Order(sellA)
	if o.Status != StatusPending || o.Qty != 2 || o.Limit != 9 || o.Remaining() != 2 {
		t.Fatalf("A 卖单不得被风险保护撤销，必须继续有效: %+v", o)
	}

	// 现金余额、A 持仓与 A 买单累计成交不变；买单占用全部释放；卖单占用保留。
	if e.Cash() != 964 || e.ReservedCash() != 0 || e.AvailableCash() != 964 ||
		e.Position("A") != 4 || e.Sellable("A") != 2 {
		t.Fatalf("撤单只释放买单占用：现金 964、持仓 4、可卖 2 不变，占用归零: "+
			"cash=%d reserved=%d available=%d posA=%d sellableA=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
	}

	// 此前的成交记录保持原值：成交价 9、数量 4、累计 4，报价快照停留在成交时的
	// A seq1(10,10)；成交发生在开日前，不得事后补写风险快照或按现价重新计价。
	var fillRec Record
	for _, r := range e.Records() {
		if r.Kind == RecordFilled && r.TradeID == 1 {
			fillRec = r
		}
	}
	if fillRec.TradeID != 1 {
		t.Fatal("必须能找到成交编号 1 的原成交记录")
	}
	if fillRec.OrderID != buyA || fillRec.TradePrice != 9 || fillRec.Qty != 4 || fillRec.Filled != 4 {
		t.Fatalf("原成交内容必须保持不重新计价: %+v", fillRec)
	}
	if !fillRec.QuoteValid || fillRec.QuoteSeq != 1 || fillRec.QuoteMoment != 10 || fillRec.QuotePrice != 10 {
		t.Fatalf("原成交记录报价快照必须保留成交时的 A seq1(10,10): %+v", fillRec)
	}
	if fillRec.RiskDay != 0 || fillRec.RiskRestrict {
		t.Fatalf("开日前的成交记录不得被事后补写风险快照: %+v", fillRec)
	}

	// 开日后其他下单条件均满足的新买单仍必须因风险保护被拒绝（不生成订单、不占用）。
	ordersBefore := len(e.Orders())
	if _, err := e.Buy("A", 1, 10); err == nil {
		t.Fatal("零上限开日触线后，新买单即使现金、报价与持仓限额都满足也必须拒绝")
	} else if !strings.Contains(err.Error(), "日内亏损保护已触发") {
		t.Fatalf("拒单原因必须来自日内亏损保护: %v", err)
	}
	if len(e.Orders()) != ordersBefore {
		t.Fatal("被风险保护拒绝的买单不得生成订单")
	}
	rej := e.Records()[len(e.Records())-1]
	if rej.Kind != RecordRejected || rej.Symbol != "A" || rej.RiskDay != 1 ||
		rej.RiskEquity != 1004 || rej.RiskLoss != 0 || !rej.RiskRestrict {
		t.Fatalf("新买单拒绝记录必须固化开日后已限制的风险快照: %+v", rej)
	}
	if e.Cash() != 964 || e.ReservedCash() != 0 {
		t.Fatalf("拒绝新买单不得改动资金与占用: cash=%d reserved=%d", e.Cash(), e.ReservedCash())
	}
}

// TestRiskStartDayPositiveLimitWithZeroLossDoesNotTrigger 覆盖正数上限边界：
// 同样的开日前状态以上限 1 开日，零亏损不触线，不新增触线或撤销记录，订单与
// 占用保持原值；保护未限制时新买单可正常接受。
func TestRiskStartDayPositiveLimitWithZeroLossDoesNotTrigger(t *testing.T) {
	e, buyA, buyB, sellA := startDayZeroLimitSetup(t)

	recsBefore := len(e.Records())
	if err := e.StartTradingDay(1, 1); err != nil {
		t.Fatalf("正数上限合法开日必须成功: %v", err)
	}
	if len(e.Records()) != recsBefore {
		t.Fatalf("零亏损开日不得新增任何记录，实际新增 %d 条", len(e.Records())-recsBefore)
	}
	st := e.RiskStatus()
	if !st.Open || st.Day != 1 || st.Baseline != 1004 || st.Equity != 1004 ||
		st.Loss != 0 || st.LossLimit != 1 || st.Restricted {
		t.Fatalf("零亏损低于正数上限 1，不应触线: %+v", st)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("零亏损正数上限开日不得产生触线记录")
	}

	// 订单与占用保持开日前原值。
	if o, _ := e.Order(buyA); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 6 {
		t.Fatalf("A 买单必须保持部分成交、剩余 6: %+v", o)
	}
	if o, _ := e.Order(buyB); o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("B 买单必须保持待成交、剩余 3: %+v", o)
	}
	if o, _ := e.Order(sellA); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("A 卖单必须保持有效: %+v", o)
	}
	if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 ||
		e.Position("A") != 4 || e.Sellable("A") != 2 {
		t.Fatalf("开日不得改动资金、占用、持仓与可卖数量: cash=%d reserved=%d available=%d posA=%d sellableA=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
	}

	// 保护未限制：其他条件满足的新买单可以接受。
	mustBuy(t, e, "A", 1, 10)
}

// TestRiskStartDayNegativeLimitRejectedWithoutSideEffects 覆盖负上限边界：负亏损
// 上限必须报错，账户仍未开日，不撤单、不释放占用、不新增记录。
func TestRiskStartDayNegativeLimitRejectedWithoutSideEffects(t *testing.T) {
	e, buyA, buyB, sellA := startDayZeroLimitSetup(t)

	recsBefore := len(e.Records())
	if err := e.StartTradingDay(1, -1); err == nil {
		t.Fatal("负亏损上限开日必须返回错误")
	} else if !strings.Contains(err.Error(), "亏损上限不能为负") {
		t.Fatalf("错误原因必须指向负亏损上限: %v", err)
	}
	if len(e.Records()) != recsBefore {
		t.Fatalf("非法开日不得新增记录，实际新增 %d 条", len(e.Records())-recsBefore)
	}
	if e.RiskStatus().Open {
		t.Fatal("负上限开日失败后账户必须仍未开日")
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("负上限开日失败不得产生触线记录")
	}

	// 不撤单、不释放占用：两张买单与卖单保持有效，资金占用原样保留。
	if o, _ := e.Order(buyA); o.Status != StatusPartial || o.Filled != 4 || o.Remaining() != 6 {
		t.Fatalf("非法开日不得改动 A 买单: %+v", o)
	}
	if o, _ := e.Order(buyB); o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("非法开日不得改动 B 买单: %+v", o)
	}
	if o, _ := e.Order(sellA); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("非法开日不得改动 A 卖单: %+v", o)
	}
	if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 ||
		e.Position("A") != 4 || e.Sellable("A") != 2 {
		t.Fatalf("非法开日不得改动资金、占用、持仓与可卖数量: cash=%d reserved=%d available=%d posA=%d sellableA=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
	}

	// 修正为零上限后仍应按主路径立即触线撤单。
	if err := e.StartTradingDay(1, 0); err != nil {
		t.Fatalf("非法尝试后合法零上限开日必须成功: %v", err)
	}
	st := e.RiskStatus()
	if !st.Open || !st.Restricted || st.Baseline != 1004 || st.Equity != 1004 || st.Loss != 0 {
		t.Fatalf("非法尝试后零上限开日仍应立即触线: %+v", st)
	}
	if o, _ := e.Order(buyA); o.Status != StatusCanceled || o.Filled != 4 || o.Remaining() != 0 {
		t.Fatalf("A 买单剩余 6 份必须被保护撤销、已成交 4 份保留: %+v", o)
	}
	if o, _ := e.Order(buyB); o.Status != StatusCanceled || o.Remaining() != 0 {
		t.Fatalf("B 买单必须被保护撤销: %+v", o)
	}
	if o, _ := e.Order(sellA); o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("A 卖单必须继续有效: %+v", o)
	}
	if e.Cash() != 964 || e.ReservedCash() != 0 || e.AvailableCash() != 964 ||
		e.Position("A") != 4 || e.Sellable("A") != 2 {
		t.Fatalf("触线撤单只释放买单占用: cash=%d reserved=%d available=%d posA=%d sellableA=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
	}
	if countRiskRecords(e) != 1 {
		t.Fatal("非法尝试不得占用触线记录，合法交易日只能有一条触线记录")
	}
}
