package ids

import (
	"regexp"
	"sort"
	"testing"
	"time"
)

var ulidRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func TestFormatMatchesContract(t *testing.T) {
	// schema/v1/common.schema.json 의 Ulid 패턴과 같은 모양이어야 한다.
	for i := 0; i < 200; i++ {
		id := New()
		if !ulidRe.MatchString(id) {
			t.Fatalf("계약 패턴에 안 맞는다: %q", id)
		}
	}
}

func TestUnique(t *testing.T) {
	seen := map[string]bool{}
	at := time.Now()
	for i := 0; i < 10000; i++ {
		// 같은 밀리초에 몰아서 만들어도 겹치면 안 된다 — 멱등키가 겹치면
		// 서로 다른 주문이 같은 주문으로 취급된다.
		id := NewAt(at)
		if seen[id] {
			t.Fatalf("중복: %s", id)
		}
		seen[id] = true
	}
}

// ★ 사전순 정렬 = 시간순. 원장을 시간순으로 읽는 일이 압도적으로 많다.
func TestLexicographicOrderMatchesTime(t *testing.T) {
	base := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	var ids []string
	for i := 0; i < 50; i++ {
		ids = append(ids, NewAt(base.Add(time.Duration(i)*time.Second)))
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("%d번째에서 순서가 깨졌다: %s vs %s", i, ids[i], sorted[i])
		}
	}
}

func TestTimestampIsStable(t *testing.T) {
	at := time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
	a, b := NewAt(at), NewAt(at)
	// 앞 10자 = 48비트 타임스탬프. 같은 시각이면 같아야 한다.
	if a[:10] != b[:10] {
		t.Fatalf("같은 시각인데 타임스탬프가 다르다: %s vs %s", a[:10], b[:10])
	}
	later := NewAt(at.Add(time.Millisecond))
	if later[:10] <= a[:10] {
		t.Fatalf("1ms 뒤가 더 작거나 같다: %s vs %s", later[:10], a[:10])
	}
}
