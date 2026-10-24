#requires -Version 5.1
# tad-upgrade-manual.test.ps1 —— 升级脚本「平台记录备份」的离线验收（评审要求）：
# 在临时沙箱里复制被测脚本并把安装路径改写进沙箱，用假的 cmd/main 与套接字模拟服务，
# 验证：
#   1) manifest / 图标备份失败（源文件缺失）时立即中止，退出码非 0 且明确报错
#   2) 中止发生在替换平台记录之前：cmd / wizard / 图标原样保留，不会被残缺备份顶掉
#   3) 程序目录照常回滚到升级前的内容，服务被拉回，平台记录备份的临时目录已清理
# 全程只动临时目录，不碰真实模块、不碰真实服务。
$ErrorActionPreference = 'Stop'
$scriptUnderTest = Join-Path $PSScriptRoot "tad-upgrade-manual.sh"
$failures = 0

function Check([bool]$Cond, [string]$Name) {
  if ($Cond) { Write-Host "  PASS $Name" } else { $script:failures++; Write-Host "  FAIL $Name" }
}

# 找 Git Bash 的 sh：系统目录里的 sh.exe 是 PuTTY 封装（不认这里的参数），必须避开。
function Find-Sh {
  $candidates = @(
    $env:TAD_UPGRADE_SH,
    "C:\Users\$env:USERNAME\AppData\Local\Programs\WorkBuddy\resources\vendor\PortableGit\usr\bin\sh.exe",
    "C:\Program Files\Git\usr\bin\sh.exe",
    "C:\Program Files\Git\bin\sh.exe"
  )
  foreach ($path in $candidates) {
    if ($path -and (Test-Path $path)) { return $path }
  }
  return $null
}

function Write-Lf([string]$Path, [string]$Text) {
  $utf8 = New-Object System.Text.UTF8Encoding($false)
  [System.IO.File]::WriteAllText($Path, ($Text -replace "`r`n", "`n"), $utf8)
}

# MSYS 工具只认自己的路径写法：C:\x\y → /c/x/y。
function To-MsysPath([string]$Path) {
  $p = $Path -replace '\\', '/'
  if ($p -match '^([A-Za-z]):/(.*)$') { return '/' + $matches[1].ToLower() + '/' + $matches[2] }
  return $p
}

$sh = Find-Sh
if ($null -eq $sh) {
  Write-Host "找不到 Git Bash 的 sh.exe（可用 TAD_UPGRADE_SH 指定），跳过脚本回归"
  exit 0
}
# 直接调用 sh.exe 时 MSYS 的 PATH 没建立，mktemp/cp/tar 都会找不到：把 Git 自带的
# usr\bin、mingw64\bin 加起来。
$gitRoot = Split-Path (Split-Path (Split-Path $sh))
$env:PATH = "$gitRoot\usr\bin;$gitRoot\mingw64\bin;$gitRoot\bin;$env:PATH"

$sb = Join-Path $env:TEMP ("tad-upgrade-test-" + [Guid]::NewGuid().ToString("N"))
$appcenter = Join-Path $sb "appcenter"
$appmeta = Join-Path $sb "appmeta"
$pc = To-MsysPath $appcenter
$pm = To-MsysPath $appmeta
try {
  # 升级前的现场：程序目录里放一个标记文件，末尾用它核对是否真的回滚了。
  New-Item -ItemType Directory -Force (Join-Path $appcenter "ui") | Out-Null
  New-Item -ItemType Directory -Force (Join-Path $appmeta "cmd"), (Join-Path $appmeta "wizard") | Out-Null
  Write-Lf (Join-Path $appcenter "ui\keep.txt") "original-ui"
  Write-Lf (Join-Path $appmeta "wizard\keep.txt") "original-wizard"
  Write-Lf (Join-Path $appmeta "ICON.PNG") "old-icon"
  Write-Lf (Join-Path $appmeta "ICON_256.PNG") "old-icon-256"
  # 故意不放 manifest：第 5 步的平台记录备份必然不完整，脚本必须就此中止。
  New-Item -ItemType File (Join-Path $appcenter "tad-module.sock") | Out-Null

  # 假服务：stop 删掉套接字、start/restart 重新创建，其余参数空转。
  $fakeMain = @"
#!/bin/sh
sock="$pc/tad-module.sock"
case "`$1" in
  stop) rm -f "`$sock" ;;
  start|restart) touch "`$sock" ;;
esac
exit 0
"@
  Write-Lf (Join-Path $appmeta "cmd\main") $fakeMain

  # 微型安装包（外层是普通 tar，与真实 .fpk 一致）。
  $stage = Join-Path $sb "pkgstage"
  New-Item -ItemType Directory -Force (Join-Path $stage "appsrc\bin") | Out-Null
  Write-Lf (Join-Path $stage "manifest") "appname=tad-module`nversion=9.9.9`n"
  Write-Lf (Join-Path $stage "appsrc\bin\tad-module") "#!/bin/sh`necho 9.9.9`n"
  Push-Location (Join-Path $stage "appsrc")
  try { tar -czf (To-MsysPath (Join-Path $stage "app.tgz")) bin } finally { Pop-Location }
  $pkg = To-MsysPath (Join-Path $sb "upgrade.fpk")
  Push-Location $stage
  try { tar -cf $pkg manifest app.tgz } finally { Pop-Location }

  # 沙箱副本：只改安装路径与 root 检查，脚本其余逻辑保持原样执行。
  $text = [System.IO.File]::ReadAllText($scriptUnderTest) -replace "`r`n", "`n"
  $text = $text.Replace('APPCENTER=/usr/local/apps/@appcenter/tad-module', ('APPCENTER="' + $pc + '"'))
  $text = $text.Replace('APPMETA=/var/apps/tad-module', ('APPMETA="' + $pm + '"'))
  $text = $text.Replace('if [ "$(id -u)" != "0" ]; then', 'if false; then')
  $sandboxScript = Join-Path $sb "tad-upgrade-manual.sh"
  Write-Lf $sandboxScript $text

  # 预期失败：stderr 会变成 PowerShell 错误记录，局部放开 Stop 再取退出码。
  $prev = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  $out = & $sh (To-MsysPath $sandboxScript) $pkg 2>&1
  $code = $LASTEXITCODE
  $ErrorActionPreference = $prev
  $log = (($out | ForEach-Object { "$_" }) -join "`n")

  Write-Host "== manifest 备份失败的处理 =="
  Check ($code -ne 0) "备份不完整时必须中止（退出码 $code）"
  Check ($log.Contains("备份平台记录（manifest / 图标）失败")) "错误信息明确指向平台记录备份失败"
  Check ([System.IO.File]::ReadAllText((Join-Path $appcenter "ui\keep.txt")) -eq "original-ui") "程序目录已回滚到升级前的内容"
  Check (Test-Path (Join-Path $appmeta "cmd\main")) "cmd 未被删除"
  Check (Test-Path (Join-Path $appmeta "wizard\keep.txt")) "wizard 未被删除或顶掉"
  Check (Test-Path (Join-Path $appmeta "ICON.PNG")) "图标原样保留"
  Check (-not (Test-Path (Join-Path $appmeta "manifest"))) "没有凭空生出一份 manifest"
  Check (Test-Path (Join-Path $appcenter "tad-module.sock")) "原本在跑的服务被重新拉起"
  Check (@(Get-ChildItem $sb -Filter "appmeta.bak-meta-*" -ErrorAction SilentlyContinue).Count -eq 0) "残缺的平台记录备份目录已清理"
} finally {
  Remove-Item -Recurse -Force $sb -ErrorAction SilentlyContinue
}

if ($failures -gt 0) {
  Write-Host "FAILED: $failures 项未通过"
  exit 1
}
Write-Host "全部通过"
