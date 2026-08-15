// yovel v1 프로토콜 계약 검증기
//
// examples/valid/*   → 반드시 통과해야 한다
// examples/invalid/* → 반드시 거절되어야 한다  ★ 이쪽이 진짜 시험이다.
//   스키마를 느슨하게 고치면 valid 는 계속 통과하지만 invalid 가 통과해버린다 —
//   "서명 없는 진입" 이나 "명령 채널의 buy" 가 조용히 뚫리는 게 정확히 그 순간이다.

import { readFileSync, readdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import Ajv2020 from "ajv/dist/2020.js";
import addFormats from "ajv-formats";

const here = dirname(fileURLToPath(import.meta.url));
const schemaDir = join(here, "v1");
const exampleDir = join(here, "examples");

const ajv = new Ajv2020({ allErrors: true, strict: false });
addFormats(ajv);

for (const f of readdirSync(schemaDir).filter((f) => f.endsWith(".json"))) {
  ajv.addSchema(JSON.parse(readFileSync(join(schemaDir, f), "utf8")));
}

const validate = ajv.getSchema("urn:yovel:v1:message");
if (!validate) {
  console.error("urn:yovel:v1:message 를 찾을 수 없다");
  process.exit(1);
}

let failed = 0;
let checked = 0;

/** @param {"valid"|"invalid"} kind */
function run(kind) {
  const dir = join(exampleDir, kind);
  const shouldPass = kind === "valid";

  for (const f of readdirSync(dir).filter((f) => f.endsWith(".json"))) {
    const doc = JSON.parse(readFileSync(join(dir, f), "utf8"));
    const ok = validate(doc);
    checked++;

    if (ok === shouldPass) {
      console.log(`  ok    ${kind}/${f}`);
      continue;
    }

    failed++;
    if (shouldPass) {
      console.log(`  FAIL  ${kind}/${f} — 통과해야 하는데 거절됨`);
      for (const e of validate.errors ?? []) {
        console.log(`          ${e.instancePath || "/"} ${e.message}`);
      }
    } else {
      console.log(`  FAIL  ${kind}/${f} — 거절돼야 하는데 통과함`);
      if (doc._why) console.log(`          ${doc._why}`);
    }
  }
}

console.log("yovel v1 계약 검증");
run("valid");
run("invalid");

console.log(`\n${checked - failed}/${checked} 통과`);
process.exit(failed === 0 ? 0 : 1);
