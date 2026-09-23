#!/usr/bin/env bash
#
# 从输入法的私有数据目录导出个人词库与剪贴板历史
# 作者: 酷安 @于乐Yule
# 版本: v1.0.6
# 时间: 2026-09-23
#

# ============================================================
#  bash 环境检查（必须早于 set 和任何 bash 特性）
# ============================================================
if [ -z "${BASH_VERSION:-}" ]; then
    printf '错误：本脚本需要 bash 运行（当前 shell 不是 bash）\n' >&2
    printf '用法: bash %s\n' "$0" >&2
    exit 1
fi
if [ "${BASH_VERSINFO[0]:-0}" -lt 4 ]; then
    printf '错误：需要 bash 4.0+（当前 %s）\n' "$BASH_VERSION" >&2
    printf '原因：脚本使用了关联数组 local -A 与 ${var^^}\n' >&2
    exit 1
fi

VERSION="v1.0.6"

set -euo pipefail

# ============================================================
#  颜色
# ============================================================
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ] && [ "${TERM:-dumb}" != "dumb" ]; then
    C_RESET=$'\033[0m'
    C_DIM=$'\033[2m'
    C_BOLD=$'\033[1m'
    C_INFO=$'\033[36m'
    C_OK=$'\033[32m'
    C_ERR=$'\033[31m'
    C_WARN=$'\033[33m'
    C_ACCENT=$'\033[35m'
else
    C_RESET=""; C_DIM=""; C_BOLD=""
    C_INFO=""; C_OK=""; C_ERR=""; C_WARN=""; C_ACCENT=""
fi

# ============================================================
#  常量
# ============================================================
WETYPE_PKGNAME="com.tencent.wetype"
BIN_NAME="gowetype-android-arm64"

GOWETYPE_DIRS=("/data/local/tmp" "/tmp" "/data/adb")
GOWETYPE_DIR=""
GOWETYPE=""

TERM_WIDTH=60
if command -v tput >/dev/null 2>&1; then
    _w="$(tput cols 2>/dev/null || echo 60)"
    [[ "$_w" =~ ^[0-9]+$ ]] && [ "$_w" -ge 30 ] && TERM_WIDTH="$_w"
fi
[ "$TERM_WIDTH" -gt 72 ] && TERM_WIDTH=72

# ============================================================
#  UI 组件
#
#  约定：可被 $() 捕获的返回值一律走 stdout；
#        所有 UI（提示、菜单、状态、告警、错误）走 stderr。
# ============================================================
hr() {
    local ch="${1:-─}" len="${2:-$TERM_WIDTH}" out=""
    local i
    for ((i = 0; i < len; i++)); do out+="$ch"; done
    printf '%s%s%s\n' "$C_DIM" "$out" "$C_RESET"
}

title() {
    local text="$1"
    echo
    printf '%s%s◈%s %s%s%s\n' "$C_DIM" "$C_ACCENT" "$C_RESET" "$C_BOLD" "$text" "$C_RESET"
    hr '─'
}

item() {
    printf '  %s%s%s  %s%-14s%s  %s%s%s\n' \
        "$C_ACCENT" "$1" "$C_RESET" \
        "$C_BOLD" "$2" "$C_RESET" \
        "$C_DIM" "$3" "$C_RESET"
}

prompt() {
    printf '%s❯%s %s' "$C_ACCENT" "$C_RESET" "$1"
}

ok()   { printf '%s✓%s %s\n' "$C_OK"   "$C_RESET" "$1"; }
err()  { printf '%s✗%s %s\n' "$C_ERR"  "$C_RESET" "$1" >&2; }
warn() { printf '%s!%s %s\n' "$C_WARN" "$C_RESET" "$1" >&2; }
info() { printf '%s·%s %s\n' "$C_INFO" "$C_RESET" "$1"; }

# 询问 y/n，返回码：0=是 1=否。UI 输出走 stderr。
ask_yes_no() {
    local q="$1" default="${2:-n}" ans="" hint
    if [ "$default" = "y" ]; then hint="[Y/n]"; else hint="[y/N]"; fi
    printf '%s❯%s %s %s: ' "$C_ACCENT" "$C_RESET" "$q" "$hint" >&2
    if ! read -r ans; then ans=""; fi
    case "${ans,,}" in
        y|yes) return 0 ;;
        n|no)  return 1 ;;
        "")    [ "$default" = "y" ] ;;
        *)     [ "$default" = "y" ] ;;
    esac
}

# ============================================================
#  辅助
# ============================================================
_resolve_self() {
    local p="$0" r=""
    if r="$(readlink -f -- "$p" 2>/dev/null)" && [ -n "$r" ]; then
        p="$r"
    elif r="$(realpath -- "$p" 2>/dev/null)" && [ -n "$r" ]; then
        p="$r"
    fi
    printf '%s\n' "$p"
}

SPATH="$(_resolve_self)"
SDIR="$(cd -- "$(dirname -- "$SPATH")" && pwd)"

trap 'echo; warn "已中断"; exit 130' INT

EXIT_SCRIPT() {
    echo
    hr '─'
    printf '%s  再见 ^_^%s\n' "$C_ACCENT" "$C_RESET"
    hr '─'
    exit 0
}

# ============================================================
#  环境检查
# ============================================================
check_env() {
    local uid=""
    uid="$(id -u 2>/dev/null || echo "")"
    if [ "$uid" = "0" ]; then
        ok "已 root"
    else
        warn "当前非 root（uid=${uid:-未知}），读取 /data 下的输入法数据可能失败"
        if command -v su >/dev/null 2>&1; then
            info "检测到 su，可 \`su -c 'sh $0'\` 重跑，或用 -data 指定拷出的数据目录"
        else
            info "未找到 su，建议把数据目录拷出来用 -data 指定"
        fi
    fi

    local installed=0
    if command -v pm >/dev/null 2>&1; then
        if pm list packages 2>/dev/null | grep -q "^package:${WETYPE_PKGNAME}$"; then
            installed=1
        fi
    fi
    if [ "$installed" -eq 0 ]; then
        local d
        for d in "/data/data/${WETYPE_PKGNAME}" "/data/user/0/${WETYPE_PKGNAME}"; do
            if [ -d "$d" ]; then installed=1; break; fi
        done
    fi
    if [ "$installed" -eq 1 ]; then
        ok "已安装微信输入法（$WETYPE_PKGNAME）"
    else
        warn "未检测到微信输入法（$WETYPE_PKGNAME）"
        info "若已安装但检测不到，可在之后用 -pkg <包名> 指定"
    fi
}

# ============================================================
#  查找二进制
# ============================================================
find_gowetype() {
    local mode="${1:-}"
    local dir

    GOWETYPE_DIR=""
    GOWETYPE=""

    case "$mode" in
        1)
            for dir in "${GOWETYPE_DIRS[@]}"; do
                if [ -d "$dir" ] && [ -w "$dir" ]; then
                    GOWETYPE_DIR="$dir"; break
                fi
            done
            ;;
        2)
            for dir in "${GOWETYPE_DIRS[@]}"; do
                if [ -f "$dir/$BIN_NAME" ]; then
                    GOWETYPE_DIR="$dir"
                    GOWETYPE="$dir/$BIN_NAME"
                    break
                fi
            done
            if [ -z "$GOWETYPE" ] && [ -f "$SDIR/$BIN_NAME" ]; then
                GOWETYPE_DIR="$SDIR"
                GOWETYPE="$SDIR/$BIN_NAME"
            fi
            ;;
        *)
            err "[find_gowetype] 传入值必须为 1 或 2"
            return 1
            ;;
    esac
    return 0
}

# ============================================================
#  菜单
# ============================================================
get_choose() {
    local uc=""
    if ! read -r uc; then
        return 1
    fi
    printf '%s\n' "$uc"
}

SHOW_MENU() {
    local args=("$@")
    local arg title func desc
    local num=1
    local -A options_map=()
    local -A desc_map=()
    local -a titles_order=()

    for arg in "${args[@]}"; do
        title="${arg%%:*}"
        local rest="${arg#*:}"
        if [[ "$rest" == *:* ]]; then
            func="${rest%%:*}"
            desc="${rest#*:}"
        else
            func="$rest"
            desc=""
        fi
        options_map["$title"]="$func"
        desc_map["$title"]="$desc"
        titles_order+=("$title")
    done

    echo
    hr '─'
    for title in "${titles_order[@]}"; do
        printf '  %s%s)%s %s%s%s' \
            "$C_ACCENT" "$num" "$C_RESET" \
            "$C_BOLD" "$title" "$C_RESET"
        if [ -n "${desc_map[$title]:-}" ]; then
            printf '  %s%s%s' "$C_DIM" "${desc_map[$title]}" "$C_RESET"
        fi
        echo
        ((num++))
    done
    hr '─'

    local choice
    prompt "选择功能 [1-$((num - 1))]: "
    choice="$(get_choose)" || return 1
    echo

    local total_items="${#titles_order[@]}"
    if [[ "$choice" =~ ^[0-9]+$ ]] && [ "$choice" -ge 1 ] && [ "$choice" -le "$total_items" ]; then
        local selected_index=$((choice - 1))
        local selected_title="${titles_order[$selected_index]}"
        local target_func="${options_map[$selected_title]}"
        if declare -f "$target_func" > /dev/null; then
            "$target_func"
        else
            err "函数 '$target_func' 未定义！"
            return 1
        fi
    else
        err "无效的选择 '$choice'，请输入 1 到 $total_items 之间的数字！"
        return 1
    fi
}

# ============================================================
#  下载
# ============================================================
DOWNLOAD_WETYPE() {
    title "下载 gowetype"

    local gowetype_github="YuleBest/gowetype"
    local latest_url="https://github.com/${gowetype_github}/releases/download/latest"

    # 询问是否走代理
    local proxy_prefix=""
    if ask_yes_no "使用 GitHub 代理 (gh.yule.best) 加速下载？" "n"; then
        proxy_prefix="https://gh.yule.best/"
        info "已启用代理: ${proxy_prefix}"
    else
        info "直连 GitHub"
    fi

    local bin_url="${proxy_prefix}${latest_url}/${BIN_NAME}"
    local hash_url="${proxy_prefix}${latest_url}/SHA256SUMS"
    local bin_path="${GOWETYPE_DIR}/${BIN_NAME}"
    local hash_path="${GOWETYPE_DIR}/SHA256SUMS"

    if [ -z "$GOWETYPE_DIR" ] || [ ! -d "$GOWETYPE_DIR" ] || [ ! -w "$GOWETYPE_DIR" ]; then
        err "找不到可写目录，请手动下载 $BIN_NAME 到 /data/local/tmp"
        return 1
    fi

    info "目标目录: $GOWETYPE_DIR"
    info "二进制 URL: $bin_url"
    info "正在下载二进制 ..."
    if ! curl --connect-timeout 10 -m 120 --retry 3 --retry-delay 2 -L \
              --progress-bar -o "$bin_path" "$bin_url"; then
        err "下载失败，请检查网络或换用代理重试"
        rm -f -- "$bin_path"
        return 1
    fi
    echo

    info "正在下载校验文件 ..."
    if ! curl --connect-timeout 10 -m 60 --retry 3 --retry-delay 2 -sSL \
              -o "$hash_path" "$hash_url"; then
        err "校验文件下载失败，请检查网络"
        rm -f -- "$bin_path" "$hash_path"
        return 1
    fi

    info "正在校验 SHA256 ..."
    if (cd -- "$GOWETYPE_DIR" && grep -- "$BIN_NAME" SHA256SUMS | sha256sum -c - >/dev/null 2>&1); then
        ok "下载并校验成功"
        rm -f -- "$hash_path"
        return 0
    else
        err "校验失败，文件可能损坏，请手动下载"
        rm -f -- "$bin_path" "$hash_path"
        return 1
    fi
}

# ============================================================
#  功能
# ============================================================
# 通过 stdout 返回答案（tsv|txt|json），UI 一律走 stderr
ask_format() {
    local default="${1:-txt}"
    local op=""
    {
        echo
        printf '  1) %sTSV%s   2) %sJSON%s   3) %sTXT%s\n' \
            "$C_BOLD" "$C_RESET" "$C_BOLD" "$C_RESET" "$C_BOLD" "$C_RESET"
        printf '%s❯%s 导出格式 [1-3，回车默认 %s]: ' \
            "$C_ACCENT" "$C_RESET" "${default^^}"
    } >&2
    if ! read -r op; then op=""; fi
    case "$op" in
        ""|3|txt|TXT)  printf 'txt\n'  ;;
        1|tsv|TSV)     printf 'tsv\n'  ;;
        2|json|JSON)   printf 'json\n' ;;
        *) warn "输入错误，使用默认 $default"; printf '%s\n' "$default" ;;
    esac
}

WORD() {
    title "导出个人词库"

    local items_num=""
    if ! items_num="$("$GOWETYPE" words -pkg "$WETYPE_PKGNAME" -length 2>/dev/null)"; then
        err "无法读取词库，请确认已 root，或使用 -data 指定数据目录"
        return 1
    fi
    ok "探测到 ${C_BOLD}${items_num}${C_RESET} 条个人词条"

    local op_fmt
    op_fmt="$(ask_format txt)"
    echo

    local out_file="${SDIR}/wetype_words.${op_fmt}"
    info "开始导出到 $out_file ..."
    if "$GOWETYPE" words -pkg "$WETYPE_PKGNAME" -format "$op_fmt" -o "$out_file"; then
        echo
        ok "提取成功: $out_file"
        return 0
    else
        echo
        err "提取失败"
        return 1
    fi
}

GREP() {
    title "筛选词库"

    local mode_choice="" keyword="" op_fmt="tsv" out_file=""

    echo
    printf '  1) %s子串匹配%s   2) %s整串相等%s   3) %s正则%s   4) %s正则(忽略大小写)%s\n' \
        "$C_BOLD" "$C_RESET" "$C_BOLD" "$C_RESET" \
        "$C_BOLD" "$C_RESET" "$C_BOLD" "$C_RESET"
    prompt "匹配方式 [1-4，回车默认 1]: "
    if ! read -r mode_choice; then mode_choice=""; fi

    prompt "要筛选的词（空格分隔多个，或关系）: "
    if ! read -r keyword; then return 1; fi
    if [ -z "$keyword" ]; then
        err "未输入关键词"
        return 1
    fi

    local -a opts=(-pkg "$WETYPE_PKGNAME")
    case "$mode_choice" in
        2) opts+=(-exact) ;;
        3) opts+=(-re) ;;
        4) opts+=(-re -i) ;;
        *) ;;
    esac

    local fmt_in
    fmt_in="$(ask_format tsv)"
    opts+=(-format "$fmt_in")

    prompt "输出到文件（回车输出到 stdout）: "
    if ! read -r out_file; then out_file=""; fi
    [ -n "$out_file" ] && opts+=(-o "$out_file")

    echo
    hr '─'
    # shellcheck disable=SC2206
    local -a patterns=($keyword)
    if "$GOWETYPE" grep "${opts[@]}" "${patterns[@]}"; then
        hr '─'
        [ -n "$out_file" ] && ok "结果已保存到 $out_file"
        return 0
    else
        hr '─'
        err "筛选失败"
        return 1
    fi
}

CLIPBOARD() {
    title "导出剪贴板历史"

    local op_fmt
    op_fmt="$(ask_format txt)"

    prompt "限制条数（回车不限制）: "
    local limit=""
    if ! read -r limit; then limit=""; fi

    local -a opts=(-pkg "$WETYPE_PKGNAME" -format "$op_fmt")
    if [ -n "$limit" ] && [[ "$limit" =~ ^[0-9]+$ ]]; then
        opts+=(-limit "$limit")
    fi

    local out_file="${SDIR}/wetype_clipboard.${op_fmt}"
    opts+=(-o "$out_file")

    echo
    info "开始导出到 $out_file ..."
    if "$GOWETYPE" clipboard "${opts[@]}"; then
        echo
        ok "提取成功: $out_file"
        return 0
    else
        echo
        err "提取失败"
        return 1
    fi
}

LIST() {
    title "列出探测到的数据库"
    hr '─'
    if "$GOWETYPE" list -pkg "$WETYPE_PKGNAME"; then
        hr '─'
        return 0
    else
        hr '─'
        err "列表失败"
        return 1
    fi
}

# ============================================================
#  欢迎
# ============================================================
WELCOME() {
    clear 2>/dev/null || true

    echo
    hr '═'
    printf '  %s%s%s微信输入法工具箱%s  %s%s%s\n' \
        "$C_BOLD" "$C_ACCENT" "" "$C_RESET" "$C_DIM" "$VERSION" "$C_RESET"
    hr '═'

    check_env
    echo
    hr '─'

    find_gowetype 2
    if [ -z "$GOWETYPE" ]; then
        warn "未找到 $BIN_NAME"
        find_gowetype 1
        if [ -z "$GOWETYPE_DIR" ]; then
            err "没有可写目录（/data/local/tmp 等），请手动放置二进制后重试"
            return 1
        fi
        echo
        info "可下载到: $GOWETYPE_DIR"
        SHOW_MENU \
            "下载 gowetype:DOWNLOAD_WETYPE:从 GitHub 下载并校验" \
            "退出:EXIT_SCRIPT:" || return 1

        find_gowetype 2
        if [ -z "$GOWETYPE" ]; then
            err "仍未找到二进制，退出"
            return 1
        fi
    fi

    chmod +x -- "$GOWETYPE" 2>/dev/null || true
    echo
    info "二进制: $GOWETYPE"
    info "目标包: $WETYPE_PKGNAME"

    SHOW_MENU \
        "导出词库:WORD:导出个人词库为 TSV/TXT/JSON" \
        "筛选词库:GREP:按词、拼音、词频筛选" \
        "导出剪贴板:CLIPBOARD:导出剪贴板历史" \
        "列出数据库:LIST:探测到的数据库列表" \
        "退出:EXIT_SCRIPT:"
}

MAIN() {
    WELCOME
}

MAIN "$@"
