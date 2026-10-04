package book

import (
	"reflect"
	"strings"
	"testing"
)

// 本文件为“行情缺口期间重复提交报价”补充回归保障：已经进入等待的报价不能被
// 后来的同序号数据覆盖——同内容重复提交成功但 0 条生效；仅改价格或仅改时刻
// 必须报内容冲突且 0 条生效，原先等待的数据原样保留，冲突请求不能被当作替换
// 报价；冲突后再提交原内容仍按相同数据处理。等待与冲突期间不得提前改变订单
// 状态、现金/持仓占用、账户金额合计与事件记录。
//
// 补齐缺失序号后，等待链按序号连续生效，最终报价必须保留最初收到的那条
// （seq3、时刻 30、价格 20），而不是任何一次被拒绝的冲突内容；若该原始报价
// 使账户总持仓金额超限，撤销记录必须锚定 seq3 原始报价定位与撤销前两类金额。
// 补齐后再次重放原 seq3 仍返回 0，不再产生撤销或改写任何记录。

// assertGapWaitFrozen 在等待/冲突提交后断言“尚未生效”的全部可观察状态不变：
// 最新报价停在 seq1@(10,10)、缺口仍在、订单仍部分成交且剩余 4、资金与持仓
// 占用不变、金额合计仍为 seq1 口径的 60、事件记录条数不变。
func assertGapWaitFrozen(t *testing.T, e *Engine, bid int64, recsLen int, phase string) {
	t.Helper()
	q, ok := e.CurrentQuote("A")
	if !ok || q != (Quote{Seq: 1, Moment: 10, Price: 10}) {
		t.Fatalf("[%s] 最新报价必须停在 seq1/(10,10): %+v ok=%v", phase, q, ok)
	}
	if !e.HasGap("A") {
		t.Fatalf("[%s] 缺口必须保持存在", phase)
	}
	o, _ := e.Order(bid)
	if o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 4 {
		t.Fatalf("[%s] 等待/冲突不得提前改变订单状态: %+v", phase, o)
	}
	if e.Cash() != 980 || e.ReservedCash() != 40 || e.Position("A") != 2 {
		t.Fatalf("[%s] 等待/冲突不得改变资金与持仓: cash=%d reserved=%d pos=%d",
			phase, e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	am := e.PositionAmountStatus()
	if !am.Enabled || am.Limit != 100 || am.Holding != 20 || am.BuyReserved != 40 || am.Total != 60 {
		t.Fatalf("[%s] 等待中的报价不得参与金额计算: %+v", phase, am)
	}
	if recs := e.Records(); len(recs) != recsLen {
		t.Fatalf("[%s] 等待/冲突不得新增事件记录: 之前 %d 条，现在 %d 条",
			phase, recsLen, len(recs))
	}
}

// TestPendingQuoteDuplicateAndConflictDoNotReplaceWaitingQuote 不涉及订单与限额，
// 专门锁定等待报价本身的幂等/冲突语义：冲突不能替换等待值，补齐后生效的是
// 最初收到的 seq3（时刻 30、价格 20）。
func TestPendingQuoteDuplicateAndConflictDoNotReplaceWaitingQuote(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	// seq3 跨号先到：提交成功但 0 条生效，进入等待。
	n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 30, Price: 20})
	if err != nil || n != 0 {
		t.Fatalf("跨号 seq3 应成功返回 0 条生效: n=%d err=%v", n, err)
	}
	q, _ := e.CurrentQuote("A")
	if q.Seq != 1 || !e.HasGap("A") {
		t.Fatalf("等待报价不得推进最新报价: q=%+v gap=%v", q, e.HasGap("A"))
	}

	// 完全相同的 seq3 重复提交：仍成功返回 0。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 20}, 0)

	// 仅把价格改为 10：内容冲突，返回 0 和错误。
	if n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 30, Price: 10}); err == nil || n != 0 {
		t.Fatalf("仅改价格必须报内容冲突并返回 0: n=%d err=%v", n, err)
	} else if !strings.Contains(err.Error(), "内容冲突") {
		t.Fatalf("错误必须说明内容冲突: %v", err)
	}
	// 仅把时刻改为 99：同样内容冲突。
	if n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 99, Price: 20}); err == nil || n != 0 {
		t.Fatalf("仅改时刻必须报内容冲突并返回 0: n=%d err=%v", n, err)
	} else if !strings.Contains(err.Error(), "内容冲突") {
		t.Fatalf("错误必须说明内容冲突: %v", err)
	}

	// 冲突后再提交原内容：仍按相同等待数据处理，成功返回 0，不是替换报价。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 20}, 0)

	// 整个等待/冲突阶段没有任何事件记录（冲突不留拒绝记录）。
	if recs := e.Records(); len(recs) != 0 {
		t.Fatalf("等待与冲突提交不得产生事件记录，实际 %d 条: %+v", len(recs), recs)
	}
	q, _ = e.CurrentQuote("A")
	if q.Seq != 1 || !e.HasGap("A") {
		t.Fatalf("冲突后最新报价仍须停在 seq1 且缺口保持: q=%+v", q)
	}

	// 补交 seq2：seq2 与等待中的原始 seq3 依次生效，共 2 条；最终报价必须是
	// 最初收到的 seq3/(时刻30,价格20)，任何冲突值都不得被采用。
	n, err = e.UpdateQuote("A", Quote{Seq: 2, Moment: 20, Price: 10})
	if err != nil || n != 2 {
		t.Fatalf("补齐缺口应使 2 条报价依次生效: n=%d err=%v", n, err)
	}
	q, ok := e.CurrentQuote("A")
	if !ok || q != (Quote{Seq: 3, Moment: 30, Price: 20}) {
		t.Fatalf("最终报价必须保留最初收到的 seq3/(30,20)，不能使用被拒绝的数据: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("补齐后缺口必须消失")
	}
}

// TestPendingQuoteConflictPreservedAcrossGapFillAmountCancel 在已有持仓与未成交
// 买单、账户总持仓金额上限 100 的场景下，端到端锁定题述行为：
//   - 初始现金 1000；价格 10 买入成交 2 单位（cash=980, pos=2）；
//     另有一笔 4 单位、限价 10 的待成交买单（现金占用 40）；
//   - seq1@(10,10) 下：持仓金额 20 + 买单金额占用 40 = 60 ≤ 100；
//   - 等待与冲突期间上述订单、资金、金额与事件记录全部冻结；
//   - 补交 seq2@(20,10) 后 2 条报价依次生效，最终保留最初的 seq3@(30,20)；
//   - seq3@20 下持仓金额 40 + 买单占用 80 = 120 > 100，撤销买单全部剩余 4，
//     释放 40 现金占用；撤单记录锚定 seq3 原始报价定位，不能使用冲突数据；
//   - 补齐后再次重放原 seq3 仍返回 0，不再撤销或改写记录。
func TestPendingQuoteConflictPreservedAcrossGapFillAmountCancel(t *testing.T) {
	e, _ := NewEngine(1000)
	mustSetMax(t, e, "A", 100) // 合约持仓限额充足
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)

	bid := mustBuy(t, e, "A", 6, 10) // 现金占用 60
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	// 成交 2 单位后：现金 980、持仓 2、买单剩余 4 占用现金 40。
	if e.Cash() != 980 || e.ReservedCash() != 40 || e.Position("A") != 2 {
		t.Fatalf("前置资金状态错误: cash=%d reserved=%d pos=%d", e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if _, err := e.SetPositionAmountLimit(100); err != nil { // 20 + 40 = 60，不撤单
		t.Fatal(err)
	}
	if am := e.PositionAmountStatus(); am.Holding != 20 || am.BuyReserved != 40 || am.Total != 60 {
		t.Fatalf("前置金额状态错误: %+v", am)
	}

	// 冻结基线：seq3 等待/冲突阶段最后一条事件必须始终是这笔成交记录。
	recsBase := e.Records()
	recsLen := len(recsBase)
	lastRec := recsBase[recsLen-1]
	if lastRec.Kind != RecordFilled || lastRec.OrderID != bid || lastRec.TradeID != 1 {
		t.Fatalf("前置最后一条记录应为成交记录: %+v", lastRec)
	}

	// seq3@(30,20) 跨号先到：成功但 0 条生效。
	if n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 30, Price: 20}); err != nil || n != 0 {
		t.Fatalf("跨号 seq3 应成功返回 0: n=%d err=%v", n, err)
	}
	assertGapWaitFrozen(t, e, bid, recsLen, "seq3 首次等待")

	// 完全相同的 seq3 再次提交：成功返回 0，一切不变。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 20}, 0)
	assertGapWaitFrozen(t, e, bid, recsLen, "seq3 相同重放")

	// 仅改价格为 10：冲突返回 0，原先等待的 (30,20) 必须保留。
	if n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 30, Price: 10}); err == nil || n != 0 {
		t.Fatalf("仅改价格必须冲突且 0 条生效: n=%d err=%v", n, err)
	}
	assertGapWaitFrozen(t, e, bid, recsLen, "seq3 改价格冲突后")

	// 仅改时刻为 99：冲突返回 0，原先等待的 (30,20) 必须保留。
	if n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 99, Price: 20}); err == nil || n != 0 {
		t.Fatalf("仅改时刻必须冲突且 0 条生效: n=%d err=%v", n, err)
	}
	assertGapWaitFrozen(t, e, bid, recsLen, "seq3 改时刻冲突后")

	// 冲突后再提交原内容：仍视为同一等待数据，成功返回 0；冲突请求不是替换报价。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 20}, 0)
	assertGapWaitFrozen(t, e, bid, recsLen, "冲突后原内容重放")
	if r := e.Records()[len(e.Records())-1]; !reflect.DeepEqual(r, lastRec) {
		t.Fatalf("事件记录必须保持原值，最后一条仍应为原成交记录: got=%+v want=%+v", r, lastRec)
	}

	// 补交缺失的 seq2@(20,10)：seq2 与等待中的 seq3 依次生效，共 2 条，
	// 缺口消失，最终报价保留最初收到的 seq3@(30,20)。
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 20, Price: 10})
	if err != nil {
		t.Fatalf("补齐缺口必须成功: %v", err)
	}
	if n != 2 {
		t.Fatalf("本次应有 2 条报价依次生效，实际 %d", n)
	}
	if e.HasGap("A") {
		t.Fatal("补齐后缺口必须消失")
	}
	q, ok := e.CurrentQuote("A")
	if !ok || q != (Quote{Seq: 3, Moment: 30, Price: 20}) {
		t.Fatalf("最终报价必须保留最初收到的 seq3/(30,20)，不得采用任何冲突值: %+v ok=%v", q, ok)
	}

	// seq3@20 生效：持仓金额 2×20=40 + 买单剩余 4×max(限价10,报价20)=80，
	// 合计 120 > 上限 100：撤销该买单全部剩余 4，释放 40 现金占用。
	o, _ := e.Order(bid)
	if o.Status != StatusCanceled || o.Filled != 2 || o.Remaining() != 0 {
		t.Fatalf("超限买单应被撤销全部剩余量、保留已成交 2: %+v", o)
	}
	if !strings.Contains(o.Reason, "账户总持仓金额上限") ||
		!strings.Contains(o.Reason, "报价序号 3") {
		t.Fatalf("撤单原因必须说明金额超限并锚定 seq3: %q", o.Reason)
	}
	if e.Cash() != 980 {
		t.Fatalf("撤销不改变已结算现金，现金余额必须保持 980: %d", e.Cash())
	}
	if e.ReservedCash() != 0 {
		t.Fatalf("被撤买单剩余现金占用 40 必须释放: %d", e.ReservedCash())
	}
	if e.Position("A") != 2 {
		t.Fatalf("已入账的 2 单位持仓必须保留: %d", e.Position("A"))
	}
	if am := e.PositionAmountStatus(); am.Holding != 40 || am.BuyReserved != 0 || am.Total != 40 {
		t.Fatalf("撤单后应只剩持仓金额 40: %+v", am)
	}

	// 本批只新增一条撤销记录，且其金额与报价定位全部锚定 seq3 原始报价
	// （不是被拒绝的 price=10 或 moment=99 冲突数据——用冲突值本不会超限撤单）。
	recs := e.Records()
	if len(recs) != recsLen+1 {
		t.Fatalf("补齐应只新增一条撤单记录，实际新增 %d 条", len(recs)-recsLen)
	}
	cr := recs[recsLen]
	if cr.Kind != RecordCanceled || cr.OrderID != bid {
		t.Fatalf("新增记录必须是该买单的撤销记录: %+v", cr)
	}
	if cr.Filled != 2 || cr.Remaining != 4 {
		t.Fatalf("撤单记录应标明已成交 2、被取消剩余 4: %+v", cr)
	}
	if !cr.AmtEnabled || cr.AmtLimit != 100 ||
		cr.AmtHolding != 40 || cr.AmtBuyReserved != 80 || cr.AmtTotal != 120 {
		t.Fatalf("撤单记录必须保存撤销前两类金额 40/80、合计 120 与上限 100: %+v", cr)
	}
	if cr.QuoteSeq != 3 || cr.QuoteMoment != 30 || cr.QuotePrice != 20 {
		t.Fatalf("撤单记录报价快照必须锚定最初收到的 seq3/(30,20): %+v", cr)
	}
	if len(cr.AmtQuoteRefs) != 1 ||
		cr.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 3, Moment: 30, Price: 20}) {
		t.Fatalf("撤单记录参与金额计算的报价定位必须是 seq3 原始报价: %+v", cr.AmtQuoteRefs)
	}

	// 补齐后再次提交原 seq3：已生效同内容重放返回 0，不再撤销或改变任何记录。
	recsLenAfter := len(e.Records())
	if n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 30, Price: 20}); err != nil || n != 0 {
		t.Fatalf("补齐后重放原 seq3 必须成功返回 0: n=%d err=%v", n, err)
	}
	if len(e.Records()) != recsLenAfter {
		t.Fatalf("补齐后重放不得新增任何记录")
	}
	o2, _ := e.Order(bid)
	if o2.Status != StatusCanceled || o2.Reason != o.Reason {
		t.Fatalf("重放不得改变撤销状态与原因: before=%q after=%q", o.Reason, o2.Reason)
	}
	if e.Cash() != 980 || e.ReservedCash() != 0 || e.Position("A") != 2 {
		t.Fatalf("重放不得改变资金与持仓: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if am := e.PositionAmountStatus(); am.Holding != 40 || am.BuyReserved != 0 || am.Total != 40 {
		t.Fatalf("重放不得改变金额状态: %+v", am)
	}
}
