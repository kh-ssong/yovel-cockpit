package kiwoom

import (
	"context"
	"strings"
	"sync"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

const (
	apiStockInfo  = "ka10100" // 종목정보 조회
	pathStockInfo = "/api/dostk/stkinfo"
)

// orderWarning — ka10100 투자유의종목여부 코드 → 이름 (스펙: 0 해당없음, 1 ETF투자주의요망, 2 정리매매,
// 3 단기과열, 4 투자위험, 5 투자경고).
var orderWarning = map[string]string{"1": "ETF투자주의", "2": "정리매매", "3": "단기과열", "4": "투자위험", "5": "투자경고"}

type statusCache struct {
	mu  sync.Mutex
	day string
	m   map[string]broker.SymbolStatus
}

// SymbolStatus — 감리구분(auditInfo)·투자유의(orderWarning). 하루 한 번 종목별로 조회해 캐시한다
// (지정은 장 전에 바뀌고 장중엔 잘 안 바뀐다 — 매 진입마다 묻지 않는다).
//
// ★ reflex 교훈 (2026-06-01, 309930): 투자경고 종목에 들어가 −21.3% (진입 슬리피지 +2.62%).
// 단기과열은 30분 단위 단일가 매매라 초 단위 신호가 성립하지 않는다.
func (b *Broker) SymbolStatus(ctx context.Context, s protocol.Symbol) (broker.SymbolStatus, error) {
	day := b.now().In(kst).Format("20060102")
	b.status.mu.Lock()
	if b.status.day != day {
		b.status.day, b.status.m = day, map[string]broker.SymbolStatus{}
	}
	if st, ok := b.status.m[s.Code]; ok {
		b.status.mu.Unlock()
		return st, nil
	}
	b.status.mu.Unlock()

	var out struct {
		AuditInfo    string `json:"auditInfo"`
		OrderWarning string `json:"orderWarning"`
	}
	if err := b.call(ctx, apiStockInfo, pathStockInfo, map[string]string{"stk_cd": s.Code}, &out); err != nil {
		return broker.SymbolStatus{}, err
	}
	var st broker.SymbolStatus
	if a := strings.TrimSpace(out.AuditInfo); a != "" && a != "정상" {
		st.Labels = append(st.Labels, a)
	}
	if w, ok := orderWarning[strings.TrimSpace(out.OrderWarning)]; ok {
		st.Labels = append(st.Labels, w)
	}

	b.status.mu.Lock()
	b.status.m[s.Code] = st
	b.status.mu.Unlock()
	return st, nil
}
