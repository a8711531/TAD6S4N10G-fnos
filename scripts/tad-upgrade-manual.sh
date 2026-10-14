#!/bin/sh
# tad-module 手动升级（保留全部配置与历史数据）
#
# 为什么需要这个脚本：
#   这台飞牛的「升级安装」流程（app-center 的 update/task）对本模块是空转的——
#   任务返回成功、服务也重启了，但 /usr/local/apps 里的程序文件一个都没被替换
#   （已用文件时间戳、平台记录的 manifest、以及线上页面加载的 ?v= 三方面核实）。
#   而「卸载后全新安装」（install/task）是通的——但那样会清空 @appconf 里的配置。
#
#   本脚本按平台本该做的方式就地替换程序文件：
#     · 动：/usr/local/apps/@appcenter/tad-module/（bin、ui、config、it87-src）
#           /var/apps/tad-module/（manifest、图标、cmd、wizard、config）
#     · 不动：@appconf/tad-module/config.json（按键映射、备份任务、自动备份规则）
#             @appdata/tad-module/（历史曲线数据）
#
# 用法：
#   sudo sh /vol1/1000/222/tad-upgrade.sh
#   sudo sh /vol1/1000/222/tad-upgrade.sh /别的路径/tad-module.fpk
set -eu

PKG=${1:-/vol1/1000/222/tad-module.fpk}
APPCENTER=/usr/local/apps/@appcenter/tad-module
APPMETA=/var/apps/tad-module
BAK=
STAGE=

# check_archive_members：解包前先看成员名单，拒绝绝对路径与带 .. 的成员。
# tar 以 root 解包，一旦包里混进这类成员就会写到 STAGE 之外（某些 tar / busybox
# 变体并不会替你挡掉）。
check_archive_members() {
  archive=$1
  bad=$(tar -tf "$archive" 2>/dev/null | grep -E '(^/|(^|/)\.\.(/|$))' || true)
  if [ -n "$bad" ]; then
    echo "安装包内有不安全的成员路径（绝对路径或 ..），已中止：" >&2
    printf '%s\n' "$bad" | head -5 >&2
    exit 1
  fi
}

# cleanup：无论怎么退出都要清掉临时暂存目录；只要还没走到最后一步就自动回滚，
# 免得升级失败后服务停着、程序目录换了一半，用户还不知道怎么退回去。
# 配置（@appconf）与历史数据（@appdata）全程没动过，回滚只涉及程序文件。
cleanup() {
  status=$?
  if [ -n "$STAGE" ] && [ -d "$STAGE" ]; then
    rm -rf "$STAGE"
  fi
  if [ "$status" != "0" ]; then
    if [ -n "$BAK" ] && [ -d "$BAK" ]; then
      echo "升级失败（退出码 $status），正在回滚程序目录到 $BAK …" >&2
      rm -rf "$APPCENTER" || true
      mv "$BAK" "$APPCENTER" || true
      sh "$APPMETA/cmd/main" restart >/dev/null 2>&1 || true
      echo "已回滚，配置与历史数据未受影响。" >&2
    else
      # 还没动过程序目录：至少把刚才停掉的服务起回来。
      sh "$APPMETA/cmd/main" start >/dev/null 2>&1 || true
    fi
  fi
  exit "$status"
}
trap cleanup EXIT

if [ "$(id -u)" != "0" ]; then
  echo "请用 sudo 运行： sudo sh $0" >&2
  exit 1
fi
if [ ! -f "$PKG" ]; then
  echo "找不到安装包：$PKG" >&2
  exit 1
fi
if [ ! -d "$APPCENTER" ] || [ ! -d "$APPMETA" ]; then
  echo "本机没有安装 tad-module，已中止（请走应用中心正常安装）" >&2
  exit 1
fi

echo "== 1/6 解包并校验 =="
# 固定路径 + rm -rf 在 /tmp 下有被抢符号链接的风险，用 mktemp 拿一个唯一目录，
# 退出时由 cleanup 负责清掉。
STAGE=$(mktemp -d /tmp/tad-upgrade.XXXXXX)
check_archive_members "$PKG"
tar -xf "$PKG" -C "$STAGE"
APP=$(sed -n 's/^appname[[:space:]]*=[[:space:]]*//p' "$STAGE/manifest" | head -1)
VER=$(sed -n 's/^version[[:space:]]*=[[:space:]]*//p' "$STAGE/manifest" | head -1)
if [ "$APP" != "tad-module" ]; then
  echo "包内 appname=$APP，不是 tad-module，已中止" >&2
  exit 1
fi
if [ ! -f "$STAGE/app.tgz" ]; then
  echo "包内缺少 app.tgz，已中止" >&2
  exit 1
fi
echo "    安装包：$PKG"
echo "    包版本：$VER"

echo "== 2/6 停止服务 =="
sh "$APPMETA/cmd/main" stop || true
sleep 1
rm -f "$APPCENTER/tad-module.sock"

echo "== 3/6 备份现有程序目录（可回滚）=="
# 先复制成 .partial，成功后再改名：cp 半途失败时留下的残缺目录不能让 cleanup
# 误当成可用备份拿去回滚。
PARTIAL="$APPCENTER.bak-$(date +%Y%m%d%H%M%S).partial"
cp -a "$APPCENTER" "$PARTIAL"
BAK="${PARTIAL%.partial}"
mv "$PARTIAL" "$BAK"
echo "    $BAK"

echo "== 4/6 展开新版程序文件 =="
check_archive_members "$STAGE/app.tgz"
tar -xzf "$STAGE/app.tgz" -C "$APPCENTER"
chmod +x "$APPCENTER/bin/tad-module" 2>/dev/null || true

echo "== 5/6 更新平台记录（manifest / 图标 / 生命周期脚本）=="
cp -a "$STAGE/manifest" "$STAGE/ICON.PNG" "$STAGE/ICON_256.PNG" "$APPMETA/"
rm -rf "$APPMETA/cmd" "$APPMETA/wizard"
cp -a "$STAGE/cmd" "$APPMETA/cmd"
cp -a "$STAGE/wizard" "$APPMETA/wizard"
cp -a "$STAGE/config/." "$APPMETA/config/" 2>/dev/null || true
chmod +x "$APPMETA"/cmd/* 2>/dev/null || true

echo "== 6/6 启动服务 =="
sh "$APPMETA/cmd/main" start
sleep 2
echo
echo "现装版本：$("$APPCENTER/bin/tad-module" version)"
echo "配置（未改动）：$APPMETA/etc/config.json（平台的 @appconf/tad-module/config.json）"
echo "历史数据（未改动）：$APPMETA/var/（平台的 @appdata/tad-module/）"
echo
echo "本次的程序目录备份留在：$BAK"
echo "如需手动回滚： sudo sh -c 'rm -rf $APPCENTER && mv $BAK $APPCENTER && sh $APPMETA/cmd/main restart'"
echo
echo "完成。请回飞牛桌面刷新模块页面（Windows 上 Ctrl+F5；手机 App 里退出该页面重进），"
echo "然后核对按钮控制页的按键映射、备份任务、自动备份规则是否都还在。"
