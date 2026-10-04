package book

import (
	"strings"
	"testing"
)

// setupModifyAmountReject 准备题述场景（现金与合约持仓限额充足、行情连续、未开日内
// 亏损保护）：现金 1000，A、B 持仓限额各 100；A 报价 seq1(时刻10)@10，B 报价
// seq1(时刻20)@20；账户总持仓金额上限 200。
// A 买单总量 10、限价 12，已按价格 9 成交 4（剩余 6）；B 买单数量 2、限价 20 未成交。
// 此时已持仓金额 4×10=40，未成交买单占用 6×max(12,10)+2×20=112，合计 152；
// 现金余额 964，买单现金占用 112。
func setupModifyAmountReject(t *testing.T) (e *Engine, aID, bID int64) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 20, Price: 20}, 1)
	if _, err := e.SetPositionAmountLimit(200); err != nil {
		t.Fatal(err)
	}

	aID = mustBuy(t, e, "A", 10, 12)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: aID, Symbol: "A", Side: Buy, Price: 9, Qty: 4}); err != nil {
		t.Fatal(err)
	}
	bID = mustBuy(t, e, "B", 2, 20)

	if e.Cash() != 964 || e.ReservedCash() != 112 || e.Position("A") != 4 || e.Position("B") != 0 {
		t.Fatalf("前置资金与持仓错误: cash=%d reserved=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"), e.Position("B"))
	}
	if st := e.PositionAmountStatus(); !st.Enabled || st.Limit != 200 ||
		st.Holding != 40 || st.BuyReserved != 112 || st.Total != 152 {
		t.Fatalf("前置金额状态错误: %+v", st)
	}
	if o, _ := e.Order(aID); o.Qty != 10 || o.Limit != 12 || o.Filled != 4 ||
		o.Status != StatusPartial || o.Remaining() != 6 {
		t.Fatalf("A 订单前置状态错误: %+v", o)
	}
	if o, _ := e.Order(bID); o.Qty != 2 || o.Limit != 20 || o.Status != StatusPending {
		t.Fatalf("B 订单前置状态错误: %+v", o)
	}
	return e, aID, bID
}

// findModifyAmountReject 返回唯一一条 A 订单的金额超限修改拒绝记录。
func findModifyAmountReject(t *testing.T, e *Engine, aID int64) Record {
	t.Helper()
	var found []Record
	for _, r := range e.Records() {
		if r.Kind == RecordRejected && r.OrderID == aID && r.AmtEnabled {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("金额超限修改拒绝记录应恰好一条，实际 %d 条", len(found))
	}
	return found[0]
}

// TestModifyPartialBuyAmountLimitRejectRegression 复现题述场景：把部分成交的 A 买单
// 改为总量 12、限价 20 后，剩余 8 的新占用 8×max(20,10)=160 替换原剩余 6 的占用 72，
// 申请后合计 152-72+160=240 超过上限 200，必须拒绝。已成交的 4 份只计入持仓金额，
// 不能再算进买单占用（否则申请前合计不会是 152）；也绝不能撤销 B 订单腾额度。
// 本次调用只追加一条拒绝记录，关联原订单并固化申请参数、判断时的上限、两类金额、
// 修改前合计与申请后合计，以及参与判断的 A、B 报价定位（按合约排列且不重复）；
// 原订单、其他订单、资金与持仓全部保持调用前的值。
func TestModifyPartialBuyAmountLimitRejectRegression(t *testing.T) {
	e, aID, bID := setupModifyAmountReject(t)

	before := len(e.Records())
	err := e.Modify(aID, 12, 20)
	if err == nil {
		t.Fatal("申请后合计 240 超过上限 200 必须拒绝修改")
	}
	expectErr(t, err, "超过账户总持仓金额上限")

	// 只追加一条拒绝记录；不得产生修改成功或撤销记录（尤其不能撤 B 单腾额度）。
	recs := e.Records()
	if len(recs) != before+1 {
		t.Fatalf("只能追加一条拒绝记录，实际新增 %d 条", len(recs)-before)
	}
	rj := recs[len(recs)-1]
	if rj.Kind != RecordRejected {
		t.Fatalf("新增记录必须是拒绝记录: %+v", rj)
	}
	for _, r := range recs[before:] {
		if r.Kind == RecordModified || r.Kind == RecordCanceled {
			t.Fatalf("金额超限拒绝不得产生修改或撤销记录: %+v", r)
		}
	}

	// 记录关联原订单并保留申请的总量与限价。
	if rj.OrderID != aID || rj.Symbol != "A" || rj.Side != Buy || rj.Qty != 12 || rj.Limit != 20 {
		t.Fatalf("拒绝记录须关联原订单并保留申请参数: %+v", rj)
	}
	if !strings.Contains(rj.Reason, "超过账户总持仓金额上限") {
		t.Fatalf("拒绝原因须说明金额超限: %q", rj.Reason)
	}
	// 固化判断时的上限、两类金额、修改前合计与申请后合计：
	// 已持仓 40、买单占用 112（只含未成交剩余量，已成交 4 份不得重复计入）、
	// 修改前合计 152、申请后合计 240。
	if !rj.AmtEnabled || rj.AmtLimit != 200 ||
		rj.AmtHolding != 40 || rj.AmtBuyReserved != 112 ||
		rj.AmtTotal != 152 || rj.AmtApplyTotal != 240 {
		t.Fatalf("拒绝记录金额快照错误: %+v", rj)
	}
	// 参与判断的 A、B 报价都留下合约、序号、时刻和价格，按合约排列且不重复。
	wantRefs := []RiskQuoteRef{
		{Symbol: "A", Seq: 1, Moment: 10, Price: 10},
		{Symbol: "B", Seq: 1, Moment: 20, Price: 20},
	}
	if len(rj.AmtQuoteRefs) != len(wantRefs) {
		t.Fatalf("报价定位应为 A、B 各一条: %+v", rj.AmtQuoteRefs)
	}
	for i, want := range wantRefs {
		if rj.AmtQuoteRefs[i] != want {
			t.Fatalf("报价定位[%d]错误: 期望 %+v，实际 %+v（须按合约排列且不重复）",
				i, want, rj.AmtQuoteRefs[i])
		}
	}

	// 拒绝不改动原订单：A 仍为总量 10、限价 12、已成交 4、剩余 6。
	if o, _ := e.Order(aID); o.Qty != 10 || o.Limit != 12 || o.Filled != 4 ||
		o.Status != StatusPartial || o.Remaining() != 6 {
		t.Fatalf("拒绝不得改变 A 订单: %+v", o)
	}
	// B 订单继续有效，绝不能被撤销腾额度。
	if o, _ := e.Order(bID); o.Qty != 2 || o.Limit != 20 || o.Status != StatusPending || o.Remaining() != 2 {
		t.Fatalf("拒绝不得影响 B 订单: %+v", o)
	}
	// 现金余额、买单现金占用与持仓均保持调用前的值。
	if e.Cash() != 964 || e.ReservedCash() != 112 || e.AvailableCash() != 852 ||
		e.Position("A") != 4 || e.Position("B") != 0 {
		t.Fatalf("拒绝不得改变资金与持仓: cash=%d reserved=%d available=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}
	if st := e.PositionAmountStatus(); st.Holding != 40 || st.BuyReserved != 112 || st.Total != 152 {
		t.Fatalf("拒绝后金额合计保持 152: %+v", st)
	}
}

// TestModifyAmountRejectRecordImmutability 验证拒绝记录保存的是拒绝发生时的事实：
// 随后更新报价、调高金额上限并成功修改同一订单后，旧记录仍保留 152、240、上限 200
// 和原报价定位；修改查询返回的记录或其中的报价信息，也不能影响再次查询得到的历史内容。
func TestModifyAmountRejectRecordImmutability(t *testing.T) {
	e, aID, bID := setupModifyAmountReject(t)

	if err := e.Modify(aID, 12, 20); err == nil {
		t.Fatal("前置：申请后合计 240 超限必须拒绝")
	}

	// 篡改查询返回的副本（含报价定位切片），再次查询的历史内容不得受影响。
	tampered := e.Records()
	for i := range tampered {
		if tampered[i].Kind == RecordRejected && tampered[i].OrderID == aID && tampered[i].AmtEnabled {
			tampered[i].AmtLimit = 9999
			tampered[i].AmtTotal = 1
			tampered[i].AmtApplyTotal = 1
			tampered[i].AmtQuoteRefs[0].Price = 9999
			tampered[i].AmtQuoteRefs[1].Seq = 99
		}
	}
	rj := findModifyAmountReject(t, e, aID)
	if rj.AmtLimit != 200 || rj.AmtTotal != 152 || rj.AmtApplyTotal != 240 {
		t.Fatalf("篡改查询副本不得改写历史记录: %+v", rj)
	}
	if rj.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 1, Moment: 10, Price: 10}) ||
		rj.AmtQuoteRefs[1] != (RiskQuoteRef{Symbol: "B", Seq: 1, Moment: 20, Price: 20}) {
		t.Fatalf("篡改查询副本不得改写历史报价定位: %+v", rj.AmtQuoteRefs)
	}

	// 随后调高上限、更新报价并成功修改同一订单。
	if _, err := e.SetPositionAmountLimit(1000); err != nil {
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 11, Price: 30}, 1)
	mustQuote(t, e, "B", Quote{Seq: 2, Moment: 21, Price: 30}, 1)
	mustModify(t, e, aID, 12, 20)
	if o, _ := e.Order(aID); o.Qty != 12 || o.Limit != 20 || o.Filled != 4 || o.Remaining() != 8 {
		t.Fatalf("调高上限后同一订单应能成功修改: %+v", o)
	}
	if o, _ := e.Order(bID); o.Status != StatusPending {
		t.Fatalf("B 订单应继续有效: %+v", o)
	}

	// 旧拒绝记录仍保留拒绝发生时的事实：合计 152、申请后 240、上限 200、
	// 原报价定位（A seq1@10、B seq1@20），不被后续报价与成功修改改写。
	rj = findModifyAmountReject(t, e, aID)
	if rj.AmtLimit != 200 || rj.AmtHolding != 40 || rj.AmtBuyReserved != 112 ||
		rj.AmtTotal != 152 || rj.AmtApplyTotal != 240 {
		t.Fatalf("后续操作不得改写旧拒绝记录的金额快照: %+v", rj)
	}
	if rj.Qty != 12 || rj.Limit != 20 || rj.OrderID != aID {
		t.Fatalf("后续成功修改不得改写旧拒绝记录的申请参数: %+v", rj)
	}
	if rj.AmtQuoteRefs[0] != (RiskQuoteRef{Symbol: "A", Seq: 1, Moment: 10, Price: 10}) ||
		rj.AmtQuoteRefs[1] != (RiskQuoteRef{Symbol: "B", Seq: 1, Moment: 20, Price: 20}) {
		t.Fatalf("后续报价不得改写旧记录的报价定位: %+v", rj.AmtQuoteRefs)
	}
}

// TestModifyAmountExactlyAtLimitAllowed 金额刚好等于上限的修改仍按已有规则允许：
// A 改为总量 12、限价 15 后，新占用 8×max(15,10)=120，申请后合计 152-72+120=200
// 恰好等于上限，必须成功且不应产生金额超限拒绝记录。
func TestModifyAmountExactlyAtLimitAllowed(t *testing.T) {
	e, aID, bID := setupModifyAmountReject(t)

	before := len(e.Records())
	mustModify(t, e, aID, 12, 15)

	recs := e.Records()
	if len(recs) != before+1 || recs[len(recs)-1].Kind != RecordModified {
		t.Fatalf("恰好等于上限的修改必须成功且只留一条修改记录，新增 %d 条", len(recs)-before)
	}
	for _, r := range recs[before:] {
		if r.Kind == RecordRejected {
			t.Fatalf("恰好等于上限不得产生金额超限拒绝记录: %+v", r)
		}
	}
	if o, _ := e.Order(aID); o.Qty != 12 || o.Limit != 15 || o.Filled != 4 ||
		o.Status != StatusPartial || o.Remaining() != 8 {
		t.Fatalf("恰好等于上限的修改应生效: %+v", o)
	}
	if st := e.PositionAmountStatus(); st.Holding != 40 || st.BuyReserved != 160 || st.Total != 200 {
		t.Fatalf("修改后合计应恰好等于上限 200: %+v", st)
	}
	if o, _ := e.Order(bID); o.Status != StatusPending || o.Qty != 2 {
		t.Fatalf("B 订单不得受影响: %+v", o)
	}
}
