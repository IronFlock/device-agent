# setup-docker-wsl.ps1
#
# Installs Docker Engine inside WSL2 (Ubuntu 24.04) so that it starts at boot without anyone
# logging in, plus the Windows docker.exe CLI and Compose plugin. All WSL steps run as a
# local service account through Scheduled Tasks (wsl.exe refuses to run as SYSTEM); their
# output is shown here.
#
# Two runs, because corporate proxies often let only a signed-in person download:
#
#   1. Download, in a NORMAL PowerShell window of the person whose login the proxy accepts:
#        powershell -NoProfile -ExecutionPolicy Bypass -File C:\IronFlock-Setup\setup-docker-wsl.ps1 -DownloadOnly
#      Fetches every file listed in $Files into C:\IronFlock-Setup\downloads, sending that
#      person's Windows login to the proxy, and checks each against its pinned SHA-256.
#
#   2. Install, in an ADMINISTRATOR PowerShell (or as SYSTEM):
#        powershell -NoProfile -ExecutionPolicy Bypass -File C:\IronFlock-Setup\setup-docker-wsl.ps1
#      Works only from those files, re-checked against the same hashes - nothing is
#      downloaded. Docker's packages are installed into Ubuntu with dpkg.
#
# Corporate certificates: Windows' trusted root certificates are imported into Ubuntu, so
# corporate CAs (TLS-inspecting proxies, appliance registries) work for image pulls. The
# agent's proxy (HTTP_PROXY/HTTPS_PROXY/NO_PROXY of the agent service) is set on dockerd.
# Re-run the install after the Windows certificate store or the proxy changed.
#
# Refuses to install while Docker Desktop is installed: this would be a second engine next to it.
# Then run connect-agent-to-wsl-docker.ps1 to connect the IronFlock agent.
#
# Safe to re-run: finished steps are skipped, and WSL and Docker are only restarted when
# their configuration changed. The service account password is rotated and both tasks are
# updated with it. Logs: C:\WSL-Setup\*.log

param([switch]$DownloadOnly)

& {
$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'
try { Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass -Force } catch { }
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

# ---- settings ---------------------------------------------------------------
$User      = 'wsldocker'                     # local service account that owns the distro
$Dir       = 'C:\WSL-Setup'                  # helper scripts + logs (admins only)
$VmDir     = 'C:\WSL\Ubuntu-24.04'           # virtual disk of the distro
$Downloads = 'C:\IronFlock-Setup\downloads'  # filled by -DownloadOnly
$MinWsl    = [version]'2.5.0'
$Proxy     = ''                              # e.g. 'http://proxy.example.com:8080'; empty = the agent's proxy, if any
$NoProxy   = ''                              # with $Proxy: hosts that bypass it, e.g. 'localhost,127.0.0.1,.corp.example.com'
$AllowDockerDesktop = $false                 # continue although Docker Desktop is installed
$Helper    = Join-Path $Dir 'run-as-wsl-user.ps1'
$WslExe    = Join-Path $env:ProgramFiles 'WSL\wsl.exe'
# -----------------------------------------------------------------------------

# Everything the installation needs, pinned by SHA-256 (from Ubuntu's and Docker's signed
# package indexes and Docker Compose's release checksums). The Ubuntu image lacks only
# iptables/nftables and their six libraries among Docker Engine's dependencies.
$Files = @(
    @{ Name = 'ubuntu-24.04.5-wsl-amd64.wsl'; Sha256 = 'bb415d824822c4b878125729af451a5d18fb13d1cf5cbed9a7393ad64ac6039e'
       Url = 'https://releases.ubuntu.com/24.04.5/ubuntu-24.04.5-wsl-amd64.wsl' }
    @{ Name = 'libip4tc2_1.8.10-3ubuntu2_amd64.deb'; Sha256 = 'eb381f8ff0469534df3f1b146c3eb8baa6e36f8cfcdf2e3e039398322930d0a4'
       Url = 'http://archive.ubuntu.com/ubuntu/pool/main/i/iptables/libip4tc2_1.8.10-3ubuntu2_amd64.deb' }
    @{ Name = 'libip6tc2_1.8.10-3ubuntu2_amd64.deb'; Sha256 = 'b98b88eb6aa14a902ea1aebc3b12ddd140572545ae1fc6428cb0868c31671d05'
       Url = 'http://archive.ubuntu.com/ubuntu/pool/main/i/iptables/libip6tc2_1.8.10-3ubuntu2_amd64.deb' }
    @{ Name = 'libnfnetlink0_1.0.2-2build1_amd64.deb'; Sha256 = 'f557a287af2ca00df5dbf632c6b77d77479139ffb51c83e3de9766eb609b5d63'
       Url = 'http://archive.ubuntu.com/ubuntu/pool/main/libn/libnfnetlink/libnfnetlink0_1.0.2-2build1_amd64.deb' }
    @{ Name = 'libnetfilter-conntrack3_1.0.9-6build1_amd64.deb'; Sha256 = '6d32a4c4618eb7d6c0d989591d28637a9acdfa86dfe22e42e9ac9cc25d882a97'
       Url = 'http://archive.ubuntu.com/ubuntu/pool/main/libn/libnetfilter-conntrack/libnetfilter-conntrack3_1.0.9-6build1_amd64.deb' }
    @{ Name = 'libnftnl11_1.2.6-2build1_amd64.deb'; Sha256 = 'e5bde5c9cbb95082700011f0b7ef2c0a41900b0a9fd7434f6e90448e2a7b4e50'
       Url = 'http://archive.ubuntu.com/ubuntu/pool/main/libn/libnftnl/libnftnl11_1.2.6-2build1_amd64.deb' }
    @{ Name = 'iptables_1.8.10-3ubuntu2_amd64.deb'; Sha256 = 'd341e8ffc42ca035f5aa0fd9a798c8d2b2edb019225dc11f4dac16edf9c57840'
       Url = 'http://archive.ubuntu.com/ubuntu/pool/main/i/iptables/iptables_1.8.10-3ubuntu2_amd64.deb' }
    @{ Name = 'libnftables1_1.0.9-1ubuntu0.1_amd64.deb'; Sha256 = 'fcaee001747a6adaa0cd9337fef437b3aa7a7a6d5246b62fbd4784d3d6a5009e'
       Url = 'http://archive.ubuntu.com/ubuntu/pool/main/n/nftables/libnftables1_1.0.9-1ubuntu0.1_amd64.deb' }
    @{ Name = 'nftables_1.0.9-1ubuntu0.1_amd64.deb'; Sha256 = 'd648859fccde3c17b8b9b89993427564f91f394a39306db1f67bd5ea3ac42c6e'
       Url = 'http://archive.ubuntu.com/ubuntu/pool/main/n/nftables/nftables_1.0.9-1ubuntu0.1_amd64.deb' }
    @{ Name = 'containerd.io_2.4.2-2~ubuntu.24.04~noble_amd64.deb'; Sha256 = '7327ab23182425e94b46a91567e9ddf0183f9b3a5ed53d5e2489f000dbf9cfe2'
       Url = 'https://download.docker.com/linux/ubuntu/dists/noble/pool/stable/amd64/containerd.io_2.4.2-2~ubuntu.24.04~noble_amd64.deb' }
    @{ Name = 'docker-ce-cli_29.9.1-1~ubuntu.24.04~noble_amd64.deb'; Sha256 = '961968342af4738eb5fb5fc03f9834106c1e454bd010d574cf1a6feae9905efc'
       Url = 'https://download.docker.com/linux/ubuntu/dists/noble/pool/stable/amd64/docker-ce-cli_29.9.1-1~ubuntu.24.04~noble_amd64.deb' }
    @{ Name = 'docker-ce_29.9.1-1~ubuntu.24.04~noble_amd64.deb'; Sha256 = '463a666215797c9f9ea7d0ad1751de5b2b3806a1b539a3550e16822ef34ccbfd'
       Url = 'https://download.docker.com/linux/ubuntu/dists/noble/pool/stable/amd64/docker-ce_29.9.1-1~ubuntu.24.04~noble_amd64.deb' }
    @{ Name = 'docker-buildx-plugin_0.38.0-1~ubuntu.24.04~noble_amd64.deb'; Sha256 = '7094f8b50bc6b239292d475620424a531f7fa0763e466c52ffc3a4037e72bfc5'
       Url = 'https://download.docker.com/linux/ubuntu/dists/noble/pool/stable/amd64/docker-buildx-plugin_0.38.0-1~ubuntu.24.04~noble_amd64.deb' }
    @{ Name = 'docker-compose-plugin_5.6.0-1~ubuntu.24.04~noble_amd64.deb'; Sha256 = '132266d499d9873aafcbe5b515997b8abd714914fa32a62ccac705c1ede7ca81'
       Url = 'https://download.docker.com/linux/ubuntu/dists/noble/pool/stable/amd64/docker-compose-plugin_5.6.0-1~ubuntu.24.04~noble_amd64.deb' }
    @{ Name = 'docker-29.9.1.zip'; Sha256 = '5d9b66509390553fd97d590ec2ff0ce10b04c2ac653fa52f2abf2f05c4c197ca'
       Url = 'https://download.docker.com/win/static/stable/x86_64/docker-29.9.1.zip' }
    @{ Name = 'docker-compose-windows-x86_64.exe'; Sha256 = '8aeaa0055e660e12cf07cd70680dc8333957fe6718528b91ec3da084eab62483'
       Url = 'https://github.com/docker/compose/releases/download/v5.6.0/docker-compose-windows-x86_64.exe' }
)

function Step($t) { Write-Host "`n== $t" -ForegroundColor Cyan }
function Show($lines) { $lines | ForEach-Object { Write-Host "   $_" } }

# Proxy: $Proxy above, else what 'device-agent.exe service install' put on the agent service.
# Values may hold credentials - they are never printed.
$proxyVars = [ordered]@{}
if ($Proxy) {
    $proxyVars['HTTP_PROXY']  = $Proxy
    $proxyVars['HTTPS_PROXY'] = $Proxy
    if ($NoProxy) { $proxyVars['NO_PROXY'] = $NoProxy }
    $proxySource = 'the $Proxy setting'
} else {
    # The agent service: ironflock-device-agent since agent 1.4.0, reagent before.
    $agentEnv = $null
    foreach ($agentSvcName in 'ironflock-device-agent', 'reagent') {
        $agentEnv = (Get-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Services\$agentSvcName" -Name Environment -ErrorAction SilentlyContinue).Environment
        if ($agentEnv) { break }
    }
    foreach ($entry in @($agentEnv | Where-Object { $_ })) {
        $name, $value = $entry -split '=', 2
        if ($value -and $name -in 'HTTP_PROXY', 'HTTPS_PROXY', 'NO_PROXY') { $proxyVars[$name.ToUpper()] = $value }
    }
    $proxySource = 'the agent service'
}
function Get-ProxyUrl([string]$Value) {
    if ($Value -and $Value -notmatch '^\w+://') { return "http://$Value" }
    return $Value
}
$httpProxy  = Get-ProxyUrl $(if ($proxyVars['HTTP_PROXY'])  { $proxyVars['HTTP_PROXY'] }  else { $proxyVars['HTTPS_PROXY'] })
$httpsProxy = Get-ProxyUrl $(if ($proxyVars['HTTPS_PROXY']) { $proxyVars['HTTPS_PROXY'] } else { $proxyVars['HTTP_PROXY'] })

function Test-Download($file) {
    $path = Join-Path $Downloads $file.Name
    return (Test-Path $path) -and ((Get-FileHash $path -Algorithm SHA256).Hash -eq $file.Sha256)
}

# =============================================================================
if ($DownloadOnly) {
    Write-Host "`n== Download into $Downloads" -ForegroundColor Cyan
    if (-not (Test-Path $Downloads)) { throw "$Downloads does not exist - it is prepared together with this script." }
    if ($httpsProxy) {
        Write-Host "   proxy from ${proxySource}, with your Windows login"
    } else {
        Write-Host '   Windows proxy settings, with your Windows login'
        [Net.WebRequest]::DefaultWebProxy.Credentials = [Net.CredentialCache]::DefaultNetworkCredentials
    }
    $failed = 0
    foreach ($file in $Files) {
        if (Test-Download $file) { Write-Host "   ok          $($file.Name)"; continue }
        $target = Join-Path $Downloads $file.Name
        $part   = "$target.part"
        try {
            $req = @{ Uri = $file.Url; OutFile = $part; UseBasicParsing = $true }
            $via = if ($file.Url -like 'https:*') { $httpsProxy } else { $httpProxy }
            if ($via) { $req.Proxy = $via; $req.ProxyUseDefaultCredentials = $true }
            Invoke-WebRequest @req
            if ((Get-FileHash $part -Algorithm SHA256).Hash -ne $file.Sha256) { throw 'checksum does not match - file discarded' }
            Move-Item $part $target -Force
            Write-Host "   downloaded  $($file.Name)"
        } catch {
            Remove-Item $part -Force -ErrorAction SilentlyContinue
            Write-Warning "$($file.Name): $($_.Exception.Message)"
            $failed++
        }
    }
    if ($failed) {
        Write-Warning "$failed of $($Files.Count) files are missing. An error with (407) means the proxy did not accept this window's login: use a normal (not administrator) window of your own account. Running this again keeps what is already there."
    } else {
        Write-Host "`nAll $($Files.Count) files are downloaded and checked." -ForegroundColor Green
    }
    return
}

# =============================================================================
if (-not [Environment]::Is64BitProcess) {
    throw 'This is 32-bit PowerShell. Start C:\Windows\SysNative\WindowsPowerShell\v1.0\powershell.exe and run the script there.'
}
if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'The installation needs an administrator PowerShell (the download step does not).'
}
if ((Test-Path (Join-Path $env:ProgramFiles 'Docker\Docker\Docker Desktop.exe')) -and -not $AllowDockerDesktop) {
    throw 'Docker Desktop is installed. Uninstall it first: this would be a second Docker engine next to it, and the agent''s apps run in Docker Desktop''s.'
}

# An appliance registry sits on the LAN, which the proxy cannot reach. Docker gets it on
# NO_PROXY in case the agent's list doesn't cover it - but only when it really answers
# directly, so a cloud registry on a proxied site keeps using the proxy.
$directRegistry = $null
if ($httpsProxy) {
    $agentSvc = Get-CimInstance Win32_Service -Filter "Name='ironflock-device-agent'"
    if (-not $agentSvc) { $agentSvc = Get-CimInstance Win32_Service -Filter "Name='reagent'" }
    if ($agentSvc -and $agentSvc.PathName -match '(?:^|\s)--?config(?:\s+|=)(?:"([^"]+)"|(\S+))') {
        $flockPath = if ($Matches[1]) { $Matches[1] } else { $Matches[2] }
        $registry  = $null
        try { $registry = (((Get-Content $flockPath -Raw | ConvertFrom-Json).docker_registry_url -replace '^https?://', '') -split '/')[0].ToLower() } catch { }
        if ($registry) {
            $scheme = if ($registry -match ':\d+$') { 'http' } else { 'https' }
            $status = 0
            $saved  = [Net.WebRequest]::DefaultWebProxy
            [Net.WebRequest]::DefaultWebProxy = New-Object Net.WebProxy
            try { $status = [int](Invoke-WebRequest "${scheme}://$registry/v2/" -Method Head -TimeoutSec 10 -UseBasicParsing).StatusCode }
            catch { if ($_.Exception.Response) { $status = [int]$_.Exception.Response.StatusCode } }
            finally { [Net.WebRequest]::DefaultWebProxy = $saved }
            # without a proxy in between, any HTTP answer comes from the registry itself
            if ($status -gt 0) {
                $directRegistry = $registry -replace ':\d+$', ''
                $proxyVars['NO_PROXY'] = (@($proxyVars['NO_PROXY'], $directRegistry) | Where-Object { $_ }) -join ','
            }
        }
    }
}

# -----------------------------------------------------------------------------
Step '1/9  Downloaded files'
$missing = @($Files | Where-Object { -not (Test-Download $_) } | ForEach-Object { $_.Name })
if ($missing.Count) {
    $missing | ForEach-Object { Write-Warning "missing or changed: $_" }
    throw "Run the download step first: setup-docker-wsl.ps1 -DownloadOnly, in a normal window of the person whose login the proxy accepts."
}
Write-Host "   all $($Files.Count) files present, checksums ok"
if ($httpsProxy) {
    Write-Host "   proxy for Docker from ${proxySource}: $($proxyVars.Keys -join ', ') (values not shown)"
    if ($directRegistry) { Write-Host "   registry $directRegistry answers directly - Docker reaches it without the proxy" }
}

# -----------------------------------------------------------------------------
Step '2/9  Windows features'
$reboot = $false
foreach ($f in 'VirtualMachinePlatform', 'Microsoft-Windows-Subsystem-Linux') {
    if ((Get-WindowsOptionalFeature -Online -FeatureName $f).State -ne 'Enabled') {
        Write-Host "   enabling $f"
        Enable-WindowsOptionalFeature -Online -FeatureName $f -All -NoRestart | Out-Null
        $reboot = $true
    }
}
if ($reboot) {
    Write-Warning 'Reboot required: run Restart-Computer, reconnect, then run this script again.'
    return
}
Write-Host '   ok'

# -----------------------------------------------------------------------------
Step '3/9  WSL version'
New-Item -ItemType Directory -Path $Dir, $VmDir -Force | Out-Null
$ver = $null
if (Test-Path $WslExe) { try { $ver = [version](Get-Item $WslExe).VersionInfo.ProductVersion } catch { } }
if (-not $ver -or $ver -lt $MinWsl) {
    throw "WSL $(if ($ver) { $ver } else { '(inbox or none)' }) is older than $MinWsl. Update WSL (wsl --update, Microsoft Store or IT), then run this script again."
}
Write-Host "   WSL $ver"

# -----------------------------------------------------------------------------
Step '4/9  Service account, folders, runner task'
$chars = [char[]]'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789-_.!'
$bytes = New-Object byte[] 24
[Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
$Plain  = (-join ($bytes | ForEach-Object { $chars[$_ % $chars.Length] })) + 'Aa1!'
$Secure = ConvertTo-SecureString $Plain -AsPlainText -Force

if (Get-LocalUser -Name $User -ErrorAction SilentlyContinue) {
    Set-LocalUser -Name $User -Password $Secure -PasswordNeverExpires $true
} else {
    New-LocalUser -Name $User -Password $Secure -PasswordNeverExpires -AccountNeverExpires `
        -Description 'Owner of the WSL/Docker distro' | Out-Null
}
try { Add-LocalGroupMember -SID 'S-1-5-32-544' -Member $User -ErrorAction Stop } catch { }  # Administrators, any UI language

# hide the account from the logon screen
$ul = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon\SpecialAccounts\UserList'
if (-not (Test-Path $ul)) { New-Item -Path $ul -Force | Out-Null }
New-ItemProperty -Path $ul -Name $User -Value 0 -PropertyType DWord -Force | Out-Null

# Only SYSTEM, Administrators and the service account may touch the helper folder or the
# distro's virtual disk. C:\ would otherwise hand "Authenticated Users: Modify" down to
# C:\WSL, letting any local user read or alter the disk while the VM is stopped.
foreach ($d in $Dir, (Split-Path $VmDir)) {
    icacls $d /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' "${User}:(OI)(CI)F" | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "icacls failed on $d (exit $LASTEXITCODE)" }
}

Set-Content -Path (Join-Path $Dir 'runner.ps1') -Encoding ASCII -Value @'
& 'C:\WSL-Setup\cmd.ps1' *>&1 | Out-File -FilePath 'C:\WSL-Setup\out.log' -Encoding utf8
'@

Set-Content -Path $Helper -Encoding ASCII -Value @'
# Runs a command as the WSL owner account (via the WSL-Runner task) and prints its output.
# Usage from a SYSTEM/admin shell:
#   Set-ExecutionPolicy -Scope Process Bypass -Force
#   & C:\WSL-Setup\run-as-wsl-user.ps1 -Bash 'docker ps -a'        # bash in Ubuntu-24.04 as root
#   & C:\WSL-Setup\run-as-wsl-user.ps1 -PowerShell 'wsl -l -v'     # PowerShell as the WSL owner
param([string]$Bash, [string]$PowerShell, [int]$TimeoutMin = 30)
$dir  = 'C:\WSL-Setup'
$utf8 = New-Object System.Text.UTF8Encoding $false
if ($Bash) {
    [IO.File]::WriteAllText("$dir\cmd.sh", ($Bash -replace "`r`n", "`n") + "`n", $utf8)
    $PowerShell = 'wsl -d Ubuntu-24.04 -u root -- bash /mnt/c/WSL-Setup/cmd.sh'
}
if (-not $PowerShell) { throw 'Use -Bash or -PowerShell' }
[IO.File]::WriteAllText("$dir\cmd.ps1", "`$env:WSL_UTF8 = '1'`r`n$PowerShell`r`n", $utf8)
Remove-Item "$dir\out.log" -ErrorAction SilentlyContinue
$t0 = (Get-Date).AddSeconds(-2)
Start-ScheduledTask -TaskName 'WSL-Runner'
$deadline = (Get-Date).AddMinutes($TimeoutMin)
do {
    Start-Sleep -Seconds 2
    $state = (Get-ScheduledTask -TaskName 'WSL-Runner').State
    $last  = (Get-ScheduledTaskInfo -TaskName 'WSL-Runner').LastRunTime
} while ((($last -lt $t0) -or ($state -eq 'Running')) -and ((Get-Date) -lt $deadline))
if ($state -eq 'Running') { Write-Warning "Still running after $TimeoutMin min" }
if (Test-Path "$dir\out.log") { Get-Content "$dir\out.log" -Encoding UTF8 }
'@

$runAct = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -ExecutionPolicy Bypass -File $Dir\runner.ps1"
$runSet = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Hours 2) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName 'WSL-Runner' -Action $runAct -Settings $runSet `
    -User "$env:COMPUTERNAME\$User" -Password $Plain -RunLevel Highest -Force | Out-Null
# The boot task needs the rotated password now, not only in step 9: a re-run that fails in
# between would otherwise leave it with a stale password and Docker without a start at boot.
if (Get-ScheduledTask -TaskName 'WSL-Docker-KeepAlive' -ErrorAction SilentlyContinue) {
    Set-ScheduledTask -TaskName 'WSL-Docker-KeepAlive' -User "$env:COMPUTERNAME\$User" -Password $Plain | Out-Null
}
Write-Host "   account '$User' ready, runner task registered"

# -----------------------------------------------------------------------------
Step '5/9  Ubuntu 24.04'
if (Test-Path (Join-Path $VmDir 'ext4.vhdx')) {
    Write-Host '   already imported, skipping'
} else {
    $img = Join-Path $Downloads 'ubuntu-24.04.5-wsl-amd64.wsl'
    Write-Host '   importing (a few minutes)...'
    $out = & $Helper -PowerShell (@'
wsl --install --from-file '@@IMG@@' --name Ubuntu-24.04 --location 'C:\WSL\Ubuntu-24.04' --no-launch
if (-not ((wsl -l -q) -match 'Ubuntu-24.04')) {
    wsl --import Ubuntu-24.04 'C:\WSL\Ubuntu-24.04' '@@IMG@@' --version 2
}
wsl -l -v
'@).Replace('@@IMG@@', $img)
    $out | Set-Content (Join-Path $Dir 'step5.log')
    Show $out
    if (-not (Test-Path (Join-Path $VmDir 'ext4.vhdx'))) { throw 'Import failed - see output above' }
}

# -----------------------------------------------------------------------------
Step '6/9  WSL settings: systemd, no Windows interop, idle timeout'
# Interop off: the VM runs as an elevated administrator, so a Windows program started from
# Linux - e.g. by an app that escaped its container - would run with those rights. Nothing
# here needs it: Windows starts wsl.exe, never the other way round.
# Automount options: the agent bind-mounts the apps' folders from C:\ProgramData (as
# /mnt/c/...). 'metadata' lets containers chown/chmod there like on Linux; umask 000 opens
# what Windows created to every container user, as Docker Desktop's mounts do.
$out = @(& $Helper -Bash @'
cat > /etc/wsl.conf.new <<'EOF'
[boot]
systemd=true

[automount]
options = "metadata,umask=000"

[interop]
enabled=false
appendWindowsPath=false
EOF
if cmp -s /etc/wsl.conf.new /etc/wsl.conf; then rm -f /etc/wsl.conf.new; echo "wsl.conf unchanged"; else mv /etc/wsl.conf.new /etc/wsl.conf; echo "wsl.conf updated"; fi
echo "pid1=$(ps -p 1 -o comm=)"
'@ | ForEach-Object { "$_".Trim() })
Show $out
$cfgOut = @(& $Helper -PowerShell @'
$cfg = Join-Path $env:USERPROFILE '.wslconfig'
$text = if (Test-Path $cfg) { [IO.File]::ReadAllText($cfg) } else { '' }
if ($text -match '(?m)^\s*vmIdleTimeout\s*=\s*-1\s*$') {
    'wslconfig unchanged'
} else {
    if ($text -match '(?m)^\s*vmIdleTimeout\s*=.*$') { $text = $text -replace '(?m)^\s*vmIdleTimeout\s*=.*$', 'vmIdleTimeout=-1' }
    elseif ($text -match '(?m)^\s*\[wsl2\]\s*$') { $text = $text -replace '(?m)^\s*\[wsl2\]\s*$', "[wsl2]`r`nvmIdleTimeout=-1" }
    else { $text = $text.TrimEnd() + "`r`n[wsl2]`r`nvmIdleTimeout=-1`r`n" }
    [IO.File]::WriteAllText($cfg, $text.TrimStart())
    'wslconfig updated'
}
'@ | ForEach-Object { "$_".Trim() })
Show $cfgOut
# a restart applies both files, but also stops every running container - only when needed
if (($out -contains 'wsl.conf updated') -or ($cfgOut -contains 'wslconfig updated') -or -not ($out -contains 'pid1=systemd')) {
    $out = & $Helper -PowerShell @'
wsl --shutdown
Start-Sleep -Seconds 8
wsl -d Ubuntu-24.04 -u root -- ps -p 1 -o comm=
'@
    $out | Set-Content (Join-Path $Dir 'step6.log')
    Show $out
    if (-not ($out -match '^\s*systemd\s*$')) { throw 'systemd is not PID 1 - see output above' }
} else {
    Write-Host '   already configured - WSL not restarted'
}

# -----------------------------------------------------------------------------
Step '7/9  Certificates, proxy, Docker Engine'
# Windows' trusted roots for Ubuntu: covers the corporate CA of a TLS-inspecting proxy and of
# an appliance registry, the way Docker Desktop would trust them through Windows.
$trustDir = Join-Path $Dir 'trust'
Remove-Item $trustDir -Recurse -Force -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Path $trustDir | Out-Null
$utf8  = New-Object Text.UTF8Encoding $false
$now   = Get-Date
$roots = @(Get-ChildItem Cert:\LocalMachine\Root | Where-Object { $_.NotAfter -gt $now })
foreach ($c in $roots) {
    $pem = [Convert]::ToBase64String($c.RawData, [Base64FormattingOptions]::InsertLineBreaks) -replace "`r`n", "`n"
    [IO.File]::WriteAllText((Join-Path $trustDir "$($c.Thumbprint).crt"), "-----BEGIN CERTIFICATE-----`n$pem`n-----END CERTIFICATE-----`n", $utf8)
}
Write-Host "   $($roots.Count) Windows root certificates exported for Ubuntu"

# dockerd pulls through the proxy (Docker Hub); systemd Environment= values: backslash and
# double quote escaped, % doubled
$dockerProxyB64 = ''
if ($httpProxy -or $httpsProxy) {
    $envLines = $proxyVars.Keys | ForEach-Object {
        $v = $proxyVars[$_].Replace('\', '\\').Replace('"', '\"').Replace('%', '%%')
        "Environment=`"$_=$v`""
    }
    $dockerProxyB64 = [Convert]::ToBase64String($utf8.GetBytes("[Service]`n" + ($envLines -join "`n") + "`n"))
}

# the downloaded packages, as WSL sees them; dpkg orders their configuration itself
$debDir = '/mnt/' + $Downloads.Substring(0, 1).ToLower() + ($Downloads.Substring(2) -replace '\\', '/')
$debs   = ($Files | Where-Object { $_.Name -like '*.deb' } | ForEach-Object { "'$($_.Name)'" }) -join ' '

$bash = @'
set -eu
export DEBIAN_FRONTEND=noninteractive
CHANGED=0
# put FILE BASE64: write FILE (root-only) when its content differs; an empty BASE64 leaves
# FILE alone. PUT_CHANGED tells the caller whether it wrote.
put() {
  PUT_CHANGED=0
  [ -n "$2" ] || return 0
  mkdir -p "$(dirname "$1")"
  echo "$2" | base64 -d > "$1.new"
  chmod 0600 "$1.new"
  if cmp -s "$1.new" "$1"; then rm -f "$1.new"; else mv "$1.new" "$1"; echo "updated $1"; PUT_CHANGED=1; fi
}
systemctl is-system-running --wait >/dev/null 2>&1 || true

# Windows' root certificates, replaced on every run; dockerd reads the store only at start
before=$(sha256sum /etc/ssl/certs/ca-certificates.crt | cut -d' ' -f1)
W=/usr/local/share/ca-certificates/windows-root
rm -rf "$W"
mkdir -p "$W"
cp /mnt/c/WSL-Setup/trust/*.crt "$W/" 2>/dev/null || echo "WARNING: no Windows root certificates to import"
update-ca-certificates 2>/dev/null | grep -E 'added|removed' || true
after=$(sha256sum /etc/ssl/certs/ca-certificates.crt | cut -d' ' -f1)
[ "$before" = "$after" ] || CHANGED=1

put /etc/systemd/system/docker.service.d/http-proxy.conf '@@DOCKER_PROXY_B64@@'
[ "$PUT_CHANGED" = 0 ] || CHANGED=1

if dpkg -s docker-ce >/dev/null 2>&1; then
  echo "Docker Engine already installed"
else
  echo "installing Docker Engine from the downloaded packages"
  cd '@@DEB_DIR@@'
  dpkg -i @@DEBS@@
  CHANGED=1
fi
mkdir -p /etc/docker
[ -f /etc/docker/daemon.json ] || echo '{ "log-driver": "local" }' > /etc/docker/daemon.json
systemctl enable containerd docker >/dev/null 2>&1

# a restart stops every running container - only when something changed
if [ "$CHANGED" = 1 ] || ! systemctl is-active --quiet docker; then
  systemctl daemon-reload
  systemctl restart docker
  echo "docker restarted"
else
  echo "docker configuration unchanged - not restarted"
fi
echo "Engine $(docker info --format '{{.ServerVersion}}' 2>/dev/null)"
if docker run --rm hello-world >/dev/null 2>&1; then
  echo "hello-world: ok"
else
  echo "WARNING: hello-world could not be pulled - the engine works, but Docker Hub images will not pull"
fi
'@
$bash = $bash.Replace('@@DOCKER_PROXY_B64@@', $dockerProxyB64).Replace('@@DEB_DIR@@', $debDir).Replace('@@DEBS@@', $debs)
$out = & $Helper -TimeoutMin 45 -Bash $bash
$out | Set-Content (Join-Path $Dir 'step7.log')
Show ($out | Select-Object -Last 25)
if (-not ($out -match '^\s*Engine \S+')) { throw 'Docker Engine is not running - full log in C:\WSL-Setup\step7.log' }

# -----------------------------------------------------------------------------
Step '8/9  Docker CLI and Compose plugin for Windows'
# The agent runs 'docker' / 'docker compose' by name; connect-agent-to-wsl-docker.ps1 points
# it at this engine. Copied only when different, so a re-run never trips over a running CLI.
function Install-IfChanged([string]$Source, [string]$Target) {
    if ((Test-Path $Target) -and (Get-FileHash $Target).Hash -eq (Get-FileHash $Source).Hash) { return 'already there' }
    New-Item -ItemType Directory -Force -Path (Split-Path $Target) | Out-Null
    Copy-Item $Source $Target -Force
    return 'installed'
}
$BinDir = Join-Path $Dir 'bin'
$zip    = ($Files | Where-Object { $_.Name -like 'docker-*.zip' }).Name
$tmp    = Join-Path $env:TEMP 'docker-cli'
Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
Expand-Archive (Join-Path $Downloads $zip) -DestinationPath $tmp
Write-Host "   docker.exe ($zip): $(Install-IfChanged "$tmp\docker\docker.exe" (Join-Path $BinDir 'docker.exe'))"
Remove-Item $tmp -Recurse -Force
$plugin = Join-Path $env:ProgramFiles 'Docker\cli-plugins\docker-compose.exe'
Write-Host "   Compose plugin: $(Install-IfChanged (Join-Path $Downloads 'docker-compose-windows-x86_64.exe') $plugin)"

# machine PATH: edit the raw value so %SystemRoot%-style entries stay unexpanded
$envKey  = 'HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager\Environment'
$rawPath = (Get-Item $envKey).GetValue('Path', '', 'DoNotExpandEnvironmentNames')
if (@($rawPath -split ';' | ForEach-Object { $_.TrimEnd('\') }) -notcontains $BinDir) {
    Set-ItemProperty -Path $envKey -Name Path -Value ($rawPath.TrimEnd(';') + ';' + $BinDir) -Type ExpandString
    Write-Host "   added $BinDir to the machine PATH"
} else {
    Write-Host "   $BinDir already on the machine PATH"
}

# -----------------------------------------------------------------------------
Step '9/9  Keep-alive at boot + power settings'
$kaAct = New-ScheduledTaskAction -Execute $WslExe -Argument '-d Ubuntu-24.04 -u root --exec /bin/sleep infinity'
$kaTrg = @(
    New-ScheduledTaskTrigger -AtStartup
    New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(1) -RepetitionInterval (New-TimeSpan -Minutes 5) -RepetitionDuration (New-TimeSpan -Days 3650)
)
$kaSet = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
Register-ScheduledTask -TaskName 'WSL-Docker-KeepAlive' -Action $kaAct -Trigger $kaTrg -Settings $kaSet `
    -User "$env:COMPUTERNAME\$User" -Password $Plain -RunLevel Highest -Force | Out-Null
Start-ScheduledTask -TaskName 'WSL-Docker-KeepAlive'
powercfg /change standby-timeout-ac 0
powercfg /change hibernate-timeout-ac 0
Start-Sleep -Seconds 5
Show (& $Helper -Bash 'systemctl is-active docker; docker version --format "Engine {{.Server.Version}}"')
Write-Host "   keep-alive task: $((Get-ScheduledTask -TaskName 'WSL-Docker-KeepAlive').State)"

Write-Host "`nDone. Docker Engine runs in WSL (Ubuntu-24.04) as '$User' and starts at boot." -ForegroundColor Green
Write-Host 'Next: connect-agent-to-wsl-docker.ps1 connects the IronFlock agent to it.'
Write-Host 'Run Docker commands from this shell with:'
Write-Host '   Set-ExecutionPolicy -Scope Process Bypass -Force'
Write-Host "   & $Helper -Bash 'docker ps -a'"
}
