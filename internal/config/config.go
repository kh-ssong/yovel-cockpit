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
	// EngineBudget — 이 콕핏에 붙은 엔진에 배정한 예산 (원). 사이징의 분모다.
	//
	// ★ 슬롯당 자본이 아니다 (protocol.md §7.1). 옛 --slot-capital 은 모든 슬롯에 같은 값을
	// 줬는데, 그러면 엔진이 슬롯 3개를 쓰는 순간 노출이 3배가 된다 — 사용자가 정한 적 없는 크기다.
	// 슬롯 사이의 분배는 엔진이 target.weight 로 한다(전략 지식이라 사용자가 알 수 없다).
	//
	// ★ 두 번째 엔진이 붙으면 이 값은 kid 별 맵이 된다 (architecture.md §4 소스 1급화).
	// 지금 스칼라인 것은 소스가 하나뿐이기 때문이고, 그때까지는 이 한 값이 곧 그 엔진의 예산이다.
	EngineBudget float64

	// PaperFeeBpBuy / PaperFeeBpSell / PaperSlipBp — paper 브로커의 비용 모델 (편도 bp).
	//
	// ★★ 왜 플래그로 뺐나 — 옛 코드는 `main.go` 에 **대칭 15bp** 로 박혀 있었다. 그래서
	// 매수 체결에 국내엔 존재하지 않는 0.15% 가 붙었고(거래세는 매도에만 붙는다), 바꾸려면
	// **재빌드**가 필요했다. paper 원장의 손익은 사람이 읽는 숫자다 — 읽히는 숫자의 전제가
	// 코드에 잠겨 있으면 안 된다.
	//
	// ★ 기본값도 **추정치**다. 확정은 실제 체결 통지의 `fee`/`tax` 로만 된다.
	// 그래서 기동 로그에 **항상 찍는다** — 어떤 비용 전제로 채점됐는지 모른 채로 원장을
	// 읽는 상황을 만들지 않기 위해서다 (잘못 채워진 측정값은 빈 값보다 나쁘다).
	PaperFeeBpBuy  float64
	PaperFeeBpSell float64
	PaperSlipBp    float64

	// UI — 로컬 대시보드를 서빙할지. 기본 켜짐.
	// ★ 끌 수 있게 둔 이유는 헤드리스 상주다 (서버·CI). 화면이 없어야 하는 자리에서
	// 화면이 떠 있으면, 그 포트로 무엇이 열려 있는지 사용자가 매번 다시 확인해야 한다.
	UI bool

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
		// ★ 국내 주식 기준 **추정치**. 매수 = 위탁수수료만 / 매도 = 위탁수수료 + 증권거래세.
		//   옛 대칭 15bp 는 매수에 없는 비용(거래세)을 매수에도 물렸다.
		//
		// ★★ **모르면 낙관이 아니라 보수 쪽으로 둔다.** 매수를 0 으로 두고 싶어지는데
		//   (수수료 무료 이벤트 계좌가 흔하다), 그건 **그 계좌의 요율**이지 기본값이 아니다.
		//   기본값이 실제보다 싸면 손익분기 근처 전략이 통과해 버린다 — 되돌리기 제일 비싼 오류다.
		//   ⟹ 이벤트 없는 일반 계좌를 가정하고, 요율이 실제로 0 인 사용자는 내려서 쓴다.
		//
		// ★ 이 숫자들은 **확정이 아니다.** 진짜 요율은 체결 통지의 fee/tax 에서 관측되고
		//   (`store.ObservedCost`), 기동 로그가 설정과 관측을 나란히 찍어 어긋남을 드러낸다.
		PaperFeeBpBuy:  1.5,
		PaperFeeBpSell: 21.5,
		// ★ 슬리피지는 0 으로 두지 않는다 — 비용 0 시뮬은 손익분기 근처 전략의 판정을 뒤집는다.
		PaperSlipBp: 10,
		UI:          true,
		Policy:      protocol.DefaultPolicy(),
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
	fs.Float64Var(&c.EngineBudget, "engine-budget", c.EngineBudget,
		"이 엔진에 배정한 예산 (원) — ★ 슬롯당이 아니라 엔진 전체. 슬롯 분배는 weight 가 한다")
	fs.DurationVar(&c.ReconcileInterval, "reconcile-interval", c.ReconcileInterval, "집행 루프 주기")
	// ★ paper 비용은 사용자마다 다르다 (이벤트·등급 우대) — 상수로 박으면 남의 요율이 된다.
	//   라이브 체결이 생기면 기동 로그에 관측치가 찍히므로, 그 값으로 맞추는 게 정답이다.
	fs.Float64Var(&c.PaperFeeBpBuy, "paper-fee-bp-buy", c.PaperFeeBpBuy,
		"paper 매수 편도 비용 (bp) — ★ 추정치. 라이브 체결로 관측되면 그 값으로 맞출 것")
	fs.Float64Var(&c.PaperFeeBpSell, "paper-fee-bp-sell", c.PaperFeeBpSell,
		"paper 매도 편도 비용 (bp) — 국내는 여기에 증권거래세가 포함된다")
	fs.Float64Var(&c.PaperSlipBp, "paper-slip-bp", c.PaperSlipBp,
		"paper 시장가 슬리피지 (bp) — ★ 0 으로 두면 손익분기 근처 판정이 뒤집힌다")
	fs.BoolVar(&c.UI, "ui", c.UI, "로컬 대시보드 서빙 (--ui=false 로 끔)")
	fs.StringVar(&c.Policy.Acct, "acct", c.Policy.Acct,
		"이 콕핏의 계정 핸들 — 다른 acct 의 목표는 E_ACCT 로 거절 (★ 비우면 검사 안 함)")

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
	if v := os.Getenv("COCKPIT_ACCT"); v != "" {
		c.Policy.Acct = v
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
