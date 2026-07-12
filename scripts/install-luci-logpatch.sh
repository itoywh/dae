#!/bin/sh
# luci-app-dae 日志页增强安装脚本
# 仅注入「清除日志」按钮（最小化 patch，不改动上游其他代码）
# 版本: v1.1.0
# 固定托管(Gist): https://gist.github.com/itoywh/3778f690647ea1636e892b003a630eae
# 用法: curl -sL https://gist.githubusercontent.com/itoywh/3778f690647ea1636e892b003a630eae/raw/install-luci-logpatch.sh | sh
#
# 原理：
#   1. log.js — 用 sed 在 scrollDownButton 后注入 clearLogButton 定义，
#      并在 DOM 返回中把 [scrollDownButton] 改为 [scrollDownButton, clearLogButton]
#   2. ACL  — 仅追加 /var/log/dae/dae.log 的写权限条目，不覆盖整个文件
#   上游更新 log.js 时只要 scrollDownButton 结构不变即可正确注入

set -e

LOG_JS="/www/luci-static/resources/view/dae/log.js"
ACL_JSON="/usr/share/rpcd/acl.d/luci-app-dae.json"

# ── 前置检查 ──────────────────────────────────────────────
if [ ! -f "$LOG_JS" ]; then
    echo "错误: 未找到 $LOG_JS"
    echo "请在已安装 luci-app-dae 的 ImmortalWrt/OpenWrt 设备上运行"
    exit 1
fi

# ── 备份 ────────────────────────────────────────────────────
cp "$LOG_JS" /tmp/log.js.bak
echo "已备份原始文件到 /tmp/log.js.bak"
[ -f "$ACL_JSON" ] && cp "$ACL_JSON" /tmp/luci-app-dae-acl.bak && echo "已备份 ACL 到 /tmp/luci-app-dae-acl.bak"

# ── 1) 注入 log.js：添加清除日志按钮 ──────────────────────
# 检查是否已经打过补丁（防止重复运行）
if grep -q 'clearLogButton' "$LOG_JS"; then
    echo "⚠️  log.js 已包含 clearLogButton，跳过注入"
else
    # 注入点 A：在 scrollDownButton 的 addEventListener 闭合后，
    #           插入 clearLogButton 定义 + 事件监听
    #
    # 匹配目标（上游原始）：
    #   scrollDownButton.addEventListener('click', () => {
    #       scrollUpButton.scrollIntoView();
    #       scrollDownButton.blur();
    #   });
    sed -i '/scrollDownButton\.addEventListener.*click/,/^        });$/{
        /^        });$/a\
\
\tconst clearLogButton = E('\''button'\'', {\
\t\t'\''id'\'': '\''clearLogButton'\'',\
\t\t'\''class'\'': '\''cbi-button cbi-button-negative'\'',\
\t\t'\''style'\'': '\''margin-left:8px'\'',\
\t}, '\''清除日志'\'');\
clearLogButton.addEventListener('\''click'\'', function() {\
\tfs.write('\''/var/log/dae/dae.log'\'', '\'''\'').then(logRefresh).catch(function(e) {\
\t\tconsole.error('\''Failed to clear log:'\'', e);\
\t});\
});
    }' "$LOG_JS"

    # 注入点 B：把 [scrollDownButton]) 改为 [scrollDownButton, clearLogButton])
    #            （让清除按钮出现在滚动到底部按钮旁边）
    sed -i 's/\[scrollDownButton]\)/[scrollDownButton, clearLogButton])/' "$LOG_JS"

    echo "✅ log.js：已注入清除日志按钮"
fi

# ── 2) 补充 ACL 写权限 ─────────────────────────────────────
if [ ! -f "$ACL_JSON" ]; then
    echo "⚠️  ACL 文件不存在 ($ACL_JSON)，跳过权限更新（可能需要手动配置）"
else
    # 检查是否已有日志写权限
    if grep -q '"/var/log/dae/dae.log".*write' "$ACL_JSON"; then
        echo "⚠️  ACL 已包含 dae.log 写权限，跳过"
    else
        # 在 write.file 段的最后一个条目后追加日志写权限
        # 匹配 "/etc/dae/config.dae": [ "write" ] 这行：
        #   ① 给匹配行补尾逗号（JSON 对象条目间需逗号分隔）
        #   ② 在其后插入 dae.log 写权限条目
        # 注意：sed 地址范围 '{...}' 内 s/a/a\ 命令顺序执行
        sed -i '\|"/etc/dae/config.dae".*"write"|{
            # 确保该行以逗号结尾（幂等：已有逗号则不重复追加）
            /,$/!s/$/,/
            # 在该行后插入新条目
            a\
\t\t\t\t"/var/log/dae/dae.log": [ "write" ]
        }' "$ACL_JSON"

        # 校验 JSON 合法性；失败则回滚备份并报错
        if command -v jsonfilter >/dev/null 2>&1; then
            if ! jsonfilter -i "$ACL_JSON" >/dev/null 2>&1; then
                echo "❌ 错误: $ACL_JSON 注入后 JSON 非法，正在回滚..."
                cp /tmp/luci-app-dae-acl.bak "$ACL_JSON"
                echo "已从备份恢复原始文件。请检查上游 ACL 格式是否发生变化"
                exit 1
            fi
        fi

        /etc/init.d/rpcd reload 2>/dev/null || true
        echo "✅ ACL：已添加 dae.log 写权限并重载 rpcd"
    fi
fi

# ── 完成 ────────────────────────────────────────────────────
echo ""
echo "✅ 安装完成！"
echo "   - 日志页面已添加「清除日志」按钮（红色，位于 Scroll to tail 旁）"
echo "   - 上游其余功能（滚动、样式、刷新）均未改动"
echo ""
echo "请刷新 LuCI 页面 (Ctrl+Shift+R) 刷浏览器缓存"
