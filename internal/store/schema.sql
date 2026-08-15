-- yovel cockpit 로컬 저장소 스키마 v1
--
-- ★ 이 DB 는 포지션의 SSOT 가 아니다. 실상태의 SSOT 는 브로커다.
-- 여기 담는 것은 브로커가 알려주지 않는 것뿐이다:
--   "이 주식을 왜 샀는가(intent_id)" · "stop 이 얼마였는가" · "이미 끝난 목표인가" ·
--   "체결이 얼마나 밀렸는가".
-- 브로커 조회 결과와 어긋나면 브로커가 맞다.

CREATE TABLE IF NOT EXISTS schema_version (
  version INTEGER NOT NULL
);

-- intents — intent_id ↔ 브로커 포지션 매핑.
-- ★ 이 테이블이 없으면 재시작 직후 전 포지션이 유령이 된다. 계좌를 조회해도
--   "005930 14주" 만 나오지, 그게 어느 목표에서 나왔고 stop 이 얼마였는지는 우리만 안다.
CREATE TABLE IF NOT EXISTS intents (
  intent_id       TEXT PRIMARY KEY,
  slot            TEXT NOT NULL,
  exchange        TEXT NOT NULL,
  code            TEXT NOT NULL,
  side            TEXT NOT NULL,
  qty             REAL NOT NULL DEFAULT 0,
  avg_entry_price REAL NOT NULL DEFAULT 0,
  stop_armed      REAL NOT NULL DEFAULT 0,
  tp_price        REAL NOT NULL DEFAULT 0,
  tp_order_id     TEXT,
  time_exit_at    TEXT,
  entry_at        TEXT,
  -- ★ closed_at 이 차 있으면 그 intent_id 로는 다시 진입하지 않는다.
  --   retained 목표는 재접속마다 그대로 다시 오므로, 이 기록이 없으면
  --   stop 에 털린 자리에 같은 목표로 곧바로 재진입한다.
  closed_at       TEXT,
  close_reason    TEXT,
  updated_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS intents_open ON intents (closed_at) WHERE closed_at IS NULL;

-- orders — 매매기록 원장. 컬럼은 schema/v1/event.order.schema.json 과 1:1.
CREATE TABLE IF NOT EXISTS orders (
  id              TEXT PRIMARY KEY,          -- ULID. 멱등키 (같은 id 재기록은 무해)
  intent_id       TEXT NOT NULL,
  phase           TEXT NOT NULL,
  exchange        TEXT NOT NULL,
  code            TEXT NOT NULL,
  side            TEXT NOT NULL,
  qty             REAL,
  price           REAL,
  broker_order_id TEXT,
  -- ★ 지연 실측 3종. 이 프로젝트가 없애려는 지연의 크기가 여기서 나온다.
  signal_ts       TEXT,
  submitted_at    TEXT,
  filled_at       TEXT,
  slippage_bp     REAL,
  fee_krw         REAL,
  exit_reason     TEXT,
  realized_pct    REAL,
  broker_code     TEXT,
  detail          TEXT,
  -- ★ mode 를 NOT NULL + CHECK 로 강제한다. paper 와 live 를 합산해 보여주면 허위 표시다
  --   (실측: live 15건 −18,725원 vs paper 63건 +49,884원 → 합치면 "+3만 수익"인데 실계좌는 손실).
  mode            TEXT NOT NULL CHECK (mode IN ('paper', 'live')),
  -- ★ 사용자가 HTS 로 직접 판 것을 봇 청산으로 오인하면 형제 레그까지 잘못 청산된다.
  source          TEXT NOT NULL CHECK (source IN ('bot', 'manual')),
  daemon_sha      TEXT,
  created_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS orders_intent ON orders (intent_id);
CREATE INDEX IF NOT EXISTS orders_filled ON orders (mode, filled_at);

-- guards — de-risk 상태. ★ 재시작하면 풀리는 일시정지는 안전장치가 아니다.
CREATE TABLE IF NOT EXISTS guards (
  id                INTEGER PRIMARY KEY CHECK (id = 1),
  paused            INTEGER NOT NULL DEFAULT 0,
  block_entry_until TEXT,
  circuit_breaker   INTEGER NOT NULL DEFAULT 0,
  liquidate_all     INTEGER NOT NULL DEFAULT 0,
  reason            TEXT,
  updated_at        TEXT NOT NULL
);

-- outbox — 업링크 미전송 큐. 릴레이가 끊긴 동안 쌓였다가 재연결 때 올라간다.
-- ★ 원장의 원본은 이 PC 에 있고 서버 쪽은 사본이다. 서버가 못 받은 건 유실이 아니라 대기다.
CREATE TABLE IF NOT EXISTS outbox (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  id         TEXT NOT NULL UNIQUE,   -- 메시지 ULID. QoS1 은 at-least-once 라 멱등이 필수
  typ        TEXT NOT NULL,
  payload    BLOB NOT NULL,
  created_at TEXT NOT NULL,
  sent_at    TEXT
);

CREATE INDEX IF NOT EXISTS outbox_unsent ON outbox (seq) WHERE sent_at IS NULL;
