#!/bin/sh
# luci-app-dae 日志页增强安装脚本
# 仅注入「清除日志」按钮 + ACL 写权限（最小化 patch，不改动上游其他代码）
# 版本: v1.2.0
# 固定托管(Gist): https://gist.github.com/itoywh/3778f690647ea1636e892b003a630eae
# 用法: curl -sL https://gist.githubusercontent.com/itoywh/3778f690647ea1636e892b003a630eae/raw/install-luci-logpatch.sh | sh
#
# 设计要点（针对 ImmortalWrt/OpenWrt 的 busybox sed/ash 稳健性）：
#   - A) log.js 注入：内容先写入临时文件，再用 sed 的 r 命令插入到
#     scrollDownButton 监听器闭合 }); 之后。r 是 POSIX 命令，在
#     busybox / BSD / GNU sed 上行为一致，彻底规避多行 a\ 的转义差异。
#   - B) ACL 注入：用标准 /regex/ 地址（不依赖 \| 扩展），
#     先给匹配行补尾逗号、再单行 a\ 追加 dae.log 写权限条目。
#   - A/B 各自独立幂等判定，互不干扰（避免一处已装、另一处漏装时重跑被跳过）。
#   - 备份仅首次保留原始文件，重跑不覆盖（保留可回滚的原版）。

set -e

LOG_JS="/www/luci-static/resources/view/dae/log.js"
ACL_JSON="/usr/share/rpcd/acl.d/luci-app-dae.json"
LOG_BAK="/tmp/log.js.bak"
ACL_BAK="/tmp/luci-app-dae-acl.bak"

# ── 前置检查 ──────────────────────────────────────────────
if [ ! -f "$LOG_JS" ]; then
    echo "错误: 未找到 $LOG_JS"
    echo "请在已安装 luci-app-dae 的 ImmortalWrt/OpenWrt 设备上运行"
    exit 1
fi

# ── 备份（仅首次，保留可回滚的原始版本）───────────────
if [ ! -f "$LOG_BAK" ]; then
    cp "$LOG_JS" "$LOG_BAK"
    echo "已备份原始 log.js 到 $LOG_BAK"
fi
if [ -f "$ACL_JSON" ] && [ ! -f "$ACL_BAK" ]; then
    cp "$ACL_JSON" "$ACL_BAK"
    echo "已备份原始 ACL 到 $ACL_BAK"
fi

# ── 1) 注入 log.js：添加清除日志按钮 ──────────────────────
# 独立幂等判定：已含 clearLogButton 则跳过（不依赖 ACL 状态）。
if grep -q 'clearLogButton' "$LOG_JS"; then
    echo "⚠️  log.js 已包含 clearLogButton，跳过注入"
else
    # 内容写入临时文件（避免 sed a\ 多行转义在 busybox 上的差异）。
    cat > /tmp/dae_clearbtn.js <<'JSEOF'
const clearLogButton = E('button', {
    'id': 'clearLogButton',
    'class': 'cbi-button cbi-button-negative',
    'style': 'margin-left:8px'
}, '清除日志');
clearLogButton.addEventListener('click', function() {
    fs.write('/var/log/dae/dae.log', '').then(logRefresh).catch(function(e) {
        console.error('Failed to clear log:', e);
    });
});
JSEOF
    # 锚点：scrollDownButton 的 click 监听器闭合 }); 之后插入。
    # 结束锚用 /});/（兼容上游 4/8 空格缩进）。
    sed -i -e '/scrollDownButton\.addEventListener.*click/,/});/{
        /});/r /tmp/dae_clearbtn.js
    }' "$LOG_JS"
    rm -f /tmp/dae_clearbtn.js

    # 校验注入是否真正成功（避免静默失败）
    if grep -q 'clearLogButton' "$LOG_JS"; then
        echo "✅ log.js：已注入清除日志按钮"
    else
        echo "❌ log.js 注入失败（上游 log.js 结构可能已变化），正在回滚..."
        cp "$LOG_BAK" "$LOG_JS"
        exit 1
    fi
fi

# ── 2) 补充 ACL 写权限 ───────────────────────────────────
# 独立幂等判定：已含 dae.log 写权限则跳过（不依赖 log.js 状态）。
if [ ! -f "$ACL_JSON" ]; then
    echo "⚠️  ACL 文件不存在 ($ACL_JSON)，跳过权限更新（可能需要手动配置）"
elif grep -q '"/var/log/dae/dae.log".*write' "$ACL_JSON"; then
    echo "⚠️  ACL 已包含 dae.log 写权限，跳过"
else
    # 在 write.file 段的 config.dae 条目后追加日志写权限：
    #   ① 给匹配行补尾逗号（JSON 对象条目间需逗号分隔）
    #   ② 在其后插入 dae.log 写权限条目
    sed -i -e '/"\/etc\/dae\/config.dae".*"write"/{
        s/$/,/
        a\
    "/var/log/dae/dae.log": [ "write" ]
    }' "$ACL_JSON"

    # 校验 JSON 合法性；失败则回滚备份并报错
    if command -v jsonfilter >/dev/null 2>&1; then
        if ! jsonfilter -i "$ACL_JSON" >/dev/null 2>&1; then
            echo "❌ 错误: $ACL_JSON 注入后 JSON 非法，正在回滚..."
            cp "$ACL_BAK" "$ACL_JSON"
            echo "已从备份恢复原始文件。请检查上游 ACL 格式是否发生变化"
            exit 1
        fi
    fi

    /etc/init.d/rpcd reload 2>/dev/null || true
    echo "✅ ACL：已添加 dae.log 写权限并重载 rpcd"
fi

# ── 完成 ───────────────────────────────────────────────────
echo ""
echo "✅ 安装完成！"
echo "   - 日志页面已添加「清除日志」按钮（红色，位于 Scroll to tail 旁）"
echo "   - 上游其余功能（滚动、样式、刷新）均未改动"
echo ""
echo "请刷新 LuCI 页面 (Ctrl+Shift+R) 刷浏览器缓存"
