package protocol

import (
	"testing"
	"time"
)

// 집행 방식 검증 (2026-11-04) — 모르는 mode 는 E_SCHEMA. ★ 예전엔 조용히 시장가였다.
func TestEntryExecModes(t *testing.T) {
	p, priv := policyWith(t)
	notAfter := baseTS.Add(35 * time.Second)
	until := baseTS.Add(30 * time.Second)
	cases := []struct {
		name  string
		entry map[string]any
		ok    bool
	}{
		{"market", map[string]any{"mode": "market", "not_after": notAfter}, true},
		{"빈 mode = 옛 발행자", map[string]any{"not_after": notAfter}, true},
		{"limit", map[string]any{"mode": "limit", "limit_price": 71000, "not_after": notAfter}, true},
		{"limit 값 없음", map[string]any{"mode": "limit", "not_after": notAfter}, false},
		{"sweep", map[string]any{"mode": "sweep", "not_after": notAfter, "cap_price": 71000, "sweep_until": until}, true},
		{"sweep cap 없음", map[string]any{"mode": "sweep", "not_after": notAfter, "sweep_until": until}, false},
		{"sweep 창 없음", map[string]any{"mode": "sweep", "not_after": notAfter, "cap_price": 71000}, false},
		{"sweep 창이 not_after 뒤", map[string]any{"mode": "sweep", "not_after": until, "cap_price": 71000,
			"sweep_until": notAfter}, false},
		{"모르는 mode", map[string]any{"mode": "cap_sweep", "not_after": notAfter, "cap_price": 71000}, false},
	}
	for _, c := range cases {
		m := targetEnv(1, baseTS, baseTS.Add(time.Minute), notAfter)
		m["body"].(map[string]any)["targets"].([]any)[0].(map[string]any)["entry"] = c.entry
		adm := Admit(signed(t, m, priv), baseTS.Add(time.Second), p, NewGuard())
		if adm.Accept != c.ok {
			t.Errorf("%s: accept=%v codes=%v, 기대 %v", c.name, adm.Accept, adm.Codes, c.ok)
		}
		if !c.ok && (len(adm.Codes) != 1 || adm.Codes[0] != CodeSchema) {
			t.Errorf("%s: 코드 %v — E_SCHEMA 여야 한다", c.name, adm.Codes)
		}
	}
}

func TestExitExecValidation(t *testing.T) {
	p, priv := policyWith(t)
	until := baseTS.Add(30 * time.Second)
	cases := []struct {
		name string
		ex   map[string]any
		ok   bool
	}{
		{"sweep", map[string]any{"mode": "sweep", "floor_price": 71000, "sweep_until": until}, true},
		{"floor 없음", map[string]any{"mode": "sweep", "sweep_until": until}, false},
		{"창 없음", map[string]any{"mode": "sweep", "floor_price": 71000}, false},
		{"모르는 mode", map[string]any{"mode": "floor_sweep", "floor_price": 71000, "sweep_until": until}, false},
	}
	for _, c := range cases {
		m := targetEnv(1, baseTS, baseTS.Add(time.Minute), baseTS.Add(time.Minute))
		tg := m["body"].(map[string]any)["targets"].([]any)[0].(map[string]any)
		tg["want"] = "flat"
		tg["exit_exec"] = c.ex
		adm := Admit(signed(t, m, priv), baseTS.Add(time.Second), p, NewGuard())
		if adm.Accept != c.ok {
			t.Errorf("%s: accept=%v codes=%v, 기대 %v", c.name, adm.Accept, adm.Codes, c.ok)
		}
	}
}
