#!/usr/bin/env bash
# Temporary, for one CI run only: reads forty Dockerfile shapes with
# .github/scripts/image-go-version.sh and prints, for each, the exit status,
# the output, the expected result and the first words of the error, with the
# temporary path written as <file>, so that the table from CI's tools can be
# compared line by line with the one from another machine.
set -u
script="$(dirname "$0")/image-go-version.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
wrong=0
case_() {
  local name=$1 want=$2 content=$3 out rc got err
  printf '%b' "$content" > "$work/Dockerfile"
  out=$(bash "$script" "$work/Dockerfile" 2>"$work/err"); rc=$?
  err=$(tr '\n' ' ' < "$work/err" | sed "s#$work/Dockerfile#<file>#g" | cut -c1-60)
  if [ "$rc" -eq 0 ]; then got="$out"; else got="fail"; fi
  if [ "$got" = "$want" ]; then verdict=ok; else verdict=WRONG; wrong=$((wrong + 1)); fi
  printf '%-44s rc=%d out=%-7s want=%-7s %-5s %s\n' "$name" "$rc" "${out:-''}" "$want" "$verdict" "$err"
}
# The review's twenty-three shapes (reader_variants.sh), in its order.
case_ "shipped form (two stages 1.26)"            1.26   "FROM golang:1.26-bookworm AS engine\nRUN x\nFROM golang:1.26-bookworm AS bundle\n"
case_ "comment holds another FROM"                1.26   "# FROM golang:1.27-bookworm AS old\nFROM golang:1.26-bookworm AS engine\nFROM golang:1.26-bookworm AS bundle\n"
case_ "digest pinned"                             1.26   "FROM golang:1.26-bookworm@sha256:a688600c AS engine\nFROM golang:1.26-bookworm@sha256:a688600c AS bundle\n"
case_ "other stage names"                         1.26   "FROM golang:1.26-bookworm AS builder\nFROM golang:1.26-bookworm AS tools\n"
case_ "patch named in both"                       1.26.8 "FROM golang:1.26.8-bookworm AS engine\nFROM golang:1.26.8-bookworm AS bundle\n"
case_ "series and patch mixed"                    fail   "FROM golang:1.26-bookworm AS engine\nFROM golang:1.26.8-bookworm AS bundle\n"
case_ "ARG in FROM (both)"                        fail   "ARG GO=1.26\nFROM golang:\${GO}-bookworm AS engine\nFROM golang:\${GO}-bookworm AS bundle\n"
case_ "ARG in one FROM, literal in other"         fail   "ARG GO=1.27\nFROM golang:\${GO}-bookworm AS engine\nFROM golang:1.26-bookworm AS bundle\n"
case_ "lower-case from (Docker accepts)"          fail   "from golang:1.27-bookworm AS engine\nFROM golang:1.26-bookworm AS bundle\n"
case_ "trailing spaces"                           1.26   "FROM golang:1.26-bookworm AS engine   \nFROM golang:1.26-bookworm AS bundle\t\n"
case_ "CRLF line ends"                            1.26   "FROM golang:1.26-bookworm AS engine\r\nFROM golang:1.26-bookworm AS bundle\r\n"
case_ "tab after FROM"                            1.26   "FROM\tgolang:1.26-bookworm AS engine\nFROM\tgolang:1.26-bookworm AS bundle\n"
case_ "--platform flag on one stage"              fail   "FROM --platform=\$BUILDPLATFORM golang:1.27-bookworm AS engine\nFROM golang:1.26-bookworm AS bundle\n"
case_ "registry prefix on one stage"              fail   "FROM docker.io/library/golang:1.27-bookworm AS engine\nFROM golang:1.26-bookworm AS bundle\n"
case_ "no variant suffix on one stage"            fail   "FROM golang:1.27 AS engine\nFROM golang:1.26-bookworm AS bundle\n"
case_ "floating tag on one stage"                 fail   "FROM golang:bookworm AS engine\nFROM golang:1.26-bookworm AS bundle\n"
case_ "indented FROM on one stage"                fail   "  FROM golang:1.27-bookworm AS engine\nFROM golang:1.26-bookworm AS bundle\n"
case_ "line continuation on one stage"            fail   "FROM \\\\\n  golang:1.27-bookworm AS engine\nFROM golang:1.26-bookworm AS bundle\n"
case_ "alpine variant"                            1.26   "FROM golang:1.26-alpine AS engine\nFROM golang:1.26-alpine AS bundle\n"
case_ "rc release named"                          fail   "FROM golang:1.27rc1-bookworm AS engine\nFROM golang:1.27rc1-bookworm AS bundle\n"
case_ "major only"                                fail   "FROM golang:1-bookworm AS engine\nFROM golang:1-bookworm AS bundle\n"
case_ "no golang stage"                           fail   "FROM debian:bookworm-slim\n"
case_ "missing file"                              fail   ""
rm -f "$work/Dockerfile"
out=$(bash "$script" "$work/Dockerfile" 2>"$work/err"); rc=$?
err=$(tr '\n' ' ' < "$work/err" | sed "s#$work/Dockerfile#<file>#g" | cut -c1-60)
if [ "$rc" -ne 0 ]; then verdict=ok; else verdict=WRONG; wrong=$((wrong + 1)); fi
printf '%-44s rc=%d out=%-7s want=%-7s %-5s %s\n' "file absent" "$rc" "${out:-''}" fail "$verdict" "$err"
# Sixteen more: each readable form in both stages, and two that must fail.
case_ "lower-case from in both"                   1.26   "from golang:1.26-bookworm AS engine\nfrom golang:1.26-bookworm AS bundle\n"
case_ "--platform flag in both"                   1.26   "FROM --platform=\$BUILDPLATFORM golang:1.26-bookworm AS engine\nFROM --platform=linux/arm64 golang:1.26-bookworm AS bundle\n"
case_ "registry prefix in both"                   1.26   "FROM docker.io/library/golang:1.26-bookworm AS engine\nFROM docker.io/library/golang:1.26-bookworm AS bundle\n"
case_ "registry with a port"                      1.26   "FROM registry.example.com:5000/library/golang:1.26-bookworm AS engine\nFROM golang:1.26-bookworm AS bundle\n"
case_ "no variant suffix in both"                 1.26   "FROM golang:1.26 AS engine\nFROM golang:1.26 AS bundle\n"
case_ "indented FROM in both"                     1.26   "  FROM golang:1.26-bookworm AS engine\n\tFROM golang:1.26-bookworm AS bundle\n"
case_ "line continuation in both"                 1.26   "FROM \\\\\n  golang:1.26-bookworm AS engine\nFROM golang:1.26-bookworm \\\\\n  AS bundle\n"
case_ "continuation with CRLF"                    1.26   "FROM \\\\\r\n  golang:1.26-bookworm AS engine\r\nFROM golang:1.26-bookworm AS bundle\r\n"
case_ "upper-case image name"                     1.26   "FROM GOLANG:1.26-bookworm AS engine\nFROM golang:1.26-bookworm AS bundle\n"
case_ "a stage built from an earlier stage"       1.26   "FROM golang:1.26-bookworm AS engine\nFROM engine AS tools\nFROM golang:1.26-bookworm AS bundle\n"
case_ "another image next to golang"              1.26   "FROM golang:1.26-bookworm AS engine\nFROM debian:bookworm-slim\nFROM golang:1.26-bookworm AS bundle\n"
case_ "an image named like golang"                1.26   "FROM golang:1.26-bookworm AS engine\nFROM mygolang:1.27 AS other\nFROM golang-tools:2 AS x\n"
case_ "no last newline"                           1.26   "FROM golang:1.26-bookworm AS engine\nFROM golang:1.26-bookworm AS bundle"
case_ "golang without a tag"                      fail   "FROM golang AS engine\nFROM golang:1.26-bookworm AS bundle\n"
case_ "golang by digest only"                     fail   "FROM golang@sha256:a688600c AS engine\nFROM golang:1.26-bookworm AS bundle\n"
case_ "a FROM with flags and no image"            1.26   "FROM --platform=linux/arm64\nFROM golang:1.26-bookworm AS bundle\n"
echo "shapes read wrongly: $wrong of 40"
exit "$wrong"
