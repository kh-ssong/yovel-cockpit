package fakekiwoom

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Spec — 키움 REST 공식 스펙(JSON)에서 뽑은 API 별 필드 목록.
//
// ★ 가짜 서버가 **우리 가정**이 아니라 **키움이 적어 둔 모양**으로 말하게 하는 장치다:
//   - 요청: 스펙에 없는 필드가 오면 거절한다 (드라이버가 필드 이름을 틀리면 여기서 드러난다)
//   - 응답: 스펙의 응답 필드를 **전부** 채운다 (드라이버가 모르는 필드가 잔뜩 섞여도 파싱되는지)
//
// 한계: 키움 서버가 자기 스펙과 다르게 굴면 못 잡는다 — 그건 모의투자(mockapi)·라이브에서만 드러난다.
type Spec struct {
	apis map[string]apiSpec
}

type apiSpec struct {
	URL      string
	Request  map[string]bool // 본문 필드 (헤더 제외)
	Response []string        // 최상위 응답 필드 (순서 유지)
	// Lists — 목록 필드 이름 → 그 행의 필드들 (스펙에서 "- 필드" 로 표기된 것)
	Lists map[string][]string
}

var headerFields = map[string]bool{"api-id": true, "authorization": true, "cont-yn": true, "next-key": true}

// LoadSpec 은 키움 REST 스펙 JSON 을 읽는다.
func LoadSpec(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// ★ 스펙 파일 최상위엔 API 가 아닌 항목(배열 등)도 섞여 있다 — 객체로 안 풀리는 건 건너뛴다.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("스펙 파싱: %w", err)
	}
	type apiDoc struct {
		URL        string `json:"url"`
		RequestIo  []item `json:"requestIo"`
		ResponseIo []item `json:"responseIo"`
	}
	s := &Spec{apis: map[string]apiSpec{}}
	for id, rawAPI := range top {
		var d apiDoc
		if err := json.Unmarshal(rawAPI, &d); err != nil || d.URL == "" {
			continue
		}
		a := apiSpec{URL: d.URL, Request: map[string]bool{}, Lists: map[string][]string{}}
		for _, it := range d.RequestIo {
			if !headerFields[it.ItemID] {
				a.Request[it.ItemID] = true
			}
		}
		// 응답: "- x" 는 바로 앞 목록 필드의 행 필드다.
		var list string
		for _, it := range d.ResponseIo {
			id := it.ItemID
			if headerFields[id] {
				continue
			}
			if strings.HasPrefix(id, "-") {
				if list != "" {
					a.Lists[list] = append(a.Lists[list], strings.TrimSpace(strings.TrimLeft(id, "- ")))
				}
				continue
			}
			a.Response = append(a.Response, id)
			list = id // 다음 "- x" 들이 오면 이게 목록이다 (안 오면 그냥 스칼라)
		}
		s.apis[id] = a
	}
	return s, nil
}

type item struct {
	ItemID string `json:"itemId"`
}

func (s *Spec) api(id string) (apiSpec, bool) {
	a, ok := s.apis[id]
	return a, ok
}

// unknownFields — 요청 본문 중 스펙에 없는 필드.
func (a apiSpec) unknownFields(body map[string]any) []string {
	var out []string
	for k := range body {
		if !a.Request[k] {
			out = append(out, k)
		}
	}
	return out
}

// fill — 스펙의 응답 필드를 전부 채운 맵. 모르는 값은 빈 문자열(키움 예시와 같은 모양).
func (a apiSpec) fill(values map[string]any, lists map[string][]map[string]any) map[string]any {
	out := map[string]any{}
	for _, f := range a.Response {
		if rows, isList := a.Lists[f]; isList {
			_ = rows
			out[f] = []any{}
			continue
		}
		out[f] = ""
	}
	for k, v := range values {
		out[k] = v
	}
	for name, rows := range lists {
		cols := a.Lists[name]
		arr := make([]any, 0, len(rows))
		for _, r := range rows {
			row := map[string]any{}
			for _, c := range cols {
				row[c] = ""
			}
			for k, v := range r {
				row[k] = v
			}
			arr = append(arr, row)
		}
		out[name] = arr
	}
	return out
}
