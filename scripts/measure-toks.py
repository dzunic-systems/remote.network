import json, sys, time, urllib.request

label, url, key, model = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
body = json.dumps({
    "model": model, "stream": True, "max_tokens": 3000,
    "stream_options": {"include_usage": True},
    "messages": [{"role": "user", "content":
        "Write a very long, detailed story about a lighthouse keeper, at least 2500 words. Keep writing without stopping."}],
}).encode()
req = urllib.request.Request(url, data=body, headers={
    "Authorization": "Bearer " + key, "Content-Type": "application/json"})
t0 = time.time()
first = None
usage = None
frames = 0
with urllib.request.urlopen(req, timeout=420) as resp:
    for raw in resp:
        line = raw.decode("utf-8", "replace").strip()
        if not line.startswith("data: ") or line == "data: [DONE]":
            continue
        frames += 1
        now = time.time()
        if first is None:
            first = now
        try:
            ev = json.loads(line[6:])
        except ValueError:
            continue
        if ev.get("usage"):
            usage = ev["usage"]
total = time.time() - t0
decode = (time.time() if usage is None else 0)
comp = (usage or {}).get("completion_tokens")
if first is None or not comp:
    print(f"{label}: INCOMPLETE first={first} usage={usage} frames={frames} total={total:.1f}s")
    sys.exit(1)
dur = time.time() - first
print(f"{label}: completion_tokens={comp} ttft={first-t0:.2f}s decode_window={dur:.1f}s "
      f"decode={comp/dur:.1f} tok/s frames={frames} e2e_total={total:.1f}s")
