package book

import (
	"strings"
	"testing"
)

// 本文件为“已生效序号的旧报价被重复/冲突提交”补充回归保障：行情已推进到
// 更大序号后，晚到的同序号旧报价——完全相同的必须幂等成功返回 0 条新生效；
// 只改价格或只改时刻的必须报内容冲突并返回 0 条。两类提交都不得让最新报价
// 回退、不得把冲突数据用于风险重算（不限制买入、不撤单、不释放占用）、不得
// 新增事件记录，已有成交记录保持首次入账时的事实；冲突被拒后不留下新的历史
// 值，再提交原始旧报价仍按相同内容成功返回 0。
//
// 共用前置状态（appliedConflictSetup）：
// 初始现金 1000；A 合约持仓限额充足；seq1（时刻 10，价格 10）已生效；
// 6 股限价 10 的买单成交 2 股 @10；随后开启亏损上限 5 的交易日 1，设置账户
// 总持仓金额上限 100，再让 seq2（时刻 20，价格 12）生效。此时：
// 现金 980、买单占用 40、持仓 2；持仓金额 24、买单金额占用 48、合计 72；
// 日内基准净值 1000、当前净值 1004、亏损 0；订单部分成交（已成交 2、剩余 4）。
func appliedConflictSetup(t *testing.T) (e *Engine, bid int64, recsBase int) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 1000)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	bid = mustBuy(t, e, "A", 6, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	if err := e.StartTradingDay(1, 5); err != nil {
		t.Fatalf("StartTradingDay 意外错误: %v", err)
	}
	if _, err := e.SetPositionAmountLimit(100); err != nil {
		t.Fatalf("SetPositionAmountLimit 意外错误: %v", err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 20, Price: 12}, 1)

	assertAppliedConflictInvariant(t, e, bid)
	return e, bid, len(e.Records())
}

// assertAppliedConflictInvariant 校验 seq2 生效后的资金、持仓、订单与两类风险查询。
func assertAppliedConflictInvariant(t *testing.T, e *Engine, bid int64) {
	t.Helper()
	if e.Cash() != 980 || e.ReservedCash() != 40 || e.AvailableCash() != 940 || e.Position("A") != 2 {
		t.Fatalf("资金或持仓状态错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	if o, _ := e.Order(bid); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 4 {
		t.Fatalf("订单应为部分成交、已成交 2、剩余 4: %+v", o)
	}
	rs := e.RiskStatus()
	if !rs.Open || rs.Day != 1 || rs.Baseline != 1000 || rs.Equity != 1004 ||
		rs.Loss != 0 || rs.LossLimit != 5 || rs.Restricted {
		t.Fatalf("日内亏损查询应保持基准 1000、净值 1004、亏损 0、未限制: %+v", rs)
	}
	am := e.PositionAmountStatus()
	if !am.Enabled || am.Limit != 100 || am.Holding != 24 || am.BuyReserved != 48 || am.Total != 72 {
		t.Fatalf("账户金额查询应保持持仓 24、买单占用 48、合计 72: %+v", am)
	}
	q, ok := e.CurrentQuote("A")
	if !ok || q != (Quote{Seq: 2, Moment: 20, Price: 12}) {
		t.Fatalf("最新报价必须停留在 seq2（时刻20,价格12），不得回退: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("不应出现行情缺口")
	}
}

// assertNoNewRecords 校验重复/冲突提交不新增任何事件记录（不撤单、不拒绝、不触线）。
func assertNoNewRecords(t *testing.T, e *Engine, recsBase int) {
	t.Helper()
	recs := e.Records()
	if len(recs) != recsBase {
		t.Fatalf("重复/冲突提交不得新增事件记录: 基础 %d 条，现有 %d 条", recsBase, len(recs))
	}
}

// TestAppliedQuoteDuplicateAndConflictKeepNewestQuote 覆盖完整场景：
// seq2 已生效后，重放 seq1 原始内容成功返回 0；只改价格（低到足以触发日内
// 亏损上限、高到足以超出账户金额上限）或只改时刻的 seq1 都只报内容冲突。
// 全程资金、持仓、累计成交量、订单剩余量与两类风险查询保持不变，不新增事件
// 记录，已有成交记录保留首次入账时的成交价与报价定位。冲突之后再提交原始
// seq1 仍按相同内容成功返回 0；最后提交 seq3（时刻 30，价格 11）只有 1 条
// 生效，持仓金额 22、买单占用 44、合计 66、净值 1002，订单保持已成交 2、
// 剩余 4。
func TestAppliedQuoteDuplicateAndConflictKeepNewestQuote(t *testing.T) {
	e, bid, recsBase := appliedConflictSetup(t)

	// 完全相同的 seq1 再次提交：成功返回 0 条新生效，状态与记录不变。
	if n, err := e.UpdateQuote("A", Quote{Seq: 1, Moment: 10, Price: 10}); err != nil || n != 0 {
		t.Fatalf("相同内容的已生效报价重放必须成功返回 0: n=%d err=%v", n, err)
	}
	assertAppliedConflictInvariant(t, e, bid)
	assertNoNewRecords(t, e, recsBase)

	// 只改价格且低到足以触发日内亏损（价格 1 时净值 982、亏损 18 ≥ 上限 5）：
	// 只能报内容冲突，不得限制买入、撤销剩余买单或释放占用。
	assertAppliedConflictRejected(t, e, bid, recsBase, Quote{Seq: 1, Moment: 10, Price: 1})

	// 只改价格且高到足以超出账户金额上限（价格 60 时持仓 120 + 买单占用 240 > 100）：
	// 同样只能报内容冲突，不得收敛撤单。
	assertAppliedConflictRejected(t, e, bid, recsBase, Quote{Seq: 1, Moment: 10, Price: 60})

	// 只改时刻：价格不变也不能通过，同样只报内容冲突。
	assertAppliedConflictRejected(t, e, bid, recsBase, Quote{Seq: 1, Moment: 99, Price: 10})

	// 冲突之后再提交原始 seq1：先前拒绝的内容不得留成新的历史值，
	// 仍按相同内容成功返回 0。
	if n, err := e.UpdateQuote("A", Quote{Seq: 1, Moment: 10, Price: 10}); err != nil || n != 0 {
		t.Fatalf("冲突后重放原始旧报价必须仍按相同内容成功返回 0: n=%d err=%v", n, err)
	}
	assertAppliedConflictInvariant(t, e, bid)
	assertNoNewRecords(t, e, recsBase)

	// 已有成交记录继续保留首次入账时的事实：成交价 10、报价定位为 seq1（时刻10,价格10）。
	var fillRec *Record
	for i, r := range e.Records() {
		if r.Kind == RecordFilled {
			fillRec = &e.Records()[i]
		}
	}
	if fillRec == nil {
		t.Fatal("必须存在成交记录")
	}
	if fillRec.TradePrice != 10 || fillRec.Qty != 2 || fillRec.Filled != 2 || fillRec.Remaining != 4 {
		t.Fatalf("成交记录必须保留首次入账的成交价与数量: %+v", *fillRec)
	}
	if !fillRec.QuoteValid || fillRec.QuoteSeq != 1 || fillRec.QuoteMoment != 10 || fillRec.QuotePrice != 10 {
		t.Fatalf("成交记录的报价定位必须保留首次入账时的 seq1（时刻10,价格10）: %+v", *fillRec)
	}

	// 正常推进 seq3（时刻 30，价格 11）：恰好 1 条新生效。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 11}, 1)
	if e.Cash() != 980 || e.ReservedCash() != 40 || e.Position("A") != 2 {
		t.Fatalf("seq3 生效后资金与持仓不变: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if o, _ := e.Order(bid); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 4 {
		t.Fatalf("seq3 生效后订单应保留已成交 2、剩余 4: %+v", o)
	}
	rs := e.RiskStatus()
	if !rs.Open || rs.Baseline != 1000 || rs.Equity != 1002 || rs.Loss != 0 || rs.Restricted {
		t.Fatalf("seq3 生效后净值应为 1002、亏损 0、未限制: %+v", rs)
	}
	am := e.PositionAmountStatus()
	if !am.Enabled || am.Holding != 22 || am.BuyReserved != 44 || am.Total != 66 {
		t.Fatalf("seq3 生效后应为持仓金额 22、买单占用 44、合计 66: %+v", am)
	}
}

// assertAppliedConflictRejected 校验对已生效序号的冲突提交：报内容冲突错误、
// 返回 0 条新生效，业务状态、两类风险查询与事件记录全部保持不变。
func assertAppliedConflictRejected(t *testing.T, e *Engine, bid int64, recsBase int, q Quote) {
	t.Helper()
	n, err := e.UpdateQuote("A", q)
	if err == nil || n != 0 {
		t.Fatalf("已生效序号的冲突报价 %+v 必须报内容冲突并返回 0: n=%d err=%v", q, n, err)
	}
	if !strings.Contains(err.Error(), "内容冲突") {
		t.Fatalf("冲突错误必须指明内容冲突: %v", err)
	}
	if strings.Contains(err.Error(), "等待中") {
		t.Fatalf("已生效序号的冲突不得误报为等待中的报价: %v", err)
	}
	assertAppliedConflictInvariant(t, e, bid)
	assertNoNewRecords(t, e, recsBase)
}
