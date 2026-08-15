// Package ids 는 ULID 를 만든다.
//
// ★ 왜 UUID 가 아니라 ULID 인가: 이 id 들은 전부 멱등키로 쓰이고(주문·업링크·ack),
// 동시에 원장의 정렬 키다. ULID 는 앞 48비트가 밀리초 타임스탬프라 **사전순 정렬 = 시간순**이 된다.
// 원장을 시간순으로 읽는 일이 압도적으로 많은데 UUIDv4 는 그걸 못 준다.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

// crockford — I, L, O, U 를 뺀 32자 (사람이 옮겨 적을 때 헷갈리는 글자들).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New 는 지금 시각으로 ULID 를 만든다.
func New() string { return NewAt(time.Now()) }

// NewAt 은 주어진 시각으로 만든다 (테스트에서 시간을 고정할 수 있도록).
func NewAt(t time.Time) string {
	var b [16]byte
	ms := uint64(t.UTC().UnixMilli())

	// 48비트 타임스탬프 (big-endian)
	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], ms)
	copy(b[0:6], tsBuf[2:8])

	// 80비트 난수. ★ 실패하면 조용히 0 을 쓰지 않는다 —
	// 멱등키가 겹치면 서로 다른 주문이 같은 주문으로 취급된다.
	if _, err := rand.Read(b[6:]); err != nil {
		panic("ids: 난수원 실패 — 멱등키를 만들 수 없다: " + err.Error())
	}
	return encode(b)
}

// encode 는 128비트를 Crockford base32 26자로 편다.
func encode(b [16]byte) string {
	out := make([]byte, 26)
	// 26 * 5 = 130 비트라 앞 2비트는 패딩이다. 최상위부터 5비트씩 끊는다.
	var carry uint16 = uint16(b[0]) >> 5 // 상위 3비트
	out[0] = crockford[carry]

	bitPos := 3 // 이미 3비트 소비
	for i := 1; i < 26; i++ {
		v := 0
		for k := 0; k < 5; k++ {
			byteIdx := bitPos / 8
			bitIdx := 7 - uint(bitPos%8)
			bit := 0
			if byteIdx < 16 {
				bit = int((b[byteIdx] >> bitIdx) & 1)
			}
			v = v<<1 | bit
			bitPos++
		}
		out[i] = crockford[v]
	}
	return string(out)
}
