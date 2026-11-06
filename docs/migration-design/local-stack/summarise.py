"""One line per captured reply, so a run is readable without opening the files."""
import json
import sys

name, code, path = sys.argv[1], sys.argv[2], sys.argv[3]

try:
    reply = json.load(open(path))
except Exception as exc:
    print(f"  {name:<14} HTTP {code}  unreadable reply: {exc}")
    sys.exit(0)

providers = (reply.get("message", {}).get("catalog", {}) or {}).get("providers", [])
if not providers:
    print(f"  {name:<14} HTTP {code}  no providers")
    sys.exit(0)

tags = providers[0].get("items", [{}])[0].get("tags", [])
days = [t.get("descriptor", {}).get("code") for t in tags]
first = {v["descriptor"]["code"]: v["value"] for v in (tags[0].get("list") or [])} if tags else {}
shown = ", ".join(f"{k}={first[k]}" for k in ("Date", "Rainfall", "Min Humidity", "Wind Direction") if k in first)
print(f"  {name:<14} HTTP {code}  days [{', '.join(filter(None, days))}]")
print(f"  {'':<14}           first day  {shown}")
