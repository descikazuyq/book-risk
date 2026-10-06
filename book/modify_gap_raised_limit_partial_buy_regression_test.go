package book

import (
	"strings"
	"testing"
)

// setupModifyGapRaiseLimit 准备题述场景：
// 账户初始现金 1000；A、B 持仓限额各 100（充足）。
// A 最新有效报价 seq1(时刻10)@10；一张总量 10、限价 10 的买单先以价格 9 成交 4，
// 现金 964、A 持仓 4、剩余 6，A 单占用现金 6×10=60。
// B 有效买单剩余 3、限价 20（B 报价 seq1(时刻20)@20），占用现金 60。
// 账户总持仓金额上限 160：A 持仓金额 4×10=40，两单买单金额占用 60+60=120，
// 合计 160 恰好达到上限。随后 A seq3(时刻30)@100 因 seq2 缺失进入等待，
// A 的有效报价仍为 seq1@10，等待中的 100 不参与任何判断。
func setupModifyGapRaiseLimit(t *testing.T) (e *Engine, aID, bID int64) {
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
	// seq3 跨号等待：不生效、不推进报价、不触发金额收敛。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 100}, 0)

	if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 {
		t.Fatalf("前置资金错误: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 4 || e.Position("B") != 0 {
		t.Fatalf("前置持仓错误: posA=%d posB=%d", e.Position("A"), e.Position("B"))
	}
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 160 ||
		st.Holding != 40 || st.BuyReserved != 120 || st.Total != 160 {
		t.Fatalf("前置金额状态错误: %+v", st)
	}
	if o, _ := e.Order(aID); o.Qty != 10 || o.Limit != 10 || o.Filled != 4 ||
		o.Status != StatusPartial || o.Remaining() != 6 {
		t.Fatalf("A 订单前置状态错误: %+v", o)
	}
	if o, _ := e.Order(bID); o.Qty != 3 || o.Limit != 20 ||
		o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("B 订单前置状态错误: %+v", o)
	}
	if !e.HasGap("A") || e.HasGap("B") {
		t.Fatalf("前置缺口状态错误: gapA=%v gapB=%v", e.HasGap("A"), e.HasGap("B"))
	}
	if q, ok := e.CurrentQuote("A"); !ok || q.Seq != 1 || q.Moment != 10 || q.Price != 10 {
		t.Fatalf("A 有效报价应停留在 seq1@10: %+v ok=%v", q, ok)
	}
	return e, aID, bID
}

// assertGapScenarioUnchanged 校验失败的修改申请之后，题述全部业务状态保持提交前值。
func assertGapScenarioUnchanged(t *testing.T, e *Engine, aID, bID int64) {
	t.Helper()
	// 资金：A 单仍按剩余 6×限价 10 占用 60，B 单 60，合计 120。
	if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 {
		t.Fatalf("失败后资金必须保持提交前值: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	// 持仓与已入账成交保留。
	if e.Position("A") != 4 || e.Position("B") != 0 {
		t.Fatalf("失败后持仓必须保持提交前值: posA=%d posB=%d", e.Position("A"), e.Position("B"))
	}
	// 金额：40 + 120 = 160 恰好顶在上限，等待中的 100 不得参与。
	if st := e.PositionAmountStatus(); st.Holding != 40 || st.BuyReserved != 120 || st.Total != 160 {
		t.Fatalf("失败后金额合计必须保持 160: %+v", st)
	}
	if o, _ := e.Order(aID); o.Qty != 10 || o.Limit != 10 || o.Filled != 4 ||
		o.Status != StatusPartial || o.Remaining() != 6 {
		t.Fatalf("失败后 A 订单必须保持原参数与累计成交: %+v", o)
	}
	if o, _ := e.Order(bID); o.Qty != 3 || o.Limit != 20 ||
		o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("失败不得影响 B 订单，更不能撤销 B 单腾额度: %+v", o)
	}
	if !e.HasGap("A") {
		t.Fatal("失败不得消除 A 的报价缺口")
	}
	if q, ok := e.CurrentQuote("A"); !ok || q.Seq != 1 || q.Price != 10 {
		t.Fatalf("失败不得推进 A 的有效报价: %+v ok=%v", q, ok)
	}
}

// TestModifyPartialBuyGapRaisedLimitWithoutIncreaseRegression 复现题述成功路径：
// 缺口期间把部分成交的 A 买单从总量 10、限价 10 改为总量 7、限价 20。
// 限价提高并不等于增加风险：剩余量 6→3、现金占用 60→3×20=60、按最新已生效报价
// seq1@10 计算的持仓金额占用 6×10=60→3×max(20,10)=60，三项均未增加，修改必须成功。
// 不能把缺口规则简化成“只准降价”；已成交的 4 份只计入持仓，不能再次计入占用；
// 合计 160 恰好等于金额上限也不得导致拒绝或撤销 B 单；等待中的 seq3@100 不参与
// 判断，修改本身不推进报价、不消除缺口。
func TestModifyPartialBuyGapRaisedLimitWithoutIncreaseRegression(t *testing.T) {
	e, aID, bID := setupModifyGapRaiseLimit(t)

	before := len(e.Records())
	mustModify(t, e, aID, 7, 20)

	// 只新增一条修改记录；不得产生拒绝或撤销记录（尤其不能撤 B 单）。
	recs := e.Records()
	if len(recs) != before+1 {
		t.Fatalf("成功修改只能新增一条记录，实际新增 %d 条", len(recs)-before)
	}
	r := recs[len(recs)-1]
	if r.Kind != RecordModified {
		t.Fatalf("新增记录必须是修改记录: %+v", r)
	}
	// 订单编号、合约、方向、累计成交 4 不变；保存原总量 10、原限价 10、已成交 4，
	// 以及新总量 7、新限价 20、新剩余 3；报价快照取 seq1(时刻10)@10。
	if r.OrderID != aID || r.Symbol != "A" || r.Side != Buy {
		t.Fatalf("修改记录关键字段错误: %+v", r)
	}
	if r.OldLimit != 10 || r.OldQty != 10 || r.OldFilled != 4 ||
		r.Limit != 20 || r.Qty != 7 || r.Filled != 4 || r.Remaining != 3 {
		t.Fatalf("修改记录必须保存修改前后参数、累计成交与新剩余量: %+v", r)
	}
	if !r.QuoteValid || r.QuoteSeq != 1 || r.QuoteMoment != 10 || r.QuotePrice != 10 {
		t.Fatalf("修改记录报价快照必须取 seq1@10（等待中的 100 不得参与）: %+v", r)
	}

	// 订单查询：编号、合约、方向、累计成交 4 不变，仍为部分成交，有效剩余量 3。
	o, _ := e.Order(aID)
	if o.Qty != 7 || o.Limit != 20 || o.Filled != 4 ||
		o.Status != StatusPartial || o.Remaining() != 3 {
		t.Fatalf("修改后 A 订单状态错误: %+v", o)
	}

	// A 单现金占用仍为 60（3×20），账户现金 964、占用 120、可用 844、A 持仓 4 全保留。
	if e.Cash() != 964 || e.ReservedCash() != 120 || e.AvailableCash() != 844 || e.Position("A") != 4 {
		t.Fatalf("修改后资金与持仓错误: cash=%d reserved=%d available=%d posA=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"))
	}
	// 持仓金额 40、买单金额占用 120（A: 3×max(20,10)=60，B: 3×20=60）、合计 160 不变。
	if st := e.PositionAmountStatus(); st.Holding != 40 || st.BuyReserved != 120 || st.Total != 160 {
		t.Fatalf("修改后金额合计必须保持 160，恰好顶在上限不得拒绝: %+v", st)
	}
	// B 单保持有效，绝不能被撤销。
	if bo, _ := e.Order(bID); bo.Qty != 3 || bo.Limit != 20 ||
		bo.Status != StatusPending || bo.Remaining() != 3 {
		t.Fatalf("B 订单不得受影响: %+v", bo)
	}

	// 修改不推进报价、不消除缺口：A 有效报价仍是 seq1@10，seq3@100 继续等待。
	if !e.HasGap("A") {
		t.Fatal("修改不得消除 A 的报价缺口")
	}
	if q, ok := e.CurrentQuote("A"); !ok || q.Seq != 1 || q.Price != 10 {
		t.Fatalf("修改不得推进 A 的有效报价: %+v ok=%v", q, ok)
	}
	// 重复提交等待中的 seq3（内容一致）仍只按等待处理：证明它没有被修改消费掉。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 30, Price: 100}, 0)

	// 补齐 seq2 后 seq2、seq3 依次生效（2 条）：等待报价在修改前后被原样保留。
	// 此前 B 单一直有效；seq3@100 生效后持仓 4×100=400 超限所触发的撤单，
	// 全部发生在本次报价之后且原因明确指向报价生效，与修改无关。
	cancelBefore := len(e.Records())
	if bo, _ := e.Order(bID); bo.Status != StatusPending {
		t.Fatalf("补齐缺口前 B 单必须仍有效: %+v", bo)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 20, Price: 10}, 2)
	if q, ok := e.CurrentQuote("A"); !ok || q.Seq != 3 || q.Price != 100 {
		t.Fatalf("补齐后应推进到 seq3@100，证明等待报价被修改原样保留: %+v ok=%v", q, ok)
	}
	for _, cr := range e.Records()[cancelBefore:] {
		if cr.Kind == RecordCanceled && !strings.Contains(cr.Reason, "报价") {
			t.Fatalf("B 单只能因 seq3@100 报价生效的金额收敛被撤，不得因修改被撤: %+v", cr)
		}
	}
}

// TestModifyPartialBuyGapIncreaseRejectedRegression 复现题述两条失败路径：
// 相同的修改前状态下，申请总量 7、限价 21 把现金占用提高到 3×21=63，必须拒绝；
// 申请总量 11、限价 5 虽把现金占用降到 7×5=35，却把剩余数量从 6 增到 7
// （金额口径占用也升到 7×10=70），同样必须拒绝。每次失败只新增一条说明
// 缺口期间不允许增加占用的拒绝记录；订单参数、已入账成交、资金、持仓与其他订单
// 全部保持提交前值，报价缺口保持不变。
func TestModifyPartialBuyGapIncreaseRejectedRegression(t *testing.T) {
	e, aID, bID := setupModifyGapRaiseLimit(t)

	cases := []struct {
		name        string
		newQty      int64
		newLimit    int64
		whyRejected string
	}{
		{name: "提高现金占用", newQty: 7, newLimit: 21, whyRejected: "剩余 3×限价 21=63 > 原占用 60"},
		{name: "增加剩余数量", newQty: 11, newLimit: 5, whyRejected: "剩余 11-4=7 > 原剩余 6"},
	}
	before := len(e.Records())
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := e.Modify(aID, c.newQty, c.newLimit)
			if err == nil {
				t.Fatalf("%s：缺口期间增加占用必须拒绝（%s）", c.name, c.whyRejected)
			}
			expectErr(t, err, "缺口")
			expectErr(t, err, "不得增加")

			// 每次失败只新增一条拒绝记录。
			recs := e.Records()
			if len(recs) != before+1 {
				t.Fatalf("每次失败只能新增一条拒绝记录，实际新增 %d 条", len(recs)-before)
			}
			rj := recs[len(recs)-1]
			if rj.Kind != RecordRejected {
				t.Fatalf("新增记录必须是拒绝记录: %+v", rj)
			}
			// 关联原订单并固化申请参数；拒绝原因说明缺口期间不允许增加占用。
			if rj.OrderID != aID || rj.Symbol != "A" || rj.Side != Buy ||
				rj.Qty != c.newQty || rj.Limit != c.newLimit {
				t.Fatalf("拒绝记录字段错误: %+v", rj)
			}
			if !strings.Contains(rj.Reason, "缺口") || !strings.Contains(rj.Reason, "不得增加") {
				t.Fatalf("拒绝原因须说明缺口期间不允许增加占用: %q", rj.Reason)
			}
			// 报价快照仍取 seq1@10；这是缺口规则拒绝，不是金额上限超额拒绝，
			// 不携带金额上限申请后合计快照。
			if !rj.QuoteValid || rj.QuoteSeq != 1 || rj.QuotePrice != 10 {
				t.Fatalf("拒绝记录报价快照必须取 seq1@10: %+v", rj)
			}
			if rj.AmtEnabled || rj.AmtApplyTotal != 0 {
				t.Fatalf("缺口规则拒绝不得伪装成金额上限超额拒绝: %+v", rj)
			}
			for _, r := range recs[before:] {
				if r.Kind == RecordModified || r.Kind == RecordCanceled {
					t.Fatalf("缺口拒绝不得产生修改或撤销记录: %+v", r)
				}
			}

			// 订单参数、已入账成交、资金、持仓与其他订单全部保持提交前值；缺口保留。
			assertGapScenarioUnchanged(t, e, aID, bID)
			before = len(e.Records())
		})
	}
}
