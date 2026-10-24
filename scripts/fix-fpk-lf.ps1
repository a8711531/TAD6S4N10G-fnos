#requires -Version 5.1
# fix-fpk-lf.ps1 —— fnpack 出包后解包，把包内文本文件 CRLF→LF，再重新打包
# 必须在 fnpack build 之后运行：fnpack 会重排 manifest 并写回 CRLF，只改源文件没用。
#
# 输出命名契约（评审要求）：
#   - 默认输出名以「输入 FPK 内」的 manifest 版本为准: tad-module-lf-<包内版本>.fpk
#     ——不是工作区 manifest: 输入包可能是早前构建的旧版本, 标新版本会误发旧包
#   - 包内 manifest 缺失/版本非法: 直接失败, 不猜测
#   - 显式 -Out: 原样使用该文件名, 不做版本推断; 文件名不是内容版本的证据,
#     完成后会打印包内实际版本供核对
#   - 发布流程加 -RequireWorkspaceMatch: 包内版本必须等于工作区 manifest 版本,
#     不一致直接失败而非继续
param(
  [string]$Fpk = "tad-module.fpk",
  [string]$Out = "",
  [switch]$RequireWorkspaceMatch
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot

function Read-ManifestVersion([string]$Path) {
  if (-not (Test-Path $Path)) { return "" }
  $m = Select-String -Path $Path -Pattern '^version\s*=\s*(\S+)'
  if ($m) { return $m.Matches[0].Groups[1].Value }
  return ""
}

Push-Location $root
try {
  $tmp = Join-Path $root ".cache\fpk-lf"
  if (Test-Path $tmp) { Remove-Item -Recurse -Force $tmp }
  New-Item -ItemType Directory -Force $tmp | Out-Null
  New-Item -ItemType Directory -Force (Join-Path $tmp "app") | Out-Null

  tar -xzf $Fpk -C $tmp
  if ($LASTEXITCODE -ne 0) { throw "解包外层 fpk 失败" }
  tar -xzf (Join-Path $tmp "app.tgz") -C (Join-Path $tmp "app")
  if ($LASTEXITCODE -ne 0) { throw "解包 app.tgz 失败" }

  # 版本以输入包内 manifest 为准：缺失或非法直接拒绝，不猜测、不回退工作区版本
  $pkgVer = Read-ManifestVersion (Join-Path $tmp "manifest")
  if (-not $pkgVer) { throw "输入包内 manifest 缺失 version 字段，无法确定内容版本，拒绝继续" }
  if ($pkgVer -notmatch '^\d+\.\d+\.\d+([-+][0-9A-Za-z.-]+)?$') { throw "输入包内版本号格式非法: $pkgVer" }

  if ($RequireWorkspaceMatch) {
    $wsVer = Read-ManifestVersion (Join-Path $root "manifest")
    if (-not $wsVer) { throw "工作区 manifest 缺失 version 字段，无法校验一致性" }
    if ($wsVer -ne $pkgVer) {
      throw "版本不一致: 输入包内为 $pkgVer，工作区为 $wsVer。发布流程要求二者一致，已拒绝继续（不要用旧包标新版本发出）"
    }
  }

  $explicitOut = $PSBoundParameters.ContainsKey('Out') -and $Out
  if (-not $Out) { $Out = "tad-module-lf-$pkgVer.fpk" }

  function Convert-ToLF([string]$Path) {
    $bytes = [System.IO.File]::ReadAllBytes($Path)
    if ($bytes.Length -eq 0) { return $false }
    # 二进制判定：前 8KB 内含 NUL 即跳过（tad-module / png / jpg / gitkeep 空文件）
    $probe = [Math]::Min($bytes.Length, 8192)
    for ($i = 0; $i -lt $probe; $i++) { if ($bytes[$i] -eq 0) { return $false } }
    $text = [System.Text.Encoding]::UTF8.GetString($bytes)
    if (-not $text.Contains("`r`n")) { return $false }
    $fixed = $text -replace "`r`n", "`n"
    [System.IO.File]::WriteAllText($Path, $fixed, (New-Object System.Text.UTF8Encoding($false)))
    return $true
  }

  $converted = @()
  # 内层 app 目录全部文本文件
  Get-ChildItem -Recurse -File (Join-Path $tmp "app") | ForEach-Object {
    if (Convert-ToLF $_.FullName) { $converted += "app/" + $_.FullName.Substring((Join-Path $tmp "app").Length + 1) }
  }
  # 外层文本文件（manifest / cmd / config / wizard）
  Get-ChildItem -Recurse -File $tmp | Where-Object { $_.FullName -notlike "*\app\*" -and $_.Name -ne "app.tgz" } | ForEach-Object {
    if (Convert-ToLF $_.FullName) { $converted += $_.FullName.Substring($tmp.Length + 1) }
  }
  Write-Host ("CRLF→LF 修正 {0} 个文件:" -f $converted.Count)
  $converted | ForEach-Object { Write-Host "  $_" }

  # 重新打包内层 app.tgz（先删旧的，从 app 目录内容打包）
  Remove-Item -Force (Join-Path $tmp "app.tgz")
  Push-Location (Join-Path $tmp "app")
  try {
    tar -czf (Join-Path $tmp "app.tgz") *
    if ($LASTEXITCODE -ne 0) { throw "重打包 app.tgz 失败" }
  } finally { Pop-Location }

  # 重新打包外层 fpk：排除 app 解包目录，只打原始条目
  if (Test-Path $Out) { Remove-Item -Force $Out }
  Push-Location $tmp
  try {
    $entries = Get-ChildItem $tmp | Where-Object { $_.Name -ne "app" } | ForEach-Object { $_.Name }
    tar -czf (Join-Path $root $Out) $entries
    if ($LASTEXITCODE -ne 0) { throw "重打包外层 fpk 失败" }
  } finally { Pop-Location }
  Write-Host "已生成 $Out（包内版本 $pkgVer）"
  if ($explicitOut) {
    Write-Host "注意: -Out 只决定文件名；包内容版本以包内 manifest 为准（$pkgVer），不要把文件名当作版本证据"
  }
} finally {
  Pop-Location
}
