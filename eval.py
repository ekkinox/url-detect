#!/usr/bin/env python3
"""Evaluate url-detect against a labeled dataset.

Reads a JSON file of [{"url": ..., "pattern": ...}], sends the URLs to the
running server, compares each returned pattern against the expected one, and
prints a per-URL result plus a recap of success/error and time spent.

Two modes (--mode):
  single  one HTTP request per URL, each timed individually (default)
  batch   all URLs in a single request ({"urls": [...]}), timed as a whole

Comparison is lenient about a leading slash, so expected patterns may be written
with or without it (the server emits one).
"""

import argparse
import json
import sys
import time
import urllib.request


def norm(p: str) -> str:
    p = (p or "").strip()
    return p[1:] if p.startswith("/") else p


def post(endpoint: str, payload: dict, timeout: float):
    body = json.dumps(payload).encode()
    req = urllib.request.Request(
        endpoint, data=body, headers={"Content-Type": "application/json"}
    )
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        data = json.load(resp)
    ms = (time.perf_counter() - t0) * 1000
    return data.get("results", []), ms


def eval_single(endpoint, cases, timeout):
    """Return list of (pattern, error, ms) per case, one request each."""
    out = []
    for c in cases:
        try:
            results, ms = post(endpoint, {"url": c["url"]}, timeout)
        except Exception as e:  # noqa: BLE001 - report any transport error
            out.append(("", f"request failed: {e}", None))
            continue
        r = results[0] if results else {}
        out.append((r.get("pattern", ""), r.get("error", "") or ("" if results else "empty results"), ms))
    return out


def eval_batch(endpoint, cases, timeout):
    """Return (list of (pattern, error, None), total_ms) for one request."""
    try:
        results, ms = post(endpoint, {"urls": [c["url"] for c in cases]}, timeout)
    except Exception as e:  # noqa: BLE001
        return [("", f"request failed: {e}", None) for _ in cases], None
    out = []
    for i in range(len(cases)):
        r = results[i] if i < len(results) else {}
        out.append((r.get("pattern", ""), r.get("error", "") or ("" if i < len(results) else "missing result"), None))
    return out, ms


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://localhost:8080/patterns")
    ap.add_argument("--file", default="eval.json")
    ap.add_argument("--mode", choices=["single", "batch"], default="single")
    ap.add_argument("--timeout", type=float, default=300.0)
    args = ap.parse_args()

    with open(args.file) as f:
        cases = json.load(f)

    batch_ms = None
    if args.mode == "batch":
        rows, batch_ms = eval_batch(args.url, cases, args.timeout)
    else:
        rows = eval_single(args.url, cases, args.timeout)

    print(f"mode: {args.mode}    cases: {len(cases)}    endpoint: {args.url}")
    print(f"{'URL':<48} {'STATUS':<7} {'TIME':>8}")
    print("-" * 78)

    passed = 0
    times = []
    for c, (got, err, ms) in zip(cases, rows):
        url, want = c["url"], c["pattern"]
        if ms is not None:
            times.append(ms)
        tcol = f"{ms:>6.0f}ms" if ms is not None else f"{'-':>8}"

        if err:
            print(f"{url:<48} {'ERROR':<7} {tcol}  {err}")
            continue

        ok = norm(got) == norm(want)
        line = f"{url:<48} {('PASS' if ok else 'FAIL'):<7} {tcol}"
        if not ok:
            line += f"  got: {got}  (want: {want})"
        print(line)
        if ok:
            passed += 1

    total = len(cases)
    print("-" * 78)
    print(f"passed: {passed}/{total}   failed: {total - passed}")
    if args.mode == "batch":
        if batch_ms is not None:
            print(
                f"time: 1 batch request {batch_ms/1000:.1f}s "
                f"({batch_ms/total:.0f}ms/url avg over {total} urls)"
            )
    elif times:
        print(
            f"time: total {sum(times)/1000:.1f}s   "
            f"avg {sum(times)/len(times):.0f}ms   "
            f"min {min(times):.0f}ms   max {max(times):.0f}ms"
        )
    # A report, not a gate: always exit 0 so `make eval` shows the recap cleanly.
    return 0


if __name__ == "__main__":
    sys.exit(main())
