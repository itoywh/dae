#!/bin/sh
# ============================================================================
# luci-app-dae LuCI 增强一键脚本（直连版 v2.2.2）
# 适用: ImmortalWrt/OpenWrt (busybox ash) + luci-app-dae
#
# 本脚本合并两个增强功能：
#   [A] 配置页新增「保存并重启」按钮（保留原「保存并应用」hot_reload 按钮不动）
#       机制: 点击 -> handleSaveApply(存 config.dae + hot_reload)
#              -> fs.exec('/etc/init.d/dae', ['restart']) 直接重启
#       说明: 直接走 rpcd exec（ACL 放行 /etc/init.d/dae restart），重启杀 dae 会短暂
#             断浏览器，故 fire-and-forget + 提示用户刷新，不依赖返回、不引入常驻进程。
#   [B] 日志页新增「清除日志」按钮（红色，位于 Scroll to tail 旁）
#   [C] 去除 init 脚本硬编码 --disable-timestamp，恢复日志 CST 时间戳
#       机制: 上游 /etc/init.d/dae 写死 --disable-timestamp，优先级高于 dae #1021
#             运行时智能抑制（#1021 逻辑：配了 --logfile -> 不走 journald ->
#             保留 CST 时间戳）；该静态 flag 强制关时间戳，导致日志无时间戳。
#       修复: sed 仅删除 --disable-timestamp 这个词（不删整行），随后
#             /etc/init.d/dae restart，dae 走 #1021 分支，
#             日志显示 CST 时间戳（如 [2026-07-15 17:22:30]）。重启/重装均持久。
#
# 统一 ACL: 两功能所需 rpcd 权限（config.dae/dae.log 读写、hot_reload/restart exec）。
#          不再需要 sentinel 写路径（v2.0.x 的 watchdog 方案已废弃）。
#
# 兼容清理: 若设备上存在 v2.0.x 遗留的 sentinel + 常驻 watchdog，自动停/禁/删。
#
# 安全: 所有被改文件先带时间戳备份; 各功能独立幂等; 末尾自校验。
# 注意: busybox 无 base64, 全程 heredoc 直写; 单引号定界符防 $ 展开。
# 托管(Gist): https://gist.github.com/itoywh/3778f690647ea1636e892b003a630eae
# 用法: curl -sL https://gist.githubusercontent.com/itoywh/3778f690647ea1636e892b003a630eae/raw/install-luci-logpatch.sh | sh
# ============================================================================
set -u
TS=$(date +%Y%m%d%H%M%S)
echo "=== luci-app-dae 增强注入（直连版 v2.2.2, 备份后缀 .bak.$TS）==="

CFG=/www/luci-static/resources/view/dae/config.js
LOGJS=/www/luci-static/resources/view/dae/log.js
ACL=/usr/share/rpcd/acl.d/luci-app-dae.json
WATCH=/usr/sbin/dae-restart-watch
INIT=/etc/init.d/dae-restart-watch
INITDAE=/etc/init.d/dae

# ── 前置检查：确认 luci-app-dae 已安装 ────────────────────────────
if [ ! -f "$CFG" ] && [ ! -f "$LOGJS" ]; then
    echo "错误: 未找到 dae 配置页/日志页 ($CFG 或 $LOGJS)"
    echo "请在已安装 luci-app-dae 的 ImmortalWrt/OpenWrt 设备上运行"
    exit 1
fi

# ── 0. 备份现有文件（每次运行都带时间戳备份，可回滚到当次运行前）──
for f in "$CFG" "$LOGJS" "$ACL" "$WATCH" "$INIT" "$INITDAE"; do
    [ -f "$f" ] && cp -a "$f" "$f.bak.$TS" && echo "backup: $f -> $f.bak.$TS"
done

# ── 0.5 清理旧版 sentinel + 常驻 watchdog（v2.0.x 遗留，过度设计，已废弃）──
if [ -f "$INIT" ] || [ -f "$WATCH" ]; then
    "$INIT" stop 2>/dev/null
    "$INIT" disable 2>/dev/null
    rm -f "$INIT" "$WATCH"
    rm -f /etc/dae/.restart-request /etc/dae/.restart-log
    echo "cleaned legacy watchdog (sentinel + /usr/sbin/dae-restart-watch)"
else
    echo "no legacy watchdog found, skip cleanup"
fi

# ── 1. 统一 rpcd ACL（精确路径/命令匹配，含 [A]+[B] 全部权限）──────
mkdir -p /usr/share/rpcd/acl.d
cat > "$ACL" <<'DAE_ACL_EOF'
{
	"luci-app-dae": {
		"description": "Grant access to dae configuration",
		"read": {
			"file": {
				"/etc/dae/config.dae": [ "read" ],
				"/etc/dae/example.dae": [ "read" ],
				"/var/log/dae/dae.log": [ "read" ],
				"/etc/init.d/dae hot_reload": [ "exec" ],
				"/etc/init.d/dae restart": [ "exec" ]
			},
			"ubus": {
				"service": [ "list" ]
			},
			"uci": [ "dae" ]
		},
		"write": {
			"file": {
				"/etc/dae/config.dae": [ "write" ],
				"/var/log/dae/dae.log": [ "write" ]
			},
			"uci": [ "dae" ]
		}
	}
}
DAE_ACL_EOF
echo "wrote ACL: $ACL"

# ── 2. [A] 配置页 config.js（新增「保存并重启」按钮, 直连 fs.exec restart）──
mkdir -p /www/luci-static/resources/view/dae
cat > "$CFG" <<'DAE_CFG_EOF'
'use strict';
'require form';
'require fs';
'require ui';
'require view';
return view.extend({
	render() {
		let m, s, o;
		let self = this;
		m = new form.Map('dae', _('Configuration'), _('Here you can edit dae configuration. It will be hot-reloaded automatically after apply.'));
		s = m.section(form.TypedSection);
		s.anonymous = true;
		s = m.section(form.NamedSection, 'config', 'dae');
		o = s.option(form.TextValue, '_configuration');
		o.rows = 30;
		o.monospace = true;
		o.load = function (section_id) {
			return fs.read_direct('/etc/dae/config.dae', 'text').then(function (content) {
				return content ?? '';
			}).catch(function (e) {
				if (e.toString().includes('NotFoundError'))
					return fs.read_direct('/etc/dae/example.dae', 'text').then(function (content) {
						return content ?? '';
					}).catch(function (e) {
						return '';
					});
				ui.addNotification(null, E('p', e.message));
				return '';
			});
		};
		o.write = function (section_id, value) {
			return fs.write('/etc/dae/config.dae', value, 384).catch(function (e) {
				ui.addNotification(null, E('p', e.message));
			});
		};
		o.remove = function (section_id, value) {
			return fs.write('/etc/dae/config.dae', '').catch(function (e) {
				ui.addNotification(null, E('p', e.message));
			});
		};

		/* 注入「保存并重启」按钮（保留原「保存并应用」reload 按钮不动）
		 * v2.1.0 直连版：点击 -> handleSaveApply(存 config.dae + hot_reload)
		 *   -> fs.exec('/etc/init.d/dae', ['restart']) 直接重启（rpcd exec ACL 已放行）
		 * 不再使用 sentinel 文件 + 常驻 watchdog（过度设计，已废弃）。
		 * 重启杀 dae 会短暂断浏览器，故 fire-and-forget + 提示用户刷新。 */
		requestAnimationFrame(function () {
			let styleEl = document.createElement('style');
			styleEl.textContent = '.cbi-dae-restart:active{transform:translateY(1px);filter:brightness(.85);box-shadow:inset 0 2px 4px rgba(0,0,0,.3);}';
			document.head.appendChild(styleEl);

			let observer = new MutationObserver(function () {
				var actions = document.querySelector('.cbi-page-actions');
				if (!actions || actions.querySelector('.cbi-dae-restart')) {
					observer.disconnect();
					return;
				}
				var btn = E('button',
					{ 'type': 'button',
					  'class': 'cbi-button cbi-button-apply cbi-dae-restart' },
					'保存并重启'
				);
				btn.addEventListener('click', function (ev) {
					ev.preventDefault();
					btn.disabled = true;
					btn.textContent = '重启中...';
					self.handleSaveApply(ev).then(function () {
						/* 直接重启：fire-and-forget（重启会断连，不依赖返回） */
						fs.exec('/etc/init.d/dae', ['restart']).catch(function () {});
						/* 自定义弹窗：消息 + 「刷新」按钮（点即 reload），无「忽略」 */
						(function(){var d=document.createElement('div');d.className='alert-message notice';d.style.cssText='position:fixed;top:60px;left:50%;transform:translateX(-50%);z-index:1000;padding:12px 16px;display:flex;gap:10px;align-items:center;box-shadow:0 2px 8px rgba(0,0,0,.15)';d.appendChild(E('span',{},_('已保存并触发重启，页面将短暂断开')));d.appendChild(E('button',{'type':'button','class':'cbi-button cbi-button-apply important','onclick':'location.reload()'},_('刷新')));document.body.appendChild(d)})();
						setTimeout(function () { btn.disabled = false; btn.textContent = '保存并重启'; }, 5000);
					}).catch(function (e) {
						console.error('dae save error:', e);
						btn.disabled = false;
						btn.textContent = '保存并重启';
					});
				});
				var saveBtn = actions.querySelector('.cbi-button-save');
				if (saveBtn) {
					actions.insertBefore(btn, saveBtn);
				} else {
					actions.appendChild(btn);
				}
				observer.disconnect();
			});
			observer.observe(document.getElementById('view'), { childList: true, subtree: true });
		});

		return m.render();
	},
	handleSaveApply(ev, mode) {
		return this.handleSave(ev).then(function () {
			return L.resolveDefault(fs.exec_direct('/etc/init.d/dae', ['hot_reload']), null);
		});
	}
});
DAE_CFG_EOF
echo "wrote config.js: $CFG"

# ── 3. [B] 日志页 log.js（新增「清除日志」按钮，整文件重写）────────
# 采用整文件重写而非 sed 锚点：原装 log.js 已压缩成单行，sed 范围注入会把
# 按钮插到 return 之后成死代码；且原装用 poll 轮询、无 logRefresh 函数。
# 安全护栏：仅当目标确是 dae 日志页（含 log_textarea + dae.log）才重写。
if [ ! -f "$LOGJS" ]; then
    echo "⚠️  未找到 $LOGJS，跳过「清除日志」按钮注入"
    LOG_STATUS="SKIP"
elif ! grep -q 'log_textarea' "$LOGJS" || ! grep -q '/var/log/dae/dae.log' "$LOGJS"; then
    echo "⚠️  $LOGJS 结构不符合预期（非 dae 日志页），跳过以免破坏"
    LOG_STATUS="SKIP"
else
    mkdir -p /www/luci-static/resources/view/dae
    cat > "$LOGJS" <<'DAE_LOGJS_EOF'
'use strict';'require dom';'require fs';'require poll';'require view';return view.extend({render:function(){let css='     \
   #log_textarea {    \
    text-align: left;  \
   }     \
   #log_textarea pre {   \
    padding: .5rem;   \
    word-break: break-all;  \
    margin: 0;   \
   }     \
   .description {    \
    background-color: #33ccff; \
   }';let log_textarea=E('div',{'id':'log_textarea'},E('img',{'src':L.resource('icons/loading.svg'),'alt':_('Loading...'),'style':'vertical-align:middle'},_('Collecting data…')));poll.add(L.bind(function(){return fs.read_direct('/var/log/dae/dae.log','text').then(function(content){let log=E('pre',{'wrap':'pre'},[content.trim()||_('Log is empty.')]);dom.content(log_textarea,log);}).catch(function(e){let log;if(e.toString().includes('NotFoundError'))
log=E('pre',{'wrap':'pre'},[_('Log file does not exist.')]);else
log=E('pre',{'wrap':'pre'},[_('Unknown error: %s').format(e)]);dom.content(log_textarea,log);});}));const scrollDownButton=E('button',{'id':'scrollDownButton','class':'cbi-button cbi-button-neutral',},_('Scroll to tail','scroll to bottom (the tail) of the log file'));scrollDownButton.addEventListener('click',()=>{scrollUpButton.focus();});const scrollUpButton=E('button',{'id':'scrollUpButton','class':'cbi-button cbi-button-neutral',},_('Scroll to head','scroll to top (the head) of the log file'));scrollUpButton.addEventListener('click',()=>{scrollDownButton.focus();});const clearLogButton=E('button',{'id':'clearLogButton','class':'cbi-button cbi-button-negative','style':'margin-left:8px'},'清除日志');clearLogButton.addEventListener('click',function(){fs.write('/var/log/dae/dae.log','').then(function(){dom.content(log_textarea,E('pre',{'wrap':'pre'},[_('Log is empty.')]));}).catch(function(e){console.error('Failed to clear log:',e);});});return E([E('style',[css]),E('h2',{},[_('Log')]),E('div',{'class':'cbi-map'},[E('div',{'style':'padding-bottom: 20px'},[scrollDownButton,clearLogButton]),E('div',{'class':'cbi-section'},[log_textarea,E('div',{'style':'text-align:right'},E('small',{},_('Refresh every %s seconds.').format(L.env.pollinterval)))]),E('div',{'style':'padding-bottom: 20px'},[scrollUpButton])])]);},handleSaveApply:null,handleSave:null,handleReset:null});
DAE_LOGJS_EOF
    if grep -q 'clearLogButton' "$LOGJS"; then
        echo "✅ log.js：已重写并加入清除日志按钮"
        LOG_STATUS="OK"
    else
        echo "❌ log.js 重写失败，回滚..."
        cp -a "$LOGJS.bak.$TS" "$LOGJS" 2>/dev/null
        LOG_STATUS="FAIL"
    fi
fi

# ── 4. 重启 rpcd（加载新 ACL）─────────────────────────────────────
/etc/init.d/rpcd restart 2>/dev/null

# ── 4.5 时间戳修复: 去除 init 脚本硬编码 --disable-timestamp ──────
# 上游 /etc/init.d/dae 写死 --disable-timestamp，优先级高于 dae #1021 运行时
# 智能抑制（#1021 逻辑：配了 --logfile -> 不走 journald -> 保留 CST 时间戳）；
# 该静态 flag 会强制关时间戳，导致日志无时间戳。去除后 dae 走 #1021 分支，
# 日志显示 CST 时间戳（如 [2026-07-15 17:22:30]）。重启/重装均持久。
if [ -f "$INITDAE" ]; then
    if grep -q 'disable-timestamp' "$INITDAE"; then
        # 只删除 --disable-timestamp 这个词，绝不删整行：
        # 某些 init 版本可能把该 flag 与其它参数写在同一行（如
        # `procd_append_param command --logfile "$X" --disable-timestamp`），删整行
        # 会误删同行参数导致日志功能受损。该文件已在 step 0 做时间戳备份。
        sed -i 's/[[:space:]]*--disable-timestamp//g' "$INITDAE"
        echo "✅ 已去除 $INITDAE 的 --disable-timestamp（时间戳将在 dae 重启后恢复）"
        # cmdline 参数变更必须 restart 而非 reload
        /etc/init.d/dae restart 2>/dev/null
        echo "dae 已重启，日志时间戳已恢复"
    else
        echo "OK  $INITDAE 无 --disable-timestamp，时间戳已正常"
    fi
else
    echo "⚠️  未找到 $INITDAE，跳过时间戳修复"
fi

# ── 5. 自校验 ─────────────────────────────────────────────────────
echo "--- 校验 ---"
for f in "$CFG" "$ACL"; do
    if [ -s "$f" ]; then echo "OK  $f ($(wc -c < "$f") bytes)"; else echo "!!! MISSING/EMPTY $f"; fi
done
if pidof dae-restart-watch >/dev/null 2>&1; then
    echo "!!! 旧 watchdog 仍在运行 (pid $(pidof dae-restart-watch)) — 请手动: $INIT stop"
else
    echo "OK  旧 watchdog 已清除（无常驻进程）"
fi
grep -q 'dae restart' "$ACL" && echo "  OK  'dae restart' exec in ACL" || echo "  !!! ACL missing restart exec"
grep -q '.restart-request' "$ACL" && echo "  !!! ACL 仍含 sentinel 残留" || echo "  OK  ACL 无 sentinel 写路径"
grep -q 'fs.exec' "$CFG" && echo "  OK  config.js 直连 fs.exec restart" || echo "  !!! config.js 未改直连"
echo "清除日志按钮: ${LOG_STATUS:-UNKNOWN}"
echo ""
echo "=== 完成 ==="
echo "浏览器硬刷新 (Cmd+Shift+R) 后："
echo "  - dae 配置页应看到「保存并重启」按钮（点击直接重启，无常驻进程）"
echo "  - dae 日志页应看到「清除日志」按钮（红色）"
echo "如需回滚: 把上面 .bak.$TS 文件复制回原名，再 /etc/init.d/rpcd restart"
