package book

import "testing"

// setup 返回一个已入金、已设限额、已有连续报价的账户。
func setup(t *testing.T, cash int64) *Account {
	t.Helper()
	a, err := NewAccount(cash)
	if err != nil {
		t.Fatalf("NewAccount: %v", err)
	}
	if err := a.SetPositionLimit("A", 100); err != nil {
		t.Fatalf("SetPositionLimit: %v", err)
	}
	if err := a.AddQuote("A", 1, 10, 100); err != nil {
		t.Fatalf("AddQuote: %v", err)
	}
	return a
}

// ---------- 账户 ----------

func TestNewAccountRejectsNegativeCash(t *testing.T) {
	if _, err := NewAccount(-1); err == nil {
		t.Fatal("expected error for negative cash")
	}
	a, err := NewAccount(0)
	if err != nil {
		t.Fatalf("zero cash should be allowed: %v", err)
	}
	if a.CashBalance() != 0 || a.AvailableCash() != 0 || a.FrozenCash() != 0 {
		t.Fatal("zero account should have zero balances")
	}
}

// ---------- 行情 ----------

func TestQuoteAdvancesConsecutive(t *testing.T) {
	a, _ := NewAccount(100)
	for i := int64(1); i <= 3; i++ {
		if err := a.AddQuote("A", i, i*10, 100+i); err != nil {
			t.Fatalf("AddQuote(%d): %v", i, err)
		}
	}
	q := a.LatestQuote("A")
	if q == nil || q.Seq != 3 || q.Time != 30 || q.Price != 103 {
		t.Fatalf("latest quote = %+v, want seq=3 time=30 price=103", q)
	}
}

func TestQuoteOutOfOrderWaitsThenDrains(t *testing.T) {
	a, _ := NewAccount(100)
	// 先到 seq=3，此时没有有效报价。
	if err := a.AddQuote("A", 3, 30, 103); err != nil {
		t.Fatalf("AddQuote(3): %v", err)
	}
	if q := a.LatestQuote("A"); q != nil {
		t.Fatalf("expected no valid quote, got %+v", q)
	}
	// 再到 seq=1，仍不是最新（缺 2）。
	if err := a.AddQuote("A", 1, 10, 101); err != nil {
		t.Fatalf("AddQuote(1): %v", err)
	}
	if q := a.LatestQuote("A"); q == nil || q.Seq != 1 {
		t.Fatalf("latest = %+v, want seq=1", q)
	}
	// 补齐 seq=2 后，3 依次生效。
	if err := a.AddQuote("A", 2, 20, 102); err != nil {
		t.Fatalf("AddQuote(2): %v", err)
	}
	q := a.LatestQuote("A")
	if q == nil || q.Seq != 3 || q.Price != 103 {
		t.Fatalf("latest = %+v, want seq=3 price=103", q)
	}
}

func TestQuoteDuplicateIdenticalIsNoop(t *testing.T) {
	a, _ := NewAccount(100)
	if err := a.AddQuote("A", 1, 10, 100); err != nil {
		t.Fatal(err)
	}
	if err := a.AddQuote("A", 1, 10, 100); err != nil {
		t.Fatalf("identical duplicate should be noop: %v", err)
	}
	if q := a.LatestQuote("A"); q == nil || q.Seq != 1 {
		t.Fatalf("latest = %+v, want seq=1", q)
	}
}

func TestQuoteDuplicateDifferentErrorsAndKeepsValue(t *testing.T) {
	a, _ := NewAccount(100)
	if err := a.AddQuote("A", 1, 10, 100); err != nil {
		t.Fatal(err)
	}
	if err := a.AddQuote("A", 1, 10, 101); err == nil {
		t.Fatal("expected error for conflicting duplicate")
	}
	if err := a.AddQuote("A", 1, 11, 100); err == nil {
		t.Fatal("expected error for conflicting duplicate")
	}
	q := a.LatestQuote("A")
	if q == nil || q.Time != 10 || q.Price != 100 {
		t.Fatalf("original quote should be kept, got %+v", q)
	}
}

func TestQuoteInvalidInputs(t *testing.T) {
	a, _ := NewAccount(100)
	for _, err := range []error{
		a.AddQuote("", 1, 1, 100),
		a.AddQuote("A", 0, 1, 100),
		a.AddQuote("A", -1, 1, 100),
		a.AddQuote("A", 1, 1, 0),
		a.AddQuote("A", 1, 1, -5),
	} {
		if err == nil {
			t.Fatal("expected error for invalid quote input")
		}
	}
}

// ---------- 下单 ----------

func TestPlaceOrderRequiresQuoteAndLimit(t *testing.T) {
	a, _ := NewAccount(1000)
	// 无行情：拒绝。
	o, err := a.PlaceOrder("A", Buy, 100, 1)
	if err != nil {
		t.Fatalf("business rejection should not return error: %v", err)
	}
	if o.Status != OrderRejected || o.RejectReason == "" {
		t.Fatalf("order should be rejected with reason, got %+v", o)
	}
	if o.Quote != nil {
		t.Fatal("rejection with no quote should have empty quote snapshot")
	}
	// 有行情、无限额：拒绝。
	if err := a.AddQuote("A", 1, 10, 100); err != nil {
		t.Fatal(err)
	}
	o, err = a.PlaceOrder("A", Buy, 100, 1)
	if err != nil || o.Status != OrderRejected {
		t.Fatalf("expected rejection, got %+v err=%v", o, err)
	}
}

func TestPlaceOrderRejectsInvalidInputs(t *testing.T) {
	a := setup(t, 1000)
	for _, tc := range []struct {
		side  Side
		price int64
		qty   int64
	}{
		{Side(9), 100, 1},
		{Buy, 0, 1},
		{Buy, -1, 1},
		{Buy, 100, 0},
		{Buy, 100, -1},
		{Sell, 0, 1},
	} {
		if _, err := a.PlaceOrder("A", tc.side, tc.price, tc.qty); err == nil {
			t.Fatalf("expected error for side=%v price=%d qty=%d", tc.side, tc.price, tc.qty)
		}
	}
	if len(a.Orders()) != 0 {
		t.Fatal("invalid inputs must not create records")
	}
}

func TestBuyFreezesCashAndSellFreezesPosition(t *testing.T) {
	a := setup(t, 1000) // 行情价 100，限额 100
	o, err := a.PlaceOrder("A", Buy, 100, 5)
	if err != nil || o.Status != OrderPending {
		t.Fatalf("buy should be accepted: %+v err=%v", o, err)
	}
	if a.FrozenCash() != 500 || a.AvailableCash() != 500 || a.CashBalance() != 1000 {
		t.Fatalf("frozen=%d available=%d cash=%d", a.FrozenCash(), a.AvailableCash(), a.CashBalance())
	}
	// 资金不足：拒绝。
	o, _ = a.PlaceOrder("A", Buy, 100, 6)
	if o.Status != OrderRejected {
		t.Fatalf("expected rejection for insufficient cash, got %+v", o)
	}
	if a.FrozenCash() != 500 || a.AvailableCash() != 500 {
		t.Fatal("rejected order must not freeze cash")
	}
	// 持仓不足限额：先买 96（持仓 5 + 96 = 101 > 100），拒绝。
	o, _ = a.PlaceOrder("A", Buy, 100, 96)
	if o.Status != OrderRejected {
		t.Fatalf("expected rejection over position limit, got %+v", o)
	}
	// 卖出：无持仓，拒绝。
	o, _ = a.PlaceOrder("A", Sell, 100, 1)
	if o.Status != OrderRejected {
		t.Fatalf("expected rejection for selling with no position, got %+v", o)
	}
}

func TestGapRejectsBuysButAllowsSellsCancelsFills(t *testing.T) {
	a := setup(t, 1000)
	// 先建持仓：买 5 并全部成交。
	o, _ := a.PlaceOrder("A", Buy, 100, 5)
	if _, err := a.SubmitFill(1, o.ID, 5, 100); err != nil {
		t.Fatal(err)
	}
	// 制造缺口：收到 seq=3，缺 seq=2。
	if err := a.AddQuote("A", 3, 30, 103); err != nil {
		t.Fatal(err)
	}
	// 缺口期间：新买单拒绝。
	bo, _ := a.PlaceOrder("A", Buy, 103, 1)
	if bo.Status != OrderRejected {
		t.Fatalf("buy during gap should be rejected, got %+v", bo)
	}
	// 卖单允许（挂 3 股，成交 2 股，剩 1 股可撤）。
	so, err := a.PlaceOrder("A", Sell, 103, 3)
	if err != nil || so.Status != OrderPending {
		t.Fatalf("sell during gap should be accepted: %+v err=%v", so, err)
	}
	// 已有订单的成交仍可处理（卖单部分成交）。
	if _, err := a.SubmitFill(2, so.ID, 2, 103); err != nil {
		t.Fatalf("fill during gap should process: %v", err)
	}
	// 撤单允许：把卖单剩余 1 股撤掉。
	if _, err := a.CancelOrder(so.ID); err != nil {
		t.Fatalf("cancel during gap should process: %v", err)
	}
	// 补齐缺口后，买单恢复。
	if err := a.AddQuote("A", 2, 20, 102); err != nil {
		t.Fatal(err)
	}
	q := a.LatestQuote("A")
	if q == nil || q.Seq != 3 {
		t.Fatalf("gap should be filled, latest=%+v", q)
	}
	bo2, _ := a.PlaceOrder("A", Buy, 103, 1)
	if bo2.Status != OrderPending {
		t.Fatalf("buy after gap filled should be accepted, got %+v", bo2)
	}
}

// ---------- 成交 ----------

func TestFillPartialAndPriceImprovementReleasesDifference(t *testing.T) {
	a := setup(t, 1000)
	o, _ := a.PlaceOrder("A", Buy, 100, 10)
	// 初始冻结 1000。
	if a.FrozenCash() != 1000 {
		t.Fatalf("frozen=%d", a.FrozenCash())
	}
	// 部分成交 4 股，成交价 90（低于限价 100）：扣 360，释放价差 40。
	if _, err := a.SubmitFill(1, o.ID, 4, 90); err != nil {
		t.Fatal(err)
	}
	if a.CashBalance() != 640 {
		t.Fatalf("cash=%d, want 640", a.CashBalance())
	}
	if a.FrozenCash() != 600 {
		t.Fatalf("frozen=%d, want 600", a.FrozenCash())
	}
	if a.AvailableCash() != 40 {
		t.Fatalf("available=%d, want 40 (released price difference)", a.AvailableCash())
	}
	if a.Position("A") != 4 {
		t.Fatalf("position=%d, want 4", a.Position("A"))
	}
	got := a.Order(o.ID)
	if got.Status != OrderPartialFilled || got.FilledQty != 4 || got.RemainingQty() != 6 {
		t.Fatalf("order=%+v", got)
	}
	// 再成交 6 股，按限价 100：扣 600，冻结归零。
	if _, err := a.SubmitFill(2, o.ID, 6, 100); err != nil {
		t.Fatal(err)
	}
	if a.CashBalance() != 40 || a.FrozenCash() != 0 || a.AvailableCash() != 40 {
		t.Fatalf("cash=%d frozen=%d available=%d", a.CashBalance(), a.FrozenCash(), a.AvailableCash())
	}
	got = a.Order(o.ID)
	if got.Status != OrderFilled || got.FilledQty != 10 {
		t.Fatalf("order=%+v", got)
	}
}

func TestSellFillAddsCashAndReducesPosition(t *testing.T) {
	a := setup(t, 1000)
	bo, _ := a.PlaceOrder("A", Buy, 100, 10)
	if _, err := a.SubmitFill(1, bo.ID, 10, 100); err != nil {
		t.Fatal(err)
	}
	so, _ := a.PlaceOrder("A", Sell, 100, 10)
	if a.Sellable("A") != 0 {
		t.Fatalf("sellable should be frozen by sell order, got %d", a.Sellable("A"))
	}
	// 卖出 6 股，价 105（高于限价 100）：收 630。
	if _, err := a.SubmitFill(2, so.ID, 6, 105); err != nil {
		t.Fatal(err)
	}
	if a.CashBalance() != 630 {
		t.Fatalf("cash=%d, want 630", a.CashBalance())
	}
	if a.Position("A") != 4 || a.Sellable("A") != 0 {
		t.Fatalf("position=%d sellable=%d (remaining 4 still frozen by sell order)", a.Position("A"), a.Sellable("A"))
	}
}

func TestFillRejectionsDontChangeState(t *testing.T) {
	a := setup(t, 1000)
	o, _ := a.PlaceOrder("A", Buy, 100, 5)
	base := a.CashBalance()
	// 超量成交。
	if _, err := a.SubmitFill(1, o.ID, 6, 100); err == nil {
		t.Fatal("expected rejection for over-fill")
	}
	// 买价高于限价。
	if _, err := a.SubmitFill(2, o.ID, 1, 101); err == nil {
		t.Fatal("expected rejection for buy price above limit")
	}
	// 订单不存在。
	if _, err := a.SubmitFill(3, 999, 1, 100); err == nil {
		t.Fatal("expected rejection for unknown order")
	}
	if a.CashBalance() != base || a.FrozenCash() != 500 {
		t.Fatal("rejected fills must not change state")
	}
	if got := a.Order(o.ID); got.Status != OrderPending || got.FilledQty != 0 {
		t.Fatalf("order should be untouched: %+v", got)
	}
	// 被拒绝的成交不占用编号，修正后可提交。
	if _, err := a.SubmitFill(1, o.ID, 5, 100); err != nil {
		t.Fatalf("corrected fill should be accepted: %v", err)
	}
}

func TestSellFillBelowLimitRejected(t *testing.T) {
	a := setup(t, 1000)
	bo, _ := a.PlaceOrder("A", Buy, 100, 5)
	if _, err := a.SubmitFill(1, bo.ID, 5, 100); err != nil {
		t.Fatal(err)
	}
	so, _ := a.PlaceOrder("A", Sell, 100, 5)
	if _, err := a.SubmitFill(2, so.ID, 5, 99); err == nil {
		t.Fatal("expected rejection for sell price below limit")
	}
	if a.Position("A") != 5 || a.CashBalance() != 500 {
		t.Fatal("rejected sell fill must not change state")
	}
}

func TestFillIdempotentReplayReturnsOriginal(t *testing.T) {
	a := setup(t, 1000)
	o, _ := a.PlaceOrder("A", Buy, 100, 5)
	f1, err := a.SubmitFill(1, o.ID, 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	// 相同编号、相同内容：返回原结果，不新增事件。
	f2, err := a.SubmitFill(1, o.ID, 3, 100)
	if err != nil {
		t.Fatalf("idempotent replay should succeed: %v", err)
	}
	if f2.ID != f1.ID || f2.Qty != f1.Qty || f2.Price != f1.Price {
		t.Fatalf("replay should return original fill: %+v vs %+v", f2, f1)
	}
	if len(a.Fills(o.ID)) != 1 {
		t.Fatalf("replay must not add event, got %d fills", len(a.Fills(o.ID)))
	}
	// 订单后来被撤销，再次回放仍返回原结果，不重复记账。
	if _, err := a.CancelOrder(o.ID); err != nil {
		t.Fatal(err)
	}
	f3, err := a.SubmitFill(1, o.ID, 3, 100)
	if err != nil {
		t.Fatalf("replay after cancel should return original: %v", err)
	}
	if f3.ID != f1.ID {
		t.Fatal("replay after cancel must not book again")
	}
	if a.CashBalance() != 700 { // 1000 - 3*100
		t.Fatalf("cash=%d, want 700 (no double booking)", a.CashBalance())
	}
}

func TestFillConflictRejectedAndStateUnchanged(t *testing.T) {
	a := setup(t, 1000)
	o, _ := a.PlaceOrder("A", Buy, 100, 5)
	if _, err := a.SubmitFill(1, o.ID, 3, 100); err != nil {
		t.Fatal(err)
	}
	// 相同编号、不同内容：明确报错，状态不变。
	if _, err := a.SubmitFill(1, o.ID, 4, 100); err == nil {
		t.Fatal("expected error for conflicting fill id")
	}
	if _, err := a.SubmitFill(1, o.ID, 3, 99); err == nil {
		t.Fatal("expected error for conflicting fill id")
	}
	if len(a.Fills(o.ID)) != 1 {
		t.Fatal("conflict must not add event")
	}
	if a.CashBalance() != 700 {
		t.Fatalf("cash=%d, want 700", a.CashBalance())
	}
}

// ---------- 撤单 ----------

func TestCancelReleasesOnlyUnfilledPart(t *testing.T) {
	a := setup(t, 1000)
	o, _ := a.PlaceOrder("A", Buy, 100, 10)
	if _, err := a.SubmitFill(1, o.ID, 4, 100); err != nil {
		t.Fatal(err)
	}
	// 撤单：释放未成交 6 股的冻结资金 600，已成交 4 股保留。
	co, err := a.CancelOrder(o.ID)
	if err != nil || co.Status != OrderCancelled {
		t.Fatalf("cancel should succeed: %+v err=%v", co, err)
	}
	if a.FrozenCash() != 0 || a.AvailableCash() != 600 || a.CashBalance() != 600 {
		t.Fatalf("cash=%d frozen=%d available=%d", a.CashBalance(), a.FrozenCash(), a.AvailableCash())
	}
	// 已成交部分保留：持仓 4，现金 600。
	if a.Position("A") != 4 {
		t.Fatalf("position=%d, want 4 (filled part kept)", a.Position("A"))
	}
	if co.CancelReason == "" {
		t.Fatal("cancel should record reason")
	}
}

func TestCancelFullyFilledOrderRejected(t *testing.T) {
	a := setup(t, 1000)
	o, _ := a.PlaceOrder("A", Buy, 100, 5)
	if _, err := a.SubmitFill(1, o.ID, 5, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CancelOrder(o.ID); err == nil {
		t.Fatal("fully filled order cannot be cancelled")
	}
}

func TestCancelUnknownOrCancelledOrRejected(t *testing.T) {
	a := setup(t, 1000)
	if _, err := a.CancelOrder(999); err == nil {
		t.Fatal("unknown order cannot be cancelled")
	}
	o, _ := a.PlaceOrder("A", Buy, 100, 5)
	if _, err := a.CancelOrder(o.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CancelOrder(o.ID); err == nil {
		t.Fatal("already cancelled order cannot be cancelled again")
	}
	// 被拒绝的订单不能撤销。
	rj, _ := a.PlaceOrder("A", Buy, 100, 99)
	if _, err := a.CancelOrder(rj.ID); err == nil {
		t.Fatal("rejected order cannot be cancelled")
	}
}

// ---------- 持仓限额 ----------

func TestLowerLimitCancelsLastBuysFirst(t *testing.T) {
	a := setup(t, 10000)
	// 三笔买单，各 10 股，持仓 0 + 买单剩余 30 ≤ 100。
	o1, _ := a.PlaceOrder("A", Buy, 100, 10)
	o2, _ := a.PlaceOrder("A", Buy, 100, 10)
	o3, _ := a.PlaceOrder("A", Buy, 100, 10)
	// 限额降到 15：持仓 0 + 买单剩余需 ≤ 15。
	// 从最后接受的买单开始：撤 o3（剩 20），仍超；撤 o2（剩 10），满足。
	if err := a.SetPositionLimit("A", 15); err != nil {
		t.Fatal(err)
	}
	if a.Order(o1.ID).Status != OrderPending {
		t.Fatal("o1 should remain pending")
	}
	if a.Order(o2.ID).Status != OrderCancelled || a.Order(o3.ID).Status != OrderCancelled {
		t.Fatal("o2 and o3 should be force-cancelled")
	}
	if a.Order(o2.ID).CancelReason == "" || a.Order(o3.ID).CancelReason == "" {
		t.Fatal("force cancel should record specific reason")
	}
	// 释放现金与持仓额度同时完成：冻结 = 10*100 = 1000。
	if a.FrozenCash() != 1000 || a.AvailableCash() != 9000 {
		t.Fatalf("frozen=%d available=%d", a.FrozenCash(), a.AvailableCash())
	}
}

func TestLowerLimitBelowPositionCancelsAllBuysAndRejectsNew(t *testing.T) {
	a := setup(t, 10000)
	// 先买 40 股并成交，持仓 40。
	o1, _ := a.PlaceOrder("A", Buy, 100, 40)
	if _, err := a.SubmitFill(1, o1.ID, 40, 100); err != nil {
		t.Fatal(err)
	}
	// 再挂两笔买单各 10 股。
	o2, _ := a.PlaceOrder("A", Buy, 100, 10)
	o3, _ := a.PlaceOrder("A", Buy, 100, 10)
	// 限额降到 20：持仓 40 已超限 → 撤掉全部未成交买单。
	if err := a.SetPositionLimit("A", 20); err != nil {
		t.Fatal(err)
	}
	if a.Order(o2.ID).Status != OrderCancelled || a.Order(o3.ID).Status != OrderCancelled {
		t.Fatal("all unfilled buys should be cancelled when position exceeds limit")
	}
	if a.Order(o2.ID).CancelReason == "" {
		t.Fatal("force cancel should explain position-over-limit reason")
	}
	// 后续买单被拒绝。
	bo, _ := a.PlaceOrder("A", Buy, 100, 1)
	if bo.Status != OrderRejected {
		t.Fatalf("new buy should be rejected while position exceeds limit, got %+v", bo)
	}
	// 卖出仍允许，不自动平仓。
	so, err := a.PlaceOrder("A", Sell, 100, 10)
	if err != nil || so.Status != OrderPending {
		t.Fatalf("sell should still be allowed: %+v err=%v", so, err)
	}
	if a.Position("A") != 40 {
		t.Fatalf("position should be kept (no forced liquidation), got %d", a.Position("A"))
	}
}

func TestSellOrdersDontOffsetLimit(t *testing.T) {
	a := setup(t, 10000)
	// 持仓 40，挂卖单 10（冻结持仓），再挂买单：买单占用按持仓+买单剩余计，卖单不抵消。
	o1, _ := a.PlaceOrder("A", Buy, 100, 40)
	if _, err := a.SubmitFill(1, o1.ID, 40, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := a.PlaceOrder("A", Sell, 100, 10); err != nil {
		t.Fatal(err)
	}
	// 持仓 40 + 买单剩余 0 + 新买单 60 = 100 ≤ 100，可以。
	bo, _ := a.PlaceOrder("A", Buy, 100, 60)
	if bo.Status != OrderPending {
		t.Fatalf("buy should be accepted, got %+v", bo)
	}
	// 持仓 40 + 60 + 1 = 101 > 100，拒绝（卖单不抵消占用）。
	bo2, _ := a.PlaceOrder("A", Buy, 100, 1)
	if bo2.Status != OrderRejected {
		t.Fatalf("sell orders must not offset position limit, got %+v", bo2)
	}
}

func TestRaisingLimitDoesNothing(t *testing.T) {
	a := setup(t, 10000)
	o, _ := a.PlaceOrder("A", Buy, 100, 50)
	if err := a.SetPositionLimit("A", 200); err != nil {
		t.Fatal(err)
	}
	if a.Order(o.ID).Status != OrderPending {
		t.Fatal("raising limit must not cancel orders")
	}
}

func TestSetLimitInvalid(t *testing.T) {
	a, _ := NewAccount(100)
	if err := a.SetPositionLimit("A", -1); err == nil {
		t.Fatal("negative limit should error")
	}
	if _, ok := a.PositionLimit("A"); ok {
		t.Fatal("failed limit set should not be recorded")
	}
}

// ---------- 记录与快照 ----------

func TestRecordsSaveSnapshotsAndAreImmutable(t *testing.T) {
	a := setup(t, 1000) // 行情 seq=1 price=100，限额 100
	o, _ := a.PlaceOrder("A", Buy, 100, 5)
	if _, err := a.SubmitFill(1, o.ID, 2, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CancelOrder(o.ID); err != nil {
		t.Fatal(err)
	}
	got := a.Order(o.ID)
	if got.Quote == nil || got.Quote.Seq != 1 || got.Quote.Price != 100 {
		t.Fatalf("acceptance snapshot wrong: %+v", got.Quote)
	}
	if !got.LimitSet || got.Limit != 100 {
		t.Fatalf("acceptance limit snapshot wrong: %+v", got)
	}
	if got.CancelQuote == nil || got.CancelQuote.Seq != 1 {
		t.Fatalf("cancel snapshot wrong: %+v", got.CancelQuote)
	}
	// 成交快照。
	fills := a.Fills(o.ID)
	if len(fills) != 1 || fills[0].Quote == nil || fills[0].Quote.Seq != 1 {
		t.Fatalf("fill snapshot wrong: %+v", fills)
	}
	// 后续行情与限额变化不能改写记录。
	if err := a.AddQuote("A", 2, 20, 200); err != nil {
		t.Fatal(err)
	}
	if err := a.SetPositionLimit("A", 10); err != nil {
		t.Fatal(err)
	}
	got = a.Order(o.ID)
	if got.Quote == nil || got.Quote.Seq != 1 || got.Quote.Price != 100 {
		t.Fatalf("acceptance record was rewritten: %+v", got.Quote)
	}
	if got.Limit != 100 {
		t.Fatalf("acceptance limit record was rewritten: %+v", got)
	}
	fills = a.Fills(o.ID)
	if fills[0].Quote == nil || fills[0].Quote.Seq != 1 || fills[0].Quote.Price != 100 {
		t.Fatalf("fill record was rewritten: %+v", fills[0].Quote)
	}
}

func TestRejectionWithNoQuoteHasEmptySnapshot(t *testing.T) {
	a, _ := NewAccount(1000)
	if err := a.SetPositionLimit("A", 100); err != nil {
		t.Fatal(err)
	}
	o, _ := a.PlaceOrder("A", Buy, 100, 1)
	if o.Status != OrderRejected {
		t.Fatalf("expected rejection, got %+v", o)
	}
	if o.Quote != nil {
		t.Fatalf("missing quote must be explicitly empty, got %+v", o.Quote)
	}
	if o.RejectReason == "" {
		t.Fatal("rejection must explain reason")
	}
}

func TestReturnedOrderIsACopy(t *testing.T) {
	a := setup(t, 1000)
	o, _ := a.PlaceOrder("A", Buy, 100, 5)
	o.Status = OrderFilled
	o.FilledQty = 5
	o2 := a.Order(o.ID)
	if o2.Status != OrderPending || o2.FilledQty != 0 {
		t.Fatal("returned order must be a copy; internal state mutated")
	}
}

func TestMultipleContractsAreIndependent(t *testing.T) {
	a := setup(t, 1000)
	if err := a.SetPositionLimit("B", 10); err != nil {
		t.Fatal(err)
	}
	if err := a.AddQuote("B", 1, 10, 50); err != nil {
		t.Fatal(err)
	}
	ao, _ := a.PlaceOrder("A", Buy, 100, 5)
	bo, _ := a.PlaceOrder("B", Buy, 50, 5)
	if ao.Status != OrderPending || bo.Status != OrderPending {
		t.Fatalf("both should be accepted: %+v %+v", ao, bo)
	}
	if a.FrozenCash() != 750 { // 5*100 + 5*50
		t.Fatalf("frozen=%d", a.FrozenCash())
	}
	// B 合约限额 10：持仓 0 + 买单 5 + 6 = 11 > 10，拒绝。
	bo2, _ := a.PlaceOrder("B", Buy, 50, 6)
	if bo2.Status != OrderRejected {
		t.Fatalf("B limit should be independent, got %+v", bo2)
	}
}
