package book

import (
	"strings"
	"testing"
)

// setupModifyGapPartial 准备题述场景（未开日内亏损保护）：
// 现金 1000；A 持仓限额 100、报价 seq1(时刻10)@10；B 持仓限额 100、报价
// seq1(时刻20)@20。A 买单总量 10、限价 10，已按价格 9 成交 4（剩余 6，占用现金
// 60）；B 买单数量 3、限价 20 未成交（占用现金 60）。账户总持仓金额上限 160：
// 已持仓金额 4×10=40，有效买单剩余占用 6×max(10,10)+3×max(20,20)=120，
// 合计 160 恰好达到上限。随后 A 报价 seq3(时刻30)@100 因 seq2 缺失而等待，
// A 最新已生效报价仍为 seq1@10，缺口存在。
func setupModifyGapPartial(t *testing.T) (e *Engine, aID, bID int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 20}, 1)

	aID = mustBuy(t, e, "A", 10, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: aID, Symbol: "A", Side: Buy, Price: 9, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	bID = mustBuy(t, e, "B", 3, 20)

	if _, err := e.SetPositionAmountLimit(160); err != nil {
		t.Fatal(err)
	}

	// A 的 seq3@100 跨号等待：返回 0 条、出现缺口，最新已生效报价停在 seq1。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 100}, 0)

	// 前置账实：已成交的 4 份只结算一次现金、只计入持仓，绝不再计入买单占用。
	if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 ||
		e.Position("A") != 4 || e.Position("B") != 0 {
		t.Fatalf("前置资金与持仓错误: cash=%d reserved=%d available=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 160 ||
		st.Holding != 40 || st.BuyReserved != 120 || st.Total != 160 {
		t.Fatalf("前置金额状态应恰好达到上限 160: %+v", st)
	}
	if o, _ := e.Order(aID); o.Qty != 10 || o.Limit != 10 || o.Filled != 4 ||
		o.Status != StatusPartial || o.Remaining() != 6 {
		t.Fatalf("A 订单前置状态错误: %+v", o)
	}
	if o, _ := e.Order(bID); o.Qty != 3 || o.Limit != 20 || o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("B 订单前置状态错误: %+v", o)
	}
	if !e.HasGap("A") {
		t.Fatal("前置应存在缺口")
	}
	if q, _ := e.CurrentQuote("A"); q.Seq != 1 || q.Price != 10 {
		t.Fatalf("等待中的 seq3@100 不得生效，最新报价应为 seq1@10: %+v", q)
	}
	for _, r := range e.Records() {
		if r.Kind == RecordCanceled {
			t.Fatalf("恰好达到金额上限不得撤销任何订单: %+v", r)
		}
	}
	return e, aID, bID
}

// TestModifyGapPartialBuyRaiseLimitAllowedRegression 复现题述场景：缺口期间把部分
// 成交的 A 买单改为总量 7、限价 20——剩余量 6→3 减少、现金占用 60→60 不增、按最新
// 已生效报价 seq1@10 计的持仓金额占用 6×10→3×max(20,10)=60 也不增，限价提高本身
// 不等于增加风险，修改必须成功。等待中的 seq3@100 绝不参与判断（否则持仓金额
// 4×100=400 早已超限）；已成交的 4 份也不能再次计入占用。修改不得推进报价或
// 消除缺口，恰好达到金额上限也不能导致拒绝或撤销 B 买单。
func TestModifyGapPartialBuyRaiseLimitAllowedRegression(t *testing.T) {
	e, aID, bID := setupModifyGapPartial(t)

	before := len(e.Records())
	mustModify(t, e, aID, 7, 20)

	// 只追加一条修改记录，保存前后参数、已成交量与当时的报价/限额快照：
	// 报价快照取最新已生效的 seq1@10，不是等待中的 seq3@100。
	recs := e.Records()
	if len(recs) != before+1 {
		t.Fatalf("成功修改只能追加一条记录，实际新增 %d 条", len(recs)-before)
	}
	r := recs[len(recs)-1]
	if r.Kind != RecordModified || r.OrderID != aID || r.Symbol != "A" || r.Side != Buy {
		t.Fatalf("新增记录必须是 A 买单的修改记录: %+v", r)
	}
	if r.OldQty != 10 || r.OldLimit != 10 || r.OldFilled != 4 ||
		r.Qty != 7 || r.Limit != 20 || r.Filled != 4 || r.Remaining != 3 {
		t.Fatalf("修改记录前后参数错误: %+v", r)
	}
	if !r.QuoteValid || r.QuoteSeq != 1 || r.QuoteMoment != 10 || r.QuotePrice != 10 {
		t.Fatalf("修改记录报价快照必须取已生效的 seq1@10: %+v", r)
	}
	if !r.MaxPositionValid || r.MaxPosition != 100 {
		t.Fatalf("修改记录必须固化当时持仓限额: %+v", r)
	}

	// 订单编号、合约、方向与累计成交 4 不变；状态仍为部分成交，有效剩余量变为 3。
	o, _ := e.Order(aID)
	if o.ID != aID || o.Symbol != "A" || o.Side != Buy || o.Filled != 4 ||
		o.Qty != 7 || o.Limit != 20 || o.Status != StatusPartial || o.Remaining() != 3 {
		t.Fatalf("修改后 A 订单状态错误: %+v", o)
	}

	// A 买单占用现金仍为 3×20=60：现金 964、占用 120、可用 844、持仓 4 全部保留。
	if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 ||
		e.Position("A") != 4 || e.Position("B") != 0 {
		t.Fatalf("修改不得改变资金与持仓: cash=%d reserved=%d available=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}
	// 持仓金额 40、买单金额占用 3×max(20,10)+3×20=120、合计 160 不变，恰好达到
	// 上限不能导致拒绝或撤销 B 买单。
	if st := e.PositionAmountStatus(); st.Holding != 40 || st.BuyReserved != 120 || st.Total != 160 {
		t.Fatalf("修改后金额合计应保持 160: %+v", st)
	}
	if o, _ := e.Order(bID); o.Qty != 3 || o.Limit != 20 || o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("B 订单不得被撤销或改动: %+v", o)
	}

	// 修改不能推进报价或消除缺口：缺口仍在，最新已生效报价仍为 seq1@10。
	if !e.HasGap("A") {
		t.Fatal("修改不得消除缺口")
	}
	if q, _ := e.CurrentQuote("A"); q.Seq != 1 || q.Price != 10 {
		t.Fatalf("修改不得推进报价: %+v", q)
	}
}

// TestModifyGapPartialBuyRejectIncreaseRegression 复现题述两个拒绝情形（同一修改前
// 状态，第一次拒绝不改变任何状态，第二次面对的状态与之一致）：缺口期间买单修改
// 只有在剩余量、现金占用、持仓金额占用均不增加时才允许——改为总量 7、限价 21 会把
// 现金占用提高到 3×21=63，拒绝；改为总量 11、限价 5 虽把现金占用降到 7×5=35，
// 却把剩余量提高到 7，同样拒绝（缺口规则不能简化为只准降价）。每次失败只追加一条
// 说明缺口期间不允许增加占用的拒绝记录，订单参数、已入账成交、资金、持仓与其他
// 订单全部保持提交前值。
func TestModifyGapPartialBuyRejectIncreaseRegression(t *testing.T) {
	e, aID, bID := setupModifyGapPartial(t)

	rejects := []struct {
		name       string
		qty, limit int64
	}{
		{"提高现金占用", 7, 21},   // 剩余 3×21=63 > 60
		{"降价但增加剩余量", 11, 5}, // 剩余 7 > 6，现金 35 虽降仍拒绝
	}
	for _, c := range rejects {
		before := len(e.Records())
		err := e.Modify(aID, c.qty, c.limit)
		if err == nil {
			t.Fatalf("%s: Modify(%d, %d) 必须拒绝", c.name, c.qty, c.limit)
		}
		expectErr(t, err, "报价存在缺口")
		expectErr(t, err, "买单修改不得增加剩余量、现金或持仓金额占用")

		// 恰好一条拒绝记录，关联原订单、保留申请参数并说明缺口原因；
		// 不得产生修改或撤销记录（尤其不能撤 B 单腾额度）。
		recs := e.Records()
		if len(recs) != before+1 {
			t.Fatalf("%s: 只能追加一条拒绝记录，实际新增 %d 条", c.name, len(recs)-before)
		}
		rj := recs[len(recs)-1]
		if rj.Kind != RecordRejected || rj.OrderID != aID || rj.Symbol != "A" || rj.Side != Buy ||
			rj.Qty != c.qty || rj.Limit != c.limit {
			t.Fatalf("%s: 拒绝记录须关联原订单并保留申请参数: %+v", c.name, rj)
		}
		if !strings.Contains(rj.Reason, "报价存在缺口") ||
			!strings.Contains(rj.Reason, "不得增加剩余量、现金或持仓金额占用") {
			t.Fatalf("%s: 拒绝原因须说明缺口期间不允许增加占用: %q", c.name, rj.Reason)
		}

		// 订单参数、已入账成交、资金、持仓与其他订单全部保持提交前值。
		if o, _ := e.Order(aID); o.Qty != 10 || o.Limit != 10 || o.Filled != 4 ||
			o.Status != StatusPartial || o.Remaining() != 6 {
			t.Fatalf("%s: 拒绝不得改变 A 订单: %+v", c.name, o)
		}
		if o, _ := e.Order(bID); o.Qty != 3 || o.Limit != 20 || o.Status != StatusPending || o.Remaining() != 3 {
			t.Fatalf("%s: 拒绝不得影响 B 订单: %+v", c.name, o)
		}
		if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 ||
			e.Position("A") != 4 || e.Position("B") != 0 {
			t.Fatalf("%s: 拒绝不得改变资金与持仓: cash=%d reserved=%d available=%d posA=%d posB=%d",
				c.name, e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
		}
		if st := e.PositionAmountStatus(); st.Holding != 40 || st.BuyReserved != 120 || st.Total != 160 {
			t.Fatalf("%s: 拒绝后金额合计保持 160: %+v", c.name, st)
		}
		if !e.HasGap("A") {
			t.Fatalf("%s: 拒绝不得消除缺口", c.name)
		}
		if q, _ := e.CurrentQuote("A"); q.Seq != 1 || q.Price != 10 {
			t.Fatalf("%s: 拒绝不得推进报价: %+v", c.name, q)
		}
	}
}
