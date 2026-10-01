// Package fakekiwoom 은 **키움 REST 공식 스펙 기반 가짜 서버**다 — 체결까지 흉내 낸다.
//
// 목적: 실전에서 한 번 당하면 비싼 상황을 마음대로 만든다 — 분할체결, 체결 지연, 체결이 멈춤(잔량),
// 증거금 부족(855056), 주문 요청 5xx(★ 주문은 들어갔는데 응답만 실패), 토큰 만료(8005), 취소 경합.
//
// 스펙이 정하는 것: API 경로, 요청 필드(모르는 필드는 거절), 응답 필드(전부 채움).
// 우리가 정하는 것(= 스펙에 없어 추정): 체결 순서·오류 문구·숫자 표기(0 채움·부호). 표기는 키움 예시를 따른다.
//
// ★ 한계 — 키움 서버가 자기 스펙과 다르게 굴면 못 잡는다. 모의투자(mockapi.kiwoom.com)·라이브 몫이다.
package fakekiwoom

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Scenario — 시장가 주문이 어떻게 체결되는가.
type Scenario struct {
	// Fill — instant(즉시 전량) | split(Chunks 조각으로 IntervalMs 마다) | stall(StallFrac 만 체결되고 멈춤)
	Fill       string  `json:"fill"`
	Chunks     int     `json:"chunks,omitempty"`
	IntervalMs int     `json:"interval_ms,omitempty"`
	StallFrac  float64 `json:"stall_frac,omitempty"`
	// SlipTicks — 시장가 체결가를 현재가에서 몇 원 불리하게 (매수 +, 매도 −). 조각마다 누적.
	SlipWon float64 `json:"slip_won,omitempty"`
}

// Fault — 다음 Count 번의 해당 API 요청에 주입할 장애.
type Fault struct {
	API        string `json:"api"`
	HTTP       int    `json:"http,omitempty"`        // 0 이 아니면 이 HTTP 상태로 응답
	ReturnCode int    `json:"return_code,omitempty"` // 0 이 아니면 이 코드로 거부
	Msg        string `json:"msg,omitempty"`
	// Applied — ★ HTTP 장애여도 **요청은 처리한다** (주문이 들어갔는데 응답만 5xx — 가장 위험한 모양).
	Applied bool `json:"applied,omitempty"`
	Count   int  `json:"count"`
}

type Config struct {
	Spec   *Spec
	Cash   float64
	Prices map[string]float64
	// 수수료·세금 (편도 비율). 기본 = 수수료 0.015%, 매도세 0.18%.
	FeeRate, TaxRate float64
	Now              func() time.Time
}

type holding struct{ qty, avg float64 }

type chunk struct {
	at  time.Time
	qty float64
}

type order struct {
	no, code, side    string // side: buy | sell
	qty, limit        float64
	market            bool
	filled, amount    float64
	fee, tax          float64
	reserved          float64 // 매수 증거금 예약 (남은 몫)
	created, lastFill time.Time
	status            string // open | done | cancelled
	pending           []chunk
}

type Server struct {
	mu     sync.Mutex
	cfg    Config
	token  string
	cash   float64
	hold   map[string]*holding
	orders map[string]*order
	seq    int
	prices map[string]float64
	scen   Scenario
	faults []*Fault
	calls  map[string]int
}

func New(cfg Config) *Server {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.FeeRate == 0 {
		cfg.FeeRate = 0.00015
	}
	if cfg.TaxRate == 0 {
		cfg.TaxRate = 0.0018
	}
	s := &Server{
		cfg: cfg, cash: cfg.Cash, hold: map[string]*holding{}, orders: map[string]*order{},
		prices: map[string]float64{}, scen: Scenario{Fill: "instant"}, calls: map[string]int{},
	}
	for k, v := range cfg.Prices {
		s.prices[k] = v
	}
	return s
}

// ── HTTP ────────────────────────────────────────────────────────────────────

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_admin/") {
		s.admin(w, r)
		return
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var body map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			reply(w, 400, map[string]any{"return_code": 1, "return_msg": "[fake] JSON 아님"})
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.advanceLocked()

	if r.URL.Path == "/oauth2/token" {
		s.calls["au10001"]++
		s.issueTokenLocked(w, body)
		return
	}

	apiID := r.Header.Get("api-id")
	s.calls[apiID]++
	spec, ok := s.cfg.Spec.api(apiID)
	if !ok || spec.URL != r.URL.Path {
		reply(w, 404, map[string]any{"return_code": 1, "return_msg": fmt.Sprintf("[fake] %s 를 %s 로 부를 수 없다 (스펙 %q)", apiID, r.URL.Path, spec.URL)})
		return
	}
	if unk := spec.unknownFields(body); len(unk) > 0 {
		sort.Strings(unk)
		reply(w, 200, map[string]any{"return_code": 1, "return_msg": "[fake] 스펙에 없는 요청 필드: " + strings.Join(unk, ",")})
		return
	}
	if r.Header.Get("authorization") != "Bearer "+s.token || s.token == "" {
		reply(w, 200, map[string]any{"return_code": 3, "return_msg": "[8005:Token이 유효하지 않습니다]"})
		return
	}

	f := s.takeFaultLocked(apiID)
	if f != nil && f.HTTP == 0 && f.ReturnCode != 0 {
		reply(w, 200, map[string]any{"return_code": f.ReturnCode, "return_msg": f.Msg})
		return
	}
	if f != nil && f.HTTP != 0 && !f.Applied {
		reply(w, f.HTTP, map[string]any{"error": "fake fault"})
		return
	}

	values, lists, rc, msg := s.handleLocked(apiID, body)
	if f != nil && f.HTTP != 0 { // Applied — 처리는 했고 응답만 실패
		reply(w, f.HTTP, map[string]any{"error": "fake fault (applied)"})
		return
	}
	if rc != 0 {
		reply(w, 200, map[string]any{"return_code": rc, "return_msg": msg})
		return
	}
	out := spec.fill(values, lists)
	out["return_code"] = 0
	out["return_msg"] = "정상적으로 처리되었습니다"
	reply(w, 200, out)
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) issueTokenLocked(w http.ResponseWriter, body map[string]any) {
	if str(body, "appkey") == "" || str(body, "secretkey") == "" {
		reply(w, 200, map[string]any{"return_code": 1, "return_msg": "[fake] appkey/secretkey 없음"})
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	s.token = hex.EncodeToString(b)
	exp := s.cfg.Now().Add(24 * time.Hour).In(kst)
	reply(w, 200, map[string]any{
		"expires_dt": exp.Format("20060102150405"), "token_type": "bearer", "token": s.token,
		"return_code": 0, "return_msg": "정상적으로 처리되었습니다",
	})
}

func (s *Server) takeFaultLocked(api string) *Fault {
	for i, f := range s.faults {
		if f.API == api && f.Count > 0 {
			f.Count--
			if f.Count == 0 {
				s.faults = append(s.faults[:i], s.faults[i+1:]...)
			}
			return f
		}
	}
	return nil
}

// ── API 처리 ────────────────────────────────────────────────────────────────

func (s *Server) handleLocked(api string, b map[string]any) (map[string]any, map[string][]map[string]any, int, string) {
	switch api {
	case "kt10000", "kt10001":
		return s.placeLocked(api == "kt10000", b)
	case "kt10003":
		return s.cancelLocked(b)
	case "kt00005":
		return s.balanceLocked()
	case "kt00011":
		return s.buyableLocked(b)
	case "ka10076":
		return s.fillsLocked(b)
	case "ka10007":
		code := str(b, "stk_cd")
		p, ok := s.prices[code]
		if !ok {
			return nil, nil, 1, fmt.Sprintf("[fake] 모르는 종목 %s", code)
		}
		now := s.cfg.Now().In(kst)
		return map[string]any{"stk_cd": code, "cur_prc": signed(p),
			"date": now.Format("20060102"), "tm": now.Format("150405")}, nil, 0, ""
	}
	return nil, nil, 1, "[fake] 흉내 내지 않는 API: " + api
}

func (s *Server) placeLocked(buy bool, b map[string]any) (map[string]any, map[string][]map[string]any, int, string) {
	code := str(b, "stk_cd")
	qty := num(str(b, "ord_qty"))
	px, ok := s.prices[code]
	if !ok {
		return nil, nil, 1, fmt.Sprintf("[fake] 모르는 종목 %s", code)
	}
	if qty <= 0 || qty != math.Trunc(qty) {
		return nil, nil, 20, "[2000](800001:주문수량이 올바르지 않습니다)"
	}
	o := &order{code: code, qty: qty, created: s.cfg.Now(), status: "open"}
	switch str(b, "trde_tp") {
	case "3":
		o.market = true
	case "0":
		o.limit = num(str(b, "ord_uv"))
		if o.limit <= 0 {
			return nil, nil, 20, "[2000](800002:주문단가를 입력하세요)"
		}
	default:
		return nil, nil, 20, "[fake] 흉내 내지 않는 매매구분 " + str(b, "trde_tp")
	}

	if buy {
		o.side = "buy"
		ref := px
		if !o.market {
			ref = o.limit
		}
		need := qty * ref * (1 + s.cfg.FeeRate)
		if avail := s.cash - s.reservedLocked(); need > avail+1e-6 {
			can := math.Floor(math.Max(avail, 0) / (ref * (1 + s.cfg.FeeRate)))
			return nil, nil, 20, fmt.Sprintf("[2000](855056:매수증거금이 부족합니다. %.0f주 매수가능)", can)
		}
		o.reserved = need
	} else {
		o.side = "sell"
		h := s.hold[code]
		if h == nil || h.qty-s.lockedLocked(code) < qty-1e-9 {
			return nil, nil, 20, "[2000](800100:매도가능수량이 부족합니다)"
		}
	}

	s.seq++
	o.no = fmt.Sprintf("%07d", s.seq)
	s.orders[o.no] = o
	if o.market {
		s.scheduleLocked(o)
	}
	s.advanceLocked()
	return map[string]any{"ord_no": o.no, "dmst_stex_tp": "KRX"}, nil, 0, ""
}

func (s *Server) scheduleLocked(o *order) {
	now := s.cfg.Now()
	switch s.scen.Fill {
	case "split":
		n := s.scen.Chunks
		if n < 2 {
			n = 2
		}
		iv := time.Duration(s.scen.IntervalMs) * time.Millisecond
		base := math.Floor(o.qty / float64(n))
		left := o.qty
		for i := 0; i < n && left > 0; i++ {
			q := base
			if i == n-1 || q <= 0 {
				q = left
			}
			o.pending = append(o.pending, chunk{at: now.Add(time.Duration(i) * iv), qty: q})
			left -= q
		}
	case "stall":
		q := math.Floor(o.qty * s.scen.StallFrac)
		if q > 0 {
			o.pending = append(o.pending, chunk{at: now, qty: q})
		}
	default:
		o.pending = append(o.pending, chunk{at: now, qty: o.qty})
	}
}

func (s *Server) cancelLocked(b map[string]any) (map[string]any, map[string][]map[string]any, int, string) {
	o := s.orders[str(b, "orig_ord_no")]
	if o == nil || o.status != "open" {
		return nil, nil, 20, "[2000](800033:취소가능수량이 없습니다)"
	}
	left := o.qty - o.filled
	o.pending = nil
	o.status = "cancelled"
	if o.filled >= o.qty {
		o.status = "done"
	}
	o.reserved = 0
	s.seq++
	return map[string]any{"ord_no": fmt.Sprintf("%07d", s.seq), "base_orig_ord_no": o.no,
		"cncl_qty": pad(left)}, nil, 0, ""
}

func (s *Server) balanceLocked() (map[string]any, map[string][]map[string]any, int, string) {
	avail := s.cash - s.reservedLocked()
	var rows []map[string]any
	codes := make([]string, 0, len(s.hold))
	for c := range s.hold {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		h := s.hold[c]
		if h.qty <= 0 {
			continue
		}
		rows = append(rows, map[string]any{
			"stk_cd": "A" + c, "cur_qty": pad(h.qty), "buy_uv": pad(math.Round(h.avg)),
			"cur_prc": pad(s.prices[c]),
		})
	}
	return map[string]any{
		"entr": pad(s.cash), "ord_alowa": pad(avail), "100ord_alow_amt": pad(avail),
	}, map[string][]map[string]any{"stk_cntr_remn": rows}, 0, ""
}

func (s *Server) buyableLocked(b map[string]any) (map[string]any, map[string][]map[string]any, int, string) {
	px := num(str(b, "uv"))
	if px <= 0 {
		px = s.prices[str(b, "stk_cd")]
	}
	if px <= 0 {
		return nil, nil, 1, "[fake] 가격 없음"
	}
	avail := s.cash - s.reservedLocked()
	q := math.Floor(math.Max(avail, 0) / (px * (1 + s.cfg.FeeRate)))
	return map[string]any{"min_ord_alowq": pad(q), "ord_alowa": pad(avail), "entr": pad(s.cash)}, nil, 0, ""
}

// fillsLocked — ka10076. ★ 주문 하나 = 행 하나(누적 체결). 키움이 체결 조각마다 행을 주는지는 스펙에
// 적혀 있지 않다 — 모의투자에서 확인할 항목이다 (조각마다라면 드라이버의 합산이 맞고, 누적이라면
// 주문번호당 한 행이어야 합산이 맞다. 이 가짜는 후자다).
func (s *Server) fillsLocked(b map[string]any) (map[string]any, map[string][]map[string]any, int, string) {
	code, qry, sellTp := str(b, "stk_cd"), str(b, "qry_tp"), str(b, "sell_tp")
	var os []*order
	for _, o := range s.orders {
		if qry == "1" && o.code != code {
			continue
		}
		if (sellTp == "1" && o.side != "sell") || (sellTp == "2" && o.side != "buy") {
			continue
		}
		os = append(os, o)
	}
	sort.Slice(os, func(i, j int) bool { return os[i].no > os[j].no }) // 최근순
	rows := make([]map[string]any, 0, len(os))
	for _, o := range os {
		io, tp := "+매수", "시장가"
		if o.side == "sell" {
			io = "-매도"
		}
		if !o.market {
			tp = "보통"
		}
		avg := 0.0
		if o.filled > 0 {
			avg = math.Round(o.amount / o.filled)
		}
		oso := 0.0
		if o.status == "open" {
			oso = o.qty - o.filled
		}
		t := o.created
		if !o.lastFill.IsZero() {
			t = o.lastFill
		}
		stt := map[string]string{"open": "접수", "done": "체결", "cancelled": "취소"}[o.status]
		rows = append(rows, map[string]any{
			"ord_no": o.no, "stk_cd": o.code, "io_tp_nm": io, "ord_qty": fmt.Sprintf("%.0f", o.qty),
			"ord_pric": fmt.Sprintf("%.0f", o.limit), "cntr_qty": fmt.Sprintf("%.0f", o.filled),
			"cntr_pric": fmt.Sprintf("%.0f", avg), "oso_qty": fmt.Sprintf("%.0f", oso),
			"tdy_trde_cmsn": fmt.Sprintf("%.0f", o.fee), "tdy_trde_tax": fmt.Sprintf("%.0f", o.tax),
			"ord_stt": stt, "trde_tp": tp, "ord_tm": t.In(kst).Format("150405"), "stex_tp": "1", "stex_tp_txt": "KRX",
		})
	}
	return nil, map[string][]map[string]any{"cntr": rows}, 0, ""
}

// ── 체결 엔진 ───────────────────────────────────────────────────────────────

// advanceLocked — 때가 된 조각을 체결하고, 지정가는 가격이 닿으면 체결한다.
func (s *Server) advanceLocked() {
	now := s.cfg.Now()
	for _, o := range s.orders {
		if o.status != "open" {
			continue
		}
		if o.market {
			var keep []chunk
			for _, c := range o.pending {
				if !c.at.After(now) {
					px := s.prices[o.code]
					if o.side == "buy" {
						px += s.scen.SlipWon
					} else {
						px -= s.scen.SlipWon
					}
					s.fillLocked(o, c.qty, px, c.at)
				} else {
					keep = append(keep, c)
				}
			}
			o.pending = keep
		} else {
			px := s.prices[o.code]
			if (o.side == "sell" && px >= o.limit) || (o.side == "buy" && px <= o.limit) {
				s.fillLocked(o, o.qty-o.filled, o.limit, now)
			}
		}
		if o.filled >= o.qty-1e-9 {
			o.status, o.reserved = "done", 0
		}
	}
}

func (s *Server) fillLocked(o *order, q, px float64, at time.Time) {
	if q <= 0 {
		return
	}
	amt := q * px
	fee := math.Floor(amt * s.cfg.FeeRate)
	o.filled += q
	o.amount += amt
	o.fee += fee
	o.lastFill = at
	h := s.hold[o.code]
	if h == nil {
		h = &holding{}
		s.hold[o.code] = h
	}
	if o.side == "buy" {
		s.cash -= amt + fee
		h.avg = (h.avg*h.qty + amt) / (h.qty + q)
		h.qty += q
		o.reserved = math.Max(0, o.reserved-q*px*(1+s.cfg.FeeRate))
	} else {
		tax := math.Floor(amt * s.cfg.TaxRate)
		o.tax += tax
		s.cash += amt - fee - tax
		h.qty -= q
		if h.qty <= 1e-9 {
			delete(s.hold, o.code)
		}
	}
}

func (s *Server) reservedLocked() float64 {
	var r float64
	for _, o := range s.orders {
		if o.status == "open" && o.side == "buy" {
			r += o.reserved
		}
	}
	return r
}

func (s *Server) lockedLocked(code string) float64 {
	var q float64
	for _, o := range s.orders {
		if o.status == "open" && o.side == "sell" && o.code == code {
			q += o.qty - o.filled
		}
	}
	return q
}

// ── 관리 (테스트·시나리오 조종) ─────────────────────────────────────────────

func (s *Server) admin(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var in map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&in)
	switch r.URL.Path {
	case "/_admin/price":
		var v struct {
			Code  string  `json:"code"`
			Price float64 `json:"price"`
		}
		remarshal(in, &v)
		s.prices[v.Code] = v.Price
		s.advanceLocked() // 가격이 움직이면 걸려 있던 지정가가 체결될 수 있다
	case "/_admin/scenario":
		remarshal(in, &s.scen)
	case "/_admin/fault":
		var f Fault
		remarshal(in, &f)
		if f.Count <= 0 {
			f.Count = 1
		}
		s.faults = append(s.faults, &f)
	case "/_admin/cash":
		var v struct {
			Cash float64 `json:"cash"`
		}
		remarshal(in, &v)
		s.cash = v.Cash
	case "/_admin/match":
		s.matchLocked() // 동시호가 단일가 체결 — 열린 시장가를 전부 지금 가격에
	case "/_admin/token/rotate":
		s.token = "" // 다음 요청은 8005 → 드라이버가 재발급해야 한다
	case "/_admin/state":
	default:
		reply(w, 404, map[string]string{"error": "모르는 관리 경로"})
		return
	}
	s.advanceLocked()
	reply(w, 200, s.stateLocked())
}

// State — 검증용 요약.
type State struct {
	Cash     float64            `json:"cash"`
	Holdings map[string]float64 `json:"holdings"`
	Orders   []OrderView        `json:"orders"`
	Calls    map[string]int     `json:"calls"`
}

type OrderView struct {
	No     string  `json:"no"`
	Code   string  `json:"code"`
	Side   string  `json:"side"`
	Qty    float64 `json:"qty"`
	Filled float64 `json:"filled"`
	Status string  `json:"status"`
	Limit  float64 `json:"limit,omitempty"`
}

func (s *Server) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advanceLocked()
	return s.stateLocked()
}

func (s *Server) stateLocked() State {
	st := State{Cash: s.cash, Holdings: map[string]float64{}, Calls: map[string]int{}}
	for c, h := range s.hold {
		st.Holdings[c] = h.qty
	}
	for k, v := range s.calls {
		st.Calls[k] = v
	}
	for _, o := range s.orders {
		st.Orders = append(st.Orders, OrderView{No: o.no, Code: o.code, Side: o.side, Qty: o.qty,
			Filled: o.filled, Status: o.status, Limit: o.limit})
	}
	sort.Slice(st.Orders, func(i, j int) bool { return st.Orders[i].No < st.Orders[j].No })
	return st
}

// 코드에서 직접 조종 (테스트용).
func (s *Server) SetPrice(code string, p float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prices[code] = p
	s.advanceLocked()
}

func (s *Server) SetScenario(sc Scenario) { s.mu.Lock(); s.scen = sc; s.mu.Unlock() }

// Match — 열려 있는 시장가 주문의 잔량을 지금 가격에 전부 체결한다 (동시호가 단일가 체결·VI 해제 흉내).
func (s *Server) Match() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.matchLocked()
}

func (s *Server) matchLocked() {
	now := s.cfg.Now()
	for _, o := range s.orders {
		if o.status == "open" && o.market {
			s.fillLocked(o, o.qty-o.filled, s.prices[o.code], now)
			o.pending = nil
			o.status, o.reserved = "done", 0
		}
	}
}
func (s *Server) AddFault(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Count <= 0 {
		f.Count = 1
	}
	s.faults = append(s.faults, &f)
}
func (s *Server) RotateToken() { s.mu.Lock(); s.token = ""; s.mu.Unlock() }

// ── 표기 ────────────────────────────────────────────────────────────────────

var kst = time.FixedZone("KST", 9*3600)

// pad — 키움 예시의 12자리 0 채움 ("000000017534").
func pad(v float64) string { return fmt.Sprintf("%012.0f", math.Max(v, 0)) }

// signed — 현재가 표기 ("+72100" — 부호가 붙어 온다).
func signed(v float64) string { return fmt.Sprintf("+%.0f", v) }

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func num(s string) float64 {
	var v float64
	_, _ = fmt.Sscanf(strings.TrimLeft(strings.TrimSpace(s), "+"), "%g", &v)
	return v
}

func remarshal(in map[string]json.RawMessage, out any) {
	b, _ := json.Marshal(in)
	_ = json.Unmarshal(b, out)
}
