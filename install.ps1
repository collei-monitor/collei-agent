<#
.SYNOPSIS
    Collei Agent Windows 一键部署脚本

.DESCRIPTION
    下载、安装、更新或卸载 Collei Agent，并可选注册为 Windows 服务。

    管理员模式下自动注册 Windows 服务（开机自启、故障自恢复）。
    普通用户模式下安装到用户目录并提示手动启动。

.PARAMETER Command
    子命令：install（默认）、update、update-ca、uninstall

.PARAMETER Url
    控制端 API 地址（安装时必须）

.PARAMETER RegToken
    全局安装密钥（与 Token 二选一）

.PARAMETER Token
    专属通信 token（与 RegToken 二选一）

.PARAMETER Name
    服务器显示名称（默认：系统主机名）

.PARAMETER Interval
    上报间隔秒数（默认：3）

.PARAMETER EnableTerminal
    启用 ConPTY 终端直连（Web 终端）

.PARAMETER EnableFileApi
    启用文件管理 API（Web 文件管理器）

.PARAMETER Force
    强制重新注册（覆盖已有配置）

.PARAMETER NoAutoUpdate
    禁用自动版本检查更新

.PARAMETER ProxyDownload
    通过面板代理下载二进制（适用于无法访问 GitHub 的环境）

.PARAMETER InstallDir
    二进制安装目录

.PARAMETER ConfigDir
    配置文件目录

.PARAMETER Version
    指定版本号（如 v0.0.2），默认 latest

.EXAMPLE
    # 自动注册模式（管理员）
    .\install.ps1 -Url https://api.example.com -RegToken YOUR_TOKEN

.EXAMPLE
    # 被动注册模式 + 启用终端和文件管理
    .\install.ps1 -Url https://api.example.com -Token YOUR_TOKEN -EnableTerminal -EnableFileApi

.EXAMPLE
    # 更新到最新版本
    .\install.ps1 update

.EXAMPLE
    # 更新到指定版本
    .\install.ps1 update -Version v0.1.0

.EXAMPLE
    # 更新 SSH CA 公钥
    .\install.ps1 update-ca

.EXAMPLE
    # 卸载
    .\install.ps1 uninstall

.EXAMPLE
    # 一键远程安装（以管理员运行 PowerShell）
    powershell -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/collei-monitor/collei-agent/main/install.ps1 -OutFile $env:TEMP\ci.ps1; & $env:TEMP\ci.ps1 -Url 'https://api.example.com' -RegToken 'TOKEN'; Remove-Item $env:TEMP\ci.ps1"
#>

[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet("install", "update", "update-ca", "uninstall")]
    [string]$Command = "install",

    [string]$ConfigFile,

    [string]$Url,
    [string]$RegToken,
    [string]$Token,
    [string]$Name,
    [double]$Interval = 0,
    [switch]$EnableTerminal,
    [switch]$EnableFileApi,
    [switch]$Force,
    [switch]$NoAutoUpdate,
    [switch]$ProxyDownload,
    [string[]]$NicWhitelist,
    [string[]]$NicBlacklist,
    [string]$InstallDir,
    [string]$ConfigDir,
    [string]$Version = "latest"
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

# 确保 TLS 1.2+（兼容旧版 Windows）
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

# ======================== 常量 ========================
$GITHUB_REPO   = "collei-monitor/collei-agent"
$BINARY_NAME   = "collei-agent.exe"
$SERVICE_NAME  = "collei-agent"
$SERVICE_DISPLAY = "Collei Agent"
$SERVICE_DESC  = "Collei server monitoring agent"

# ======================== 颜色输出 ========================
function Write-Info  { param([string]$Msg) Write-Host "[INFO] " -ForegroundColor Green -NoNewline; Write-Host $Msg }
function Write-Warn  { param([string]$Msg) Write-Host "[WARN] " -ForegroundColor Yellow -NoNewline; Write-Host $Msg }
function Write-Err   { param([string]$Msg) Write-Host "[ERROR] " -ForegroundColor Red -NoNewline; Write-Host $Msg }
function Write-Step  { param([string]$Msg) Write-Host "[STEP] " -ForegroundColor Cyan -NoNewline; Write-Host $Msg }

# ======================== 工具函数 ========================

function Test-IsAdmin {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Get-DefaultInstallDir {
    if (Test-IsAdmin) {
        return Join-Path $env:ProgramFiles "collei-agent"
    }
    return Join-Path $env:LOCALAPPDATA "collei-agent"
}

function Get-DefaultConfigDir {
    if (Test-IsAdmin) {
        return Join-Path $env:ProgramData "collei-agent"
    }
    return Join-Path $env:APPDATA "collei-agent"
}

function Get-SystemArch {
    switch ($env:PROCESSOR_ARCHITECTURE) {
        "AMD64" { return "amd64" }
        "ARM64" { return "arm64" }
        default {
            Write-Err "不支持的架构: $env:PROCESSOR_ARCHITECTURE（目前仅支持 AMD64）"
            exit 1
        }
    }
}

function Invoke-Download {
    param([string]$Uri, [string]$Dest)
    Invoke-WebRequest -Uri $Uri -OutFile $Dest -UseBasicParsing -ErrorAction Stop
}

function Invoke-WebGet {
    param([string]$Uri)
    return (Invoke-WebRequest -Uri $Uri -UseBasicParsing -ErrorAction Stop).Content
}

function Invoke-TryProxyDownload {
    param(
        [string]$Dest,
        [string]$PanelUrl,
        [string]$AuthToken,
        [string]$DownloadUrl
    )

    if (-not $PanelUrl -or -not $AuthToken -or -not $DownloadUrl) { return $false }

    $encodedUrl = [System.Uri]::EscapeDataString($DownloadUrl)
    $proxyUrl = "$PanelUrl/api/v1/agent/download?token=$AuthToken&url=$encodedUrl"

    Write-Info "尝试通过面板代理下载..."
    Write-Info "上游 URL: $DownloadUrl"

    try {
        Invoke-Download -Uri $proxyUrl -Dest $Dest
        if ((Test-Path $Dest) -and (Get-Item $Dest).Length -gt 0) {
            $header = Get-Content $Dest -TotalCount 1 -ErrorAction SilentlyContinue
            if ($header -match '<!doctype|<html|\{"detail"') {
                Write-Warn "代理下载内容校验失败（可能为 HTML 或错误响应）"
                Remove-Item $Dest -ErrorAction SilentlyContinue
                return $false
            }
            Write-Info "通过面板代理下载成功"
            return $true
        }
    } catch {
        Write-Warn "代理下载请求失败: $_"
    }

    Remove-Item $Dest -ErrorAction SilentlyContinue
    return $false
}

function Add-ToUserPath {
    param([string]$Dir)
    $currentPath = [Environment]::GetEnvironmentVariable("PATH", "User")
    if (($currentPath -split ";" | Where-Object { $_ -eq $Dir }).Count -gt 0) { return }
    Write-Step "添加 $Dir 到用户 PATH..."
    [Environment]::SetEnvironmentVariable("PATH", "$currentPath;$Dir", "User")
    $env:PATH = "$env:PATH;$Dir"
    Write-Info "已添加到 PATH（重新打开终端后生效）"
}

function Add-ToSystemPath {
    param([string]$Dir)
    $currentPath = [Environment]::GetEnvironmentVariable("PATH", "Machine")
    if (($currentPath -split ";" | Where-Object { $_ -eq $Dir }).Count -gt 0) { return }
    Write-Step "添加 $Dir 到系统 PATH..."
    [Environment]::SetEnvironmentVariable("PATH", "$currentPath;$Dir", "Machine")
    $env:PATH = "$env:PATH;$Dir"
    Write-Info "已添加到系统 PATH（重新打开终端后生效）"
}

function Test-InPath {
    param([string]$Dir)
    if (($env:PATH -split ";" | Where-Object { $_ -eq $Dir }).Count -gt 0) { return $true }
    $machinePath = [Environment]::GetEnvironmentVariable("PATH", "Machine")
    if ($machinePath -and ($machinePath -split ";" | Where-Object { $_ -eq $Dir }).Count -gt 0) { return $true }
    $userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
    if ($userPath -and ($userPath -split ";" | Where-Object { $_ -eq $Dir }).Count -gt 0) { return $true }
    return $false
}

function Stop-AgentService {
    $svc = Get-Service -Name $SERVICE_NAME -ErrorAction SilentlyContinue
    if ($svc -and $svc.Status -eq "Running") {
        Write-Step "停止服务..."
        Stop-Service -Name $SERVICE_NAME -Force -ErrorAction SilentlyContinue
        # 等待进程退出以释放文件锁
        Start-Sleep -Seconds 2
        Write-Info "服务已停止"
    }
}

function Get-TempFilePath {
    param([string]$Prefix = "collei")
    $random = [guid]::NewGuid().ToString('N').Substring(0, 8)
    return Join-Path $env:TEMP "${Prefix}-${random}.exe"
}

# ======================== 参数校验 ========================

function Assert-InstallArgs {
    if (-not $script:Url) {
        Write-Err "缺少 -Url 参数"
        exit 1
    }
    if (-not $script:Token -and -not $script:RegToken) {
        Write-Err "需要提供 -Token 或 -RegToken 其中之一"
        exit 1
    }
    if ($script:Token -and $script:RegToken) {
        Write-Err "-Token 和 -RegToken 不能同时指定"
        exit 1
    }

    # 去掉 URL 尾部斜杠
    $script:Url = $script:Url.TrimEnd("/")

    # 设置默认目录
    if (-not $script:InstallDir) { $script:InstallDir = Get-DefaultInstallDir }
    if (-not $script:ConfigDir)  { $script:ConfigDir  = Get-DefaultConfigDir }
}

# ======================== 下载安装二进制 ========================

function Install-AgentBinary {
    Write-Step "检测系统架构..."
    $arch = Get-SystemArch
    $assetName = "collei-agent-windows-${arch}.exe"
    Write-Info "架构: $arch"

    $tmpFile = Get-TempFilePath -Prefix "collei-download"
    $downloaded = $false

    # 仅在指定 -ProxyDownload 时通过面板代理下载
    if ($script:ProxyDownload) {
        $authToken = if ($script:RegToken) { $script:RegToken } else { $script:Token }
        if ($script:Version -eq "latest") {
            $proxyDownloadUrl = "https://github.com/$GITHUB_REPO/releases/latest/download/$assetName"
        } else {
            $proxyDownloadUrl = "https://github.com/$GITHUB_REPO/releases/download/$($script:Version)/$assetName"
        }
        if (Invoke-TryProxyDownload -Dest $tmpFile -PanelUrl $script:Url -AuthToken $authToken -DownloadUrl $proxyDownloadUrl) {
            $downloaded = $true
        } else {
            Remove-Item $tmpFile -ErrorAction SilentlyContinue
            Write-Err "面板代理下载失败"
            exit 1
        }
    }

    # 默认从 GitHub 下载
    if (-not $downloaded) {
        Write-Step "从 GitHub 获取下载地址..."

        if ($script:Version -eq "latest") {
            $apiUrl = "https://api.github.com/repos/$GITHUB_REPO/releases/latest"
            try {
                $releaseJson = Invoke-WebGet -Uri $apiUrl
                $release = ConvertFrom-Json $releaseJson
            } catch {
                Remove-Item $tmpFile -ErrorAction SilentlyContinue
                Write-Err "无法访问 GitHub API，请检查网络连接: $_"
                exit 1
            }

            $asset = $release.assets | Where-Object { $_.name -eq $assetName } | Select-Object -First 1
            if (-not $asset) {
                Remove-Item $tmpFile -ErrorAction SilentlyContinue
                Write-Err "未找到架构 ${arch} 的发布文件"
                exit 1
            }

            $downloadUrl = $asset.browser_download_url
            Write-Info "最新版本: $($release.tag_name)"
        } else {
            $downloadUrl = "https://github.com/$GITHUB_REPO/releases/download/$($script:Version)/$assetName"
            Write-Info "目标版本: $($script:Version)"
        }

        Write-Step "下载 Agent 二进制文件..."
        Write-Info "下载地址: $downloadUrl"

        try {
            Invoke-Download -Uri $downloadUrl -Dest $tmpFile
        } catch {
            Remove-Item $tmpFile -ErrorAction SilentlyContinue
            Write-Err "下载失败，请检查网络或版本号是否正确: $_"
            exit 1
        }
    }

    # 安装到目标路径
    if (-not (Test-Path $script:InstallDir)) {
        New-Item -ItemType Directory -Path $script:InstallDir -Force | Out-Null
    }

    $target = Join-Path $script:InstallDir $BINARY_NAME

    # 如果目标文件存在且被锁定（服务运行中），先停止服务
    if (Test-Path $target) {
        try {
            Move-Item $tmpFile $target -Force
        } catch {
            Write-Warn "文件被占用，尝试停止服务后重试..."
            Stop-AgentService
            Start-Sleep -Seconds 1
            Move-Item $tmpFile $target -Force
        }
    } else {
        Move-Item $tmpFile $target -Force
    }

    Write-Info "已安装到 $target"

    # 检查 PATH
    if (-not (Test-InPath $script:InstallDir)) {
        if (Test-IsAdmin) {
            Add-ToSystemPath $script:InstallDir
        } else {
            Add-ToUserPath $script:InstallDir
        }
    }
}

# ======================== 生成配置文件 ========================

function New-AgentConfig {
    Write-Step "生成配置文件..."

    if (-not (Test-Path $script:ConfigDir)) {
        New-Item -ItemType Directory -Path $script:ConfigDir -Force | Out-Null
    }

    $configFile = Join-Path $script:ConfigDir "agent.yaml"

    $lines = @()
    $lines += "server_url: $($script:Url)"

    # 被动注册模式：预写 token
    if ($script:Token) {
        $lines += "token: $($script:Token)"
    }

    # 终端配置
    if ($script:EnableTerminal) {
        $lines += "terminal:"
        $lines += "  enabled: true"
    }

    # 文件 API 配置
    if ($script:EnableFileApi) {
        $lines += "file_api:"
        $lines += "  enabled: true"
    }

    # 自动更新
    if ($script:NoAutoUpdate) {
        $lines += "auto_update: false"
    }

    # 网卡过滤
    if ($script:NicWhitelist -or $script:NicBlacklist) {
        $lines += "nic_filter:"
        if ($script:NicWhitelist) {
            $lines += "  whitelist:"
            foreach ($p in $script:NicWhitelist) {
                $p = $p.Trim()
                if ($p) { $lines += "  - `"$p`"" }
            }
        }
        if ($script:NicBlacklist) {
            $lines += "  blacklist:"
            foreach ($p in $script:NicBlacklist) {
                $p = $p.Trim()
                if ($p) { $lines += "  - `"$p`"" }
            }
        }
    }

    # 写入 UTF-8 无 BOM
    $content = $lines -join "`n"
    [System.IO.File]::WriteAllText($configFile, $content, [System.Text.UTF8Encoding]::new($false))

    # 限制配置文件权限（仅管理员和 SYSTEM 可访问）
    if (Test-IsAdmin) {
        icacls $configFile /inheritance:r /grant:r "BUILTIN\Administrators:(F)" "NT AUTHORITY\SYSTEM:(F)" 2>$null | Out-Null
    }

    Write-Info "配置文件已生成: $configFile"
}

# ======================== Windows 服务管理 ========================

function Register-AgentService {
    if (-not (Test-IsAdmin)) {
        Write-Info "非管理员模式，跳过 Windows 服务创建"
        Show-StartCommand
        return
    }

    Write-Step "注册 Windows 服务..."

    $binaryPath = Join-Path $script:InstallDir $BINARY_NAME
    $configPath = Join-Path $script:ConfigDir "agent.yaml"

    # 构建服务命令行
    $binPathArg = "`"$binaryPath`" run --config `"$configPath`""

    if ($script:RegToken) {
        $binPathArg += " --reg-token `"$($script:RegToken)`""
    }
    if ($script:Name) {
        $binPathArg += " --name `"$($script:Name)`""
    }
    if ($script:Interval -gt 0) {
        $binPathArg += " --interval $($script:Interval)"
    }
    # 注意：不将 --force 写入服务命令行
    # --force 仅用于首次安装时强制注册，不应在每次服务重启时生效

    # 检查是否已存在同名服务
    $existingService = Get-Service -Name $SERVICE_NAME -ErrorAction SilentlyContinue
    if ($existingService) {
        Write-Warn "检测到已有服务，将重新配置..."
        Stop-AgentService
        sc.exe delete $SERVICE_NAME 2>$null | Out-Null
        Start-Sleep -Seconds 2
    }

    # 创建服务
    New-Service -Name $SERVICE_NAME `
        -BinaryPathName $binPathArg `
        -DisplayName $SERVICE_DISPLAY `
        -Description $SERVICE_DESC `
        -StartupType Automatic | Out-Null

    # 配置恢复策略：5s / 10s / 30s 后重启，重置周期 1 天
    sc.exe failure $SERVICE_NAME reset= 86400 actions= restart/5000/restart/10000/restart/30000 2>$null | Out-Null

    # 启动服务
    Write-Step "启动服务..."
    Start-Service -Name $SERVICE_NAME

    Write-Info "Windows 服务已创建并启动"
    Write-Info "查看状态: Get-Service $SERVICE_NAME"
    Write-Info "查看日志: Get-Content `"$env:ProgramData\collei-agent\agent.log`" -Encoding UTF8 -Tail 50 -Wait"
}

function Show-StartCommand {
    $binaryPath = Join-Path $script:InstallDir $BINARY_NAME
    $configPath = Join-Path $script:ConfigDir "agent.yaml"

    $cmd = "& `"$binaryPath`" run --config `"$configPath`""
    if ($script:RegToken) {
        $cmd += " --reg-token `"$($script:RegToken)`""
    }
    if ($script:Name) {
        $cmd += " --name `"$($script:Name)`""
    }
    if ($script:Interval -gt 0) {
        $cmd += " --interval $($script:Interval)"
    }

    Write-Info "请手动启动 Agent："
    Write-Host "  $cmd" -ForegroundColor Cyan
    Write-Host ""
    Write-Info "或以管理员身份重新运行此脚本以注册为 Windows 服务"
}

# ======================== 安装主流程 ========================

function Invoke-Install {
    Assert-InstallArgs

    Write-Host ""
    Write-Host "============================================"
    Write-Host "      Collei Agent Windows 一键部署"
    Write-Host "============================================"
    Write-Host ""
    Write-Info "控制端地址: $($script:Url)"
    Write-Info "安装目录:   $($script:InstallDir)"
    Write-Info "配置目录:   $($script:ConfigDir)"
    Write-Info "管理员模式: $(if (Test-IsAdmin) { '是（将注册 Windows 服务）' } else { '否' })"
    Write-Info "终端直连:   $(if ($script:EnableTerminal) { '启用' } else { '未启用' })"
    Write-Info "文件管理:   $(if ($script:EnableFileApi) { '启用' } else { '未启用' })"
    Write-Host ""

    # 1. 下载并安装二进制
    Install-AgentBinary

    # 2. 生成配置文件
    New-AgentConfig

    # 3. 注册服务或显示启动命令
    Register-AgentService

    Write-Host ""
    Write-Host "============================================"
    Write-Info "Collei Agent 部署完成！"
    Write-Host "============================================"
    Write-Host ""
}

# ======================== 更新流程 ========================

function Invoke-Update {
    if (-not $script:InstallDir) { $script:InstallDir = Get-DefaultInstallDir }

    $binaryPath = Join-Path $script:InstallDir $BINARY_NAME

    Write-Host ""
    Write-Host "============================================"
    Write-Host "      Collei Agent Windows 更新"
    Write-Host "============================================"
    Write-Host ""

    # 检查当前是否已安装
    if (-not (Test-Path $binaryPath)) {
        Write-Err "未找到已安装的 Agent（$binaryPath），请先执行 install"
        exit 1
    }

    # 获取当前版本
    $currentVersion = ""
    try {
        $versionOutput = (& $binaryPath version 2>$null) | Select-Object -First 1
        if ($versionOutput -match 'Collei Agent (\S+)') {
            $currentVersion = $Matches[1]
        }
        Write-Info "当前版本: $currentVersion"
    } catch { }

    # 检测架构
    Write-Step "检测系统架构..."
    $arch = Get-SystemArch
    $assetName = "collei-agent-windows-${arch}.exe"
    Write-Info "架构: $arch"

    $targetTag = ""
    $downloadUrl = ""

    # 面板代理模式
    if ($script:ProxyDownload) {
        if ($script:Version -ne "latest") { $targetTag = $script:Version }
        Write-Info "使用面板代理下载（版本: $($script:Version)）"
    }
    elseif ($script:Version -eq "latest") {
        Write-Step "从 GitHub 获取最新版本..."
        $apiUrl = "https://api.github.com/repos/$GITHUB_REPO/releases/latest"
        try {
            $releaseJson = Invoke-WebGet -Uri $apiUrl
            $release = ConvertFrom-Json $releaseJson
        } catch {
            Write-Err "无法访问 GitHub API: $_"
            exit 1
        }

        $asset = $release.assets | Where-Object { $_.name -eq $assetName } | Select-Object -First 1
        if (-not $asset) {
            Write-Err "未找到架构 ${arch} 的发布文件"
            exit 1
        }

        $downloadUrl = $asset.browser_download_url
        $targetTag = $release.tag_name
        Write-Info "目标版本: $targetTag"
    } else {
        $targetTag = $script:Version
        $downloadUrl = "https://github.com/$GITHUB_REPO/releases/download/$($script:Version)/$assetName"
        Write-Info "目标版本: $targetTag"
    }

    # 检查是否与当前版本相同
    if ($targetTag -and $currentVersion -and $currentVersion -eq $targetTag) {
        Write-Info "当前已是最新版本（$targetTag），无需更新"
        return
    }

    # 下载新版本
    Write-Step "下载新版本..."
    $tmpFile = Get-TempFilePath -Prefix "collei-update"

    # 从 agent.yaml 读取面板地址和 token（用于代理下载）
    $panelUrl  = ""
    $authToken = ""
    $configFile = ""

    foreach ($candidate in @(
        (Join-Path $env:ProgramData "collei-agent\agent.yaml"),
        (Join-Path $env:APPDATA "collei-agent\agent.yaml")
    )) {
        if (Test-Path $candidate) {
            $configFile = $candidate
            break
        }
    }

    if ($configFile) {
        $cfgContent = Get-Content $configFile -Raw -ErrorAction SilentlyContinue
        if ($cfgContent -match '(?m)^server_url:\s*(\S+)') { $panelUrl  = $Matches[1].TrimEnd("/") }
        if ($cfgContent -match '(?m)^token:\s*(\S+)')      { $authToken = $Matches[1] }
    }

    $downloaded = $false

    if ($script:ProxyDownload) {
        if ($script:Version -eq "latest") {
            $proxyDlUrl = "https://github.com/$GITHUB_REPO/releases/latest/download/$assetName"
        } else {
            $proxyDlUrl = "https://github.com/$GITHUB_REPO/releases/download/$($script:Version)/$assetName"
        }
        if (Invoke-TryProxyDownload -Dest $tmpFile -PanelUrl $panelUrl -AuthToken $authToken -DownloadUrl $proxyDlUrl) {
            $downloaded = $true
        } else {
            Remove-Item $tmpFile -ErrorAction SilentlyContinue
            Write-Err "面板代理下载失败"
            exit 1
        }
    }

    if (-not $downloaded) {
        try {
            Invoke-Download -Uri $downloadUrl -Dest $tmpFile
        } catch {
            Remove-Item $tmpFile -ErrorAction SilentlyContinue
            Write-Err "下载失败: $_"
            exit 1
        }
    }

    # 停止服务
    $serviceWasRunning = $false
    if (Test-IsAdmin) {
        $svc = Get-Service -Name $SERVICE_NAME -ErrorAction SilentlyContinue
        if ($svc -and $svc.Status -eq "Running") {
            Stop-AgentService
            $serviceWasRunning = $true
        }
    }

    # 替换二进制
    Write-Step "替换二进制文件..."
    try {
        Move-Item $tmpFile $binaryPath -Force
    } catch {
        Remove-Item $tmpFile -ErrorAction SilentlyContinue
        if ($serviceWasRunning) {
            Write-Warn "替换失败，正在恢复服务..."
            Start-Service -Name $SERVICE_NAME -ErrorAction SilentlyContinue
        }
        Write-Err "替换二进制文件失败: $_"
        exit 1
    }
    Write-Info "已更新 $binaryPath"

    # 重启服务
    if (Test-IsAdmin) {
        $svc = Get-Service -Name $SERVICE_NAME -ErrorAction SilentlyContinue
        if ($serviceWasRunning -or ($svc -and $svc.StartType -eq "Automatic")) {
            Write-Step "启动服务..."
            Start-Service -Name $SERVICE_NAME
            Write-Info "服务已启动"
        }
    }

    # 显示新版本
    $newVersion = ""
    try {
        $versionOutput = (& $binaryPath version 2>$null) | Select-Object -First 1
        if ($versionOutput -match 'Collei Agent (\S+)') { $newVersion = $Matches[1] }
    } catch { }

    Write-Host ""
    Write-Host "============================================"
    if ($newVersion) {
        Write-Info "Collei Agent 已更新: $(if ($currentVersion) { $currentVersion } else { '未知' }) -> $newVersion"
    } else {
        Write-Info "Collei Agent 更新完成！"
    }
    Write-Host "============================================"
    Write-Host ""
}

# ======================== update-ca 流程 ========================

function Invoke-UpdateCA {
    if (-not (Test-IsAdmin)) {
        Write-Err "update-ca 需要管理员权限"
        exit 1
    }

    Write-Host ""
    Write-Host "============================================"
    Write-Host "      Collei Agent CA 公钥更新"
    Write-Host "============================================"
    Write-Host ""

    # 确定配置文件路径
    $cfgFile = $script:ConfigFile
    if (-not $cfgFile) {
        foreach ($candidate in @(
            (Join-Path $env:ProgramData "collei-agent\agent.yaml"),
            (Join-Path $env:APPDATA "collei-agent\agent.yaml")
        )) {
            if (Test-Path $candidate) {
                $cfgFile = $candidate
                break
            }
        }
    }
    if (-not $cfgFile -or -not (Test-Path $cfgFile)) {
        Write-Err "未找到配置文件，请使用 -ConfigFile 指定路径"
        exit 1
    }
    Write-Info "使用配置文件: $cfgFile"

    # 从 agent.yaml 读取 server_url
    $cfgContent = Get-Content $cfgFile -Raw -ErrorAction Stop
    $serverUrl = ""
    if ($cfgContent -match '(?m)^server_url:\s*(\S+)') {
        $serverUrl = $Matches[1].TrimEnd("/")
    }
    if (-not $serverUrl) {
        Write-Err "配置文件中未找到 server_url"
        exit 1
    }

    # 确定 CA 公钥路径
    $configDir = Split-Path $cfgFile -Parent
    $caFile = Join-Path $configDir "ca.pub"

    # 从服务端获取 CA 公钥
    $caApiUrl = "$serverUrl/api/v1/clients/ssh/ca-public-key"
    Write-Step "从服务端获取 CA 公钥..."
    try {
        $responseText = Invoke-WebGet -Uri $caApiUrl
        $response = ConvertFrom-Json $responseText
    } catch {
        Write-Err "无法获取 CA 公钥: $_"
        exit 1
    }

    $pubKey = $response.public_key
    $oldPubKey = $response.old_public_key

    if (-not $pubKey) {
        Write-Err "CA 公钥响应格式异常"
        exit 1
    }

    # 写入公钥文件
    $caContent = $pubKey
    if ($oldPubKey) {
        $caContent += "`n$oldPubKey"
        Write-Info "检测到密钥轮换过渡期，已同时写入新旧公钥"
    }
    [System.IO.File]::WriteAllText($caFile, $caContent, [System.Text.UTF8Encoding]::new($false))
    Write-Info "CA 公钥已更新: $caFile"

    Write-Host ""
    Write-Host "============================================"
    Write-Info "CA 公钥更新完成！"
    Write-Host "============================================"
    Write-Host ""
}

# ======================== 卸载流程 ========================

function Invoke-Uninstall {
    if (-not $script:InstallDir) { $script:InstallDir = Get-DefaultInstallDir }
    if (-not $script:ConfigDir)  { $script:ConfigDir  = Get-DefaultConfigDir }

    Write-Host ""
    Write-Host "============================================"
    Write-Host "      Collei Agent Windows 卸载"
    Write-Host "============================================"
    Write-Host ""

    # 1. 停止并移除 Windows 服务
    if (Test-IsAdmin) {
        $svc = Get-Service -Name $SERVICE_NAME -ErrorAction SilentlyContinue
        if ($svc) {
            Stop-AgentService
            Write-Step "移除 Windows 服务..."
            sc.exe delete $SERVICE_NAME 2>$null | Out-Null
            Start-Sleep -Seconds 2
            Write-Info "Windows 服务已移除"
        } else {
            Write-Info "未发现 Windows 服务，跳过"
        }
    } else {
        Write-Info "非管理员模式，跳过服务清理"
    }

    # 2. 删除二进制文件
    $binaryPath = Join-Path $script:InstallDir $BINARY_NAME
    if (Test-Path $binaryPath) {
        Write-Step "删除二进制文件..."
        Remove-Item $binaryPath -Force
        Write-Info "已删除 $binaryPath"

        # 如果安装目录为空，删除目录
        if ((Test-Path $script:InstallDir) -and -not (Get-ChildItem $script:InstallDir -ErrorAction SilentlyContinue)) {
            Remove-Item $script:InstallDir -Force
        }
    } else {
        Write-Info "未找到二进制文件 $binaryPath，跳过"
    }

    # 3. 删除配置目录
    if (Test-Path $script:ConfigDir) {
        Write-Step "删除配置目录..."
        Remove-Item $script:ConfigDir -Recurse -Force
        Write-Info "已删除 $($script:ConfigDir)"
    } else {
        Write-Info "未找到配置目录 $($script:ConfigDir)，跳过"
    }

    # 4. 从 PATH 中移除（用户模式）
    if (-not (Test-IsAdmin)) {
        $userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
        if ($userPath) {
            $paths = $userPath -split ";" | Where-Object { $_ -ne $script:InstallDir -and $_ -ne "" }
            $newPath = $paths -join ";"
            if ($newPath -ne $userPath) {
                [Environment]::SetEnvironmentVariable("PATH", $newPath, "User")
                Write-Info "已从用户 PATH 中移除 $($script:InstallDir)"
            }
        }
    }

    Write-Host ""
    Write-Host "============================================"
    Write-Info "Collei Agent 卸载完成！"
    Write-Host "============================================"
    Write-Host ""
}

# ======================== 入口 ========================

if ($env:OS -ne "Windows_NT") {
    Write-Err "此脚本仅适用于 Windows 系统，Linux 请使用 install.sh"
    exit 1
}

switch ($Command) {
    "install"    { Invoke-Install }
    "update"     { Invoke-Update }
    "update-ca"  { Invoke-UpdateCA }
    "uninstall"  { Invoke-Uninstall }
}
