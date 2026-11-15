#!/usr/bin/env python3
"""Benchmark an lfind binary vs find and plocate on this machine (needs sudo).
usage: bench.py [BINARY] [LABEL]   -> prints JSON
Starts its own root daemon on a private socket/db, so it can compare builds side by side."""
import subprocess, time, statistics, re, shutil, os, json, sys
L = sys.argv[1] if len(sys.argv) > 1 else os.path.join(os.path.dirname(os.path.abspath(__file__)), "lfind")
LABEL = sys.argv[2] if len(sys.argv) > 2 else "lfind"
DB, SOCK = f"/tmp/lfbench-{LABEL}.db", f"/tmp/lfbench-{LABEL}.sock"
ENV = f"LFIND_DB={DB} LFIND_SOCK={SOCK}"
OTHERS = "--others" in sys.argv

def sh(cmd):
    return subprocess.run(cmd, shell=True, capture_output=True, text=True)

def timed(cmd, n):
    ts, out = [], None
    for _ in range(n):
        t = time.perf_counter(); out = sh(cmd); ts.append((time.perf_counter() - t) * 1000)
    return statistics.median(ts), out

def ms(m):
    v = float(m.group(1)); u = m.group(2)
    return v / 1000 if u == "µs" else v * 1000 if u == "s" else v

def drop():
    sh("sync; echo 3 | sudo tee /proc/sys/vm/drop_caches >/dev/null")

res = {"label": LABEL}
drop(); t = time.perf_counter(); r = sh(f"sudo {ENV} {L} index"); res["index_cold_s"] = round(time.perf_counter() - t, 3)
res["index_out"] = r.stdout.strip()
res["index_warm_ms"] = statistics.median([timed(f"sudo {ENV} {L} index", 1)[0] for _ in range(3)])
if "-meta" in sh(f"{L} index -h 2>&1").stdout:
    res["index_warm_nometa_ms"] = statistics.median([timed(f"sudo {ENV} {L} index -meta=false -db /tmp/lfbench-nometa.db", 1)[0] for _ in range(3)])
    sh("sudo rm -f /tmp/lfbench-nometa.db")
res["cache_bytes"] = os.path.getsize(DB)
loads = [ms(re.search(r"loaded in ([\d.]+)(ms|µs|s)", sh(f"LFIND_DB={DB} LFIND_SOCK=/nonexistent {L} stats").stdout)) for _ in range(5)]
res["cache_load_ms"] = statistics.median(loads)
if OTHERS and shutil.which("updatedb"):
    drop(); res["updatedb_cold_s"] = round(timed("sudo updatedb", 1)[0] / 1000, 3)
    res["updatedb_warm_ms"] = statistics.median([timed("sudo updatedb", 1)[0] for _ in range(3)])

# daemon (default settings: loads cache, catch-up rescan, then idle)
sh(f"sudo rm -f {SOCK}")
d = subprocess.Popen(f"sudo {ENV} {L} daemon > /tmp/lfbench-{LABEL}.log 2>&1", shell=True)
for _ in range(200):
    lg = f"/tmp/lfbench-{LABEL}.log"
    if os.path.exists(lg) and "rescan done" in open(lg).read() and os.path.exists(SOCK):
        break
    time.sleep(0.1)
time.sleep(6)
pid = sh(f"pgrep -f 'lfind daemon' -n").stdout.strip()
pids = sh(f"pgrep -f '{L} daemon'").stdout.split()
rss = [int(sh(f"ps -o rss= -p {p}").stdout.strip() or 0) for p in pids]
res["daemon_rss_mb"] = round(max(rss) / 1024, 1)
st = sh(f"{ENV} {L} stats").stdout
res["daemon_stats"] = st.splitlines()[0] if st else ""
m = re.search(r"entries=(\d+)", res["index_out"]); n = int(m.group(1))
res["entries"] = n
res["daemon_bytes_per_entry"] = round(max(rss) * 1024 / n, 1)

queries = [
    ("a (very broad)",   "a",                  "-iname '*a*'",                               "a"),
    ("report",           "report",             "-iname '*report*'",                          "report"),
    ("thesis jakarta",   "thesis jakarta",     "-iname '*thesis*' -iname '*jakarta*'",       "thesis jakarta"),
    ("*.mp4",            "'*.mp4'",            "-iname '*.mp4'",                             "'*.mp4'"),
    ("ext:pdf laporan",  "ext:pdf laporan",    "-iname '*laporan*' -iname '*.pdf'",          "laporan '*.pdf'"),
    ("libc.so",          "libc.so",            "-iname '*libc.so*'",                         "libc.so"),
    ("zzqqxx (none)",    "zzqqxx",             "-iname '*zzqqxx*'",                          "zzqqxx"),
    ("regex ^main_.*\\.go$", "-r '^main_.*\\.go$'", "-regextype posix-extended -iregex '.*/main_[^/]*\\.go'", "--regex '^main_.*\\.go$'"),
]
N = 21
rows = []
for name, lq, fq, pq in queries:
    row = {"query": name}
    srv = []
    for _ in range(N):
        r = sh(f"{ENV} {L} -t -count {lq}")
        srv.append(ms(re.search(r"search ([\d.]+)(ms|µs|s)", r.stderr)))
        row["matches"] = int(r.stdout.strip() or 0)
    row["daemon_search_ms"] = statistics.median(srv)
    row["cli_count_ms"] = timed(f"{ENV} {L} -count {lq}", N)[0]
    row["cli_first100_ms"] = timed(f"{ENV} {L} -n 100 {lq} >/dev/null", N)[0]
    row["cli_nodaemon_ms"] = timed(f"LFIND_DB={DB} LFIND_SOCK=/nonexistent {L} -local -count {lq}", N)[0]
    if OTHERS:
        if shutil.which("plocate"):
            t, r = timed(f"plocate -i -b -c {pq}", N); row["plocate_ms"] = t; row["plocate_count"] = r.stdout.strip()
        t, r = timed(f"sudo find / -xdev \\( {fq} \\) -print 2>/dev/null | wc -l", 3); row["find_ms"] = t; row["find_count"] = r.stdout.strip()
    rows.append(row)
res["queries"] = rows
if "-sort" in sh(f"{L} help").stdout:
    res["sorted_first100_ms"] = {k: timed(f"{ENV} {L} -n 100 -sort {k} report >/dev/null", N)[0] for k in ["name", "size", "dm", "path"]}
# interactive typing simulation: prefixes of a query, 50 results each (what lfind -i requests)
typing = []
for i in range(1, len("thesis jakarta") + 1):
    q = "thesis jakarta"[:i]
    typing.append(timed(f"{ENV} {L} -n 50 '{q}' >/dev/null", 5)[0])
res["typing_per_keystroke_ms_median"] = statistics.median(typing)
res["typing_per_keystroke_ms_max"] = max(typing)
sh(f"sudo pkill -f '{L} daemon'"); time.sleep(1)
sh(f"sudo rm -f {DB} {SOCK}")
print(json.dumps(res, indent=1, ensure_ascii=False))
