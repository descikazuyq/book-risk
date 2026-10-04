package book

import (
	"strings"
	"testing"
)

// 本文件为行情缺口期间“重复提交同序号报价”补充回归保障：已经进入等待的
// 报价不能被后来的同序号数据覆盖——相同内容幂等返回 0，不同内容报内容冲突
// 并保留原先等待的数据；等待与冲突都不得提前改变订单状态、资金占用、持仓
// 金额或事件记录。缺口补齐后报价按序号逐条生效，最终必须保留最初收到的
// 等待报价；若正是该报价使账户总持仓金额超限，撤单记录必须锚定最初等待的
// 原始报价定位，不能使用曾被冲突拒绝的数据。
//
// 共用前置状态（pendingConflictSetup）：
// 初始现金 1000；A 合约持仓限额充足；seq1（时刻 10，价格 10）已生效。
// 一笔 6 股限价 10 的买单成交 2 股 @10 后：现金 980、持仓 2，买单剩余 4 股
// 占用现金 40；账户总持仓金额上限设为 100：持仓金额 20 + 买单占用 40 = 60。
// 日内亏损保护不开启，设置上限时不撤单。
func pendingConflictSetup(t *testing.T) (e *Engine, bid int64, recsBase int) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 1000) // 合约持仓限额充足，撤单只能来自账户金额上限
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	bid = mustBuy(t, e, "A", 6, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetPositionAmountLimit(100); err != nil {
		t.Fatal(err)
	}

	if e.Cash() != 980 || e.ReservedCash() != 40 || e.AvailableCash() != 940 || e.Position("A") != 2 {
		t.Fatalf("前置资金状态错误: cash=%d reserved=%d available=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	am := e.PositionAmountStatus()
	if !am.Enabled || am.Limit != 100 || am.Holding != 20 || am.BuyReserved != 40 || am.Total != 60 {
		t.Fatalf("前置金额状态错误: %+v", am)
	}
	if o, _ := e.Order(bid); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 4 {
		t.Fatalf("前置订单应为部分成交、剩余 4 股: %+v", o)
	}
	return e, bid, len(e.Records())
}

// assertWaitingInvariant 校验等待/冲突期间任何请求都不得改变业务状态与事件记录。
func assertWaitingInvariant(t *testing.T, e *Engine, bid int64, recsBase int) {
	t.Helper()
	q, ok := e.CurrentQuote("A")
	if !ok || q != (Quote{Seq: 1, Moment: 10, Price: 10}) {
		t.Fatalf("等待期间最新已生效报价必须停在 seq1（时刻10,价格10）: %+v ok=%v", q, ok)
	}
	if !e.HasGap("A") {
		t.Fatal("等待期间缺口必须保持存在")
	}
	if o, _ := e.Order(bid); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 4 {
		t.Fatalf("等待/冲突不得提前改变订单状态: %+v", o)
	}
	if e.Cash() != 980 || e.ReservedCash() != 40 || e.AvailableCash() != 940 || e.Position("A") != 2 {
		t.Fatalf("等待/冲突不得提前改变资金或持仓: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	am := e.PositionAmountStatus()
	if am.Holding != 20 || am.BuyReserved != 40 || am.Total != 60 {
		t.Fatalf("等待/冲突不得提前改变金额占用: %+v", am)
	}
	if recs := e.Records(); len(recs) != recsBase {
		t.Fatalf("等待/冲突不得新增事件记录: 基础 %d 条，现有 %d 条", recsBase, len(recs))
	}
}

// TestPendingQuoteDuplicateConflictKeptAndUsedOnGapFill 覆盖完整场景：
// seq1@（时刻10,价格10）为最新有效报价时先收到 seq3@（时刻30,价格20）进入
// 等待；相同内容重放成功返回 0，仅改价格或仅改时刻的请求报内容冲突并返回 0，
// 冲突后再提交原内容仍按相同数据处理。随后补交 seq2@（时刻20,价格10），2 条
// 报价依次生效且最终保留最初收到的 seq3@（时刻30,价格20）；该报价使持仓
// 金额 40 + 买单占用 80 = 120 超过上限 100，撤销买单全部剩余 4 股并释放 40
// 现金占用。补齐后重放原 seq3 仍返回 0，不再产生撤销或记录。
func TestPendingQuoteDuplicateConflictKeptAndUsedOnGapFill(t *testing.T) {
	e, bid, recsBase := pendingConflictSetup(t)

	// 跨号 seq3 先到：提交成功但 0 条生效，最新报价停在 seq1，缺口保持。
	n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 30, Price: 20})
	if err != nil || n != 0 {
		t.Fatalf("跨号 seq3 应成功并返回 0 条生效: n=%d err=%v", n, err)
	}
	assertWaitingInvariant(t, e, bid, recsBase)

	// 完全相同的 seq3 再次提交：成功返回 0，不替换、不新增记录。
	if n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 30, Price: 20}); err != nil || n != 0 {
		t.Fatalf("相同内容的等待报价重放必须成功返回 0: n=%d err=%v", n, err)
	}
	assertWaitingInvariant(t, e, bid, recsBase)

	// 仅把价格改为 10：内容冲突，返回 0；原先等待的 (时刻30,价格20) 必须保留。
	if n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 30, Price: 10}); err == nil || n != 0 {
		t.Fatalf("同序号不同价格必须报内容冲突并返回 0: n=%d err=%v", n, err)
	} else if !strings.Contains(err.Error(), "内容冲突") || !strings.Contains(err.Error(), "等待中") {
		t.Fatalf("冲突错误必须指明等待中的报价内容冲突: %v", err)
	}
	assertWaitingInvariant(t, e, bid, recsBase)

	// 仅把时刻改为 99：同样内容冲突、返回 0、保留原等待数据。
	if n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 99, Price: 20}); err == nil || n != 0 {
		t.Fatalf("同序号不同时刻必须报内容冲突并返回 0: n=%d err=%v", n, err)
	}
	assertWaitingInvariant(t, e, bid, recsBase)

	// 冲突后再提交原内容：仍被视为相同数据（成功返回 0），冲突请求不是替换报价。
	if n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 30, Price: 20}); err != nil || n != 0 {
		t.Fatalf("冲突后重放原内容必须仍按相同数据处理: n=%d err=%v", n, err)
	}
	assertWaitingInvariant(t, e, bid, recsBase)

	// 补交缺失的 seq2：seq2@（时刻20,价格10）与等待的 seq3@（时刻30,价格20）
	// 依次生效，共 2 条；最终报价必须是最初收到的 seq3。
	recsBefore := len(e.Records())
	n, err = e.UpdateQuote("A", Quote{Seq: 2, Moment: 20, Price: 10})
	if err != nil {
		t.Fatalf("补齐缺口必须成功: %v", err)
	}
	if n != 2 {
		t.Fatalf("应有 2 条报价依次生效（seq2、seq3），实际 %d", n)
	}
	q, ok := e.CurrentQuote("A")
	if !ok || q != (Quote{Seq: 3, Moment: 30, Price: 20}) {
		t.Fatalf("最终报价必须保留最初收到的 seq3（时刻30,价格20），不得被冲突数据替换: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("补齐后缺口必须消失")
	}

	// seq3@20 生效后：持仓金额 2×20=40，买单剩余 4×max(限价10,报价20)=80，
	// 合计 120 > 上限 100，撤销买单全部剩余量并释放 40 现金占用。
	o, _ := e.Order(bid)
	if o.Status != StatusCanceled || o.Filled != 2 || o.Remaining() != 0 {
		t.Fatalf("超限买单应撤销全部剩余 4 股、保留已成交 2 股: %+v", o)
	}
	if !strings.Contains(o.Reason, "金额上限") || !strings.Contains(o.Reason, "超限") ||
		!strings.Contains(o.Reason, "报价序号 3") || !strings.Contains(o.Reason, "时刻 30") ||
		!strings.Contains(o.Reason, "价格 20") {
		t.Fatalf("撤单原因必须说明金额超限并锚定最初等待的 seq3（时刻30,价格20）: %q", o.Reason)
	}
	if e.Cash() != 980 || e.ReservedCash() != 0 || e.AvailableCash() != 980 || e.Position("A") != 2 {
		t.Fatalf("撤单只释放剩余量现金占用，现金余额与已入账 2 股持仓保留: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	am := e.PositionAmountStatus()
	if !am.Enabled || am.Limit != 100 || am.Holding != 40 || am.BuyReserved != 0 || am.Total != 40 {
		t.Fatalf("补齐撤单后金额状态应为持仓 40、买单占用 0: %+v", am)
	}

	// 本批只新增一条撤销记录（seq2@10 下合计 60 不超限，不产生记录）。
	recs := e.Records()
	newRecs := recs[recsBefore:]
	if len(newRecs) != 1 {
		t.Fatalf("补齐本批应只新增一条撤单记录，实际 %d 条", len(newRecs))
	}
	cr := newRecs[0]
	if cr.Kind != RecordCanceled || cr.OrderID != bid || cr.Side != Buy ||
		cr.Filled != 2 || cr.Remaining != 4 || cr.Qty != 6 || cr.Limit != 10 {
		t.Fatalf("唯一新增记录必须是该买单剩余 4 股的撤销记录: %+v", cr)
	}
	// 撤销前两类金额、合计与上限必须固化：40 + 80 = 120 > 100。
	if !cr.AmtEnabled || cr.AmtLimit != 100 || cr.AmtHolding != 40 ||
		cr.AmtBuyReserved != 80 || cr.AmtTotal != 120 {
		t.Fatalf("撤单记录应保存撤销前金额 40/80、合计 120 与上限 100: %+v", cr)
	}
	// 报价定位必须是最初收到并等待的 seq3@（时刻30,价格20），
	// 不能使用曾被冲突拒绝的 (时刻30,价格10) 或 (时刻99,价格20)。
	if !cr.QuoteValid || cr.QuoteSeq != 3 || cr.QuoteMoment != 30 || cr.QuotePrice != 20 {
		t.Fatalf("撤单记录报价快照应锚定最初等待的 seq3（时刻30,价格20）: %+v", cr)
	}
	if len(cr.AmtQuoteRefs) != 1 ||
		cr.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 3, Moment: 30, Price: 20}) {
		t.Fatalf("撤单金额报价定位必须锚定最初等待的 seq3: %+v", cr.AmtQuoteRefs)
	}
	if cr.RiskDay != 0 {
		t.Fatalf("未开启日内亏损保护时撤单记录不得携带风险快照: %+v", cr)
	}
	for _, r := range recs[recsBase:] {
		if r.Kind == RecordRejected {
			t.Fatalf("全过程不得留下拒绝记录（含等待期间的内容冲突）: %+v", r)
		}
	}

	// 补齐后再次提交原 seq3：已是已生效报价，相同内容返回 0，
	// 不再产生撤销或任何记录，资金、持仓与订单保持。
	if n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 30, Price: 20}); err != nil || n != 0 {
		t.Fatalf("补齐后重放原 seq3 必须成功返回 0: n=%d err=%v", n, err)
	}
	if len(e.Records()) != len(recs) {
		t.Fatal("补齐后重放 seq3 不得新增任何记录")
	}
	if o2, _ := e.Order(bid); o2.Status != StatusCanceled || o2.Filled != 2 {
		t.Fatalf("重放不得再次改变订单: %+v", o2)
	}
	if e.Cash() != 980 || e.ReservedCash() != 0 || e.Position("A") != 2 {
		t.Fatalf("重放不得改变资金与持仓: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
}

// TestPendingQuoteConflictRejectedDataNeverReplacesWaitingQuote 在无订单、无限额
// 的最小构造下单独锁定报价语义：对等待中的同序号报价，改价或改时刻的冲突
// 请求都只是被拒绝，真正决定补齐链内容的始终是第一次等待的数据。
func TestPendingQuoteConflictRejectedDataNeverReplacesWaitingQuote(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	recsBase := len(e.Records())

	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 20}, 0)

	// 两组冲突数据先后被拒。
	for _, q := range []Quote{
		{Seq: 3, Moment: 30, Price: 10}, // 仅价格不同
		{Seq: 3, Moment: 99, Price: 20}, // 仅时刻不同
	} {
		if n, err := e.UpdateQuote("A", q); err == nil || n != 0 {
			t.Fatalf("冲突报价 %+v 必须报错并返回 0: n=%d err=%v", q, n, err)
		}
	}

	// 最新报价仍停在 seq1，缺口保持，事件记录一条不多。
	q, ok := e.CurrentQuote("A")
	if !ok || q.Seq != 1 || q.Moment != 10 || q.Price != 10 {
		t.Fatalf("冲突请求不得改变最新已生效报价: %+v ok=%v", q, ok)
	}
	if !e.HasGap("A") {
		t.Fatal("冲突请求不得消除缺口")
	}
	if len(e.Records()) != recsBase {
		t.Fatalf("等待重放与内容冲突都不得新增事件记录: 基础 %d，现有 %d",
			recsBase, len(e.Records()))
	}

	// 补齐 seq2：必须按最初等待的 (时刻30,价格20) 推进，而不是任一冲突数据。
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 20, Price: 10})
	if err != nil || n != 2 {
		t.Fatalf("补齐应使 seq2/seq3 依次生效: n=%d err=%v", n, err)
	}
	q, ok = e.CurrentQuote("A")
	if !ok || q != (Quote{Seq: 3, Moment: 30, Price: 20}) {
		t.Fatalf("补齐后必须保留最初等待的 seq3（时刻30,价格20）: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("补齐后缺口必须消失")
	}
}
