#!/usr/bin/env python3
"""yovel v1 서명 적합성 — 파이썬 쪽 참조 구현 겸 검증기.

flat6(pitwall)가 서명하고 콕핏(Go)이 검증한다. Ed25519 는 결정적이라 두 구현이 갈릴 수 있는
곳은 서명 알고리즘이 아니라 **정규화(RFC 8785 JCS)** 뿐이다 — 키 정렬 · 공백 · 유니코드
이스케이프 · 숫자 표기.

★ 그 차이는 조용하다. 평범한 메시지에서는 우연히 일치하다가 한글이 섞이거나 소수점이
   등장하는 순간 "가끔 실패하는 인증" 이 된다. 그래서 이 스크립트는 저장소의 픽스처와
   **바이트 단위로** 대조한다.

    python schema/fixtures/signing/verify.py

flat6 는 이 파일의 canonicalize()/sign_envelope() 를 그대로 가져다 쓰면 된다.
의존성은 `cryptography` 하나.
"""

from __future__ import annotations

import base64
import hashlib
import json
import math
import sys
from pathlib import Path

from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey

# ── JCS (RFC 8785) ────────────────────────────────────────────────────────────
#
# 우리가 쓰는 부분집합만 구현한다. 전체 스펙이 필요해지면 라이브러리로 교체하되,
# 교체 후 반드시 이 픽스처를 다시 돌릴 것.

# ES6 JSON.stringify 가 이스케이프하는 것 = " \ 그리고 제어문자(<0x20) 뿐이다.
# ★ 한글·이모지는 이스케이프하지 않고 UTF-8 로 그대로 쓴다. \uXXXX 로 쓰면 Go 와 갈린다.
_ESCAPES = {'"': '\\"', "\\": "\\\\", "\b": "\\b", "\f": "\\f",
            "\n": "\\n", "\r": "\\r", "\t": "\\t"}


def _string(s: str) -> str:
    out = ['"']
    for ch in s:
        if ch in _ESCAPES:
            out.append(_ESCAPES[ch])
        elif ord(ch) < 0x20:
            out.append(f"\\u{ord(ch):04x}")
        else:
            out.append(ch)
    out.append('"')
    return "".join(out)


def _number(n) -> str:
    """ES6 Number::toString.

    ★ 파이썬과 갈리는 두 지점:
      1. 정수값 float — JS 는 71200, 파이썬 repr 은 71200.0
      2. 지수 표기 — JS 는 1e-7, 파이썬은 1e-07
    """
    if isinstance(n, bool):  # bool 은 int 의 하위형이라 먼저 걸러야 한다
        raise TypeError("bool 은 숫자가 아니다")
    if isinstance(n, int):
        return str(n)
    if math.isnan(n) or math.isinf(n):
        raise ValueError("JSON 에 NaN/Infinity 는 없다")
    if n == int(n) and abs(n) < 1e21:
        return str(int(n))
    r = repr(n)
    if "e" in r:  # 1e-07 → 1e-7
        mant, exp = r.split("e")
        sign = "+" if not exp.startswith("-") else "-"
        exp = exp.lstrip("+-").lstrip("0") or "0"
        r = f"{mant}e{sign}{exp}"
    return r


def _key_order(k: str):
    """★ JCS 는 **UTF-16 코드 단위** 순으로 정렬한다 (파이썬 기본은 코드포인트 순).

    ASCII 키만 쓰면 결과가 같지만, 이모지가 키에 들어가면 갈린다. 규칙대로 둔다.
    """
    return k.encode("utf-16-be")


def canonicalize(value) -> bytes:
    """JCS 정규화 바이트."""
    return _emit(value).encode("utf-8")


def _emit(v) -> str:
    if v is None:
        return "null"
    if isinstance(v, bool):
        return "true" if v else "false"
    if isinstance(v, (int, float)):
        return _number(v)
    if isinstance(v, str):
        return _string(v)
    if isinstance(v, list):
        return "[" + ",".join(_emit(x) for x in v) + "]"
    if isinstance(v, dict):
        items = sorted(v.items(), key=lambda kv: _key_order(kv[0]))
        return "{" + ",".join(_string(k) + ":" + _emit(x) for k, x in items) + "}"
    raise TypeError(f"JSON 타입이 아니다: {type(v)}")


# ── 서명 ──────────────────────────────────────────────────────────────────────

def signing_input(envelope: dict) -> bytes:
    """서명 대상 = sig 를 뺀 봉투의 JCS 바이트.

    ★ 부분 필드만 서명하면 서명 안 된 필드로 의미를 바꾸는 공격이 열린다.
    ★ 모르는 필드를 지우면 안 된다 — 서버가 필드를 늘리는 순간 옛 클라의 검증이 깨진다.
    """
    body = {k: v for k, v in envelope.items() if k != "sig"}
    return canonicalize(body)


def sign_envelope(envelope: dict, kid: str, priv: Ed25519PrivateKey) -> dict:
    sig = priv.sign(signing_input(envelope))
    out = dict(envelope)
    out["sig"] = {"alg": "ed25519", "kid": kid,
                  "val": base64.b64encode(sig).decode()}
    return out


def fixture_key(seed_source: str) -> Ed25519PrivateKey:
    """★ 키를 파일에 저장하지 않는다. 양쪽이 같은 문자열에서 결정적으로 유도한다."""
    seed = hashlib.sha256(seed_source.encode()).digest()
    return Ed25519PrivateKey.from_private_bytes(seed)


# ── 검증 ──────────────────────────────────────────────────────────────────────

def main() -> int:
    path = Path(__file__).with_name("cases.json")
    # ★ encoding 을 반드시 명시한다. 윈도우 파이썬 기본은 cp949 라 한글 케이스에서 터진다.
    fixture = json.loads(path.read_text(encoding="utf-8"))

    priv = fixture_key(fixture["seed_source"])
    pub = priv.public_key()
    pub_b64 = base64.b64encode(
        pub.public_bytes_raw() if hasattr(pub, "public_bytes_raw") else _raw(pub)
    ).decode()

    failed = 0
    if pub_b64 != fixture["public_key_b64"]:
        print(f"  FAIL 공개키 유도 불일치\n       got  {pub_b64}\n       want {fixture['public_key_b64']}")
        failed += 1

    for c in fixture["cases"]:
        name = c["name"]
        got = signing_input(c["message"])
        want = c["signing_input"].encode("utf-8")

        if got != want:
            failed += 1
            print(f"  FAIL {name} — 정규화 바이트가 다르다")
            print(f"       got  {got.decode('utf-8', 'replace')[:160]}")
            print(f"       want {want.decode('utf-8', 'replace')[:160]}")
            # 첫 차이 위치를 짚어준다 (어디서 갈렸는지가 곧 원인이다)
            for i, (a, b) in enumerate(zip(got, want)):
                if a != b:
                    print(f"       첫 차이 offset {i}: {got[max(0,i-30):i+30]!r}")
                    break
            continue

        digest = base64.b64encode(hashlib.sha256(got).digest()).decode()
        if digest != c["signing_input_sha256"]:
            failed += 1
            print(f"  FAIL {name} — 해시 불일치")
            continue

        sig = base64.b64decode(c["signature"])
        try:
            pub.verify(sig, got)
        except Exception as e:  # noqa: BLE001
            failed += 1
            print(f"  FAIL {name} — 서명 검증 실패: {e}")
            continue

        # 우리가 서명해도 같은 값이 나와야 한다 (Ed25519 는 결정적).
        mine = base64.b64encode(priv.sign(got)).decode()
        if mine != c["signature"]:
            failed += 1
            print(f"  FAIL {name} — 우리가 만든 서명이 다르다")
            continue

        print(f"  ok   {name}")

    total = len(fixture["cases"]) + 1
    print(f"\n{total - failed}/{total} 통과")
    return 1 if failed else 0


def _raw(pub) -> bytes:
    from cryptography.hazmat.primitives import serialization
    return pub.public_bytes(serialization.Encoding.Raw,
                            serialization.PublicFormat.Raw)


if __name__ == "__main__":
    print("yovel v1 서명 적합성 (Python ↔ Go)")
    sys.exit(main())
