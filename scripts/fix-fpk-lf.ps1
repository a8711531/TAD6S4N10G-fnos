#requires -Version 5.1
# fix-fpk-lf.ps1 —— fnpack 出包后解包，把包内文本文件 CRLF→LF，再重新打包为 tad-module-lf.fpk
# 必须在 fnpack build 之后运行：fnpack 会重排 manifest 并写回 CRLF，只改源文件没用。
param(
  [string]$Fpk = "tad-module.fpk",
  [string]$Out = "tad-module-lf.fpk"
)
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
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
  Write-Host "已生成 $Out"
} finally {
  Pop-Location
}
