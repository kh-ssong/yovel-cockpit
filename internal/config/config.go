// Package config 는 데몬의 로컬 설정이다.
//
// ★ 여기 있는 값은 전부 "서버가 바꿀 수 없는 것"이다. 서버가 끌 수 있는 안전장치는
// 안전장치가 아니다 (protocol.md §6 가드레일).
package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

type Config struct {
	// Port — localhost API 포트. 인바운드 공개는 하지 않는다.
	Port int
	// DataDir — 토큰·상태·원장이 사는 곳.
	DataDir string
	// Mode — paper | live. ★ 기본값은 paper 다. 실주문은 사용자가 명시적으로 켠다.
	Mode protocol.Mode

	// TargetMaxAge — 목표 스냅샷이 이보다 늙으면 진입을 막는다 (청산·stop 은 계속 동작).
	TargetMaxAge time.Duration
	// ReconcileInterval — 목표 수신과 무관하게 도는 주기.
	ReconcileInterval time.Duration
	// HeartbeatInterval — 서버측 데드맨의 분해능을 정한다.
	HeartbeatInterval time.Duration
	// MaxOrdersPerTick — reconcile 한 번에 낼 수 있는 주문 수 상한 (폭주 차단).
	MaxOrdersPerTick int

	// Broker — paper | kiwoom. 기본 paper.
	Broker string
	// KiwoomMock — 모의투자 도메인.
	KiwoomMock bool
	// KiwoomTokenFile — 토큰 파일 경로.
	//
	// ★ flat6 와 **같은 앱키**를 쓴다면 반드시 flat6 의 파일을 가리켜야 한다
	// (`../yovel-flat6/data/kiwoom_token.json`). 각자 발급하면 1계정 1토큰이라
	// 서로의 토큰을 죽인다 — 증상은 "가끔 8005" 가 아니라 상대 세션 통째 유실이다.
	KiwoomTokenFile string
	// SlotCapitalDefault — 슬롯별 자본이 따로 없을 때 쓰는 값 (원).
	// ★ 서버는 비중만 보낸다. 얼마를 걸지는 사용자가 정한다.
	SlotCapitalDefault float64

	Policy protocol.Policy

	// modeFlag — Bind 와 Finish 사이의 임시 저장소 (플래그는 파싱 후에야 값이 찬다).
	modeFlag *string
}

func Default() Config {
	return Config{
		Port:              7737,
		DataDir:           defaultDataDir(),
		Mode:              protocol.ModePaper,
		TargetMaxAge:      180 * time.Second,
		ReconcileInterval: 5 * time.Second,
		HeartbeatInterval: 20 * time.Second,
		MaxOrdersPerTick:  5,
		Broker:            "paper",
		Policy:            protocol.DefaultPolicy(),
	}
}

func defaultDataDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "yovel-cockpit")
	}
	return ".cockpit"
}

// Bind 는 플래그를 건다. 환경변수 COCKPIT_* 가 기본값을 먼저 덮고, 플래그가 마지막에 이긴다.
func (c *Config) Bind(fs *flag.FlagSet) {
	c.applyEnv()

	fs.IntVar(&c.Port, "port", c.Port, "localhost API 포트")
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "상태·토큰 저장 경로")
	mode := string(c.Mode)
	fs.StringVar(&mode, "mode", mode, "paper | live")
	fs.DurationVar(&c.TargetMaxAge, "target-max-age", c.TargetMaxAge,
		"목표 스냅샷이 이보다 늙으면 진입 금지 (청산은 계속)")
	fs.BoolVar(&c.Policy.AcceptUnsignedDerisk, "accept-unsigned-derisk", c.Policy.AcceptUnsignedDerisk,
		"서명 없는 de-risk 를 수용할지 (★ 진입은 어느 쪽이든 서명 필수)")
	fs.DurationVar(&c.Policy.MaxSkew, "max-skew", c.Policy.MaxSkew, "허용 시계 오차")
	fs.StringVar(&c.Broker, "broker", c.Broker, "paper | kiwoom")
	fs.BoolVar(&c.KiwoomMock, "kiwoom-mock", c.KiwoomMock, "키움 모의투자 도메인 사용")
	fs.StringVar(&c.KiwoomTokenFile, "kiwoom-token-file", c.KiwoomTokenFile,
		"토큰 파일 경로 (★ flat6 와 같은 앱키면 flat6 의 파일을 가리킬 것)")
	fs.Float64Var(&c.SlotCapitalDefault, "slot-capital", c.SlotCapitalDefault, "슬롯당 자본 (원)")
	fs.DurationVar(&c.ReconcileInterval, "reconcile-interval", c.ReconcileInterval, "집행 루프 주기")

	// mode 는 파싱 후에 반영해야 해서 포인터를 잡아둔다.
	c.modeFlag = &mode
}

func (c *Config) applyEnv() {
	if v := os.Getenv("COCKPIT_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Port = n
		}
	}
	if v := os.Getenv("COCKPIT_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("COCKPIT_MODE"); v != "" {
		c.Mode = protocol.Mode(v)
	}
	if v := os.Getenv("COCKPIT_BROKER"); v != "" {
		c.Broker = v
	}
	if v := os.Getenv("COCKPIT_KIWOOM_TOKEN_FILE"); v != "" {
		c.KiwoomTokenFile = v
	}
}

// KiwoomCreds 는 키움 자격증명을 **환경변수에서만** 읽는다.
//
// ★ 플래그로 받지 않는 이유: 커맨드라인은 같은 PC 의 다른 프로세스에서 그대로 보인다
// (ps / 작업관리자). 증권사 앱키가 거기 찍히면 그 순간 유출이다.
func KiwoomCreds() (appKey, secret string) {
	return os.Getenv("COCKPIT_KIWOOM_APPKEY"), os.Getenv("COCKPIT_KIWOOM_SECRET")
}

// Finish 는 플래그 파싱 후 검증한다.
func (c *Config) Finish() error {
	if c.modeFlag != nil {
		c.Mode = protocol.Mode(*c.modeFlag)
	}
	switch c.Mode {
	case protocol.ModePaper, protocol.ModeLive:
	default:
		return fmt.Errorf("mode 는 paper 또는 live 여야 한다 (받은 값 %q)", c.Mode)
	}
	switch c.Broker {
	case "paper", "kiwoom":
	default:
		return fmt.Errorf("broker 는 paper 또는 kiwoom 이어야 한다 (받은 값 %q)", c.Broker)
	}
	// ★ live 모드인데 paper 브로커면 실주문이 안 나간다. 그 상태를 "돌고 있다" 로 보이게 두지 않는다.
	if c.Mode == protocol.ModeLive && c.Broker == "paper" {
		return fmt.Errorf("mode=live 인데 broker=paper 다 — 실주문이 나가지 않는다. broker=kiwoom 을 지정하거나 mode=paper 로 둘 것")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("포트가 범위 밖이다: %d", c.Port)
	}
	if c.DataDir == "" {
		return errors.New("data-dir 이 비었다")
	}
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("data-dir 생성 실패: %w", err)
	}
	keys, err := loadTrustedKeys(c.DataDir)
	if err != nil {
		return err
	}
	c.Policy.TrustedKeys = keys
	return nil
}

// TrustedKeysPath 는 개발용 신뢰키 파일.
//
// ★ 릴리스 빌드는 pitwall 공개키를 **바이너리에 pin** 한다 (그게 §1 "공개"의 근거다 —
// 사용자가 소스에서 키를 확인할 수 있어야 한다). 파일에서 읽는 이 경로는 개발·테스트용이고,
// pin 이 붙으면 파일 쪽은 pin 을 덮어쓰지 못하게 막아야 한다.
func TrustedKeysPath(dataDir string) string {
	return filepath.Join(dataDir, "trusted_keys.json")
}

func loadTrustedKeys(dataDir string) (map[string]ed25519.PublicKey, error) {
	out := map[string]ed25519.PublicKey{}
	for kid, b64 := range pinnedKeys {
		pub, err := decodeKey(b64)
		if err != nil {
			return nil, fmt.Errorf("pin 된 키 %s: %w", kid, err)
		}
		out[kid] = pub
	}

	raw, err := os.ReadFile(TrustedKeysPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("trusted_keys.json 파싱 실패: %w", err)
	}
	for kid, b64 := range m {
		if _, pinned := pinnedKeys[kid]; pinned {
			// pin 된 kid 를 파일이 덮어쓸 수 있으면 pin 이 pin 이 아니다.
			return nil, fmt.Errorf("kid %q 는 바이너리에 pin 되어 있어 파일로 덮어쓸 수 없다", kid)
		}
		pub, err := decodeKey(b64)
		if err != nil {
			return nil, fmt.Errorf("trusted_keys.json 의 %s: %w", kid, err)
		}
		out[kid] = pub
	}
	return out, nil
}

func decodeKey(b64 string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("Ed25519 공개키 크기가 아니다 (%d 바이트)", len(b))
	}
	return ed25519.PublicKey(b), nil
}
