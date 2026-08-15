// devsign — 개발용 서명 도구.
//
// ★ 이건 pitwall 이 아니다. 릴레이가 붙기 전까지 루프백(/v1/downlink)으로 계약을 굴려보기 위한
// 개발 편의 도구이고, 여기서 만든 키는 개발용 kid 로만 쓴다.
// 운영 서명키는 pitwall 이 들고 있고, 그 공개키는 데몬 바이너리에 pin 된다
// (internal/config/pinned_keys.go — 파일로 덮어쓸 수 없다).
//
//	devsign keygen --kid dev-1 --out dev-key.json
//	devsign sign --key dev-key.json < target.json > signed.json
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kh-ssong/yovel-cockpit/internal/protocol"
)

type devKey struct {
	Kid     string `json:"kid"`
	Public  string `json:"public"`
	Private string `json:"private"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: devsign keygen|sign")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "keygen":
		err = keygen(os.Args[2:])
	case "sign":
		err = sign(os.Args[2:])
	default:
		err = fmt.Errorf("모르는 명령 %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "devsign:", err)
		os.Exit(1)
	}
}

func keygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	kid := fs.String("kid", "dev-1", "키 식별자")
	out := fs.String("out", "dev-key.json", "개인키를 쓸 파일")
	if err := fs.Parse(args); err != nil {
		return err
	}

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	k := devKey{
		Kid:     *kid,
		Public:  base64.StdEncoding.EncodeToString(pub),
		Private: base64.StdEncoding.EncodeToString(priv),
	}
	b, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, b, 0o600); err != nil {
		return err
	}

	// 데몬이 읽는 형식 그대로 뱉어준다 — 손으로 옮겨 적다 틀리는 일이 없도록.
	trusted, _ := json.MarshalIndent(map[string]string{k.Kid: k.Public}, "", "  ")
	fmt.Printf("개인키 → %s\n{data-dir}/trusted_keys.json 에 넣을 내용:\n%s\n", *out, trusted)
	return nil
}

func sign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	keyPath := fs.String("key", "dev-key.json", "keygen 이 만든 키 파일")
	if err := fs.Parse(args); err != nil {
		return err
	}

	kb, err := os.ReadFile(*keyPath)
	if err != nil {
		return err
	}
	var k devKey
	if err := json.Unmarshal(kb, &k); err != nil {
		return err
	}
	privBytes, err := base64.StdEncoding.DecodeString(k.Private)
	if err != nil {
		return err
	}
	if len(privBytes) != ed25519.PrivateKeySize {
		return fmt.Errorf("개인키 크기가 %d 바이트다", len(privBytes))
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	signed, err := protocol.Sign(raw, k.Kid, ed25519.PrivateKey(privBytes))
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(signed, '\n'))
	return err
}
