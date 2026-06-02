#!/usr/bin/env python3
"""Evaluate url-detect against a labeled dataset.

Reads a JSON file of [{"url": ..., "pattern": ...}], sends the URLs to the
running server, compares each returned pattern against the expected one, and
prints a colored, aligned result table plus a recap of success/error and time.

Two modes (--mode):
  single  one HTTP request per URL, each timed individually (default)
  batch   all URLs in a single request ({"urls": [...]}), timed as a whole

Comparison is lenient about a leading slash, so expected patterns may be written
with or without it (the server emits one).
"""

import argparse
import json
import os
import sys
import time
import urllib.request

# ANSI colors (enabled only when writing to a TTY and NO_COLOR is unset).
_USE_COLOR = sys.stdout.isatty() and os.environ.get("NO_COLOR") is None
_CODES = {"green": "32", "red": "31", "yellow": "33", "dim": "2", "bold": "1"}


def color(s: str, name: str) -> str:
    if not _USE_COLOR:
        return s
    return f"\033[{_CODES[name]}m{s}\033[0m"


def pad(s: str, width: int) -> str:
    """Left-justify to a display width (ignores ANSI codes already in s)."""
    return s + " " * max(0, width - len(s))


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
    ap.add_argument("--no-color", action="store_true", help="disable ANSI colors")
    args = ap.parse_args()

    if args.no_color:
        global _USE_COLOR
        _USE_COLOR = False

    with open(args.file) as f:
        cases = json.load(f)

    batch_ms = None
    if args.mode == "batch":
        rows, batch_ms = eval_batch(args.url, cases, args.timeout)
    else:
        rows = eval_single(args.url, cases, args.timeout)

    url_w = max([len("URL")] + [len(c["url"]) for c in cases])

    print(f"mode: {args.mode}    cases: {len(cases)}    endpoint: {args.url}\n")
    header = f"{pad('RESULT', 6)}  {pad('TIME', 8)}  {pad('URL', url_w)}  PATTERN / DETAILS"
    print(color(header, "bold"))
    print(color("─" * len(header), "dim"))

    passed = 0
    times = []
    for c, (got, err, ms) in zip(cases, rows):
        url, want = c["url"], c["pattern"]
        if ms is not None:
            times.append(ms)
        tcol = f"{ms:>5.0f} ms" if ms is not None else f"{'-':>5}   "

        if err:
            status = color(pad("ERROR", 6), "yellow")
            detail = color(err, "yellow")
        elif norm(got) == norm(want):
            passed += 1
            status = color(pad("PASS", 6), "green")
            detail = got
        else:
            status = color(pad("FAIL", 6), "red")
            detail = f"{got}  {color('✗ want ' + want, 'red')}"

        print(f"{status}  {pad(tcol, 8)}  {pad(url, url_w)}  {detail}")

    total = len(cases)
    failed = total - passed
    print()
    summary = (
        f"{color(f'passed {passed}/{total}', 'green')}   "
        f"{color(f'failed {failed}', 'red' if failed else 'dim')}"
    )
    print(summary)
    if args.mode == "batch" and batch_ms is not None:
        print(f"time: 1 batch request {batch_ms / 1000:.1f}s "
              f"({batch_ms / total:.0f} ms/url avg over {total} urls)")
    elif times:
        print(f"time: total {sum(times) / 1000:.1f}s   avg {sum(times) / len(times):.0f} ms   "
              f"min {min(times):.0f} ms   max {max(times):.0f} ms")
    # A report, not a gate: always exit 0 so `make eval` shows the recap cleanly.
    return 0


if __name__ == "__main__":
    sys.exit(main())
