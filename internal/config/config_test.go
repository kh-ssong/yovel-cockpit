package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func parse(t *testing.T, args ...string) (Config, error) {
	t.Helper()
	c := Default()
	c.DataDir = t.TempDir()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	c.Bind(fs)
	// Bind 가 기본값을 잡은 뒤이므로 임시 디렉토리를 다시 지정해 준다.
	args = append([]string{"--data-dir", c.DataDir}, args...)
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	return c, c.Finish()
}

// ★ 기본값이 paper 여야 한다. 실주문은 사용자가 명시적으로 켜는 것이지,
// 설정을 빠뜨렸을 때 도달하는 상태가 아니다.
func TestDefaultModeIsPaper(t *testing.T) {
	c, err := parse(t)
	if err != nil {
		t.Fatal(err)
	}
	if c.Mode != "paper" {
		t.Fatalf("기본 mode=%q", c.Mode)
	}
}

func TestBadModeRejected(t *testing.T) {
	if _, err := parse(t, "--mode", "livee"); err == nil {
		t.Fatal("오타난 mode 가 통과했다")
	}
}

func writeKeys(t *testing.T, dir string, m map[string]string) {
	t.Helper()
	b, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dir, "trusted_keys.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func genKeyB64(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func TestTrustedKeysFromFile(t *testing.T) {
	c := Default()
	c.DataDir = t.TempDir()
	writeKeys(t, c.DataDir, map[string]string{"dev-1": genKeyB64(t)})
	if err := c.Finish(); err != nil {
		t.Fatal(err)
	}
	if len(c.Policy.TrustedKeys) != 1 {
		t.Fatalf("키 %d 개", len(c.Policy.TrustedKeys))
	}
}

// ★ pin 을 파일이 덮어쓸 수 있으면 pin 이 pin 이 아니다.
// 이 저장소가 공개인 근거("소스에서 신뢰 근거를 읽을 수 있다")가 바로 여기 걸려 있다.
func TestFileCannotOverridePinnedKid(t *testing.T) {
	const kid = "pw-pinned-test"
	pinnedKeys[kid] = genKeyB64(t)
	t.Cleanup(func() { delete(pinnedKeys, kid) })

	c := Default()
	c.DataDir = t.TempDir()
	writeKeys(t, c.DataDir, map[string]string{kid: genKeyB64(t)})

	err := c.Finish()
	if err == nil {
		t.Fatal("파일이 pin 된 kid 를 덮어썼다")
	}
	if !strings.Contains(err.Error(), kid) {
		t.Fatalf("어느 kid 가 문제인지 안 알려준다: %v", err)
	}
}

func TestMalformedKeyRejected(t *testing.T) {
	c := Default()
	c.DataDir = t.TempDir()
	// 길이가 Ed25519 공개키가 아닌 값 — 조용히 무시하면 안 되는 종류의 잘못이다.
	writeKeys(t, c.DataDir, map[string]string{"dev-1": base64.StdEncoding.EncodeToString([]byte("too short"))})
	if err := c.Finish(); err == nil {
		t.Fatal("잘못된 키가 통과했다")
	}
}

func TestMissingKeyFileIsNotAnError(t *testing.T) {
	// 개발 초기에는 키가 없는 게 정상이다 (데몬이 경고만 하고 뜬다).
	c := Default()
	c.DataDir = t.TempDir()
	if err := c.Finish(); err != nil {
		t.Fatalf("키 파일이 없다고 기동이 막혔다: %v", err)
	}
	if len(c.Policy.TrustedKeys) != 0 {
		t.Fatal("없는 키가 생겼다")
	}
}
