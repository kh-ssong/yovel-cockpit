// fakekiwoom — 키움 REST 공식 스펙 기반 가짜 서버 (체결까지 흉내).
//
//	fakekiwoom -addr 127.0.0.1:7801 -spec ~/Downloads/kiwoom-rest-api-spec.json \
//	           -cash 10000000 -price 005930=286500 -price 000660=195000
//
//	cockpitd --broker kiwoom --kiwoom-api-url http://127.0.0.1:7801 ...
//
// 조종 (curl):
//
//	POST /_admin/price     {"code":"005930","price":293000}          가격 이동 (걸린 지정가가 체결될 수 있다)
//	POST /_admin/scenario  {"fill":"split","chunks":3,"interval_ms":500} | {"fill":"stall","stall_frac":0.5}
//	POST /_admin/fault     {"api":"kt10000","http":502,"applied":true,"count":1}   주문은 들어가고 응답만 502
//	POST /_admin/fault     {"api":"kt10000","return_code":20,"msg":"[2000](855056:... 6주 매수가능)"}
//	POST /_admin/token/rotate                                          다음 요청 8005
//	GET  /_admin/state                                                 현금·보유·주문·호출 수
//
// ★ 실키로 붙지 말 것 — 이 서버는 아무 appkey 나 받는다. 콕핏의 키움 토큰 파일도 **테스트용 경로**를 쓸 것
// (실제 토큰 파일을 가리키면 가짜 토큰이 덮어쓴다).
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kh-ssong/yovel-cockpit/internal/fakekiwoom"
)

type prices map[string]float64

func (p prices) String() string { return fmt.Sprint(map[string]float64(p)) }
func (p prices) Set(v string) error {
	code, px, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("code=price 형식")
	}
	f, err := strconv.ParseFloat(px, 64)
	if err != nil {
		return err
	}
	p[code] = f
	return nil
}

func main() {
	home, _ := os.UserHomeDir()
	addr := flag.String("addr", "127.0.0.1:7801", "듣는 주소 (★ 루프백만)")
	specPath := flag.String("spec", filepath.Join(home, "Downloads", "kiwoom-rest-api-spec.json"), "키움 REST 스펙 JSON")
	cash := flag.Float64("cash", 10_000_000, "시작 예수금")
	px := prices{}
	flag.Var(px, "price", "종목 가격 code=price (여러 번)")
	flag.Parse()

	spec, err := fakekiwoom.LoadSpec(*specPath)
	if err != nil {
		log.Fatalf("스펙: %v", err)
	}
	if len(px) == 0 {
		px["005930"] = 286_500
	}
	fk := fakekiwoom.New(fakekiwoom.Config{Spec: spec, Cash: *cash, Prices: px})

	// 요청이 없어도 분할체결 조각이 제때 체결되게 주기적으로 돌린다.
	go func() {
		for range time.Tick(50 * time.Millisecond) {
			fk.State()
		}
	}()
	log.Printf("fakekiwoom %s — 스펙 %s, 예수금 %.0f, 가격 %v", *addr, *specPath, *cash, px)
	log.Fatal(http.ListenAndServe(*addr, fk))
}
