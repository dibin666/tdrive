#!/usr/bin/env bash

set -Eeuo pipefail

# Always resolve paths from the project directory, so the script also works
# when it is invoked through an absolute path from somewhere else.
ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT_DIR"

BINARY="${TDRIVE_BINARY:-$ROOT_DIR/tdrive}"
DATA_DIR="${TDRIVE_DATA_DIR:-$ROOT_DIR/data}"

# Keep relative overrides anchored to the project as well.
if [[ "$BINARY" != /* ]]; then
  BINARY="$ROOT_DIR/$BINARY"
fi

# The application resolves relative paths from its working directory. Make the
# default explicit and keep a relative custom value predictable as well.
if [[ "$DATA_DIR" != /* ]]; then
  DATA_DIR="$ROOT_DIR/$DATA_DIR"
fi
export TDRIVE_DATA_DIR="$DATA_DIR"

changed_file=""
if [[ ! -x "$BINARY" ]]; then
  changed_file="(可执行文件不存在)"
else
  # Runtime data, VCS metadata, and installed frontend dependencies are not
  # source inputs. Source files under cmd/, internal/, and ui/ plus the Go
  # module metadata participate in the build freshness check. The generated
  # ui/dist is included because the Go binary embeds it. Keeping all internal
  # files here also covers embedded assets such as schema.sql.
  changed_file="$(find "$ROOT_DIR" \
    -path "$ROOT_DIR/.git" -prune -o \
    -path "$ROOT_DIR/.codegraph" -prune -o \
    -path "$DATA_DIR" -prune -o \
    -path "$ROOT_DIR/ui/node_modules" -prune -o \
    -path "$ROOT_DIR/ui/tsconfig.tsbuildinfo" -prune -o \
    -type f \( \
      -name '*.go' -o \
      -name 'go.mod' -o \
      -name 'go.sum' -o \
      -name 'go.work' -o \
      -path "$ROOT_DIR/cmd/*" -o \
      -path "$ROOT_DIR/internal/*" -o \
      -path "$ROOT_DIR/ui/*" \
    \) -newer "$BINARY" -print -quit)"
fi

if [[ -n "$changed_file" ]]; then
  if [[ "$changed_file" == /* && "$changed_file" != "(可执行文件不存在)" ]]; then
    printf '[tdrive] 检测到文件更新：%s\n' "${changed_file#"$ROOT_DIR/"}"
  else
    printf '[tdrive] %s，开始编译。\n' "$changed_file"
  fi

  command -v go >/dev/null 2>&1 || {
    printf '[tdrive] 未找到 go，请先安装 Go。\n' >&2
    exit 1
  }
  command -v pnpm >/dev/null 2>&1 || {
    printf '[tdrive] 未找到 pnpm，请先安装 pnpm。\n' >&2
    exit 1
  }

  # Keep the generated frontend and the embedded UI in sync whenever a build
  # is needed. pnpm install is safe to repeat and handles a fresh checkout or
  # a changed lockfile without requiring a separate setup step.
  (
    cd "$ROOT_DIR/ui"
    pnpm install --frozen-lockfile
    pnpm build
  )

  mkdir -p "$(dirname -- "$BINARY")"
  temporary_binary="${BINARY}.tmp.$$"
  trap 'rm -f -- "$temporary_binary"' EXIT
  go build -trimpath -o "$temporary_binary" ./cmd/tdrive
  chmod +x "$temporary_binary"
  mv -f -- "$temporary_binary" "$BINARY"
  trap - EXIT
else
  printf '[tdrive] 未检测到文件更新，直接启动。\n'
fi

printf '[tdrive] 数据目录：%s\n' "$TDRIVE_DATA_DIR"
exec "$BINARY" "$@"
