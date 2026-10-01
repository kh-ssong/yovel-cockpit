package notify

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/store"
)

// Fill — 체결 한 건을 사람이 읽는 한 줄로.
//
//	🟢 매수 d205 · 005930 12주 @71,200 (슬립 +3.1bp)
//	🔴 청산 d205 · 005930 12주 @73,000 · flat · +2.53% (+21,600원)
func Fill(o store.Order, slot string) string {
	mode := strings.ToUpper(string(o.Mode))
	who := slot
	if who == "" {
		who = "-"
	}
	switch o.Phase {
	case "filled":
		s := fmt.Sprintf("🟢 [%s] 매수 %s · %s %s @%s", mode, who, o.Symbol.Code, qty(o.Qty), won(o.Price))
		if o.SlippageBp != 0 {
			s += fmt.Sprintf(" (슬립 %+.1fbp)", o.SlippageBp)
		}
		return s
	case "exit_filled":
		icon := "🔴"
		if o.RealizedPct > 0 {
			icon = "🔵"
		}
		s := fmt.Sprintf("%s [%s] 청산 %s · %s %s", icon, mode, who, o.Symbol.Code, qty(o.Qty))
		if o.Price > 0 {
			s += " @" + won(o.Price)
		}
		if o.ExitReason != "" {
			s += " · " + reasonKo(o.ExitReason)
		}
		if o.Price > 0 && o.RealizedPct != 0 {
			s += fmt.Sprintf(" · %+.2f%%", o.RealizedPct*100)
		}
		if o.Detail != "" {
			s += "\n  ↳ " + o.Detail
		}
		return s
	}
	if o.Phase == "rejected" {
		// ★ 매수 결과 미상 등 — 사람이 계좌를 봐야 하는 건이라 눈에 띄게.
		return fmt.Sprintf("🚨 [%s] 주문 확인 필요 %s · %s %s\n  ↳ %s", mode, who, o.Symbol.Code, qty(o.Qty), o.Detail)
	}
	return fmt.Sprintf("[%s] %s %s %s %s", mode, o.Phase, who, o.Symbol.Code, qty(o.Qty))
}

func reasonKo(r string) string {
	switch r {
	case "flat":
		return "판단자 청산"
	case "time":
		return "⏰ 시간청산"
	case "stop":
		return "🛑 로컬 stop"
	case "tp":
		return "🎯 TP"
	case "derisk":
		return "🚨 de-risk"
	case "reduce":
		return "✂️ 분할매도"
	case "manual":
		return "수동/사후감지"
	}
	return r
}

func won(p float64) string {
	if p >= 1000 && p == math.Trunc(p) {
		return commas(int64(p))
	}
	return fmt.Sprintf("%.4g", p)
}

func qty(q float64) string {
	if q == math.Trunc(q) {
		return commas(int64(q)) + "주"
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.8f", q), "0"), ".") + "개"
}

func commas(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// Throttle — 같은 종류의 경보를 일정 간격에 한 번만 통과시킨다.
// ★ 5초 틱마다 같은 오류를 보내면 폰이 울리다 무시당하고, 진짜 사고가 묻힌다.
type Throttle struct {
	mu    sync.Mutex
	every time.Duration
	last  map[string]time.Time
}

func NewThrottle(every time.Duration) *Throttle {
	return &Throttle{every: every, last: map[string]time.Time{}}
}

func (t *Throttle) Allow(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if l, ok := t.last[key]; ok && now.Sub(l) < t.every {
		return false
	}
	t.last[key] = now
	return true
}
