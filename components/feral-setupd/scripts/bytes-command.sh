#!/bin/bash

set -euo pipefail

encode_varint() {
  local val=$1
  local output=()
  while (( val >= 0x80 )); do
    output+=($(( (val & 0x7F) | 0x80 )))
    (( val >>= 7 ))
  done
  output+=($val)
  echo "${output[@]}"
}

encode_str() {
  local str="$1"
  local -a len_bytes
  local -a str_bytes

  len_bytes=($(encode_varint ${#str}))
  for (( i = 0; i < ${#str}; i++ )); do
    printf -v byte "%d" "'${str:i:1}"
    str_bytes+=($byte)
  done

  echo "${len_bytes[@]} ${str_bytes[@]}"
}

main() {
  if (( $# < 2 )); then
    echo "Usage: $0 <command> <reply_id> [param1] [param2] ..."
    exit 1
  fi

  local -a result
  for arg in "$@"; do
    result+=($(encode_str "$arg"))
  done

  echo "hex payload (spaced):"
  printf "%02X " "${result[@]}"
  echo

  echo "hex payload (no spaces):"
  printf "%02X" "${result[@]}"
  echo
}

main "$@"