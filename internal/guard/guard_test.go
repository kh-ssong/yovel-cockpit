package guard

import (
	"testing"
	"time"
)

var now = time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)

func at(d time.Duration) *time.Time { t := now.Add(d); return &t }

func TestStopBreach(t *testing.T) {
	d := Evaluate(Rules{StopPrice: 71_200}, Inputs{
		LastPrice: 71_100, PriceAsOf: now, Now: now, MaxPriceAge: time.Minute,
	})
	if !d.Exit || d.Reason != ReasonStop {
		t.Fatalf("%+v", d)
	}
}

func TestAboveStopHolds(t *testing.T) {
	d := Evaluate(Rules{StopPrice: 71_200}, Inputs{
		LastPrice: 71_300, PriceAsOf: now, Now: now, MaxPriceAge: time.Minute,
	})
	if d.Exit {
		t.Fatalf("%+v", d)
	}
}

func TestTimeExit(t *testing.T) {
	d := Evaluate(Rules{StopPrice: 1, TimeExitAt: at(-time.Second)}, Inputs{
		LastPrice: 100, PriceAsOf: now, Now: now, MaxPriceAge: time.Minute,
	})
	if !d.Exit || d.Reason != ReasonTime {
		t.Fatalf("%+v", d)
	}
}

// ★ 시세가 끊겼다고 자동 청산하지 않는다.
// 한 번의 피드 글리치로 전 포지션을 시장가로 터는 것은 그 자체가 사고고, 되돌릴 수 없다.
func TestStalePriceDoesNotAutoExit(t *testing.T) {
	d := Evaluate(Rules{StopPrice: 71_200}, Inputs{
		LastPrice:   71_100, // stop 아래지만 —
		PriceAsOf:   now.Add(-10 * time.Minute),
		Now:         now,
		MaxPriceAge: time.Minute,
	})
	if d.Exit {
		t.Fatal("늙은 시세로 청산했다")
	}
	if !d.Blind {
		t.Fatal("판단 불가를 보고하지 않았다 — 침묵하면 '안전함'으로 읽힌다")
	}
}

// ★ 반면 시간청산은 시세가 없어도 성립한다 (시계만 있으면 된다).
// 피드가 죽은 채로 장 마감을 맞는 상황에서 마지막까지 남는 안전장치다.
func TestTimeExitSurvivesBlindFeed(t *testing.T) {
	d := Evaluate(Rules{StopPrice: 71_200, TimeExitAt: at(-time.Second)}, Inputs{
		LastPrice:   0,
		PriceAsOf:   now.Add(-time.Hour),
		Now:         now,
		MaxPriceAge: time.Minute,
	})
	if !d.Exit || d.Reason != ReasonTime {
		t.Fatalf("피드가 죽었다고 시간청산까지 멈췄다: %+v", d)
	}
	if !d.Blind {
		t.Fatal("Blind 는 여전히 보고해야 한다")
	}
}

func TestTimeExitNotYet(t *testing.T) {
	d := Evaluate(Rules{StopPrice: 1, TimeExitAt: at(time.Hour)}, Inputs{
		LastPrice: 100, PriceAsOf: now, Now: now, MaxPriceAge: time.Minute,
	})
	if d.Exit {
		t.Fatalf("%+v", d)
	}
}

// stop 이 0 이면 "청산 조건 없음" 이지 "즉시 청산" 이 아니다.
func TestZeroStopIsNotAnImmediateExit(t *testing.T) {
	d := Evaluate(Rules{}, Inputs{LastPrice: 100, PriceAsOf: now, Now: now})
	if d.Exit {
		t.Fatalf("%+v", d)
	}
}

func TestNoMaxPriceAgeMeansNoBlindCheck(t *testing.T) {
	d := Evaluate(Rules{StopPrice: 200}, Inputs{
		LastPrice: 100, PriceAsOf: now.Add(-24 * time.Hour), Now: now, MaxPriceAge: 0,
	})
	if !d.Exit || d.Blind {
		t.Fatalf("%+v", d)
	}
}
