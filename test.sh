#!/bin/sh
# Functional + live-update tests on a throwaway tree.
# Runs the daemon as the current user (inotify); run with LFIND_TEST_SUDO=1 to also test fanotify via sudo.
set -e
L=$(cd "$(dirname "$0")" && pwd)/lfind; T=$(mktemp -d); export LFIND_DB=$T/db LFIND_SOCK=$T/sock
DP=
fail() { echo "FAIL: $*"; [ -n "$DP" ] && { kill $DP 2>/dev/null || sudo kill $DP 2>/dev/null; }; exit 1; }
eq() { [ "$1" = "$2" ] || fail "$3: got [$1] want [$2]"; }
r() { $L "$@" | sed "s|$T/r||" | tr '\n' ' ' | sed 's/ $//'; }
mkdir -p $T/r/a/src $T/r/B/Docs
cd $T/r
touch a/src/main.go a/src/Util.GO B/Docs/Report.PDF B/Docs/notes.txt "B/Docs/my file.txt" café.txt CAFE.md Straße.txt ΣΟΦΙΑ.txt zeta.txt Alpha.txt
head -c 50000 /dev/zero > big.bin; touch -d 2020-01-01 old.txt
$L index -root $T/r >/dev/null
eq "$($L -count go)" 2 substring
eq "$(r '*.go')" "/a/src/main.go /a/src/Util.GO" "glob+name-sort"
eq "$(r txt)" "/Alpha.txt /café.txt /B/Docs/my file.txt /B/Docs/notes.txt /old.txt /Straße.txt /zeta.txt /ΣΟΦΙΑ.txt" "default sort by name"
eq "$(r -sort size -desc -n 1 file:)" "/big.bin" "sort size desc"
eq "$($L -count ext:txt)" 8 ext
eq "$($L -count path:docs)" 4 path
eq "$($L -count '!main' ext:go)" 1 negate
eq "$($L -count '"my file"')" 1 quoted
eq "$($L -count -c util)" 0 case-flag
eq "$(r case:CAFE)" "/CAFE.md" "case:"
eq "$(r cafe)" "/CAFE.md /café.txt" "diacritics-insensitive default"
eq "$(r diacritics:café)" "/café.txt" "diacritics:"
eq "$(r σοφια)" "/ΣΟΦΙΑ.txt" "unicode case fold"
eq "$(r STRASSE)" "" "no full folding of ß (same as simple folding)"
eq "$(r 'caf?.txt')" "/café.txt" "? = one UTF-8 character"
eq "$(r '*.go|*.pdf')" "/a/src/main.go /B/Docs/Report.PDF /a/src/Util.GO" "OR"
eq "$(r 'ext:go | ext:md' '!util')" "/CAFE.md /a/src/main.go" "OR + AND"
eq "$(r ww:file)" "/B/Docs/my file.txt" "wholeword"
eq "$(r folder: doc)" "/B/Docs" "folder:"
eq "$($L -count file: doc)" 0 "file:"
eq "$(r 'size:>10kb')" "/big.bin" "size:"
eq "$(r size:empty ext:md)" "/CAFE.md" "size:empty"
eq "$(r dm:2020)" "/old.txt" "dm:year"
eq "$($L -count dm:today ext:txt)" 7 "dm:today"
eq "$($L -count -r '^(main|notes)\.')" 2 regex
eq "$(r -r '^caf.\.')" "/CAFE.md /café.txt" "regex (case/accent-insensitive, literal prefilter)"
eq "$(r -c -r '^caf')" "/café.txt" "case-sensitive regex"
eq "$(r -sort path "ext:pdf|ext:go")" "/a/src/main.go /a/src/Util.GO /B/Docs/Report.PDF" "sort by path"
eq "$(r parent:$T/r/a/src)" "/a/src/main.go /a/src/Util.GO" "parent:"

DM=""; [ "$LFIND_TEST_SUDO" = 1 ] && DM="sudo -E"
for mode in user $( [ -n "$DM" ] && echo root ); do
  if [ $mode = root ]; then sudo -E $L daemon -root $T/r -no-rescan 2>$T/log.$mode & else $L daemon -root $T/r -no-rescan 2>$T/log.$mode & fi
  DP=$!; sleep 0.6
  touch new_$mode.xyz; mkdir -p d_$mode/x; touch d_$mode/x/in_$mode.xyz
  mv a a_$mode; rm -f B/Docs/notes.txt; head -c 30000 /dev/zero >> zeta.txt; sleep 0.4
  eq "$($L -count _$mode.xyz)" 2 "$mode live-create"
  eq "$($L -count path:a_$mode/src)" 3 "$mode live-rename"
  eq "$($L -count notes)" 0 "$mode live-delete"
  eq "$(r 'size:>20kb' ext:txt)" "/zeta.txt" "$mode live size update"
  eq "$(r -n 3 _$mode.xyz)" "/d_$mode/x/in_$mode.xyz /new_$mode.xyz" "$mode delta merged in name order"
  if [ $mode = root ]; then sudo kill $DP; else kill $DP; fi; sleep 0.5; DP=
  mv a_$mode a; touch B/Docs/notes.txt
  eq "$($L -local -count _$mode.xyz)" 2 "$mode persisted (re-sorted cache)"
  $L index -root $T/r >/dev/null
done
cd /; rm -rf $T 2>/dev/null || sudo rm -rf $T; echo "all tests passed"
