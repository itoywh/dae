#!/bin/sh
# luci-app-dae 日志页增强安装脚本
# 添加"清除日志"按钮，修复滚动按钮
# 用法: curl -sL https://raw.githubusercontent.com/itoywh/dae/v2.0-custom/scripts/install-luci-logpatch.sh | sh

set -e

# 检查是否在路由器上运行
if [ ! -f /www/luci-static/resources/view/dae/log.js ]; then
    echo "错误: 未找到 luci-app-dae 日志页面 (/www/luci-static/resources/view/dae/log.js)"
    echo "请在已安装 luci-app-dae 的 ImmortalWrt/OpenWrt 设备上运行"
    exit 1
fi

# 备份原始文件
cp /www/luci-static/resources/view/dae/log.js /tmp/log.js.bak
echo "已备份原始文件到 /tmp/log.js.bak"

# 写入 log.js
cat > /www/luci-static/resources/view/dae/log.js << 'LOGFILE'
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
   }';let log_textarea=E('div',{'id':'log_textarea'},E('img',{'src':L.resource('icons/loading.svg'),'alt':_('Loading...'),'style':'vertical-align:middle'},_('Collecting data…')));let logRefresh=function(){return fs.read_direct('/var/log/dae/dae.log','text').then(function(content){let log=E('pre',{'wrap':'pre'},[content.trim()||_('Log is empty.')]);dom.content(log_textarea,log);}).catch(function(e){let log;if(e.toString().includes('NotFoundError'))log=E('pre',{'wrap':'pre'},[_('Log file does not exist.')]);else log=E('pre',{'wrap':'pre'},[_('Unknown error: %s').format(e)]);dom.content(log_textarea,log);});};poll.add(L.bind(logRefresh));const scrollDownButton=E('button',{'id':'scrollDownButton','class':'cbi-button cbi-button-neutral',},_('Scroll to tail','scroll to bottom (the tail) of the log file'));scrollDownButton.addEventListener('click',function(){window.scrollTo(0,document.body.scrollHeight);});const clearLogButton=E('button',{'id':'clearLogButton','class':'cbi-button cbi-button-negative','style':'margin-left:8px',},'清除日志');clearLogButton.addEventListener('click',function(){fs.write('/var/log/dae/dae.log','').then(logRefresh).catch(function(e){console.error('Failed to clear log:',e);});});const scrollUpButton=E('button',{'id':'scrollUpButton','class':'cbi-button cbi-button-neutral',},_('Scroll to head','scroll to top (the head) of the log file'));scrollUpButton.addEventListener('click',function(){window.scrollTo(0,0);});return E([E('style',[css]),E('h2',{},[_('Log')]),E('div',{'class':'cbi-map'},[E('div',{'style':'padding-bottom:20px'},[scrollDownButton,clearLogButton]),E('div',{'class':'cbi-section'},[log_textarea,E('div',{'style':'text-align:right'},E('small',{},_('Refresh every %s seconds.').format(L.env.pollinterval)))]),E('div',{'style':'padding-bottom:20px'},[scrollUpButton])])]);},handleSaveApply:null,handleSave:null,handleReset:null});
LOGFILE

# 写入 ACL 权限文件
cat > /usr/share/rpcd/acl.d/luci-app-dae.json << 'ACLFILE'
{
	"luci-app-dae": {
		"description": "Grant access to dae configuration",
		"read": {
			"file": {
				"/etc/dae/config.dae": [ "read" ],
				"/etc/dae/example.dae": [ "read" ],
				"/var/log/dae/dae.log": [ "read" ],
				"/etc/init.d/dae hot_reload": [ "exec" ]
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
ACLFILE

# 重启 rpcd 使 ACL 生效
/etc/init.d/rpcd reload

echo ""
echo "✅ 安装完成！"
echo "   - 日志页面已添加「清除日志」按钮（红色）"
echo "   - 滚动按钮已修复（实际滚动，不再只 focus）"
echo "   - ACL 权限已更新（日志文件可写）"
echo ""
echo "请刷新 LuCI 页面 (Ctrl+Shift+R) 刷浏览器缓存"
