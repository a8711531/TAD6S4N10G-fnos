#requires -Version 5.1
# fix-fpk-lf.test.ps1 —— fix-fpk-lf.ps1 输出命名契约的离线验收（评审要求）：
# 用两个不同版本的微型本地 FPK fixture 验证：
#   1) 工作区新/输入旧 → 默认输出名取包内旧版本，不得产出标新版本的旧包
#   2) 工作区与包内一致 → 输出名正确
#   3) 包内 manifest 缺失/非法版本 → 清晰拒绝
#   4) -RequireWorkspaceMatch 不一致 → 失败而非继续；一致 → 通过
#   5) 显式 -Out → 原样使用文件名，并打印包内实际版本（文件名不作版本证据）
# 全程在临时沙箱复制脚本运行，不触碰工作区 manifest 与真实 fpk。
$ErrorActionPreference = 'Stop'
$scriptUnderTest = Join-Path $PSScriptRoot "fix-fpk-lf.ps1"
$failures = 0

function Check([bool]$Cond, [string]$Name) {
  if ($Cond) { Write-Host "  PASS $Name" } else { $script:failures++; Write-Host "  FAIL $Name" }
}

# 造微型 FPK fixture：外层 tar.gz 含 manifest + app.tgz（内层含一个 CRLF 文本文件）
function New-MiniFpk([string]$Work, [string]$Version, [string]$Dest) {
  $stage = Join-Path $Work "stage-$([Guid]::NewGuid().ToString('N'))"
  $appSrc = Join-Path $stage "app-src"
  New-Item -ItemType Directory -Force $appSrc | Out-Null
  $utf8 = New-Object System.Text.UTF8Encoding($false)
  if ($null -ne $Version) {
    [System.IO.File]::WriteAllText((Join-Path $stage "manifest"), "version=$Version`r`n", $utf8)
  } else {
    [System.IO.File]::WriteAllText((Join-Path $stage "manifest"), "appname=test`r`n", $utf8)
  }
  [System.IO.File]::WriteAllText((Join-Path $appSrc "test.txt"), "hello`r`n", $utf8)
  Push-Location $appSrc
  try { tar -czf (Join-Path $stage "app.tgz") * } finally { Pop-Location }
  Push-Location $stage
  try { tar -czf $Dest manifest app.tgz } finally { Pop-Location }
  Remove-Item -Recurse -Force $stage
}

# 独立沙箱：scripts\fix-fpk-lf.ps1（复制被测脚本）+ 工作区 manifest（按场景给版本）
function New-Sandbox([string]$Name, [string]$WsVersion) {
  $sb = Join-Path $env:TEMP ("fpk-lf-test-" + $Name + "-" + [Guid]::NewGuid().ToString("N"))
  New-Item -ItemType Directory -Force (Join-Path $sb "scripts") | Out-Null
  Copy-Item $scriptUnderTest (Join-Path $sb "scripts\fix-fpk-lf.ps1")
  if ($null -ne $WsVersion) {
    $utf8 = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText((Join-Path $sb "manifest"), "version=$WsVersion`n", $utf8)
  }
  return $sb
}

function Invoke-Fix([string]$Sb, [string[]]$Arguments) {
  # 被测脚本预期会失败退出（stderr 非空）：局部放开 Stop，stderr 行转纯文本
  $prev = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  $out = & powershell.exe -NoProfile -ExecutionPolicy Bypass -File (Join-Path $Sb "scripts\fix-fpk-lf.ps1") @Arguments 2>&1
  $code = $LASTEXITCODE
  $ErrorActionPreference = $prev
  return @{ Code = $code; Text = (($out | ForEach-Object { "$_" }) -join "`n") }
}

# 解包输出 fpk 读包内 manifest 版本（验证输出内容确实是输入包）
function Read-FpkVersion([string]$Work, [string]$FpkPath) {
  $probe = Join-Path $Work "probe-$([Guid]::NewGuid().ToString('N'))"
  New-Item -ItemType Directory -Force $probe | Out-Null
  tar -xzf $FpkPath -C $probe
  $ver = ""
  $m = Select-String -Path (Join-Path $probe "manifest") -Pattern '^version\s*=\s*(\S+)'
  if ($m) { $ver = $m.Matches[0].Groups[1].Value }
  Remove-Item -Recurse -Force $probe
  return $ver
}

Write-Host "场景1: 工作区 0.10.73 / 输入包 0.10.72 → 默认输出名必须取包内旧版本"
$sb = New-Sandbox "old-pkg" "0.10.73"
New-MiniFpk $sb "0.10.72" (Join-Path $sb "input.fpk")
$r = Invoke-Fix $sb @("-Fpk", "input.fpk")
Check ($r.Code -eq 0) "脚本执行成功"
Check (Test-Path (Join-Path $sb "tad-module-lf-0.10.72.fpk")) "输出名取包内版本 0.10.72"
Check (-not (Test-Path (Join-Path $sb "tad-module-lf-0.10.73.fpk"))) "不得产出标新版本 0.10.73 的旧包"
Check ((Read-FpkVersion $sb (Join-Path $sb "tad-module-lf-0.10.72.fpk")) -eq "0.10.72") "输出包内容与输入一致 (0.10.72)"
Remove-Item -Recurse -Force $sb

Write-Host "场景2: 工作区与包内一致 (0.10.73) → 输出名正确"
$sb = New-Sandbox "same" "0.10.73"
New-MiniFpk $sb "0.10.73" (Join-Path $sb "input.fpk")
$r = Invoke-Fix $sb @("-Fpk", "input.fpk")
Check ($r.Code -eq 0) "脚本执行成功"
Check (Test-Path (Join-Path $sb "tad-module-lf-0.10.73.fpk")) "输出名 tad-module-lf-0.10.73.fpk"
Remove-Item -Recurse -Force $sb

Write-Host "场景3: 包内 manifest 缺失 version → 清晰拒绝"
$sb = New-Sandbox "no-ver" "0.10.73"
New-MiniFpk $sb $null (Join-Path $sb "input.fpk")
$r = Invoke-Fix $sb @("-Fpk", "input.fpk")
Check ($r.Code -ne 0) "必须失败退出"
Check ($r.Text -match "缺失 version") "错误信息说明缺失版本"
Check ((Get-ChildItem $sb -Filter "tad-module-lf-*.fpk").Count -eq 0) "不得产出任何输出包"
Remove-Item -Recurse -Force $sb

Write-Host "场景4: 包内版本非法 (abc) → 清晰拒绝"
$sb = New-Sandbox "bad-ver" "0.10.73"
New-MiniFpk $sb "abc" (Join-Path $sb "input.fpk")
$r = Invoke-Fix $sb @("-Fpk", "input.fpk")
Check ($r.Code -ne 0) "必须失败退出"
Check ($r.Text -match "格式非法") "错误信息说明版本非法"
Remove-Item -Recurse -Force $sb

Write-Host "场景5: -RequireWorkspaceMatch 且版本不一致 → 失败而非继续"
$sb = New-Sandbox "mismatch" "0.10.73"
New-MiniFpk $sb "0.10.72" (Join-Path $sb "input.fpk")
$r = Invoke-Fix $sb @("-Fpk", "input.fpk", "-RequireWorkspaceMatch")
Check ($r.Code -ne 0) "必须失败退出"
Check ($r.Text -match "不一致") "错误信息说明版本不一致"
Check ((Get-ChildItem $sb -Filter "*.fpk" | Where-Object { $_.Name -ne "input.fpk" }).Count -eq 0) "不得产出输出包"
Remove-Item -Recurse -Force $sb

Write-Host "场景6: -RequireWorkspaceMatch 且版本一致 → 通过"
$sb = New-Sandbox "match" "0.10.73"
New-MiniFpk $sb "0.10.73" (Join-Path $sb "input.fpk")
$r = Invoke-Fix $sb @("-Fpk", "input.fpk", "-RequireWorkspaceMatch")
Check ($r.Code -eq 0) "脚本执行成功"
Check (Test-Path (Join-Path $sb "tad-module-lf-0.10.73.fpk")) "输出名正确"
Remove-Item -Recurse -Force $sb

Write-Host "场景7: 显式 -Out → 原样使用文件名，并打印包内实际版本"
$sb = New-Sandbox "explicit-out" "0.10.73"
New-MiniFpk $sb "0.10.72" (Join-Path $sb "input.fpk")
$r = Invoke-Fix $sb @("-Fpk", "input.fpk", "-Out", "custom-name.fpk")
Check ($r.Code -eq 0) "脚本执行成功"
Check (Test-Path (Join-Path $sb "custom-name.fpk")) "生成指定文件名"
Check ($r.Text -match "0\.10\.72") "输出提示包内实际版本 0.10.72（文件名不作版本证据）"
Remove-Item -Recurse -Force $sb

Write-Host ""
if ($failures -gt 0) { Write-Host "共 $failures 项失败"; exit 1 }
Write-Host "全部场景通过"
exit 0
