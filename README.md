# lfind: instant filename search for Linux

**lfind** is an Everything-style search engine for Linux, modeled on
[voidtools Everything](https://www.voidtools.com/). It keeps the name, size and
modification time of every file on your mounted filesystems in a compact in-RAM index.
It returns matches **sorted by name in a few milliseconds while you type**, and it stays
current in real time through `fanotify` (or `inotify` when not run as root).

- Single static Go binary (linux/amd64, linux/arm64) with no runtime dependencies
- 1.6 M files indexed in **0.75 s** (warm) and searched in **0.5–7.4 ms**
- **~60 bytes of RAM per file**, and the cache loads in **0.5 ms**
- Everything-compatible search syntax: wildcards, `|` OR, `!` NOT, `ext:`, `size:`, `dm:`, `folder:`, `file:`, `case:`, `ww:`, `path:`, `regex:` …
- Interactive search-as-you-type terminal UI (`lfind -i`)
- Unicode-aware: case folding (`σοφια` finds `ΣΟΦΙΑ`), accent-insensitive (`cafe` finds `café`), and `?` matches one character

```console
$ lfind -n 3 thesis jakarta pdf
/home/me/docs/Jakarta_Thesis_17904.pdf
/home/me/archive/Thesis_Jakarta_23728.pdf
/home/me/old/Thesis_Jakarta_34821.pdf
```

---

## Table of contents
1. [Install](#install)
2. [Quick start](#quick-start)
3. [Usage](#usage)
4. [Search syntax](#search-syntax)
5. [Interactive mode](#interactive-mode)
6. [How it works](#how-it-works)
7. [Benchmarks](#benchmarks)
8. [Comparison with Everything](#comparison-with-everything)
9. [Limitations](#limitations)
10. [Security note](#security-note)
11. [Development](#development)
12. [Ringkasan (Bahasa Indonesia)](#ringkasan-bahasa-indonesia)
13. [License](#license)

---

## Install

### From a release (recommended)

Download the archive for your CPU from the
[Releases page](https://github.com/podsni/lfind/releases), check it, and install:

```sh
VERSION=v1.0.0
ARCH=amd64            # or arm64
curl -LO https://github.com/podsni/lfind/releases/download/$VERSION/lfind-$VERSION-linux-$ARCH.tar.gz
curl -LO https://github.com/podsni/lfind/releases/download/$VERSION/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
tar xzf lfind-$VERSION-linux-$ARCH.tar.gz
sudo install -m755 lfind-$VERSION-linux-$ARCH/lfind /usr/local/bin/lfind
lfind version
```

The binaries are statically linked (`CGO_ENABLED=0`) and run on any Linux distribution.

### From source

Requires Go ≥ 1.26.

```sh
git clone https://github.com/podsni/lfind && cd lfind
go build -trimpath -ldflags "-s -w" -o lfind .
sudo install -m755 lfind /usr/local/bin/lfind
```

or `go install github.com/podsni/lfind@latest`.

### Run the daemon as a systemd service

The daemon keeps the index in memory and up to date. Run it as root so it can see the
whole filesystem and use fanotify:

```sh
sudo cp lfind.service /etc/systemd/system/lfind.service
sudo systemctl daemon-reload
sudo systemctl enable --now lfind
journalctl -u lfind -f          # watch it index and save
```

Without systemd: `sudo lfind daemon &`.

## Quick start

```sh
sudo lfind daemon &          # first start scans everything (~1 s per 1-2 M files), then stays live
lfind report                 # names containing "report", sorted by name
lfind -i                     # interactive search-as-you-type
lfind -l ext:pdf dm:thisweek # PDFs modified this week, with size and date
lfind -count '*.mp4'         # just count
```

There's no daemon? `lfind index` builds the cache file once, and queries then read it
directly. That works, but it isn't live.

## Usage

```
lfind index  [-root /] [-j N] [-exclude a,b] [-include-fuse] [-meta=false] [-db FILE]
lfind daemon [-root /] [-j N] [-exclude a,b] [-meta=false] [-no-rescan] [-db FILE] [-sock FILE]
lfind stats                    show daemon and cache statistics
lfind version
lfind [options] QUERY...       search (options may appear anywhere)
lfind -i [QUERY]               interactive search-as-you-type
```

| option | meaning |
|---|---|
| `-n N` | maximum results (default 0 = all) |
| `-count` | print only the number of matches |
| `-sort name\|path\|size\|dm` | sort key (default `name`, which costs nothing because the index is presorted) |
| `-desc` | descending order |
| `-l` | long output: size, modification time, path |
| `-c` | match case (all terms) |
| `-w` | match whole words (all terms) |
| `-p` | match against the full path (all terms) |
| `-r` | treat the whole query as one regular expression |
| `-t` | print match count and latency to stderr |
| `-local` | ignore the daemon and search the cache file directly |

Indexer and daemon options:

| option | meaning |
|---|---|
| `-root DIR` | directory to index (default `/`) |
| `-j N` | scan workers (default 2 × CPUs) |
| `-exclude a,b` | extra paths to skip |
| `-include-fuse` | also index FUSE mounts (skipped by default) |
| `-meta=false` | don't store size and mtime (−12 B/entry, ~35% faster scan) |
| `-no-rescan` | daemon: skip the catch-up rescan after loading the cache |
| `-db FILE`, `-sock FILE` | cache file and socket location |

**Defaults:**
- **Cache file:** `/var/cache/lfind/index.db` for root, `~/.cache/lfind/index.db` for other users.
- **Socket:** `/tmp/lfind.sock`.
- **Overrides:** environment variables `LFIND_DB` and `LFIND_SOCK`.
- **Debugging:** `LFIND_DEBUG=1` prints the timing of each build phase.

**Lookup order:** a query goes to the daemon if one is running. Otherwise it maps the
user's own cache file, falling back to root's.

**Skipped by default:**
- `/proc`, `/sys`, `/dev` and `/run`
- Pseudo filesystems (proc, sysfs, cgroup, tracefs, …)
- Network filesystems (nfs, cifs, sshfs, 9p)
- FUSE mounts

## Search syntax

Terms are combined with **AND**. `|` means **OR** and binds tighter than AND, as in
Everything. By default matching is on the file **name**, ignores case, and ignores accents.

| syntax | meaning | example |
|---|---|---|
| `foo bar` | name contains `foo` **and** `bar` | `lfind invoice 2026` |
| `*` `?` | wildcards; match the **whole** name; `?` = exactly one character | `lfind 'IMG_????.jpg'` |
| `a \| b` | OR | `lfind '*.jpg|*.png'`, `lfind '.mp4 | .mkv'` |
| `!term` | NOT | `lfind report '!draft'` |
| `"…"` | phrase with spaces | `lfind '"annual report"'` |
| `ext:a;b` | extension list | `lfind ext:jpg;png;webp holiday` |
| `size:` | file size: `>`, `>=`, `<`, `<=`, `a..b`, units b/kb/mb/gb/tb (1024-based); constants `empty` `tiny`(≤10 KB) `small`(≤100 KB) `medium`(≤1 MB) `large`(≤16 MB) `huge`(≤128 MB) `gigantic` | `lfind 'size:>1gb'`, `lfind size:1mb..10mb ext:pdf` |
| `dm:` | date modified: `today` `yesterday` `thisweek` `lastweek` `thismonth` `lastmonth` `thisyear` `lastyear` `last3days` `last2weeks` `last6hours`, a year `2026`, month `2026-11`, day `2026-11-16`, with `>`/`>=`/`<`/`<=` or ranges `a..b` | `lfind dm:today`, `lfind 'dm:>=2026-01-01' ext:docx` |
| `folder:` / `file:` | only folders / only files; alone or as a prefix | `lfind folder:node_modules`, `lfind file: size:huge` |
| `path:` or any term containing `/` | match against the full path | `lfind path:projects/src main`, `lfind docs/ .pdf` |
| `parent:DIR` | direct children of a folder | `lfind parent:/usr/bin py` |
| `case:` | match case for this term | `lfind case:README` |
| `ww:` / `wholeword:` | whole-word match | `lfind ww:test` (matches `my test.txt`, not `testing.txt`) |
| `diacritics:` | accent-sensitive (still case-insensitive) | `lfind diacritics:café` |
| `nocase:` `nowholeword:` `nopath:` `nodiacritics:` | turn a modifier off for one term | |
| `regex:` | regular expression on the name (case/accent-insensitive unless `case:`) | `lfind 'regex:^IMG_\d{4}\.jpe?g$'` |

Modifiers can be combined: `lfind case:ww:Makefile`, `lfind 'folder:path:src/'`.

More examples:

```sh
lfind -l -sort size -desc -n 20 file:            # 20 biggest files on the system
lfind -l -sort dm -desc -n 50 dm:today           # what changed today, newest first
lfind ext:go '!_test.go' path:/home/me/code      # Go sources that are not tests
lfind '*.log' 'size:>100mb'                      # large log files
lfind -r '^\d{8}_.*\.(jpg|heic)$'                # camera-style photo names
cd "$(dirname "$(lfind -i)")"                    # jump to the folder of what you pick
```

## Interactive mode

`lfind -i [initial query]` opens a full-screen terminal UI. Results, with size, date and
path, update on every keystroke. Each refresh takes about 0.4 ms at 125 k entries and
about 5 ms at 1.6 M entries, including the socket round trip.

| key | action |
|---|---|
| typing | refine the query (full syntax supported) |
| ↑ / ↓ | move the selection |
| Enter | exit and print the selected path to stdout |
| Ctrl-U / Ctrl-W | clear the query / delete the last word |
| Esc / Ctrl-C | quit |

## How it works

| Everything (Windows) | lfind (Linux) |
|---|---|
| reads the NTFS Master File Table | parallel directory walk with raw `getdents64` (2×CPU workers) plus one `fstatat` per entry for size/mtime |
| compact database kept sorted by name | **base table sorted by (folded name, parent path)**: per entry `parent u32 + nameOffset u32 + flags u8 + size u64 + mtime u32` = 21 B, plus the folded name; the exact name is stored only when it differs from the folded form |
| `Everything.db` | the cache file **is** the in-memory layout; it's `mmap`ed and the index slices point into it (load < 1 ms) |
| USN change journal | `fanotify` with `FAN_MARK_FILESYSTEM` + `FAN_REPORT_DFID_NAME` (create, delete, move, close-write, attrib) as root; per-directory `inotify` otherwise |
| Everything service + IPC | `lfind daemon` serving queries over a unix socket |

**Search.** The query is parsed into AND-groups of OR-terms, and the longest literal any
match must contain is picked as a *driver*.
- **Parallel sweep:** each CPU sweeps one chunk of the folded name arena with SIMD
  `bytes.Index`. A hit offset maps to its entry with a galloping search over the monotonic
  offset table, and only those candidates are checked against the remaining terms.
- **Results already sorted:** the arena is in name order, so results come out in
  Everything's default order with no sort step.
- **Other sort keys:** size, date and path use a bounded top-k heap.

**Live updates.** Each fanotify/inotify event is reduced to `(directory, name)`, then
`lstat`ed and reconciled with the index (add, delete, or update size/mtime). Because the
reconcile is driven by the actual filesystem state, merged, duplicated or reordered events
are harmless.
- **Lookups:** finding `(parent, name)` is a binary search on the sorted table, so no hash
  table is needed.
- **New entries** go to a small unsorted *delta*, merged into results in name order.
- **Deleting a folder** sets one tombstone, which hides its whole subtree.
- **Background rebuild:** the delta and tombstones are folded into a freshly sorted base
  every 5 minutes, or sooner after large changes. Queries keep running during the rebuild,
  which takes ~0.2 s for 1.6 M entries.

**Startup.** The daemon maps the cache and serves queries immediately. It starts watching,
then runs a background catch-up rescan that is swapped in atomically. Events that arrive
during the rescan are replayed.

**Text folding.** This matches Everything's defaults, with Match Case and Match Diacritics off.
- Unicode simple case folding (`ToLower(ToUpper(r))`): Σ/σ/ς, ſ/s and K/k (Kelvin sign) all match.
- Diacritics are removed via NFD decomposition: `cafe` finds `café`.

## Benchmarks

All numbers below were **measured on one machine**: 8 vCPU, 15 GB RAM, ext4 on a virtio
disk, Debian 13, kernel 6.12, Go 1.27.

**Method** ([`bench.py`](bench.py), raw results in [`bench/`](bench)):
- A synthetic tree of ~1.47 M files in 30 000 folders was added to the root filesystem,
  giving **≈1,596,250 indexed entries**. It was deleted afterwards.
- *Before* is the first version (unsorted index, hash-table live lookups). *After* is the
  current version. Both ran back-to-back on the same tree.
- Query times are medians of 21 runs. `find` is the median of 3 warm runs.
- "CLI" is a whole process (start, socket, output) using `-count`.

### Index, memory, load

| metric | before | **after (current)** |
|---|---:|---:|
| full index, cold page cache | 0.88 s | 2.00 s *(now also stats every file for size + mtime)* |
| full index, warm cache | 0.29 s | 0.75 s *(0.50 s with `-meta=false`)* |
| cache file load | 31 ms | **0.54 ms** |
| cache file size | 85.7 MB | 86.4 MB *(includes size + mtime)* |
| daemon memory (RSS) | 425 MB (279 B/entry) | **92 MB (60.5 B/entry)** |
| default result order | walk order | **sorted by name, free** |
| search-as-you-type, `thesis jakarta`, 50 results per keystroke (median / max) | 7.1 / 17.6 ms | **5.4 / 9.6 ms** |
| `plocate` `updatedb`, same tree (cold / warm) | 3.45 s / 2.17 s | — |

### Query latency

| query | matches | daemon search: before → after | CLI via daemon: before → after | CLI without daemon: before → after | `plocate -i -b` | `find / -xdev` |
|---|---:|---:|---:|---:|---:|---:|
| `a` (very broad) | ~1.11 M | 14.0 → **7.4 ms** | 17.2 → **9.4 ms** | 47.1 → **5.7 ms** | 995 ms | 984 ms |
| `report` | 95,460 | 3.9 → **2.4 ms** | 7.5 → **5.6 ms** | 34.7 → **5.1 ms** | 492 ms | 1004 ms |
| `thesis jakarta` | 3,085 | 2.4 → **1.7 ms** | 5.6 → **3.8 ms** | 31.3 → **4.8 ms** | 319 ms | 987 ms |
| `*.mp4` | 79,618 | 4.4 → **4.3 ms** | 7.7 → **6.8 ms** | 34.5 → **7.9 ms** | 452 ms | 992 ms |
| `ext:pdf laporan` | 5,275 | 3.3 → **2.1 ms** | 6.3 → **4.4 ms** | 32.1 → **5.5 ms** | 291 ms | 1015 ms |
| `libc.so` | 3 | 2.4 → **1.5 ms** | 5.6 → **4.0 ms** | 32.6 → **4.7 ms** | 2.2 ms | 972 ms |
| `zzqqxx` (no match) | 0 | 0.46 → 0.48 ms | 3.7 → **3.1 ms** | 29.8 → **3.8 ms** | 2.1 ms | 926 ms |
| regex `^main_.*\.go$` | 2,750 | 12.3 → **11.0 ms** | 16.0 → **14.2 ms** | 42.6 → **15.2 ms** | 46 ms | 2072 ms |

Sorted first page (`report -n 100`, whole CLI): name 5.9 ms, size 6.5 ms, date 6.1 ms, path 20.2 ms.

**Other measurements:**
- **Live updates:** a newly created file was searchable after 2.4 ms median (4.6 ms max,
  10 probes, fanotify, 125 k-entry index).
- **Mass change:** deleting the 1.47 M-file tree while the daemon ran produced 1.42 M
  fanotify events. The daemon kept up, rebuilt itself three times, and settled at
  16.6 MB RSS for the remaining ~125 k entries.

plocate's trigram index matches lfind on very rare literals (~2 ms), but on common substrings
it is roughly 65–105× slower than the whole lfind CLI call, and its database is only refreshed when `updatedb` runs.

## Comparison with Everything

> ⚠️ Everything's figures come from the [voidtools FAQ](https://www.voidtools.com/faq/).
> They were measured by voidtools on **different hardware, OS and file system** and are
> rounded, so read this as a rough orientation, not a head-to-head benchmark.

| | Everything (published) | lfind (measured above) |
|---|---|---|
| index time | ~250 k files ≈ 5 s; 1 M files ≈ 1 min | 1.6 M entries: 0.75 s warm, 2.0 s cold (incl. size + mtime) |
| RAM | ~250 k files ≈ 35 MB; 1 M files ≈ 100 MB (~100 B/file) | 60.5 B/entry at 1.6 M; 16.6 MB at 125 k |
| database on disk | 1 M files ≈ 45 MB | ≈ 54 B/entry (≈ 54 MB per 1 M), uncompressed so it can be mmapped |
| search | "instant", sorted by name | 0.5–7.4 ms in the daemon, presorted by name |
| live updates | real time (NTFS USN journal) | real time (fanotify / inotify) |
| changes made while not running | replayed from the USN journal | background catch-up rescan at start (~0.8 s for 1.6 M), cache served meanwhile |
| search syntax | very rich | the name/path/size/date subset listed above |

## Limitations

- **No MFT or USN journal on Linux.** Changes made while the daemon was stopped are found by
  a rescan at startup, not by a journal replay. Cold-cache indexing speed is bounded by inode
  reads from disk.
- **Not implemented:** `content:` search, folder sizes, date created/accessed, a GUI, the
  HTTP/ETP servers, bookmarks/filters, `< >` grouping and macros.
- **Sorting:** name order is presorted. Size, date and path are sorted per query (≈6 ms for
  95 k hits, path ≈20 ms) instead of being kept as extra presorted indexes.
- **Case folding** is *simple* Unicode folding, so `ß` does not match `ss`.
- **Size/mtime updates** happen on close-after-write and on attribute changes. A file being
  written but kept open (e.g. a growing log) shows its new size after it is closed or after
  the next rescan.
- **fanotify** filesystem marks need root and kernel ≥ 5.9. A filesystem that refuses FID
  reporting gets no live updates, only rescans.
- **inotify fallback** (non-root) needs one watch per directory, limited by
  `fs.inotify.max_user_watches`.
- **Platform:** Linux only, because it uses `getdents64`, fanotify and inotify.

## Security note

To see the whole filesystem, the daemon usually runs as **root**. Its unix socket
(`/tmp/lfind.sock`) is created with mode **0666**, so **any local user can query it and
learn the names, sizes and dates of every file on the system**, including files in folders
they can't read. The Everything service behaves the same way. On a multi-user machine:

- put the socket in a private directory, e.g. `lfind daemon -sock /run/lfind/lfind.sock`
  with a root-only (or group-restricted) `/run/lfind`
- or run the daemon as your own user with `-root $HOME`, so it indexes only what you can read
- or use `-exclude` to keep sensitive trees out of the index.

The cache file (`/var/cache/lfind/index.db`) is world-readable (0644) and contains the same
information. Tighten its permissions if that matters.

## Development

```sh
go vet ./... && go build -o lfind .
sh ./test.sh                     # 36 checks: syntax, sorting, Unicode, live updates (inotify), persistence
LFIND_TEST_SUDO=1 sh ./test.sh   # also runs the live-update checks through a root fanotify daemon
python3 bench.py ./lfind after --others   # benchmark (needs sudo); --others adds find/plocate
```

Source layout:

| file | contents |
|---|---|
| `scan.go` | parallel `getdents64` walker, mount detection and skipping |
| `build.go` | folding, sorting and layout of the final index |
| `index.go` | index structure, lookups, live delta, cache file format (mmap) |
| `fold.go` | Unicode case and diacritic folding |
| `query.go` | query parser (Everything syntax), size/date parsing, glob, whole word |
| `search.go` | parallel SIMD search, result ordering |
| `daemon.go` | daemon, reconcile, rebuild/save, socket protocol and client |
| `live.go` | fanotify and inotify watchers |
| `tui.go` | interactive terminal UI |
| `main.go` | command-line interface |

Pushes to `main` and `v*` tags run [`.github/workflows/release.yml`](.github/workflows/release.yml),
which does three things:
- runs the tests;
- cross-builds static linux/amd64 and linux/arm64 binaries;
- publishes them with `SHA256SUMS` on the GitHub Release (`v1.0.0` for main, or the tag name).

## Ringkasan (Bahasa Indonesia)

**lfind** adalah mesin pencari nama file super cepat untuk Linux, terinspirasi dari
*Everything* (voidtools) di Windows.
- **Cara kerja:** semua nama file, ukuran, dan tanggal ubahnya disimpan di RAM dalam indeks
  ringkas yang sudah **terurut berdasarkan nama**. Karena itu hasil pencarian keluar dalam
  hitungan milidetik, langsung urut, bahkan saat kamu masih mengetik (`lfind -i`).
- **Selalu terkini:** perubahan file dipantau real time lewat `fanotify` (jika dijalankan
  sebagai root) atau `inotify`.
- **Hasil uji:** pada 1,6 juta file, indeks dibuat dalam 0,75 detik dan pencarian umumnya
  butuh 2–7 ms. Untuk kata yang umum, itu kira-kira 65–105× lebih cepat dari `plocate` dan lebih dari 100× lebih
  cepat dari `find`. Memorinya sekitar 60 byte per file.
- **Sintaks:** mendukung sintaks ala Everything: `*` `?`, `|` (OR), `!` (NOT), `ext:`,
  `size:`, `dm:`, `folder:`, `file:`, `case:`, `ww:`, `path:`, dan `regex:`.

Instalasi singkat: unduh arsip dari halaman *Releases*, ekstrak, lalu jalankan:

```sh
sudo install -m755 lfind /usr/local/bin/lfind
sudo lfind daemon &        # atau pasang lfind.service di systemd
lfind laporan              # cari file yang namanya mengandung "laporan"
lfind -i                   # mode interaktif
```

Perlu diingat: daemon yang berjalan sebagai root membuat nama semua file bisa dicari oleh
semua user lokal lewat socket. Baca bagian [Security note](#security-note).

## License

[MIT](LICENSE) © 2026 Hendra Bangun Dwi R
