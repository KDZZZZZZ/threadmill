#!/usr/bin/env bash
set -euo pipefail

readonly manifest="${1:?usage: fetch-maven.sh <manifest>}"
readonly repo=/root/.m2/repository
# Official mirror documentation: https://developer.aliyun.com/mirror/maven
readonly mirror=https://maven.aliyun.com/repository/public
readonly parallelism="${THREADMILL_MAVEN_FETCH_JOBS:-8}"

[[ "$parallelism" =~ ^[1-9][0-9]*$ ]] || {
  printf 'fetch-maven: invalid parallelism: %s\n' "$parallelism" >&2
  exit 2
}
mkdir -p -- "$repo"

fetch_one() {
  local coord="$1"
  local group artifact version packaging classifier extra part
  IFS=: read -r group artifact version packaging classifier extra <<<"$coord"
  [[ -n "$group" && -n "$artifact" && -n "$version" && -n "$packaging" && -z "${extra:-}" ]] || {
    printf 'fetch-maven: invalid coordinate: %s\n' "$coord" >&2
    return 1
  }
  for part in "$group" "$artifact" "$version" "$packaging" "${classifier:-}"; do
    [[ "$part" != */* && "$part" != . && "$part" != .. ]] || {
      printf 'fetch-maven: unsafe coordinate: %s\n' "$coord" >&2
      return 1
    }
  done

  local group_path="${group//./\/}"
  local suffix="${classifier:+-$classifier}"
  local filename="$artifact-$version$suffix.$packaging"
  local relative="$group_path/$artifact/$version/$filename"
  local destination="$repo/$relative"
  local url="$mirror/$relative"
  local temporary checksum_body expected actual attempt complete=false

  mkdir -p -- "$(dirname -- "$destination")" || return 1
  if ! checksum_body="$(
    curl --fail --silent --show-error --location --noproxy '*' \
      --connect-timeout 10 --max-time 30 \
      --retry 5 --retry-delay 1 --retry-max-time 120 --retry-all-errors \
      "$url.sha1"
  )"; then
    printf 'fetch-maven: checksum unavailable: %s\n' "$coord" >&2
    return 1
  fi
  if [[ "$checksum_body" =~ ([0-9a-fA-F]{40}) ]]; then
    expected="${BASH_REMATCH[1]}"
  else
    printf 'fetch-maven: invalid checksum: %s\n' "$coord" >&2
    return 1
  fi

  temporary="$(mktemp "$destination.tmp.XXXXXX")" || return 1
  for attempt in {1..12}; do
    if curl --fail --silent --show-error --location --noproxy '*' \
      --connect-timeout 10 --max-time 30 --continue-at - \
      --output "$temporary" "$url"; then
      complete=true
      break
    fi
    actual="$(sha1sum "$temporary")"
    actual="${actual%% *}"
    if [[ "${actual,,}" == "${expected,,}" ]]; then
      complete=true
      break
    fi
    printf 'fetch-maven: retry %d/12 after %s bytes: %s\n' \
      "$attempt" "$(stat -c %s "$temporary")" "$coord" >&2
    sleep 1
  done
  if [[ "$complete" != true ]]; then
    printf 'fetch-maven: artifact unavailable: %s\n' "$coord" >&2
    rm -f -- "$temporary"
    return 1
  fi
  actual="$(sha1sum "$temporary")"
  actual="${actual%% *}"
  if [[ "${actual,,}" != "${expected,,}" ]]; then
    printf 'fetch-maven: checksum mismatch: %s\n' "$coord" >&2
    rm -f -- "$temporary"
    return 1
  fi
  if ! chmod 0644 "$temporary"; then
    rm -f -- "$temporary"
    return 1
  fi
  mv -f -- "$temporary" "$destination"
}
export -f fetch_one
export mirror repo

mapfile -t requested < <(grep -vE '^\s*(#|$)' "$manifest")
declare -A seen=()
coords=()
for coord in "${requested[@]}"; do
  IFS=: read -r group artifact version packaging classifier extra <<<"$coord"
  if [[ -z "${seen[$coord]:-}" ]]; then
    seen[$coord]=1
    coords+=("$coord")
  fi
  descriptor="$group:$artifact:$version:pom"
  if [[ "$packaging" != pom && -z "${seen[$descriptor]:-}" ]]; then
    seen[$descriptor]=1
    coords+=("$descriptor")
  fi
done
printf 'fetch-maven: %d pinned coordinates, %d files, %d workers\n' \
  "${#requested[@]}" "${#coords[@]}" "$parallelism"
printf '%s\0' "${coords[@]}" |
  xargs -0 -r -n1 -P "$parallelism" bash -c 'fetch_one "$1"' _

found="$(
  find "$repo/com/google/code/gson" -mindepth 2 -maxdepth 2 \
    -type d -name 2.10.1 -print 2>/dev/null || true
)"
if [[ -n "$found" ]]; then
  printf 'fetch-maven: forbidden gson 2.10.1 artifacts:\n%s\n' "$found" >&2
  exit 1
fi

jars="$(find "$repo" -name '*.jar' | wc -l)"
poms="$(find "$repo" -name '*.pom' | wc -l)"
printf 'fetch-maven: ok — %s jars, %s poms, %s\n' \
  "$jars" "$poms" "$(du -sh "$repo" | cut -f1)"
