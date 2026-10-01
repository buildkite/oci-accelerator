# shellcheck shell=bash
# Shared helpers for canonical MAJOR.MINOR.PATCH versions. Source this file.

# read_version prints the canonical version stored in a file, or fails.
read_version() {
  local content
  content=$(<"$1")
  parse_version "$content"
}

# parse_version prints its argument if it is a canonical version, or fails.
parse_version() {
  if [[ ! "$1" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    echo 'VERSION must contain a canonical MAJOR.MINOR.PATCH version' >&2
    return 1
  fi
  printf '%s\n' "$1"
}

# version_greater succeeds when canonical version $1 is greater than $2.
version_greater() {
  local -a a b
  local i
  IFS=. read -r -a a <<< "$1"
  IFS=. read -r -a b <<< "$2"
  for i in 0 1 2; do
    # Compare as strings by length first so large components never overflow.
    if ((${#a[i]} != ${#b[i]})); then
      ((${#a[i]} > ${#b[i]}))
      return
    fi
    if [[ "${a[i]}" != "${b[i]}" ]]; then
      [[ "${a[i]}" > "${b[i]}" ]]
      return
    fi
  done
  return 1
}
