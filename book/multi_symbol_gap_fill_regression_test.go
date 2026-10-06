package book

import (
	"strings"
	"testing"
)

// 本文件为多个合约同时出现行情缺口时的报价补齐补充回归保障：两个合约各自从
// 序号 1 开始独立判断连续、等待与缺口，互不串号——一边补齐缺口只能推进本合约
// 的最新报价、解除本合约的缺口并放行本合约的新买单，另一合约的最新报价、缺口
// 与等待中的报价必须原样保留。是否补齐只看报价序号，模拟时刻不代替序号判断。
//
// 场景按给定数值构造：A、B 先各有 seq1 的有效报价（A 时刻100/价格10，
// B 时刻200/价格20），序号相同、价格与时刻各不相同；随后交错提交 A seq3@300/12、
// A seq5@500/14、B seq3@600/2，两边都缺 seq2，全部只进入等待并返回新生效 0。
// 补交 A seq2@200/11 一次生效 seq2/seq3 两条（最新报价推进到 seq3@12，seq5 仍
// 等待，A 仍报缺口）；再补 A seq4@400/13，seq4 与等待的 seq5 一次生效两条，
// A 推进到 seq5@14 并解除缺口；最后补 B seq2@400/20，seq2 与最早等待的
// seq3@2 一次生效两条，B 解除缺口，A 不受影响。
//
// 全程不开启交易日与账户总持仓金额上限，并保留此前已接受的未成交订单与已入账
// 持仓：报价进入等待或补齐生效本身不结算成交，现金余额、买单现金占用、持仓
// 数量与既有订单状态全部保持原值；只有缺口解除后成功接受的新买单才按原有
// 限价 × 数量规则增加现金占用。接受与拒绝记录分别固化所属合约当时已生效报价
// 的序号、时刻与价格，不能混用另一合约的报价，也不能记录尚在等待的报价。

// multiSymbolGapSetup 构造共用前置状态：
// 初始现金 1000，A、B 持仓限额均充足。
//   - A：2 股买单 @10 全部成交（cash=980, posA=2），另有 1 股 @10 未成交买单
//     aLive（占用现金 10）；
//   - B：1 股买单 @20 全部成交（cash=960, posB=1），另有 1 股 @20 未成交买单
//     bLive（占用现金 20）。
//
// 报价基线：A seq1@100/10、B seq1@200/20。
// 返回引擎、两个未成交订单编号与 setup 结束后的记录条数。
func multiSymbolGapSetup(t *testing.T) (e *Engine, aLive, bLive int64, recsBase int) {
	t.Helper()
	e, _ = NewEngine(1000)
	mustSetMax(t, e, "A", 100)
	mustSetMax(t, e, "B", 100)
	mustQuote(t, e, "A", Quote{Seq: 1, Moment: 100, Price: 10}, 1)
	mustQuote(t, e, "B", Quote{Seq: 1, Moment: 200, Price: 20}, 1)

	aFilled := mustBuy(t, e, "A", 2, 10)
	if _, err := e.Fill(Trade{TradeID: 1, OrderID: aFilled, Symbol: "A", Side: Buy, Price: 10, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	aLive = mustBuy(t, e, "A", 1, 10)

	bFilled := mustBuy(t, e, "B", 1, 20)
	if _, err := e.Fill(Trade{TradeID: 2, OrderID: bFilled, Symbol: "B", Side: Buy, Price: 20, Qty: 1}); err != nil {
		t.Fatal(err)
	}
	bLive = mustBuy(t, e, "B", 1, 20)

	if e.Cash() != 960 || e.ReservedCash() != 30 || e.AvailableCash() != 930 ||
		e.Position("A") != 2 || e.Position("B") != 1 {
		t.Fatalf("前置状态错误: cash=%d reserved=%d available=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}
	if o, _ := e.Order(aFilled); o.Status != StatusFilled {
		t.Fatalf("A 已成交订单应全部成交: %+v", o)
	}
	if o, _ := e.Order(bFilled); o.Status != StatusFilled {
		t.Fatalf("B 已成交订单应全部成交: %+v", o)
	}
	return e, aLive, bLive, len(e.Records())
}

// assertLedgerFrozen 校验报价等待/补齐过程中不得结算成交：现金、买单现金占用、
// 持仓与既有未成交订单全部保持 setup 结束时的值。
func assertLedgerFrozen(t *testing.T, e *Engine, aLive, bLive int64) {
	t.Helper()
	if e.Cash() != 960 || e.ReservedCash() != 30 || e.AvailableCash() != 930 ||
		e.Position("A") != 2 || e.Position("B") != 1 {
		t.Fatalf("报价等待或生效不得结算成交: cash=%d reserved=%d available=%d posA=%d posB=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}
	if o, _ := e.Order(aLive); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 1 {
		t.Fatalf("A 既有未成交订单必须原样保留: %+v", o)
	}
	if o, _ := e.Order(bLive); o.Status != StatusPending || o.Filled != 0 || o.Remaining() != 1 {
		t.Fatalf("B 既有未成交订单必须原样保留: %+v", o)
	}
}

func TestMultiSymbolGapFillIndependent(t *testing.T) {
	e, aLive, bLive, recsBase := multiSymbolGapSetup(t)

	// 两项保护均不启用。
	if s := e.RiskStatus(); s.Open {
		t.Fatalf("场景要求不开启日内亏损保护: %+v", s)
	}
	if s := e.PositionAmountStatus(); s.Enabled {
		t.Fatalf("场景要求不启用账户总持仓金额上限: %+v", s)
	}

	// 两边交错提交同序号、不同价格与时刻的跨号报价：都缺 seq2，全部只等待。
	mustQuote(t, e, "A", Quote{Seq: 3, Moment: 300, Price: 12}, 0)
	mustQuote(t, e, "A", Quote{Seq: 5, Moment: 500, Price: 14}, 0)
	mustQuote(t, e, "B", Quote{Seq: 3, Moment: 600, Price: 2}, 0)

	if !e.HasGap("A") || !e.HasGap("B") {
		t.Fatal("A、B 都缺少 seq2，必须各自报告缺口")
	}
	if q, ok := e.CurrentQuote("A"); !ok || q != (Quote{Seq: 1, Moment: 100, Price: 10}) {
		t.Fatalf("等待不推进最新报价，A 应停在 seq1@100/10: %+v ok=%v", q, ok)
	}
	if q, ok := e.CurrentQuote("B"); !ok || q != (Quote{Seq: 1, Moment: 200, Price: 20}) {
		t.Fatalf("等待不推进最新报价，B 应停在 seq1@200/20: %+v ok=%v", q, ok)
	}
	assertLedgerFrozen(t, e, aLive, bLive)

	// 两边都有缺口时新买单分别被拒；拒绝记录固化各自 seq1 报价。
	if _, err := e.Buy("A", 1, 10); err == nil || !strings.Contains(err.Error(), "缺口") {
		t.Fatalf("A 缺口期间新买单必须因缺口被拒绝，实际 %v", err)
	}
	if _, err := e.Buy("B", 1, 20); err == nil || !strings.Contains(err.Error(), "缺口") {
		t.Fatalf("B 缺口期间新买单必须因缺口被拒绝，实际 %v", err)
	}
	assertLedgerFrozen(t, e, aLive, bLive)

	// 补交 A seq2（时刻 200 与 B seq1 相同，但时刻不参与补齐判断）：
	// 链为 seq2@11 → seq3@12，一次生效 2 条；seq5 仍等待，A 仍报缺口。
	n, err := e.UpdateQuote("A", Quote{Seq: 2, Moment: 200, Price: 11})
	if err != nil || n != 2 {
		t.Fatalf("补交 A seq2 应新生效 2 条: n=%d err=%v", n, err)
	}
	if q, ok := e.CurrentQuote("A"); !ok || q != (Quote{Seq: 3, Moment: 300, Price: 12}) {
		t.Fatalf("A 最新报价应推进到 seq3@300/12（不能跳过缺口直接用等待的 seq5）: %+v ok=%v", q, ok)
	}
	if !e.HasGap("A") {
		t.Fatal("seq5 仍在等待，A 必须继续报告缺口")
	}
	// 等待中的 seq5 同内容重放为无操作，证明等待报价保留且尚未生效。
	mustQuote(t, e, "A", Quote{Seq: 5, Moment: 500, Price: 14}, 0)

	// 只补齐 A 不得推进 B：B 最新报价、缺口与等待中的 seq3 全部原样保留。
	if q, ok := e.CurrentQuote("B"); !ok || q != (Quote{Seq: 1, Moment: 200, Price: 20}) {
		t.Fatalf("补齐 A 不得推进 B 的报价: %+v ok=%v", q, ok)
	}
	if !e.HasGap("B") {
		t.Fatal("补齐 A 不得解除 B 的缺口")
	}
	mustQuote(t, e, "B", Quote{Seq: 3, Moment: 600, Price: 2}, 0) // B seq3 仍在等待
	assertLedgerFrozen(t, e, aLive, bLive)

	// A 仍缺 seq4：新买单继续被拒，拒绝记录锚定当时已生效的 seq3@300/12，
	// 不能记录等待中的 seq5，也不能借用 B 的报价。
	if _, err := e.Buy("A", 1, 10); err == nil || !strings.Contains(err.Error(), "缺口") {
		t.Fatalf("A 仍有缺口时新买单必须被拒绝，实际 %v", err)
	}

	// 补 A seq4：seq4 与原等待的 seq5 一起生效，返回 2。
	n, err = e.UpdateQuote("A", Quote{Seq: 4, Moment: 400, Price: 13})
	if err != nil || n != 2 {
		t.Fatalf("补交 A seq4 应使 seq4/seq5 新生效 2 条: n=%d err=%v", n, err)
	}
	if q, ok := e.CurrentQuote("A"); !ok || q != (Quote{Seq: 5, Moment: 500, Price: 14}) {
		t.Fatalf("A 最新报价应成为 seq5@500/14: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("seq2..seq5 已连续生效，A 缺口必须解除")
	}
	// B 不受 A 补齐影响。
	if q, _ := e.CurrentQuote("B"); q.Seq != 1 || !e.HasGap("B") {
		t.Fatalf("A 解除缺口后 B 必须维持原样: quote=%+v gap=%v", q, e.HasGap("B"))
	}

	// A 缺口解除、现金与持仓限额充足：新买单接受，按限价×数量增加占用 14×3=42；
	// 拒绝不消耗订单编号，新单编号接在 setup 最后一个订单（4）之后为 5。
	newBuy := mustBuy(t, e, "A", 3, 14)
	if newBuy != 5 {
		t.Fatalf("此前被拒的委托不得消耗订单编号，新单应为 5，实际 %d", newBuy)
	}
	if e.Cash() != 960 || e.ReservedCash() != 72 || e.AvailableCash() != 888 {
		t.Fatalf("接受新买单只增加限价占用 42: cash=%d reserved=%d available=%d",
			e.Cash(), e.ReservedCash(), e.AvailableCash())
	}
	if e.Position("A") != 2 {
		t.Fatalf("买单接受未成交前不得改变持仓: %d", e.Position("A"))
	}

	// B 仍有本合约缺口：B 的新买单继续被拒，且不能因为 A 已解除缺口而放行。
	if _, err := e.Buy("B", 1, 20); err == nil || !strings.Contains(err.Error(), "缺口") {
		t.Fatalf("B 本合约仍有缺口，新买单必须被拒绝，实际 %v", err)
	}
	// B 被拒不改变 A 新单占用与账实。
	if e.ReservedCash() != 72 || e.AvailableCash() != 888 ||
		e.Position("A") != 2 || e.Position("B") != 1 {
		t.Fatalf("B 新买单被拒不得改变任何账实: reserved=%d available=%d posA=%d posB=%d",
			e.ReservedCash(), e.AvailableCash(), e.Position("A"), e.Position("B"))
	}

	// 最后补 B seq2：seq2 与最初等待的 seq3@2 一次生效 2 条，B 解除缺口。
	n, err = e.UpdateQuote("B", Quote{Seq: 2, Moment: 400, Price: 20})
	if err != nil || n != 2 {
		t.Fatalf("补交 B seq2 应使 seq2/seq3 新生效 2 条: n=%d err=%v", n, err)
	}
	if q, ok := e.CurrentQuote("B"); !ok || q != (Quote{Seq: 3, Moment: 600, Price: 2}) {
		t.Fatalf("B 最新报价应成为最初等待的 seq3@600/2: %+v ok=%v", q, ok)
	}
	if e.HasGap("B") {
		t.Fatal("B 缺口必须解除")
	}
	// A 的报价与缺口状态不受 B 补齐影响。
	if q, ok := e.CurrentQuote("A"); !ok || q != (Quote{Seq: 5, Moment: 500, Price: 14}) {
		t.Fatalf("补齐 B 不得影响 A 的最新报价: %+v ok=%v", q, ok)
	}
	if e.HasGap("A") {
		t.Fatal("A 必须保持无缺口")
	}

	// 既有未成交订单与已入账持仓自始至终保留。
	assertLedgerFrozenExceptNewBuy := func() {
		if o, _ := e.Order(aLive); o.Status != StatusPending || o.Remaining() != 1 {
			t.Fatalf("A 既有未成交订单必须保留: %+v", o)
		}
		if o, _ := e.Order(bLive); o.Status != StatusPending || o.Remaining() != 1 {
			t.Fatalf("B 既有未成交订单必须保留: %+v", o)
		}
		if e.Position("A") != 2 || e.Position("B") != 1 {
			t.Fatalf("已入账持仓必须保留: posA=%d posB=%d", e.Position("A"), e.Position("B"))
		}
	}
	assertLedgerFrozenExceptNewBuy()
	if o, _ := e.Order(newBuy); o.Status != StatusPending || o.Remaining() != 3 {
		t.Fatalf("新接受的 A 买单应待成交、剩余 3: %+v", o)
	}

	// 记录核对：setup 之后只允许出现 2 条 A 拒绝、2 条 B 拒绝、1 条 A 接受；
	// 报价提交本身不产生记录，且全程无成交、撤销与触线记录。
	recs := e.Records()[recsBase:]
	var rejectedA, rejectedB []Record
	var acceptedNew *Record
	for _, r := range recs {
		switch r.Kind {
		case RecordRejected:
			if r.Symbol == "A" {
				rejectedA = append(rejectedA, r)
			} else if r.Symbol == "B" {
				rejectedB = append(rejectedB, r)
			} else {
				t.Fatalf("出现非 A/B 合约的拒绝记录: %+v", r)
			}
			if !r.QuoteValid {
				t.Fatalf("缺口拒绝记录必须携带当时已生效报价快照: %+v", r)
			}
		case RecordAccepted:
			if r.OrderID == newBuy {
				rr := r
				acceptedNew = &rr
			} else {
				t.Fatalf("setup 之后只应接受新 A 买单，意外接受记录: %+v", r)
			}
		case RecordFilled, RecordCanceled, RecordRiskTriggered, RecordModified:
			t.Fatalf("报价补齐场景不得产生 %s 记录: %+v", r.Kind, r)
		}
	}
	if len(rejectedA) != 2 {
		t.Fatalf("A 应有 2 条缺口拒绝（seq3 时、seq5 等待时各一条），实际 %d", len(rejectedA))
	}
	if len(rejectedB) != 2 {
		t.Fatalf("B 应有 2 条缺口拒绝（初始等待时、A 解除后各一条），实际 %d", len(rejectedB))
	}

	// A 第一条拒绝锚定 seq1@100/10（当时最新仍是 seq1）。
	if r := rejectedA[0]; r.QuoteSeq != 1 || r.QuoteMoment != 100 || r.QuotePrice != 10 {
		t.Fatalf("A 首条拒绝应固化 seq1@100/10: seq=%d moment=%d price=%d",
			r.QuoteSeq, r.QuoteMoment, r.QuotePrice)
	}
	// A 第二条拒绝发生在 seq2/seq3 生效之后：锚定 seq3@300/12，
	// 不能记录尚在等待的 seq5@14，也不能借用 B 的报价。
	if r := rejectedA[1]; r.QuoteSeq != 3 || r.QuoteMoment != 300 || r.QuotePrice != 12 {
		t.Fatalf("A 第二条拒绝应固化当时已生效的 seq3@300/12: seq=%d moment=%d price=%d",
			r.QuoteSeq, r.QuoteMoment, r.QuotePrice)
	}
	// B 的两条拒绝始终锚定 B seq1@200/20——即使 A 已推进到 seq5，也不能混用。
	for i, r := range rejectedB {
		if r.QuoteSeq != 1 || r.QuoteMoment != 200 || r.QuotePrice != 20 {
			t.Fatalf("B 第 %d 条拒绝应固化本合约 seq1@200/20，不得混用 A 的报价: seq=%d moment=%d price=%d",
				i+1, r.QuoteSeq, r.QuoteMoment, r.QuotePrice)
		}
	}
	// 新接受的 A 买单在 seq5@500/14 解除缺口后产生，记录必须锚定 seq5。
	if acceptedNew == nil {
		t.Fatal("缺少新 A 买单的接受记录")
	}
	if acceptedNew.QuoteSeq != 5 || acceptedNew.QuoteMoment != 500 || acceptedNew.QuotePrice != 14 {
		t.Fatalf("新 A 买单接受记录应固化 seq5@500/14: seq=%d moment=%d price=%d",
			acceptedNew.QuoteSeq, acceptedNew.QuoteMoment, acceptedNew.QuotePrice)
	}
	if acceptedNew.Limit != 14 || acceptedNew.Qty != 3 || acceptedNew.Remaining != 3 {
		t.Fatalf("接受记录应保存委托本身: %+v", acceptedNew)
	}
}
