// brokerctl — 브로커 드라이버를 손으로 두드려 보는 도구.
//
// 데몬과 **같은 드라이버**(internal/broker/*)를 그대로 부른다. 드라이버를 붙이거나 고친 뒤
// "진짜 계좌에서 사고 팔리는가" 를 소액으로 확인하는 용도다.
//
//	brokerctl -broker upbit  cash
//	brokerctl -broker kiwoom positions
//	brokerctl -broker upbit  quote KRW-BTC
//	brokerctl -broker upbit  buy  KRW-BTC -krw 6000 -yes
//	brokerctl -broker kiwoom buy  005930  -qty 1 -yes
//	brokerctl -broker upbit  sell KRW-BTC -all -yes
//	brokerctl -broker kiwoom ledger 20261023 -out broker_ledger_20261023.json
//
// ★ ledger 는 조회만 하고 토큰을 **발급하지 않는다** — 도는 데몬의 토큰 파일을 빌려 쓴다
// (만료·무효면 멈춘다). 키움 응답을 그대로 담는다 (kiwoom.Ledger).
// ★ 이건 **원장 밖** 주문이다. 데몬이 보유 중인 종목을 여기서 팔면, 데몬은 다음 틱에
// 그 포지션을 "사후 감지 manual" 로 종결한다. 데몬이 쥔 종목은 건드리지 말 것.
// ★ 자격증명은 데몬과 같은 환경변수에서만 읽는다 (플래그로 받으면 ps 에 찍힌다).
// ★ 키움은 1계정 1토큰 — 반드시 데몬·flat6 와 같은 토큰 파일을 쓴다(-kiwoom-token-file
// 또는 COCKPIT_KIWOOM_TOKEN_FILE). 따로 발급하면 상대 세션이 통째로 죽는다.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/broker"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/kiwoom"
	"github.com/kh-ssong/yovel-cockpit/internal/broker/upbit"
	"github.com/kh-ssong/yovel-cockpit/internal/config"
	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "brokerctl:", err)
		os.Exit(1)
	}
}

type opts struct {
	broker    string
	dataDir   string
	tokenFile string
	mock      bool
	qty       float64
	krw       float64
	limit     float64
	all       bool
	yes       bool
	out       string
	noIssue   bool
}

func run(args []string) error {
	if _, err := config.LoadDotEnv(".env"); err != nil {
		return fmt.Errorf(".env: %w", err)
	}
	fs := flag.NewFlagSet("brokerctl", flag.ContinueOnError)
	var o opts
	fs.StringVar(&o.broker, "broker", "", "kiwoom | upbit")
	fs.StringVar(&o.dataDir, "data-dir", envOr("COCKPIT_DATA_DIR", "./data"), "키움 토큰 파일 기본 위치")
	fs.StringVar(&o.tokenFile, "kiwoom-token-file", os.Getenv("COCKPIT_KIWOOM_TOKEN_FILE"),
		"키움 토큰 파일 (★ 데몬·flat6 와 같은 파일)")
	fs.BoolVar(&o.mock, "kiwoom-mock", false, "키움 모의투자 도메인")
	fs.Float64Var(&o.qty, "qty", 0, "수량 (주식=주, 코인=개)")
	fs.Float64Var(&o.krw, "krw", 0, "금액(원) — 기준가로 수량을 계산한다")
	fs.Float64Var(&o.limit, "limit", 0, "지정가 (0 = 시장가)")
	fs.BoolVar(&o.all, "all", false, "sell: 보유 전량(매도 가능 수량)")
	fs.BoolVar(&o.yes, "yes", false, "★ 실주문을 낸다. 없으면 무엇을 낼지 보여주기만 한다")
	fs.StringVar(&o.out, "out", "", "ledger: 저장할 파일 (비면 화면)")

	// 플래그가 서브커맨드·종목 뒤에 와도 받는다 (`buy KRW-BTC -krw 6000 -yes`).
	var pos []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if len(pos) == 0 {
		return errors.New("명령이 없다: cash | positions | quote SYM | buy SYM | sell SYM | ledger [YYYYMMDD]")
	}
	o.noIssue = pos[0] == "ledger"

	// 업비트 시세는 공개 API — 키 없이도 quote 는 된다.
	if o.broker == "upbit" && pos[0] == "quote" {
		if ak, _ := config.UpbitCreds(); ak == "" {
			o.broker = "upbit-public"
		}
	}
	br, exch, err := build(o)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sym := func() (protocol.Symbol, error) {
		if len(pos) < 2 {
			return protocol.Symbol{}, fmt.Errorf("%s: 종목이 없다", pos[0])
		}
		return protocol.Symbol{Exchange: exch, Code: strings.ToUpper(pos[1])}, nil
	}

	switch pos[0] {
	case "cash":
		c, err := br.Cash(ctx)
		return show(c, err)
	case "positions":
		p, err := br.Positions(ctx)
		return show(p, err)
	case "quote":
		s, err := sym()
		if err != nil {
			return err
		}
		q, err := br.Quote(ctx, s)
		return show(q, err)
	case "buy", "sell":
		s, err := sym()
		if err != nil {
			return err
		}
		return order(ctx, br, pos[0], s, o)
	case "ledger":
		return ledger(ctx, br, pos[1:], o.out)
	}
	return fmt.Errorf("모르는 명령 %q", pos[0])
}

func build(o opts) (broker.Broker, string, error) {
	switch o.broker {
	case "upbit":
		ak, sk := config.UpbitCreds()
		if ak == "" || sk == "" {
			return nil, "", errors.New("COCKPIT_UPBIT_ACCESS_KEY / COCKPIT_UPBIT_SECRET_KEY 가 없다")
		}
		b, err := upbit.New(upbit.Config{AccessKey: ak, SecretKey: sk})
		return b, upbit.Exchange, err
	case "upbit-public":
		return upbit.NewPublic(upbit.Config{}), upbit.Exchange, nil
	case "kiwoom":
		ak, sk := config.KiwoomCreds()
		if ak == "" || sk == "" {
			return nil, "", errors.New("COCKPIT_KIWOOM_APPKEY / COCKPIT_KIWOOM_SECRET 가 없다")
		}
		if o.tokenFile == "" && !o.noIssue { // 발급 금지면 data-dir 의 데몬 토큰을 빌려 쓸 뿐이다
			fmt.Fprintln(os.Stderr, "★ 경고: 키움 토큰 파일 미지정 — data-dir 에 따로 발급한다. "+
				"flat6·데몬과 같은 앱키면 그쪽 세션이 죽는다 (-kiwoom-token-file)")
		}
		b, err := kiwoom.New(kiwoom.Config{
			AppKey: ak, SecretKey: sk, DataDir: o.dataDir, TokenFile: o.tokenFile, Mock: o.mock,
			NoTokenIssue: o.noIssue,
		})
		return b, "KRX", err
	}
	return nil, "", errors.New("-broker 는 kiwoom 또는 upbit")
}

func order(ctx context.Context, br broker.Broker, side string, s protocol.Symbol, o opts) error {
	q, err := br.Quote(ctx, s)
	if err != nil {
		return fmt.Errorf("기준가 조회: %w", err)
	}

	qty := o.qty
	switch {
	case side == "sell" && o.all:
		hs, err := br.Positions(ctx)
		if err != nil {
			return err
		}
		for _, h := range hs {
			if h.Symbol.Code == s.Code {
				qty = h.Sellable
			}
		}
		if qty <= 0 {
			return fmt.Errorf("%s 매도 가능 수량 0", s.Code)
		}
	case o.krw > 0:
		qty = o.krw / q.Price
		if lot := br.LotSize(s); lot >= 1 {
			qty = float64(int64(qty/lot)) * lot
		}
	}
	if qty <= 0 {
		return errors.New("수량이 0 — -qty 또는 -krw (sell 은 -all 도 가능)")
	}

	req := broker.OrderRequest{
		IntentID: "manual-" + time.Now().Format("150405"),
		Symbol:   s, Qty: qty, LimitPrice: o.limit, RefPrice: q.Price,
	}
	kind := "시장가"
	if o.limit > 0 {
		kind = fmt.Sprintf("지정가 %v", o.limit)
	}
	fmt.Printf("%s %s %s %v (기준가 %v · 약 %.0f원) via %s\n",
		br.Name(), side, s.Code, qty, q.Price, qty*q.Price, kind)
	if !o.yes {
		fmt.Println("-yes 가 없어 내지 않았다.")
		return nil
	}

	var fill broker.Fill
	if side == "buy" {
		fill, err = br.Buy(ctx, req)
	} else {
		fill, err = br.Sell(ctx, req)
	}
	if err != nil {
		// ★ 실패여도 주문번호가 있으면 보여준다 — 주문은 나갔을 수 있다.
		if fill.BrokerOrderID != "" {
			fmt.Println("주문번호:", fill.BrokerOrderID)
		}
		return err
	}
	return show(fill, nil)
}

// ledger — 그날 증권사 원장을 파일(또는 화면)로. 날짜를 안 주면 오늘(한국 날짜).
func ledger(ctx context.Context, br broker.Broker, args []string, out string) error {
	kb, ok := br.(*kiwoom.Broker)
	if !ok {
		return fmt.Errorf("ledger 는 키움만 (%s)", br.Name())
	}
	kst := time.FixedZone("KST", 9*3600)
	day := time.Now().In(kst)
	if len(args) > 0 {
		d, err := time.ParseInLocation("20060102", args[0], kst)
		if err != nil {
			return fmt.Errorf("날짜는 YYYYMMDD: %w", err)
		}
		day = d
	}
	l := kb.Ledger(ctx, day)
	for _, s := range l.Sections {
		state := fmt.Sprintf("%d쪽", len(s.Pages))
		if s.Error != "" {
			state += " · 오류 " + s.Error
		}
		fmt.Fprintf(os.Stderr, "%s %s — %s\n", s.API, s.Name, state)
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	if out == "" {
		fmt.Println(string(b))
	} else if err := writeAtomic(out, append(b, '\n')); err != nil {
		return err
	}
	if !l.OK() {
		return errors.New("조회가 전부 실패했다")
	}
	return nil
}

// writeAtomic — 읽는 쪽(mac-sync)이 반쯤 쓴 파일을 집어 가지 않게 임시 파일에 쓰고 바꿔 끼운다.
func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func show(v any, err error) error {
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
