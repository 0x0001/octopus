#!/bin/bash
set -euo pipefail

# 本地单平台 Docker 镜像打包, 与 CI 的 scripts/build.sh 互补:
# 不依赖 Android NDK, 不构建多平台矩阵, 面向"本地打镜像 + scp 上服务器"的个人部署流程。
#
# 版本号策略: Version 跟随上游最新 tag(面板检查更新按版本号与上游对比),
# Commit 注入本仓库提交哈希用于识别自建版本。
# 前端构建必须注入 VITE_APP_VERSION, 否则面板报前后端版本不一致。
#
# 用法:
#   scripts/build-local-docker.sh                    # 完整流程, 目标 linux/amd64
#   PLATFORM=linux/arm64 scripts/build-local-docker.sh
#   SKIP_FRONTEND=1 scripts/build-local-docker.sh    # 前端无改动时跳过前端构建提速
#   USE_UPX=0 scripts/build-local-docker.sh          # 跳过 upx 压缩
#
# 产物: build/dist/octopus-<os>-<arch>-<version>[-upx].tar.gz 与 .sha256

readonly APP_NAME="octopus"
readonly OUTPUT_DIR="build"
readonly DIST_DIR="${OUTPUT_DIR}/dist"
readonly PLATFORM="${PLATFORM:-linux/amd64}"
readonly GOOS="${PLATFORM%%/*}"
readonly GOARCH="${PLATFORM##*/}"
readonly USE_UPX="${USE_UPX:-1}"
readonly SKIP_FRONTEND="${SKIP_FRONTEND:-0}"
readonly UPSTREAM_REPO="https://github.com/bestruirui/octopus"

readonly IMAGE_BASE="octopus"
readonly DOCKERFILE="scripts/dockerfile/Dockerfile"

log() { printf '\033[36m[build]\033[0m %s\n' "$1"; }
die() { printf '\033[31m[build]\033[0m %s\n' "$1" >&2; exit 1; }

require() {
    command -v "$1" >/dev/null 2>&1 || die "缺少依赖: $1 (请先安装)"
}

# ---- 版本: 上游最新 tag, 没有则给出明确的修复指引。 ----
git fetch --tags "${UPSTREAM_REPO}" 2>/dev/null || log "警告: 拉取上游 tags 失败(网络?), 按本地已有 tags 继续"
readonly VERSION="$(git describe --tags --abbrev=0 2>/dev/null || true)"
[ -n "${VERSION}" ] || die "本地没有任何 tag; 先执行: git fetch --tags ${UPSTREAM_REPO}"
readonly COMMIT="$(git rev-parse --short HEAD)"
readonly BUILD_TIME="$(TZ='Asia/Shanghai' date +'%F %T %z')"
readonly LDFLAGS="-X 'github.com/bestruirui/${APP_NAME}/internal/conf.Version=${VERSION}' \
-X 'github.com/bestruirui/${APP_NAME}/internal/conf.BuildTime=${BUILD_TIME}' \
-X 'github.com/bestruirui/${APP_NAME}/internal/conf.Author=bestrui' \
-X 'github.com/bestruirui/${APP_NAME}/internal/conf.Commit=${COMMIT}' -s -w"

readonly UPX_SUFFIX=$([ "${USE_UPX}" = "1" ] && echo "-upx" || echo "")
readonly IMAGE_TAG="${IMAGE_BASE}:${VERSION}${UPX_SUFFIX}"
readonly ARCHIVE="${DIST_DIR}/${APP_NAME}-${GOOS}-${GOARCH}-${VERSION}${UPX_SUFFIX}.tar.gz"

log "版本 ${VERSION} (commit ${COMMIT}) 平台 ${PLATFORM} upx=${USE_UPX}"

# ---- 前端: VITE_APP_VERSION 注入构建版本, 缺失会导致面板前后端版本不一致提示。 ----
if [ "${SKIP_FRONTEND}" != "1" ]; then
    require pnpm
    (cd web && { [ -d node_modules ] || pnpm install --frozen-lockfile; } &&
        VITE_APP_VERSION="${VERSION}" pnpm build)
    # vite 的 emptyOutDir 会删掉被 git 跟踪的占位文件, 还原以保持工作区干净。
    git checkout -- static/out/README.md 2>/dev/null || true
else
    log "跳过前端构建(沿用 static/out 现有产物)"
    grep -rq "${VERSION}" static/out 2>/dev/null ||
        die "static/out 缺少版本 ${VERSION} 的前端产物; 去掉 SKIP_FRONTEND 重跑"
fi

# ---- 清理前端产物中的无用资源: screenshot/*.png 只被 README 与 PWA 安装宣传引用,
# 运行时不加载, 却占 ~5.9MB 嵌入体积; PNG 已压缩 gzip 无收益, 只能直接删。
rm -rf static/out/screenshot

# ---- Go 交叉编译: 与上游 build.sh 同口径(CGO 关闭, trimpath, 版本注入)。 ----
require go
log "编译 ${GOOS}/${GOARCH}"
mkdir -p "${DIST_DIR}" "${OUTPUT_DIR}/docker/${PLATFORM}"
env GOOS="${GOOS}" GOARCH="${GOARCH}" CGO_ENABLED=0 \
    go build -trimpath -o "${OUTPUT_DIR}/docker/${PLATFORM}/${APP_NAME}" \
    -ldflags="${LDFLAGS}" .

# ---- upx: 大幅缩小镜像体积; --best 对 50MB 二进制过慢, 用默认级别。 ----
if [ "${USE_UPX}" = "1" ]; then
    require upx
    log "upx 压缩"
    upx "${OUTPUT_DIR}/docker/${PLATFORM}/${APP_NAME}" >/dev/null
fi

# ---- 许可证报告: 上游 Dockerfile COPY 的硬依赖。 ----
log "生成 THIRD_PARTY_LICENSES.csv"
GOFLAGS= go run github.com/google/go-licenses/v2@v2.0.1 report . \
    --ignore "github.com/bestruirui/${APP_NAME}" >"${OUTPUT_DIR}/THIRD_PARTY_LICENSES.csv"

# ---- 镜像构建与分发归档。 ----
require docker
log "构建镜像 ${IMAGE_TAG}"
docker buildx build --platform "${PLATFORM}" -f "${DOCKERFILE}" \
    -t "${IMAGE_TAG}" --load . >/dev/null

log "归档 ${ARCHIVE}"
docker save "${IMAGE_TAG}" | gzip >"${ARCHIVE}"
(cd "${DIST_DIR}" && { command -v sha256sum >/dev/null 2>&1 && sha256sum "$(basename "${ARCHIVE}")" || shasum -a 256 "$(basename "${ARCHIVE}")"; } \
    >"$(basename "${ARCHIVE}").sha256")

log "完成: ${ARCHIVE}"
log "镜像: ${IMAGE_TAG} (${COMMIT}/${VERSION})"
echo
echo "部署到服务器:"
echo "  scp ${ARCHIVE} user@your-server:/tmp/"
echo "  ssh user@your-server 'docker load -i /tmp/$(basename "${ARCHIVE}")'"
echo "  ssh user@your-server 'docker rm -f octopus && docker run -d --name octopus \\"
echo "    -v /opt/octopus/data:/app/data -p 8088:8080 --restart unless-stopped ${IMAGE_TAG}'"
