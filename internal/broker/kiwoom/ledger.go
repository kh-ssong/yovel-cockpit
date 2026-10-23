package kiwoom

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"
)

// ── 증권사 원장 (장 뒤 대조) ─────────────────────────────────────────────────
//
// ★ 콕핏 원장(order 행의 price·qty·fee_krw)은 체결 하나하나를 콕핏이 적은 것이다.
// 증권사는 하루·종목 합으로 수수료를 절사하는 등 계산이 다를 수 있어서, 그 숫자와 맞대 보려면
// **키움이 준 응답을 그대로** 남겨야 한다 — 여기서 다시 계산하거나 칸을 고르지 않는다.
// 해석(필드 뜻·부호)은 읽는 쪽(flat6 회고)이 한다. docs/ops.md «증권사 원장».

const ledgerSchema = "broker_ledger/1"

// ledgerMaxPages — 한 조회의 연속조회 상한. 하루 체결이 이걸 넘을 일은 없다 — 넘으면 끝없는 next-key 를 의심한다.
const ledgerMaxPages = 20

// Ledger — 하루치 증권사 원장. Sections 는 조회마다 하나 (실패한 조회는 Error 만 차 있다).
type Ledger struct {
	Schema    string          `json:"schema"`
	Broker    string          `json:"broker"`
	Date      string          `json:"date"` // YYYYMMDD (한국 날짜)
	FetchedAt time.Time       `json:"fetched_at"`
	Sections  []LedgerSection `json:"sections"`
}

// LedgerSection — 조회 하나. Pages 는 키움 응답 본문 그대로 (연속조회면 쪽마다 하나).
type LedgerSection struct {
	API     string            `json:"api"`
	Name    string            `json:"name"`
	Request map[string]string `json:"request"`
	Pages   []json.RawMessage `json:"pages,omitempty"`
	Error   string            `json:"error,omitempty"`
}

// OK — 조회 하나라도 성공했다.
func (l Ledger) OK() bool {
	for _, s := range l.Sections {
		if s.Error == "" && len(s.Pages) > 0 {
			return true
		}
	}
	return false
}

// ledgerQueries — 날짜 d(YYYYMMDD)에 묻는 조회들. today 가 아니면 당일 전용 조회(ka10076)는 뺀다.
//
//   - ka10170 당일매매일지 — 종목별 매수·매도 수량·평단·금액·수수료+세금·손익 (영웅문 매매일지)
//   - ka10074 일자별실현손익 — 계좌 합 실현손익·수수료·세금 (HTS «오늘 매매결과» 와 같은 숫자 — reflex 실측)
//   - ka10076 체결 — 주문번호별 체결 수량·가격·당일 수수료·세금 (★ 오늘 것만 준다)
//   - kt00007 계좌별주문체결내역상세 — 주문번호별 체결 (지난 날짜도 된다 · 수수료 없음)
//   - kt00015 위탁종합거래내역 — 매매 거래내역 (거래별 수수료·세금·정산금액)
func ledgerQueries(d string, today bool) []LedgerSection {
	q := []LedgerSection{
		{API: "ka10170", Name: "당일매매일지", Request: map[string]string{
			"base_dt": d, "ottks_tp": "2", "ch_crd_tp": "0"}}, // ottks_tp 2 = 당일매도 전체
		{API: "ka10074", Name: "일자별실현손익", Request: map[string]string{
			"strt_dt": d, "end_dt": d}},
	}
	if today {
		q = append(q, LedgerSection{API: apiFills, Name: "체결", Request: map[string]string{
			"stk_cd": "", "qry_tp": "0", "sell_tp": "0", "ord_no": "", "stex_tp": "0"}})
	}
	return append(q,
		LedgerSection{API: "kt00007", Name: "계좌별주문체결내역상세", Request: map[string]string{
			"ord_dt": d, "qry_tp": "4", "stk_bond_tp": "1", "sell_tp": "0", "stk_cd": "", "fr_ord_no": "",
			"dmst_stex_tp": "%"}},
		LedgerSection{API: "kt00015", Name: "위탁종합거래내역", Request: map[string]string{
			"strt_dt": d, "end_dt": d, "tp": "3", "stk_cd": "", "crnc_cd": "", "gds_tp": "1",
			"frgn_stex_code": "", "dmst_stex_tp": "%"}},
	)
}

// Ledger — 한국 날짜 day 의 증권사 원장을 조회한다 (조회만 · 주문 없음).
//
// ★ 조회 하나가 실패해도 나머지는 계속한다 — 실패는 그 칸의 Error 로 남는다.
// 전부 실패했는지는 OK() 로 본다.
func (b *Broker) Ledger(ctx context.Context, day time.Time) Ledger {
	d := day.In(kst).Format("20060102")
	today := d == b.now().In(kst).Format("20060102")
	out := Ledger{Schema: ledgerSchema, Broker: b.Name(), Date: d}
	for _, s := range ledgerQueries(d, today) {
		s.Pages, s.Error = b.pages(ctx, s.API, s.Request)
		out.Sections = append(out.Sections, s)
	}
	out.FetchedAt = b.now().In(kst)
	return out
}

// pages — 연속조회를 끝까지 (ledgerMaxPages 까지) 따라간다. 받은 쪽은 실패해도 돌려준다.
func (b *Broker) pages(ctx context.Context, apiID string, body map[string]string) ([]json.RawMessage, string) {
	var out []json.RawMessage
	next := ""
	for i := 0; i < ledgerMaxPages; i++ {
		var raw json.RawMessage
		more, err := b.callPage(ctx, apiID, pathAcnt, body, next, &raw)
		if err != nil {
			return out, err.Error()
		}
		out = append(out, raw)
		if more == "" {
			return out, ""
		}
		next = more
	}
	return out, "연속조회 상한 도달 — 뒤쪽이 더 있을 수 있다"
}

// ── 원장 읽기 (대조용) ─────────────────────────────────────────────────────

// LedgerOrder — 증권사 쪽 주문번호 하나의 체결 합.
type LedgerOrder struct {
	OrderID string  `json:"ord_no"`
	Code    string  `json:"code"`
	Side    string  `json:"side"` // buy | sell
	Qty     float64 `json:"qty"`
	Price   float64 `json:"price"`   // 체결 평단
	FeeKRW  float64 `json:"fee_krw"` // 주문별 수수료+세금 (ka10076 만 — kt00007 에는 없다)
}

// section — 성공한 조회 하나.
func (l Ledger) section(api string) (LedgerSection, bool) {
	for _, s := range l.Sections {
		if s.API == api && s.Error == "" && len(s.Pages) > 0 {
			return s, true
		}
	}
	return LedgerSection{}, false
}

func (s LedgerSection) rows(marker string) []json.RawMessage {
	var out []json.RawMessage
	for _, p := range s.Pages {
		var raw map[string]json.RawMessage
		if json.Unmarshal(p, &raw) == nil {
			out = append(out, pickRows(raw, marker)...)
		}
	}
	return out
}

// Orders — 체결된 주문을 주문번호별로 합친다. 오늘이면 ka10076 (수수료 있음), 아니면 kt00007.
// 둘째 값 = 어느 조회에서 읽었나 ("" = 둘 다 실패) · 셋째 = 주문별 수수료가 실렸나.
func (l Ledger) Orders() ([]LedgerOrder, string, bool) {
	type row struct {
		OrdNo string `json:"ord_no"`
		StkCd string `json:"stk_cd"`
		IoTp  string `json:"io_tp_nm"`
		Qty   string `json:"cntr_qty"`
		Pric  string `json:"cntr_pric"` // ka10076
		Uv    string `json:"cntr_uv"`   // kt00007
		Cmsn  string `json:"tdy_trde_cmsn"`
		Tax   string `json:"tdy_trde_tax"`
	}
	from, withFee := apiFills, true
	s, ok := l.section(apiFills)
	if !ok {
		from, withFee = "kt00007", false
		if s, ok = l.section(from); !ok {
			return nil, "", false
		}
	}
	byOrd := map[string]*LedgerOrder{}
	var order []string
	for _, raw := range s.rows("cntr_qty") {
		var r row
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		q := math.Abs(num(r.Qty))
		if q <= 0 {
			continue
		}
		px := math.Abs(num(r.Pric))
		if !withFee {
			px = math.Abs(num(r.Uv))
		}
		o := byOrd[r.OrdNo]
		if o == nil {
			side := "buy"
			if strings.Contains(r.IoTp, "매도") {
				side = "sell"
			}
			o = &LedgerOrder{OrderID: r.OrdNo, Code: code(r.StkCd), Side: side}
			byOrd[r.OrdNo] = o
			order = append(order, r.OrdNo)
		}
		amt := o.Price*o.Qty + q*px
		o.Qty += q
		o.Price = amt / o.Qty
		o.FeeKRW += num(r.Cmsn) + num(r.Tax)
	}
	out := make([]LedgerOrder, 0, len(order))
	for _, id := range order {
		out = append(out, *byOrd[id])
	}
	return out, from, withFee
}

// DayTotals — 계좌 하루 합 (ka10074): 수수료+세금 = **실제 청구**, 실현손익.
//
// ★ 수수료는 하루 거래 합에 매긴다 — 주문별 수수료(ka10076)는 주문마다 10원 미만을 잘라
// 작은 주문은 0 으로 오고, 그 합은 실제 청구보다 작다 (10/20~23 실측 4,076 vs 4,586원).
func (l Ledger) DayTotals() (feeKRW, realizedKRW float64, ok bool) {
	s, ok := l.section("ka10074")
	if !ok {
		return 0, 0, false
	}
	var t struct {
		Cmsn string `json:"trde_cmsn"`
		Tax  string `json:"trde_tax"`
		PL   string `json:"rlzt_pl"`
	}
	if json.Unmarshal(s.Pages[0], &t) != nil {
		return 0, 0, false
	}
	return num(t.Cmsn) + num(t.Tax), num(t.PL), true
}
