package config

// pinnedKeys — pitwall 서명 공개키. kid → base64(32바이트 Ed25519 공개키).
//
// ★ 이 맵이 이 저장소가 공개인 이유의 절반이다.
// "릴레이가 털려도 진입을 지어낼 수 없다" 는 주장은, 사용자가 **소스에서 이 값을 읽고**
// 릴리스 바이너리가 같은 소스에서 나왔음을 확인할 수 있을 때만 검증 가능한 주장이 된다
// (그래서 재현 가능 빌드 -trimpath / CGO_ENABLED=0 가 목표다).
//
// 값을 바꾸는 것은 **신뢰 근거를 바꾸는 것**이다. 코드 리뷰 없이 고치지 말 것.
// 키 회전은 유예기간 동안 구/신 kid 를 함께 두었다가 옛 kid 를 지우는 순서로 한다.
//
// 지금은 비어 있다 — pitwall 서명키가 아직 없다. 개발 중에는
// {data-dir}/trusted_keys.json 으로 키를 주입한다 (pin 된 kid 는 덮어쓸 수 없다).
var pinnedKeys = map[string]string{}
