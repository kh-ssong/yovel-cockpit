package kiwoom

import (
	"strconv"
	"strings"
)

// num 은 키움 응답의 숫자 문자열을 float 으로 바꾼다.
//
// ★ 키움은 숫자를 **문자열로** 준다. 그리고 그 문자열이 한 가지 모양이 아니다:
// 부호가 붙고("+000012345"), 콤마가 들어가고("1,234"), 앞에 0 이 깔리고, 공백이 섞인다.
// strconv.ParseFloat 하나로 받으면 조용히 0 이 되는데, **0 은 이 도메인에서 유효한 값**이라
// (수량 0 · 잔고 0) 파싱 실패와 구분되지 않는다. 그래서 여기서 한 번에 정규화한다.
func num(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	s = strings.ReplaceAll(s, ",", "")
	s = strings.TrimPrefix(s, "+")

	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

// numOK 는 값과 함께 "파싱이 됐는지" 를 준다. 0 과 실패를 구분해야 하는 곳에서 쓴다.
func numOK(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}
	t = strings.ReplaceAll(t, ",", "")
	t = strings.TrimPrefix(t, "+")
	v, err := strconv.ParseFloat(t, 64)
	return v, err == nil
}

// code 는 종목코드를 정규화한다.
//
// ★ 잔고 응답의 stk_cd 에는 "A" 프리픽스가 붙을 수 있다("A005930"). 붙은 채로 비교하면
// 우리가 산 종목을 못 찾아 "보유 없음" 으로 읽고, 그러면 방금 산 포지션을 유령으로 종결시킨다.
func code(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "A")
}
