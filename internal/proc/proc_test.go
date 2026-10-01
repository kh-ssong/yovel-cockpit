package proc

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLockBlocksLiveOwnerAndTakesOverDead(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "x.lock")
	beat := filepath.Join(dir, "beat.json")
	now := time.Now()

	// 살아 있는 주인 = 이 테스트 프로세스의 부모(쉘) pid 를 빌려 쓴다 — 확실히 살아 있다.
	owner := os.Getppid()
	os.WriteFile(lock, []byte(`{"pid":`+strconv.Itoa(owner)+`,"heartbeat":"`+filepath.ToSlash(beat)+`"}`), 0o644)
	if err := WriteBeat(beat, Beat{TS: now, PID: owner}); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(lock, LockInfo{PID: os.Getpid()}, now); !errors.Is(err, ErrLocked) {
		t.Fatalf("살아 있는 주인을 밀어냈다: %v", err)
	}

	// ★ 프로세스는 살아 있어도 하트비트가 늙었으면 멈춘 것 — 넘겨받는다.
	WriteBeat(beat, Beat{TS: now.Add(-10 * time.Minute), PID: owner})
	rel, err := Acquire(lock, LockInfo{PID: os.Getpid()}, now)
	if err != nil {
		t.Fatalf("죽은 잠금을 못 넘겨받았다: %v", err)
	}
	rel()
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("놓은 잠금이 남았다")
	}
}

func TestAccountKeyHidesSecret(t *testing.T) {
	k := AccountKey("kiwoom", "my-secret-appkey")
	if k == "" || len(k) != len("kiwoom-")+12 || strings.Contains(k, "secret") {
		t.Fatalf("%q", k)
	}
	if AccountKey("kiwoom", "a") == AccountKey("kiwoom", "b") {
		t.Fatal("다른 키가 같은 잠금")
	}
}

func TestAliveSelfAndDead(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("자기 자신이 죽었다고 본다")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$") // 바로 끝나는 자식
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if Alive(cmd.Process.Pid) {
		t.Fatal("★ 끝난 프로세스를 살아 있다고 본다 — 죽은 콕핏의 잠금이 재시작을 막는다")
	}
}
