#!/usr/bin/env bash
# ============================================================================
# Collei Agent 一键部署脚本
#
# 用法（安装 — 从 GitHub 直连）:
#   wget -O- https://raw.githubusercontent.com/collei-monitor/collei-agent/main/install.sh | bash -s -- \
#       --url https://api.example.com --reg-token YOUR_TOKEN
#
# 用法（安装 — 通过面板中转）:
#   curl -fsSL 'https://panel.example.com/api/v1/agent/install-script?token=TOKEN' | bash -s -- \
#       --url https://panel.example.com --reg-token TOKEN --proxy-download
#
#   或下载后执行:
#   bash install.sh --url https://api.example.com --reg-token YOUR_TOKEN [OPTIONS]
#
# 下载策略:
#   默认从 GitHub Releases 直接下载 Agent 二进制。
#   指定 --proxy-download 时，通过面板代理端点 (GET /api/v1/agent/download) 下载，
#   适用于目标机器无法访问 GitHub 的场景。
#
# 用法（更新）:
#   wget -O- https://raw.githubusercontent.com/collei-monitor/collei-agent/main/install.sh | bash -s -- update
#
#   指定版本:
#   bash install.sh update --version v0.1.0
#
# 用法（卸载）:
#   wget -O- https://raw.githubusercontent.com/collei-monitor/collei-agent/main/install.sh | bash -s -- uninstall
#
#   或下载后执行:
#   bash install.sh uninstall
#
# 子命令:
#   install (默认)  安装 Agent
#   update          更新 Agent 到最新或指定版本
#   update-ca       更新 SSH CA 公钥（密钥轮换）
#   uninstall       卸载 Agent 并清理配置
# ============================================================================
set -euo pipefail

# ======================== 默认值 ========================
COLLEI_URL=""
REG_TOKEN=""
TOKEN=""
SERVER_NAME=""
INTERVAL=""
ENABLE_SSH=false
SETUP_CA=false
FORCE=false
PROXY_DOWNLOAD=false
INSTALL_DIR=""
CONFIG_DIR=""
VERSION="latest"
COMMAND="install"

GITHUB_REPO="collei-monitor/collei-agent"
BINARY_NAME="collei-agent"
CA_FILE="/etc/ssh/collei-ca.pub"
SSHD_CONFIG="/etc/ssh/sshd_config"
SSHD_MATCH_START="# BEGIN COLLEI AGENT SSH CA"
SSHD_MATCH_END="# END COLLEI AGENT SSH CA"

# ======================== 颜色输出 ========================
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }
step()  { echo -e "${BLUE}[STEP]${NC} $*"; }

# ======================== 工具函数 ========================

is_root() {
    [[ "$(id -u)" -eq 0 ]]
}

# 检测 HTTP 下载工具
detect_downloader() {
    if command -v wget &>/dev/null; then
        DOWNLOADER="wget"
    elif command -v curl &>/dev/null; then
        DOWNLOADER="curl"
    else
        error "未找到 wget 或 curl，请先安装其一"
        exit 1
    fi
}

# 通用 HTTP GET（输出到 stdout）
http_get() {
    local url="$1"
    if [[ "$DOWNLOADER" == "wget" ]]; then
        wget --timeout=15 -qO- "$url"
    else
        curl -sfL --connect-timeout 15 "$url"
    fi
}

# 通用 HTTP GET（下载到文件）
http_download() {
    local url="$1"
    local dest="$2"
    if [[ "$DOWNLOADER" == "wget" ]]; then
        wget --timeout=30 -qO "$dest" "$url"
    else
        curl -sfL --connect-timeout 30 -o "$dest" "$url"
    fi
}

# 通过面板代理下载 Agent 二进制（成功返回 0，失败返回 1）
try_proxy_download() {
    local dest="$1"
    local arch="$2"
    local panel_url="$3"
    local auth_token="$4"

    if [[ -z "$panel_url" || -z "$auth_token" ]]; then
        return 1
    fi

    local proxy_url="${panel_url}/api/v1/agent/download?token=${auth_token}&arch=${arch}"
    info "尝试通过面板代理下载..."

    if http_download "$proxy_url" "$dest" 2>/dev/null; then
        # 检查下载的文件是否有效（非空且不是 HTML 错误页）
        if [[ -s "$dest" ]] && ! head -c 20 "$dest" | grep -qi '<!doctype\|<html\|{"detail"'; then
            info "通过面板代理下载成功"
            return 0
        fi
    fi

    warn "面板代理下载失败，将回退到 GitHub 下载"
    rm -f "$dest"
    return 1
}

# 从 JSON 中提取字段值（无 jq 的备选方案）
json_extract() {
    local json="$1"
    local key="$2"
    if command -v jq &>/dev/null; then
        echo "$json" | jq -r ".$key // empty"
    else
        # grep + sed 备选方案
        echo "$json" | grep -o "\"$key\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" | sed "s/\"$key\"[[:space:]]*:[[:space:]]*\"//;s/\"$//"
    fi
}

# 检测系统架构
detect_arch() {
    local arch
    arch=$(uname -m)
    case "$arch" in
        x86_64|amd64)
            echo "amd64"
            ;;
        aarch64|arm64)
            echo "arm64"
            ;;
        *)
            error "不支持的架构: $arch（仅支持 amd64 和 arm64）"
            exit 1
            ;;
    esac
}

# 检测默认安装目录
default_install_dir() {
    if is_root; then
        echo "/usr/local/bin"
    else
        echo "$HOME/.local/bin"
    fi
}

# 检测默认配置目录
default_config_dir() {
    if is_root; then
        echo "/etc/collei-agent"
    else
        local xdg="${XDG_CONFIG_HOME:-$HOME/.config}"
        echo "$xdg/collei-agent"
    fi
}

# 检测路径是否在 $PATH 中
check_in_path() {
    local dir="$1"
    if ! echo "$PATH" | tr ':' '\n' | grep -qx "$dir"; then
        warn "$dir 不在 \$PATH 中，你可能需要手动添加："
        warn "  export PATH=\"$dir:\$PATH\""
    fi
}

backup_sshd_config() {
    local backup_file="$1"
    cp "$SSHD_CONFIG" "$backup_file"
}

validate_sshd_config() {
    if ! command -v sshd &>/dev/null; then
        error "未找到 sshd，无法校验 SSH 配置"
        return 1
    fi

    sshd -t -f "$SSHD_CONFIG"
}

reload_sshd_service() {
    if systemctl reload sshd 2>/dev/null || systemctl reload ssh 2>/dev/null; then
        info "sshd 已重载"
    else
        warn "无法重载 sshd，请手动执行: systemctl reload sshd"
    fi
}

cleanup_collei_sshd_config() {
    sed -i "\|^TrustedUserCAKeys ${CA_FILE}$|d" "$SSHD_CONFIG"

    if grep -qF "$SSHD_MATCH_START" "$SSHD_CONFIG" 2>/dev/null; then
        sed -i "/^${SSHD_MATCH_START}$/,/^${SSHD_MATCH_END}$/d" "$SSHD_CONFIG"
    fi
}

configure_sshd_ca_match() {
    local backup_file
    backup_file=$(mktemp)
    backup_sshd_config "$backup_file"

    cleanup_collei_sshd_config

    cat >> "$SSHD_CONFIG" <<EOF

${SSHD_MATCH_START}
Match Address 127.0.0.1,::1
    TrustedUserCAKeys ${CA_FILE}
${SSHD_MATCH_END}
EOF

    if ! validate_sshd_config; then
        cp "$backup_file" "$SSHD_CONFIG"
        rm -f "$backup_file"
        error "sshd 配置校验失败，已恢复原配置"
        return 1
    fi

    rm -f "$backup_file"
    return 0
}

# ======================== SSH 端口检测 ========================

detect_ssh_port() {
    local port=""

    # 方法1：解析 sshd_config
    if [[ -f /etc/ssh/sshd_config ]]; then
        port=$(grep -E '^\s*Port\s+' /etc/ssh/sshd_config 2>/dev/null | awk '{print $2}' | tail -1)
    fi

    # 方法2：ss 检测实际监听端口（需 root）
    if [[ -z "$port" ]] && is_root; then
        port=$(ss -tlnp 2>/dev/null | grep -E 'sshd|ssh' | grep -oP ':\K[0-9]+' | head -1)
    fi

    # 方法3：默认 22
    if [[ -z "$port" ]]; then
        port=22
    fi

    echo "$port"
}

# ======================== 参数解析 ========================

parse_args() {
    # 如果第一个参数是子命令
    if [[ $# -gt 0 && "$1" != --* ]]; then
        COMMAND="$1"
        shift
    fi

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --url)
                COLLEI_URL="$2"; shift 2 ;;
            --reg-token)
                REG_TOKEN="$2"; shift 2 ;;
            --token)
                TOKEN="$2"; shift 2 ;;
            --name)
                SERVER_NAME="$2"; shift 2 ;;
            --interval)
                INTERVAL="$2"; shift 2 ;;
            --enable-ssh)
                ENABLE_SSH=true; shift ;;
            --setup-ca)
                SETUP_CA=true; shift ;;
            --force)
                FORCE=true; shift ;;
            --proxy-download)
                PROXY_DOWNLOAD=true; shift ;;
            --install-dir)
                INSTALL_DIR="$2"; shift 2 ;;
            --config-dir)
                CONFIG_DIR="$2"; shift 2 ;;
            --version)
                VERSION="$2"; shift 2 ;;
            --config)
                CONFIG_FILE="$2"; shift 2 ;;

            -h|--help)
                show_help; exit 0 ;;
            *)
                error "未知参数: $1"
                show_help
                exit 1
                ;;
        esac
    done
}

show_help() {
    cat <<'EOF'
Collei Agent 一键部署脚本

用法:
  install.sh [install] [OPTIONS]   安装 Agent
  install.sh update [OPTIONS]      更新 Agent 到最新或指定版本
  install.sh update-ca [OPTIONS]   更新 SSH CA 公钥
  install.sh uninstall [OPTIONS]   卸载 Agent 并清理

update 选项:
  --version <VER>         指定版本（如 v0.0.2），默认 latest
  --install-dir <DIR>     二进制安装目录（默认自动检测）
  --proxy-download        通过面板代理下载（而非直连 GitHub）

安装选项:
  --url <URL>             控制端 API 地址（必须）
  --reg-token <TOKEN>     全局安装密钥（与 --token 二选一）
  --token <TOKEN>         专属通信 token（与 --reg-token 二选一）
  --name <NAME>           服务器显示名称（默认: 主机名）
  --interval <SEC>        上报间隔秒数（默认: 2）
  --enable-ssh            启用 Web SSH 隧道
  --setup-ca              配置 SSH CA 免密登录（需 root，需搭配 --enable-ssh）
  --force                 强制重新注册
  --proxy-download        通过面板代理下载 Agent 二进制（而非直连 GitHub）
  --install-dir <DIR>     二进制安装目录
  --config-dir <DIR>      配置文件目录
  --version <VER>         指定版本（如 v0.0.2），默认 latest

update-ca 选项:
  --config <PATH>         配置文件路径（默认自动检测）

uninstall 选项:
  --install-dir <DIR>     二进制安装目录（默认自动检测）
  --config-dir <DIR>      配置文件目录（默认自动检测）

通用:
  -h, --help              显示此帮助信息
EOF
}

# ======================== 参数校验 ========================

validate_install_args() {
    if [[ -z "$COLLEI_URL" ]]; then
        error "缺少 --url 参数"
        exit 1
    fi

    if [[ -z "$TOKEN" && -z "$REG_TOKEN" ]]; then
        error "需要提供 --token 或 --reg-token 其中之一"
        exit 1
    fi

    if [[ "$SETUP_CA" == true && "$ENABLE_SSH" == false ]]; then
        error "--setup-ca 必须搭配 --enable-ssh 使用"
        exit 1
    fi

    if [[ "$SETUP_CA" == true ]] && ! is_root; then
        error "--setup-ca 需要 root 权限"
        exit 1
    fi

    # 去掉 URL 尾部斜杠
    COLLEI_URL="${COLLEI_URL%/}"

    # 设置默认目录
    [[ -z "$INSTALL_DIR" ]] && INSTALL_DIR=$(default_install_dir)
    [[ -z "$CONFIG_DIR" ]] && CONFIG_DIR=$(default_config_dir)
}

# ======================== 下载安装二进制 ========================

download_and_install() {
    step "检测系统架构..."
    local arch
    arch=$(detect_arch)
    local asset_name="collei-agent-linux-${arch}"
    info "架构: ${arch}"

    local tmp_file
    tmp_file=$(mktemp)

    local downloaded=false

    # 仅在指定 --proxy-download 时通过面板代理下载
    if [[ "$PROXY_DOWNLOAD" == true ]]; then
        local auth_token="${REG_TOKEN:-${TOKEN:-}}"
        if try_proxy_download "$tmp_file" "$arch" "$COLLEI_URL" "$auth_token"; then
            downloaded=true
        else
            rm -f "$tmp_file"
            error "面板代理下载失败，请检查面板是否已配置 agent_url"
            exit 1
        fi
    fi

    # 默认从 GitHub 下载
    if [[ "$downloaded" == false ]]; then
        step "从 GitHub 获取下载地址..."
        local download_url

        if [[ "$VERSION" == "latest" ]]; then
            local api_url="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
            local release_info
            release_info=$(http_get "$api_url") || {
                rm -f "$tmp_file"
                error "无法访问 GitHub API，请检查网络连接"
                exit 1
            }

            download_url=$(echo "$release_info" | grep -o "\"browser_download_url\"[[:space:]]*:[[:space:]]*\"[^\"]*${asset_name}[^\"]*\"" | sed 's/"browser_download_url"[[:space:]]*:[[:space:]]*"//;s/"$//')

            if [[ -z "$download_url" ]]; then
                rm -f "$tmp_file"
                error "未找到架构 ${arch} 的发布文件"
                exit 1
            fi

            local tag
            tag=$(json_extract "$release_info" "tag_name")
            info "最新版本: ${tag}"
        else
            download_url="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}/${asset_name}"
        fi

        step "从 GitHub 下载 Agent 二进制文件..."
        info "下载地址: ${download_url}"

        http_download "$download_url" "$tmp_file" || {
            rm -f "$tmp_file"
            error "下载失败，请检查网络或版本号是否正确"
            exit 1
        }
    fi

    # 安装到目标路径 (proxy_download 和 github 两条路径都会到达此处)
    mkdir -p "$INSTALL_DIR"
    local target="${INSTALL_DIR}/${BINARY_NAME}"

    mv "$tmp_file" "$target"
    chmod +x "$target"
    info "已安装到 ${target}"

    # 检查 PATH
    check_in_path "$INSTALL_DIR"
}

# ======================== CA 信任配置 ========================

setup_ca_trust() {
    step "配置 SSH CA 信任..."

    local ca_url="${COLLEI_URL}/api/v1/clients/ssh/ca-public-key"
    local response
    response=$(http_get "$ca_url") || {
        error "无法获取 CA 公钥（${ca_url}）"
        return 1
    }

    local pub_key
    pub_key=$(json_extract "$response" "public_key")

    if [[ -z "$pub_key" ]]; then
        error "CA 公钥响应格式异常"
        return 1
    fi

    echo "$pub_key" > "$CA_FILE"
    chmod 644 "$CA_FILE"
    info "CA 公钥已写入 ${CA_FILE}"

    if configure_sshd_ca_match; then
        info "已更新 sshd 配置，仅允许 localhost 使用该 CA"
    else
        return 1
    fi

    reload_sshd_service

    return 0
}

# ======================== 生成配置文件 ========================

generate_config() {
    step "生成配置文件..."

    mkdir -p "$CONFIG_DIR"
    local config_file="${CONFIG_DIR}/agent.yaml"

    local ssh_port=22
    if [[ "$ENABLE_SSH" == true ]]; then
        step "检测 SSH 端口..."
        ssh_port=$(detect_ssh_port)
        info "检测到 SSH 端口: ${ssh_port}"
    fi

    local ca_configured=false
    if [[ "$SETUP_CA" == true ]]; then
        ca_configured=true
    fi

    # 构建 YAML 内容
    {
        echo "server_url: ${COLLEI_URL}"
        # 被动注册模式：预写 token
        if [[ -n "$TOKEN" ]]; then
            echo "token: ${TOKEN}"
        fi
        if [[ "$ENABLE_SSH" == true ]]; then
            echo "ssh:"
            echo "  enabled: true"
            echo "  port: ${ssh_port}"
            echo "  ca_configured: ${ca_configured}"
        fi
    } > "$config_file"

    chmod 600 "$config_file"
    info "配置文件已生成: ${config_file}"
}

# ======================== 创建 systemd 服务 ========================

create_systemd_service() {
    # 仅 root 且 systemd 可用时创建
    if ! is_root; then
        info "非 root 用户，跳过 systemd 服务创建"
        info "请手动启动 Agent:"
        show_start_command
        return
    fi

    if ! command -v systemctl &>/dev/null; then
        info "未检测到 systemd，跳过服务创建"
        show_start_command
        return
    fi

    step "创建 systemd 服务..."

    local service_file="/etc/systemd/system/collei-agent.service"
    local exec_cmd="${INSTALL_DIR}/${BINARY_NAME} run --config ${CONFIG_DIR}/agent.yaml"

    # 追加运行参数
    if [[ -n "$REG_TOKEN" ]]; then
        exec_cmd+=" --reg-token ${REG_TOKEN}"
    fi
    if [[ -n "$SERVER_NAME" ]]; then
        exec_cmd+=" --name ${SERVER_NAME}"
    fi
    if [[ -n "$INTERVAL" ]]; then
        exec_cmd+=" --interval ${INTERVAL}"
    fi
    # 注意: 不将 --force 写入 ExecStart
    # --force 仅用于首次安装时强制注册，不应在每次服务重启时生效
    # 否则 Agent 每次重启（含 update-ca 触发的 sshd reload 导致的间接重启）
    # 都会重新注册并创建重复的服务器记录

    cat > "$service_file" <<SERVICEEOF
[Unit]
Description=Collei Agent - Server Monitoring
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${exec_cmd}
Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
SERVICEEOF

    systemctl daemon-reload
    systemctl enable collei-agent
    systemctl start collei-agent

    info "systemd 服务已创建并启动"
    info "查看日志: journalctl -u collei-agent -f"
}

show_start_command() {
    local cmd="${INSTALL_DIR}/${BINARY_NAME} run --config ${CONFIG_DIR}/agent.yaml"
    if [[ -n "$REG_TOKEN" ]]; then
        cmd+=" --reg-token \$COLLEI_REG_TOKEN"
        info "请先设置环境变量: export COLLEI_REG_TOKEN='${REG_TOKEN}'"
    fi
    info "启动命令: ${cmd}"
}

# ======================== update-ca 子命令 ========================

do_update_ca() {
    if ! is_root; then
        error "update-ca 需要 root 权限"
        exit 1
    fi

    # 确定配置文件路径
    local config_file="${CONFIG_FILE:-}"
    if [[ -z "$config_file" ]]; then
        if [[ -f "/etc/collei-agent/agent.yaml" ]]; then
            config_file="/etc/collei-agent/agent.yaml"
        elif [[ -f "${HOME}/.config/collei-agent/agent.yaml" ]]; then
            config_file="${HOME}/.config/collei-agent/agent.yaml"
        else
            error "未找到配置文件，请使用 --config 指定路径"
            exit 1
        fi
    fi

    info "使用配置文件: ${config_file}"

    # 从 agent.yaml 读取 server_url
    local url
    url=$(grep 'server_url:' "$config_file" | awk '{print $2}')
    if [[ -z "$url" ]]; then
        error "配置文件中未找到 server_url"
        exit 1
    fi
    url="${url%/}"

    step "从服务端获取 CA 公钥..."
    local ca_url="${url}/api/v1/clients/ssh/ca-public-key"
    local response
    response=$(http_get "$ca_url") || {
        error "无法获取 CA 公钥"
        exit 1
    }

    local pub
    pub=$(json_extract "$response" "public_key")
    local old_pub
    old_pub=$(json_extract "$response" "old_public_key")

    if [[ -z "$pub" ]]; then
        error "CA 公钥响应格式异常"
        exit 1
    fi

    # 写入当前公钥（覆盖）
    echo "$pub" > "$CA_FILE"

    # 过渡期：追加旧公钥
    if [[ -n "$old_pub" ]]; then
        echo "$old_pub" >> "$CA_FILE"
        info "检测到密钥轮换过渡期，已同时写入新旧公钥"
    fi

    chmod 644 "$CA_FILE"
    info "CA 公钥已更新: ${CA_FILE}"

    if ! validate_sshd_config; then
        error "当前 sshd 配置校验失败，已停止自动重载"
        exit 1
    fi

    reload_sshd_service

    info "CA 公钥更新完成"
}

# ======================== 更新流程 ========================

do_update() {
    [[ -z "$INSTALL_DIR" ]] && INSTALL_DIR=$(default_install_dir)

    local binary="${INSTALL_DIR}/${BINARY_NAME}"

    echo ""
    echo "============================================"
    echo "      Collei Agent 更新"
    echo "============================================"
    echo ""

    # 检查当前是否已安装
    if [[ ! -f "$binary" ]]; then
        error "未找到已安装的 Agent（${binary}），请先执行 install"
        exit 1
    fi

    # 获取当前版本（使用 version 子命令，仅取第一个字段即版本号）
    local current_version=""
    if "$binary" version &>/dev/null; then
        current_version=$("$binary" version 2>/dev/null | awk 'NR==1{print $NF}')
        info "当前版本: ${current_version}"
    fi

    detect_downloader

    # 获取目标版本信息
    step "检测系统架构..."
    local arch
    arch=$(detect_arch)
    local asset_name="collei-agent-linux-${arch}"
    info "架构: ${arch}"

    step "获取下载地址..."
    local download_url=""
    local target_tag=""

    # 使用面板代理时跳过 GitHub API 查询（目标机器可能无法访问 GitHub）
    # 注意：proxy-download + latest 时无法预知目标版本号，跳过版本比对
    if [[ "$PROXY_DOWNLOAD" == true ]]; then
        target_tag=""
        if [[ "$VERSION" != "latest" ]]; then
            target_tag="$VERSION"
        fi
        info "使用面板代理下载（版本: ${VERSION})"  
    elif [[ "$VERSION" == "latest" ]]; then
        local api_url="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
        local release_info
        release_info=$(http_get "$api_url") || {
            error "无法访问 GitHub API，请检查网络连接"
            exit 1
        }

        download_url=$(echo "$release_info" | grep -o '"browser_download_url"[[:space:]]*:[[:space:]]*"[^"]*'"${asset_name}"'[^"]*"' | sed 's/"browser_download_url"[[:space:]]*:[[:space:]]*"//;s/"$//')

        if [[ -z "$download_url" ]]; then
            error "未找到架构 ${arch} 的发布文件"
            exit 1
        fi

        target_tag=$(json_extract "$release_info" "tag_name")
        info "目标版本: ${target_tag}"
    else
        target_tag="$VERSION"
        download_url="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}/${asset_name}"
        info "目标版本: ${target_tag}"
    fi

    # 检查是否与当前版本相同（target_tag 为空表示无法预知目标版本，跳过此检查）
    if [[ -n "$target_tag" && -n "$current_version" && "$current_version" == "$target_tag" ]]; then
        info "当前已是最新版本（${target_tag}），无需更新"
        return
    fi

    # 停止服务
    local service_was_running=false
    if is_root && command -v systemctl &>/dev/null; then
        if systemctl is-active collei-agent &>/dev/null; then
            step "停止 Agent 服务..."
            systemctl stop collei-agent
            service_was_running=true
            info "服务已停止"
        fi
    fi

    # 下载新版本
    step "下载新版本..."
    local tmp_file
    tmp_file=$(mktemp)

    # 从 agent.yaml 读取面板地址和 token
    local panel_url=""
    local auth_token=""
    local config_file=""
    if [[ -f "/etc/collei-agent/agent.yaml" ]]; then
        config_file="/etc/collei-agent/agent.yaml"
    elif [[ -f "${HOME}/.config/collei-agent/agent.yaml" ]]; then
        config_file="${HOME}/.config/collei-agent/agent.yaml"
    fi
    if [[ -n "$config_file" ]]; then
        panel_url=$(grep 'server_url:' "$config_file" | awk '{print $2}')
        panel_url="${panel_url%/}"
        auth_token=$(grep 'token:' "$config_file" | head -1 | awk '{print $2}')
    fi

    local downloaded=false

    # 仅在指定 --proxy-download 时通过面板代理下载
    if [[ "$PROXY_DOWNLOAD" == true ]]; then
        if try_proxy_download "$tmp_file" "$arch" "$panel_url" "$auth_token"; then
            downloaded=true
        else
            rm -f "$tmp_file"
            if [[ "$service_was_running" == true ]]; then
                warn "代理下载失败，正在恢复服务..."
                systemctl start collei-agent
            fi
            error "面板代理下载失败，请检查面板是否已配置 agent_url"
            exit 1
        fi
    fi

    if [[ "$downloaded" == false ]]; then
        http_download "$download_url" "$tmp_file" || {
            rm -f "$tmp_file"
            if [[ "$service_was_running" == true ]]; then
                warn "下载失败，正在恢复服务..."
                systemctl start collei-agent
            fi
            error "下载失败，请检查网络或版本号是否正确"
            exit 1
        }
    fi

    # 替换二进制文件
    step "替换二进制文件..."
    mv "$tmp_file" "$binary"
    chmod +x "$binary"
    info "已更新 ${binary}"

    # 重启服务
    if is_root && command -v systemctl &>/dev/null; then
        if [[ "$service_was_running" == true ]] || systemctl is-enabled collei-agent &>/dev/null; then
            step "启动 Agent 服务..."
            systemctl start collei-agent
            info "服务已启动"
        fi
    fi

    # 显示新版本
    local new_version=""
    if "$binary" version &>/dev/null; then
        new_version=$("$binary" version 2>/dev/null | awk 'NR==1{print $NF}')
    fi

    echo ""
    echo "============================================"
    if [[ -n "$new_version" ]]; then
        info "Collei Agent 已更新: ${current_version:-未知} → ${new_version}"
    else
        info "Collei Agent 更新完成！"
    fi
    echo "============================================"
    echo ""
}

# ======================== 卸载流程 ========================

do_uninstall() {
    [[ -z "$INSTALL_DIR" ]] && INSTALL_DIR=$(default_install_dir)
    [[ -z "$CONFIG_DIR" ]] && CONFIG_DIR=$(default_config_dir)

    echo ""
    echo "============================================"
    echo "      Collei Agent 卸载"
    echo "============================================"
    echo ""

    # 1. 停止并移除 systemd 服务
    if is_root && command -v systemctl &>/dev/null; then
        if systemctl list-unit-files collei-agent.service &>/dev/null; then
            step "停止 systemd 服务..."
            systemctl stop collei-agent 2>/dev/null || true
            systemctl disable collei-agent 2>/dev/null || true
            rm -f /etc/systemd/system/collei-agent.service
            systemctl daemon-reload
            info "systemd 服务已移除"
        else
            info "未发现 systemd 服务，跳过"
        fi
    else
        info "非 root 或无 systemd，跳过服务清理"
    fi

    # 2. 删除二进制文件
    local binary="${INSTALL_DIR}/${BINARY_NAME}"
    if [[ -f "$binary" ]]; then
        step "删除二进制文件..."
        rm -f "$binary"
        info "已删除 ${binary}"
    else
        info "未找到二进制文件 ${binary}，跳过"
    fi

    # 3. 删除配置目录
    if [[ -d "$CONFIG_DIR" ]]; then
        step "删除配置目录..."
        rm -rf "$CONFIG_DIR"
        info "已删除 ${CONFIG_DIR}"
    else
        info "未找到配置目录 ${CONFIG_DIR}，跳过"
    fi

    # 4. 清除 CA 配置（仅 root）
    if is_root; then
        step "清除 SSH CA 配置..."

        if [[ -f "$CA_FILE" ]]; then
            rm -f "$CA_FILE"
            info "已删除 ${CA_FILE}"
        fi

        if grep -qF "$SSHD_MATCH_START" "$SSHD_CONFIG" 2>/dev/null || grep -q "^TrustedUserCAKeys ${CA_FILE}$" "$SSHD_CONFIG" 2>/dev/null; then
            local backup_file
            backup_file=$(mktemp)
            backup_sshd_config "$backup_file"

            cleanup_collei_sshd_config

            if validate_sshd_config; then
                info "已从 sshd_config 移除 Collei CA 配置"
                reload_sshd_service
            else
                cp "$backup_file" "$SSHD_CONFIG"
                error "sshd 配置校验失败，已恢复原配置"
            fi

            rm -f "$backup_file"
        else
            info "sshd_config 中未找到 Collei CA 配置，跳过"
        fi
    fi

    echo ""
    echo "============================================"
    info "Collei Agent 卸载完成！"
    echo "============================================"
    echo ""
}

# ======================== 安装主流程 ========================

do_install() {
    validate_install_args

    echo ""
    echo "============================================"
    echo "      Collei Agent 一键部署"
    echo "============================================"
    echo ""
    info "控制端地址: ${COLLEI_URL}"
    info "安装目录:   ${INSTALL_DIR}"
    info "配置目录:   ${CONFIG_DIR}"
    info "启用 SSH:   ${ENABLE_SSH}"
    info "配置 CA:    ${SETUP_CA}"
    echo ""

    # 1. 检测下载工具
    detect_downloader

    # 2. 下载并安装二进制
    download_and_install

    # 3. CA 信任配置（可选）
    local ca_ok=false
    if [[ "$SETUP_CA" == true ]]; then
        if setup_ca_trust; then
            ca_ok=true
        else
            warn "CA 配置失败，Agent 基本功能不受影响，但 SSH 免密登录不可用"
            SETUP_CA=false
        fi
    fi

    # 4. 生成配置文件
    generate_config

    # 5. 创建 systemd 服务并启动
    create_systemd_service

    echo ""
    echo "============================================"
    info "Collei Agent 部署完成！"
    echo "============================================"
    echo ""
}

# ======================== 入口 ========================

main() {
    # 检查操作系统
    if [[ "$(uname -s)" != "Linux" ]]; then
        error "此脚本仅支持 Linux 系统"
        exit 1
    fi

    parse_args "$@"

    case "$COMMAND" in
        install)
            do_install
            ;;
        update)
            do_update
            ;;
        update-ca)
            detect_downloader
            do_update_ca
            ;;
        uninstall)
            do_uninstall
            ;;
        *)
            error "未知命令: $COMMAND"
            show_help
            exit 1
            ;;
    esac
}

main "$@"
