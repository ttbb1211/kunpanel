#!/bin/bash
# ============================================================================
#  坤面板 KunPanel - 交叉编译 & 打包发布脚本
#  产物: release/ 目录下的 linux(amd64/arm64/arm) 与 windows(amd64) 安装包
# ============================================================================
set -e
cd "$(dirname "$0")"

VERSION="${1:-v1.0.0}"
# 第二个参数:发布仓库(owner/repo)。用 fork 自行发布时必须传,否则面板的
# 「一键更新」会去上游找 Release,把自己的修复覆盖掉。
REPO="${2:-}"
OUT="release"
LDFLAGS="-s -w -X main.version=${VERSION}"
if [[ -n "$REPO" ]]; then
  LDFLAGS="$LDFLAGS -X kunpanel/internal/panelupdate.Repo=${REPO}"
fi

echo "==> 构建坤面板 ${VERSION}${REPO:+ (发布源 ${REPO})}"

TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "linux/arm"
  "windows/amd64"
)

rm -rf "$OUT"
mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"   # 绝对路径:后续在子目录里 cd 后仍可用

for target in "${TARGETS[@]}"; do
  GOOS="${target%/*}"
  GOARCH="${target#*/}"
  NAME="kunpanel"
  [[ "$GOOS" == "windows" ]] && NAME="kunpanel.exe"
  echo "---- 编译 $GOOS/$GOARCH ----"
  CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/pkg_$GOOS-$GOARCH/$NAME" .
done

echo "---- 打包 ----"
for d in "$OUT"/pkg_*; do
  base=$(basename "$d")
  os="${base#pkg_}"; os="${os%-*}"
  arch="${base##*-}"
  name="kunpanel_${VERSION}_${os}_${arch}"
  # 归档扁平化:二进制/脚本在顶层(install.sh 按 /opt/kunpanel/bin/kunpanel 直接解压)
  if [[ "$os" == "windows" ]]; then
    cp scripts/install.ps1 scripts/install.bat "$d/"
    (cd "$d" && zip -qr "$OUT/${name}.zip" .)
    # 无版本命名副本:供 GitHub releases/latest/download 直链
    (cd "$OUT" && cp "${name}.zip" "kunpanel_windows_${arch}.zip")
  else
    cp scripts/install.sh "$d/"
    COPYFILE_DISABLE=1 tar --no-xattrs -czf "$OUT/${name}.tar.gz" -C "$d" .
    (cd "$OUT" && cp "${name}.tar.gz" "kunpanel_${os}_${arch}.tar.gz")
  fi
  rm -rf "$d"
done

echo "==> 完成,产物清单:"
ls -lh "$OUT" | grep -v '^total' | awk '{print "  " $NF "  (" $5 ")"}'
echo "==> 校验和:"
# macOS 用 shasum,Linux 用 sha256sum;两者输出格式一致(<hash>  ./<name>)
( cd "$OUT" && rm -f SHA256SUMS && \
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum ./kunpanel_*.tar.gz ./kunpanel_*.zip > SHA256SUMS
  else
    shasum -a 256 ./kunpanel_*.tar.gz ./kunpanel_*.zip > SHA256SUMS
  fi && cat SHA256SUMS )
