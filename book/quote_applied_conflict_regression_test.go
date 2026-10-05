package book

import (
	"strings"
	"testing"
)

// 本文件为“已生效旧报价晚到被调用方重复提交”补充回归保障：行情推进到更新的
// 已生效序号后，同一旧序号的原始内容再次提交必须幂等成功并返回零条新生效报价；
// 只改价格或只改模拟时刻都只能得到内容冲突错误与零条新生效报价。最新报价必须
// 停留在更大的已生效序号，旧数据不能让行情回退，也不能把冲突内容当成新行情
// 重新计算日内亏损保护或账户总持仓金额上限——即使冲突价格为正且低到足以触线、
// 或高到足以使金额超限，也不得限制买入、撤销剩余买单或释放占用。
//
// 共用前置状态（appliedConflictSetup），完全按场景给定的数值构造：
// 初始现金 1000，A 合约最大持仓量充足；seq1@（时刻10,价格10）已生效；
// 数量 6、限价 10 的买单已按价格 10 成交 2 股；随后开启亏损上限为 5 的交易日，
// 再设置账户总持仓金额上限 100，并让 seq2@（时刻20,价格12）生效。
// 此时现金余额 980、买单现金占用 40、持仓 2；持仓金额 24、剩余买单金额占用
// 48（4 股 × max(限价10, 报价12)）、合计 72；日内基准净值 1000、当前净值
// 1004、亏损为零、未触线；订单仍为部分成交（总量 6、已成交 2、剩余 4）。

// appliedConflictSnap 是旧报价重放/冲突前后必须逐项保持的全部可观察状态。
type appliedConflictSnap struct {
	cash, reserved, available, position int64
	sellable                            int64

	risk RiskStatus
	amt  PositionAmountStatus

	quote   Quote
	quoteOK bool

	order   Order
	orderOK bool

	recs int
}

func appliedConflictSetup(t *testing.T) (e *Engine, bid int64, recsBase int) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 1000) // 合约持仓限额充足，任何撤单都只能来自风险保护
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 10, Price: 10}, 1)
	bid = mustBuy(t, e, "A", 6, 10)
	res, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res != (FillResult{TradeID: 1, OrderID: bid, Price: 10, Qty: 2,
		Filled: 2, Remaining: 4, Status: StatusPartial}) {
		t.Fatalf("前置成交结果应为累计 2、剩余 4、部分成交: %+v", res)
	}
	if err := e.StartTradingDay(1, 5); err != nil { // 基准净值 = 980 + 2×10 = 1000
		t.Fatal(err)
	}
	if _, err := e.SetPositionAmountLimit(100); err != nil { // 60 ≤ 100，不撤单
		t.Fatal(err)
	}
	mustQuote(t, e, "A", Quote{Seq: 2, Moment: 20, Price: 12}, 1)

	// 题述给定状态逐项核对。
	if e.Cash() != 980 || e.ReservedCash() != 40 || e.AvailableCash() != 940 ||
		e.Position("A") != 2 || e.Sellable("A") != 2 {
		t.Fatalf("前置资金/持仓错误: cash=%d reserved=%d available=%d pos=%d sellable=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"))
	}
	am := e.PositionAmountStatus()
	if !am.Enabled || am.Limit != 100 || am.Holding != 24 || am.BuyReserved != 48 || am.Total != 72 {
		t.Fatalf("前置金额状态错误: %+v", am)
	}
	risk := e.RiskStatus()
	if !risk.Open || risk.Day != 1 || risk.Baseline != 1000 || risk.Equity != 1004 ||
		risk.Loss != 0 || risk.LossLimit != 5 || risk.Restricted {
		t.Fatalf("前置风险状态错误: %+v", risk)
	}
	if o, ok := e.Order(bid); !ok || o.Status != StatusPartial || o.Qty != 6 ||
		o.Filled != 2 || o.Remaining() != 4 || o.Limit != 10 {
		t.Fatalf("前置订单应为部分成交、总量 6、已成交 2、剩余 4: %+v ok=%v", o, ok)
	}
	q, ok := e.CurrentQuote("A")
	if !ok || q != (Quote{Seq: 2, Moment: 20, Price: 12}) {
		t.Fatalf("前置最新报价应为 seq2（时刻20,价格12）: %+v ok=%v", q, ok)
	}
	return e, bid, len(e.Records())
}

func snapshotApplied(t *testing.T, e *Engine, bid int64) appliedConflictSnap {
	t.Helper()
	s := appliedConflictSnap{
		cash:      e.Cash(),
		reserved:  e.ReservedCash(),
		available: e.AvailableCash(),
		position:  e.Position("A"),
		sellable:  e.Sellable("A"),
		risk:      e.RiskStatus(),
		amt:       e.PositionAmountStatus(),
		recs:      len(e.Records()),
	}
	s.quote, s.quoteOK = e.CurrentQuote("A")
	s.order, s.orderOK = e.Order(bid)
	return s
}

// assertAppliedUnchanged 校验旧报价重放/冲突后资金、持仓、两类风险查询、最新报价、
// 部分成交订单与事件记录数量全部保持快照值；msg 用于定位是哪一次提交。
func assertAppliedUnchanged(t *testing.T, e *Engine, bid int64, s appliedConflictSnap, msg string) {
	t.Helper()
	if e.Cash() != s.cash || e.ReservedCash() != s.reserved || e.AvailableCash() != s.available ||
		e.Position("A") != s.position || e.Sellable("A") != s.sellable {
		t.Fatalf("%s后资金/持仓必须保持提交前值: cash=%d reserved=%d available=%d pos=%d sellable=%d（快照 %+v）",
			msg, e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Sellable("A"), s)
	}
	if risk := e.RiskStatus(); risk != s.risk {
		t.Fatalf("%s后日内风险查询必须保持不变: %+v，快照 %+v", msg, risk, s.risk)
	}
	if am := e.PositionAmountStatus(); am != s.amt {
		t.Fatalf("%s后账户金额查询必须保持不变: %+v，快照 %+v", msg, am, s.amt)
	}
	if q, ok := e.CurrentQuote("A"); ok != s.quoteOK || q != s.quote {
		t.Fatalf("%s后最新报价必须停留在更大的已生效序号: %+v ok=%v，快照 %+v ok=%v",
			msg, q, ok, s.quote, s.quoteOK)
	}
	if o, ok := e.Order(bid); ok != s.orderOK || o != s.order {
		t.Fatalf("%s后订单（含累计成交量与剩余量）必须保持: %+v ok=%v，快照 %+v",
			msg, o, ok, s.order)
	}
	if n := len(e.Records()); n != s.recs {
		t.Fatalf("%s不得新增事件记录: 现有 %d 条，快照 %d 条", msg, n, s.recs)
	}
}

// assertOriginalFillFact 核对首次成交入账留下的事实永不被改写：
// 成交价 10、成交时报价定位 seq1@（时刻10,价格10），且始终只有这一条成交记录。
func assertOriginalFillFact(t *testing.T, e *Engine, bid int64, msg string) {
	t.Helper()
	var found *Record
	recs := e.Records()
	for i := range recs {
		r := &recs[i]
		if r.Kind != RecordFilled {
			continue
		}
		if found != nil {
			t.Fatalf("%s后出现多条成交记录，首次成交事实不得被重复记账", msg)
		}
		found = r
	}
	if found == nil {
		t.Fatalf("首次成交记录必须保留")
	}
	if found.TradeID != 1 || found.OrderID != bid || found.Side != Buy ||
		found.TradePrice != 10 || found.Qty != 2 || found.Filled != 2 {
		t.Fatalf("%s后成交记录的成交价与成交量必须保留首次入账事实: %+v", msg, *found)
	}
	if !found.QuoteValid || found.QuoteSeq != 1 || found.QuoteMoment != 10 || found.QuotePrice != 10 {
		t.Fatalf("%s后成交记录的报价定位必须保留首次入账时的 seq1（时刻10,价格10）: %+v",
			msg, *found)
	}
}

// TestAppliedOldQuoteReplayAndConflictNeverReapplies 覆盖主场景：seq2 已生效后，
// 晚到的 seq1 原始报价重放成功返回 0；低价（若真生效会使日内亏损触线）、高价
// （若真生效会使账户金额超限）与仅改时刻的冲突提交都只报内容冲突、返回 0，
// 资金、持仓、两类风险查询、订单剩余量、事件记录与首次成交事实全部不变；
// 冲突之后原始旧报价仍按相同内容成功返回 0（被拒内容没有留成新的历史值）；
// 随后正常的 seq3 只生效一条，按价格 11 收敛金额与净值，原订单保留已成交
// 2 股、剩余 4 股。
func TestAppliedOldQuoteReplayAndConflictNeverReapplies(t *testing.T) {
	e, bid, recsBase := appliedConflictSetup(t)
	snap := snapshotApplied(t, e, bid)

	// 完全相同的 seq1 再次提交：成功返回零条新生效报价。
	if n, err := e.UpdateQuote("A", Quote{Seq: 1, Moment: 10, Price: 10}); err != nil || n != 0 {
		t.Fatalf("已生效旧报价原样重放必须成功返回 0 条: n=%d err=%v", n, err)
	}
	assertAppliedUnchanged(t, e, bid, snap, "原始旧报价重放")

	// 冲突低价 7（正价）：若被当成新行情，净值 980+2×7=994、亏损 6 ≥ 上限 5，
	// 本应触线撤单；实际只能报内容冲突，不得限制买入或撤销剩余买单。
	low := Quote{Seq: 1, Moment: 10, Price: 7}
	if n, err := e.UpdateQuote("A", low); err == nil || n != 0 {
		t.Fatalf("旧序号只改价格（低到触线）必须报内容冲突并返回 0: n=%d err=%v", n, err)
	} else if !strings.Contains(err.Error(), "内容冲突") || !strings.Contains(err.Error(), "序号 1") {
		t.Fatalf("错误必须指明报价序号 1 内容冲突: %v", err)
	}
	assertAppliedUnchanged(t, e, bid, snap, "触线价冲突")
	if e.RiskStatus().Restricted {
		t.Fatal("冲突低价不得触发日内亏损限制")
	}

	// 冲突高价 50：若被当成新行情，持仓金额 100 + 买单占用 4×50=200，合计 300
	// 远超上限 100，本应撤光买单；实际只能报内容冲突，不得撤销或释放占用。
	high := Quote{Seq: 1, Moment: 10, Price: 50}
	if n, err := e.UpdateQuote("A", high); err == nil || n != 0 {
		t.Fatalf("旧序号只改价格（高到超限）必须报内容冲突并返回 0: n=%d err=%v", n, err)
	}
	assertAppliedUnchanged(t, e, bid, snap, "超限价冲突")
	if o, _ := e.Order(bid); o.Status != StatusPartial || o.Remaining() != 4 {
		t.Fatalf("冲突高价不得撤销剩余买单或释放占用: %+v", o)
	}

	// 仅改模拟时刻：价格不变同样不能通过。
	if n, err := e.UpdateQuote("A", Quote{Seq: 1, Moment: 99, Price: 10}); err == nil || n != 0 {
		t.Fatalf("旧序号只改时刻必须报内容冲突并返回 0: n=%d err=%v", n, err)
	}
	assertAppliedUnchanged(t, e, bid, snap, "改时刻冲突")

	// 冲突之后再提交原始旧报价：仍作为相同内容成功返回零条；先前被拒的价格
	// 没有留成 seq1 的新历史值（用冲突价再提交一次仍须报冲突来反证）。
	if n, err := e.UpdateQuote("A", Quote{Seq: 1, Moment: 10, Price: 10}); err != nil || n != 0 {
		t.Fatalf("冲突后原始旧报价重放必须仍成功返回 0 条: n=%d err=%v", n, err)
	}
	assertAppliedUnchanged(t, e, bid, snap, "冲突后原始旧报价重放")
	if n, err := e.UpdateQuote("A", low); err == nil || n != 0 {
		t.Fatalf("被拒的冲突价格不得留成新的历史值，再次提交仍须冲突: n=%d err=%v", n, err)
	}
	assertAppliedUnchanged(t, e, bid, snap, "冲突价二次提交")

	// 以上重放与冲突提交均不得新增任何记录，也不得出现触线/撤销/拒绝记录。
	if len(e.Records()) != recsBase {
		t.Fatalf("重复与冲突提交均不得新增事件记录: 基础 %d，现有 %d", recsBase, len(e.Records()))
	}
	for _, r := range e.Records()[recsBase:] {
		t.Fatalf("重复与冲突提交不得留下任何记录，意外出现 %s: %+v", r.Kind, r)
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("旧报价冲突不得产生风险触线记录")
	}
	assertOriginalFillFact(t, e, bid, "旧报价冲突")

	// 随后提交正常的 seq3@（时刻30,价格11）：只有一条报价生效，不触发任何保护。
	recsBefore3 := len(e.Records())
	if n, err := e.UpdateQuote("A", Quote{Seq: 3, Moment: 30, Price: 11}); err != nil || n != 1 {
		t.Fatalf("正常新报价 seq3 应唯一生效 1 条: n=%d err=%v", n, err)
	}
	q, ok := e.CurrentQuote("A")
	if !ok || q != (Quote{Seq: 3, Moment: 30, Price: 11}) {
		t.Fatalf("最新报价应推进到 seq3（时刻30,价格11），旧报价不得回退: %+v ok=%v", q, ok)
	}
	// 持仓金额 2×11=22；买单 4×max(限价10,报价11)=44；合计 66 ≤ 100，不撤单。
	am := e.PositionAmountStatus()
	if !am.Enabled || am.Limit != 100 || am.Holding != 22 || am.BuyReserved != 44 || am.Total != 66 {
		t.Fatalf("seq3 后金额应按价格 11 计为 22/44、合计 66: %+v", am)
	}
	// 净值 980+22=1002，盈利使亏损记零，仍未触线；现金与限价占用不变。
	risk := e.RiskStatus()
	if !risk.Open || risk.Baseline != 1000 || risk.Equity != 1002 || risk.Loss != 0 ||
		risk.LossLimit != 5 || risk.Restricted {
		t.Fatalf("seq3 后风险状态应为净值 1002、亏损 0、未触线: %+v", risk)
	}
	if e.Cash() != 980 || e.ReservedCash() != 40 || e.AvailableCash() != 940 ||
		e.Position("A") != 2 {
		t.Fatalf("seq3 不改变现金、占用与持仓: cash=%d reserved=%d pos=%d",
			e.Cash(), e.ReservedCash(), e.Position("A"))
	}
	if o, _ := e.Order(bid); o.Status != StatusPartial || o.Qty != 6 ||
		o.Filled != 2 || o.Remaining() != 4 {
		t.Fatalf("原订单必须继续保留已成交 2 份、剩余 4 份: %+v", o)
	}
	// seq3 在两项保护下均不产生记录。
	if len(e.Records()) != recsBefore3 {
		t.Fatalf("未触线、未超限的报价生效不得新增记录: 之前 %d，现有 %d",
			recsBefore3, len(e.Records()))
	}

	// seq3 之后旧 seq1 的原始内容与冲突内容语义不变：前者成功 0 条，后者仍冲突。
	if n, err := e.UpdateQuote("A", Quote{Seq: 1, Moment: 10, Price: 10}); err != nil || n != 0 {
		t.Fatalf("推进到 seq3 后原始旧报价重放仍须成功返回 0: n=%d err=%v", n, err)
	}
	if n, err := e.UpdateQuote("A", Quote{Seq: 1, Moment: 10, Price: 7}); err == nil || n != 0 {
		t.Fatalf("推进到 seq3 后旧序号冲突仍须报错返回 0: n=%d err=%v", n, err)
	}
	if q2, ok2 := e.CurrentQuote("A"); !ok2 || q2 != (Quote{Seq: 3, Moment: 30, Price: 11}) {
		t.Fatalf("旧报价提交不得让最新报价从 seq3 回退: %+v ok=%v", q2, ok2)
	}

	// 已入账成交的幂等重放返回第一次结果（成交价 10），不新增记录、不改累计成交量。
	replay, err := e.Fill(Trade{TradeID: 1, OrderID: bid, Symbol: "A", Side: Buy, Price: 10, Qty: 2})
	if err != nil {
		t.Fatalf("首次成交的原样重放必须按幂等规则成功: %v", err)
	}
	if replay != (FillResult{TradeID: 1, OrderID: bid, Price: 10, Qty: 2,
		Filled: 2, Remaining: 4, Status: StatusPartial}) {
		t.Fatalf("成交重放必须原样返回首次入账结果: %+v", replay)
	}
	if o, _ := e.Order(bid); o.Filled != 2 || o.Remaining() != 4 {
		t.Fatalf("成交重放不得改变累计成交量与剩余量: %+v", o)
	}
	assertOriginalFillFact(t, e, bid, "成交重放")
}

// TestAppliedOldQuoteConflictNeverRestrictsNewBuys 单独锁定“冲突不限制买入”：
// 足以触线的旧序号冲突价被拒后，引擎既没有进入亏损限制，也没有撤销原剩余
// 买单；一笔满足现金与金额上限的新买单必须正常接受。随后再次提交同一冲突价，
// 仍只是内容冲突，两笔买单与全部占用保持不变。
func TestAppliedOldQuoteConflictNeverRestrictsNewBuys(t *testing.T) {
	e, bid, _ := appliedConflictSetup(t)

	if n, err := e.UpdateQuote("A", Quote{Seq: 1, Moment: 10, Price: 7}); err == nil || n != 0 {
		t.Fatalf("足以触线的旧序号冲突价必须只报冲突、0 条生效: n=%d err=%v", n, err)
	}
	if e.RiskStatus().Restricted {
		t.Fatal("冲突价不得让引擎进入亏损限制状态")
	}

	// 新买单 1 股限价 10：现金充足；金额占用按 max(限价10, 报价12)=12 计，
	// 72+12=84 ≤ 上限 100，必须接受——若冲突价被当成新行情触线，此单会被拒绝。
	recsBeforeBuy := len(e.Records())
	newBid := mustBuy(t, e, "A", 1, 10)

	if e.Cash() != 980 || e.ReservedCash() != 50 || e.AvailableCash() != 930 {
		t.Fatalf("新买单接受后只增加自身 10 股现金占用: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	am := e.PositionAmountStatus()
	if am.Holding != 24 || am.BuyReserved != 60 || am.Total != 84 {
		t.Fatalf("新买单后金额应为持仓 24、买单占用 48+12=60、合计 84: %+v", am)
	}
	if o, _ := e.Order(bid); o.Status != StatusPartial || o.Filled != 2 || o.Remaining() != 4 {
		t.Fatalf("原部分成交买单必须继续保留剩余 4 股: %+v", o)
	}
	if o, _ := e.Order(newBid); o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("新买单必须正常待成交: %+v", o)
	}
	recs := e.Records()
	if len(recs) != recsBeforeBuy+1 {
		t.Fatalf("接受新买单应只新增一条接受记录，实际 %d 条", len(recs)-recsBeforeBuy)
	}
	ar := recs[len(recs)-1]
	if ar.Kind != RecordAccepted || ar.OrderID != newBid || !ar.QuoteValid ||
		ar.QuoteSeq != 2 || ar.QuoteMoment != 20 || ar.QuotePrice != 12 {
		t.Fatalf("新买单接受记录的报价定位必须是当前最新 seq2（时刻20,价格12）: %+v", ar)
	}

	// 再次提交同一冲突价：仍只是内容冲突，不新增记录，不撤销任何一笔买单。
	if n, err := e.UpdateQuote("A", Quote{Seq: 1, Moment: 10, Price: 7}); err == nil || n != 0 {
		t.Fatalf("新买入后旧序号冲突价仍须只报冲突、0 条生效: n=%d err=%v", n, err)
	}
	if len(e.Records()) != len(recs) {
		t.Fatal("冲突提交不得新增事件记录")
	}
	if o, _ := e.Order(bid); o.Status != StatusPartial || o.Remaining() != 4 {
		t.Fatalf("冲突不得撤销原买单剩余量: %+v", o)
	}
	if o, _ := e.Order(newBid); o.Status != StatusPending || o.Remaining() != 1 {
		t.Fatalf("冲突不得撤销新接受的买单: %+v", o)
	}
	if e.ReservedCash() != 50 {
		t.Fatalf("冲突不得释放任何买单占用: reserved=%d", e.ReservedCash())
	}
	if countRiskRecords(e) != 0 {
		t.Fatal("全过程不得产生风险触线记录")
	}
}
