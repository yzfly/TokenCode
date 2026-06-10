#!/bin/sh
# 从 models.dev 拉取最新 provider 目录并裁剪出 TokenCode 需要的字段，
# 写入 internal/catalog/modelsdev.json（go:embed 进二进制）。
# 用法：scripts/update-catalog.sh   （网络受限时 export https_proxy=...）
set -e
cd "$(dirname "$0")/.."
curl -sL --fail -m 60 https://models.dev/api.json -o /tmp/modelsdev-raw.json
python3 - <<'EOF'
import json

raw = json.load(open("/tmp/modelsdev-raw.json"))
out = {}
for pid, p in raw.items():
    models = {}
    for mid, m in (p.get("models") or {}).items():
        models[mid] = {
            "name": m.get("name") or mid,
            "context": (m.get("limit") or {}).get("context") or 0,
        }
    out[pid] = {
        "name": p.get("name") or pid,
        "env": p.get("env") or [],
        "npm": p.get("npm") or "",
        "api": p.get("api") or "",
        "doc": p.get("doc") or "",
        "models": models,
    }
json.dump(out, open("internal/catalog/modelsdev.json", "w"),
          ensure_ascii=False, separators=(",", ":"), sort_keys=True)
print("providers:", len(out))
EOF
wc -c internal/catalog/modelsdev.json
