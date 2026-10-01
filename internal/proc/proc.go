// Package proc 는 프로세스 안전장치다 — 단일 실행 잠금과 하트비트.
//
// ★ reflex 교훈 (2026-07-16): 감시자 2개 × 봇 2벌이 **같은 계좌·같은 DB** 에 붙었다. 주문을 낼 수 있는
// 프로세스가 둘이면 같은 신호로 두 번 산다. 잠금은 두 겹이다:
//   - data-dir 하나에 콕핏 하나 (원장 DB 를 둘이 쓰지 않게)
//   - **브로커 키 하나에 콕핏 하나** — data-dir 이 달라도 같은 계좌면 막는다 (키는 해시로만 남긴다)
//
// 잠금이 살아 있다 = 그 pid 의 프로세스가 있고 **그 하트비트가 신선하다.** pid 만 보면 재사용된 pid 에 막히고,
// 하트비트만 보면 막 뜬 프로세스를 놓친다.
package proc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Beat — 하트비트. 감시자가 읽어 "살아 있는데 멈췄나" 를 본다.
type Beat struct {
	TS            time.Time `json:"ts"`
	PID           int       `json:"pid"`
	Version       string    `json:"version,omitempty"`
	SHA           string    `json:"sha,omitempty"`
	Mode          string    `json:"mode,omitempty"`
	Broker        string    `json:"broker,omitempty"`
	OpenLots      int       `json:"open_lots"`
	WorkingOrders int       `json:"working_orders"`
	// LastTick — 집행 루프가 마지막으로 돈 시각. ★ 프로세스가 살아 있어도 이게 멈추면 멈춘 것이다.
	LastTick time.Time `json:"last_tick"`
}

// WriteBeat — 원자적으로 쓴다 (임시 파일 → 이름 바꾸기). 반쯤 쓴 파일을 감시자가 읽지 않게.
func WriteBeat(path string, b Beat) error {
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ReadBeat(path string) (Beat, error) {
	var b Beat
	raw, err := os.ReadFile(path)
	if err != nil {
		return b, err
	}
	return b, json.Unmarshal(raw, &b)
}

// BeatFresh — 하트비트가 maxAge 안에 쓰였는가.
func BeatFresh(path string, now time.Time, maxAge time.Duration) bool {
	b, err := ReadBeat(path)
	return err == nil && now.Sub(b.TS) < maxAge
}

// LockInfo — 잠금 파일 내용.
type LockInfo struct {
	PID       int       `json:"pid"`
	Started   time.Time `json:"started"`
	DataDir   string    `json:"data_dir"`
	Heartbeat string    `json:"heartbeat"` // 이 인스턴스의 하트비트 파일
}

// ErrLocked — 다른 콕핏이 이미 돌고 있다.
var ErrLocked = errors.New("다른 콕핏이 이미 돌고 있다")

// staleAfter — 하트비트가 이보다 늙은 잠금은 죽은 것으로 본다.
const staleAfter = 3 * time.Minute

// Acquire 는 잠금을 잡는다. 살아 있는 주인이 있으면 ErrLocked. 죽은 주인의 잠금은 넘겨받는다.
func Acquire(path string, info LockInfo, now time.Time) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			raw, _ := json.Marshal(info)
			_, werr := f.Write(raw)
			f.Close()
			if werr != nil {
				os.Remove(path)
				return nil, werr
			}
			return func() {
				// 내 잠금일 때만 지운다 (그새 다른 인스턴스가 넘겨받았으면 건드리지 않는다).
				if cur, err := readLock(path); err == nil && cur.PID == info.PID {
					os.Remove(path)
				}
			}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		cur, rerr := readLock(path)
		if rerr == nil && cur.PID != os.Getpid() && Alive(cur.PID) && BeatFresh(cur.Heartbeat, now, staleAfter) {
			return nil, fmt.Errorf("%w (pid %d, data-dir %s, %s 부터)", ErrLocked, cur.PID, cur.DataDir,
				cur.Started.Format(time.RFC3339))
		}
		// 죽었거나 멈춘 주인 — 넘겨받는다.
		os.Remove(path)
	}
	return nil, fmt.Errorf("잠금 %s 를 잡지 못했다", path)
}

func readLock(path string) (LockInfo, error) {
	var l LockInfo
	raw, err := os.ReadFile(path)
	if err != nil {
		return l, err
	}
	return l, json.Unmarshal(raw, &l)
}

// Alive — 그 pid 의 프로세스가 **살아서 돌고 있는가** (OS 별 구현: alive_*.go).
func Alive(pid int) bool { return pid > 0 && alive(pid) }

// AccountKey — 브로커 키로 만든 계좌 잠금 이름. ★ 키 자체는 남기지 않는다 (해시 앞 12자리).
func AccountKey(broker, key string) string {
	h := sha256.Sum256([]byte(key))
	return broker + "-" + hex.EncodeToString(h[:])[:12]
}
