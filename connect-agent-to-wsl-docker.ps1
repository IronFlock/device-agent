# connect-agent-to-wsl-docker.ps1
#
# Makes the WSL Docker Engine (installed by setup-docker-wsl.ps1) usable by the
# IronFlock device agent running as a Windows service (SYSTEM; ironflock-device-agent since
# agent 1.4.0, reagent before):
#   - dockerd additionally listens on tcp://127.0.0.1:2376 (loopback only)
#   - mutual TLS: only clients holding the generated client certificate get in
#   - client certs land in C:\WSL-Setup\certs (SYSTEM, Administrators, WSL account only)
#   - plain-HTTP appliance registries are added to the WSL engine's /etc/docker/daemon.json
#     (from the agent's device.flock, plus $ExtraInsecureRegistries below)
#     (proxy and corporate CAs for the engine are set up by setup-docker-wsl.ps1)
#   - the cifs kernel module is loaded now and at every boot, so apps can use SMB volumes
#   - the agent service gets DOCKER_HOST, DOCKER_TLS_VERIFY, DOCKER_CERT_PATH and PATH in
#     its own service environment (not machine-wide) and is restarted when that changed
# It downloads nothing: the docker.exe CLI and the Compose plugin come from
# setup-docker-wsl.ps1, which must have run first.
#
# Refuses to run while Docker Desktop is installed: the agent's apps live in Docker
# Desktop's engine, and switching the agent to the WSL engine would start them twice.
#
# Run as SYSTEM (IronFlock device terminal) or in an administrator PowerShell:
#   powershell -NoProfile -ExecutionPolicy Bypass -File C:\IronFlock-Setup\connect-agent-to-wsl-docker.ps1
# Safe to re-run: existing certificates are kept, and Docker and the agent are only
# restarted when their configuration actually changed.
#
# Re-run it after every 'device-agent.exe service install': the install recreates the
# service and, with a proxy, overwrites its environment. A device installed before agent
# 1.4.0 keeps its environment when the agent moves it to the new service name.
#
# Apps need agent 1.2.11 or newer on this engine: older agents pass Windows paths
# (C:\ProgramData\IronFlock\...) as the bind-mount sources of every app, which a Linux
# engine rejects. The script warns when the installed agent is older.

& {
$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'
try { Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass -Force } catch { }
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

# ---- settings ---------------------------------------------------------------
# Extra plain-HTTP registries (host:port) for the WSL engine, e.g. when the agent is not
# installed yet. Registries from the agent's device.flock are added automatically.
$ExtraInsecureRegistries = @()
# Restart the agent service when its environment changed (device offline for ~30 s).
$RestartAgent = $true
# Continue although Docker Desktop is installed. Only when you are sure its engine no
# longer runs the agent's apps - otherwise they run twice.
$AllowDockerDesktop = $false
# -----------------------------------------------------------------------------

$Helper      = 'C:\WSL-Setup\run-as-wsl-user.ps1'
$CertDir     = 'C:\WSL-Setup\certs'
$BinDir      = 'C:\WSL-Setup\bin'
$PluginDir   = Join-Path $env:ProgramFiles 'Docker\cli-plugins'
# The agent service: ironflock-device-agent since agent 1.4.0, reagent before (a device
# installed earlier moves to the new name by itself once it runs 1.4.0).
$ServiceName = 'ironflock-device-agent'
if (-not (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) { $ServiceName = 'reagent' }
$DockerHost  = 'tcp://127.0.0.1:2376'
$MinAgentVersion = [version]'1.2.11'   # first agent that translates mount paths for this engine

if ((Test-Path (Join-Path $env:ProgramFiles 'Docker\Docker\Docker Desktop.exe')) -and -not $AllowDockerDesktop) {
    throw 'Docker Desktop is installed. Uninstall it first: the agent''s apps run in its engine and would be started a second time in the WSL engine (duplicate apps, port conflicts).'
}
if (-not (Test-Path $Helper)) { throw 'C:\WSL-Setup\run-as-wsl-user.ps1 not found - run setup-docker-wsl.ps1 first' }
$docker  = Join-Path $BinDir 'docker.exe'
$compose = Join-Path $PluginDir 'docker-compose.exe'
foreach ($f in $docker, $compose) {
    if (-not (Test-Path $f)) { throw "$f not found - run setup-docker-wsl.ps1 first" }
}

# Same normalization as the agent: no scheme, no path, lower case.
function Get-RegistryHost([string]$Entry) {
    $e = $Entry.Trim() -replace '^https?://', ''
    return ($e -split '/')[0].ToLower()
}

# Value of a '-name value' / '-name=value' argument on the service command line.
function Get-ServiceArg([string]$CommandLine, [string]$Name) {
    if ($CommandLine -match ('(?:^|\s)--?' + $Name + '(?:\s+|=)(?:"([^"]+)"|(\S+))')) {
        if ($Matches[1]) { return $Matches[1] }
        return $Matches[2]
    }
    return $null
}

Write-Host "`n== 1/5  Agent service and registries" -ForegroundColor Cyan
$svc = Get-CimInstance Win32_Service -Filter "Name='$ServiceName'"
$AgentExe   = $null
$FlockPath  = $null
$ServiceEnv = @()
if ($svc -and ($svc.PathName -match '^\s*"([^"]+)"' -or $svc.PathName -match '^\s*(\S+)')) {
    $AgentExe = $Matches[1]
    Write-Host "   agent service: $AgentExe ($($svc.State))"
    $FlockPath = Get-ServiceArg $svc.PathName 'config'
    if (-not $FlockPath) { $FlockPath = Join-Path (Split-Path $AgentExe) 'device.flock' }
    $agentVersion = $null
    try { $agentVersion = [version]("$(& $AgentExe -version)".Trim()) } catch { }
    if ($agentVersion -and $agentVersion -lt $MinAgentVersion) {
        Write-Warning "agent $agentVersion is older than ${MinAgentVersion}: apps will not start on this engine until the agent is updated"
    } elseif ($agentVersion) {
        Write-Host "   agent version $agentVersion"
    }
    $ServiceEnv = @((Get-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Services\$ServiceName" -Name Environment -ErrorAction SilentlyContinue).Environment | Where-Object { $_ })
} else {
    Write-Host '   agent service not installed yet'
}

$candidates = @($ExtraInsecureRegistries)
if ($FlockPath -and (Test-Path $FlockPath)) {
    try {
        $flock  = Get-Content $FlockPath -Raw | ConvertFrom-Json
        # a JSON-array string on backend-issued flocks, a bare host:port/ on appliance ones
        $listed = "$($flock.'insecure-registries')".Trim()
        if ($listed) {
            # Windows PowerShell hands a parsed JSON array down the pipeline as ONE object, so
            # foreach takes it apart; a bare host:port/ is no JSON and stays whole
            try { foreach ($entry in ($listed | ConvertFrom-Json)) { $candidates += "$entry" } } catch { $candidates += $listed }
        }
        # an explicit port marks the appliance's plain-HTTP registry (same rule as the agent)
        $registryHost = Get-RegistryHost "$($flock.docker_registry_url)"
        if ($registryHost -match ':\d+$') { $candidates += $registryHost }
    } catch {
        Write-Warning "Could not read $FlockPath - its registries are skipped"
    }
}
$registries = New-Object System.Collections.Generic.List[string]
foreach ($c in $candidates) {
    $h = Get-RegistryHost "$c"
    # dockerd refuses to start at all with an entry it cannot parse
    if ($h -and $h -notmatch '^[a-z0-9._-]+(:\d+)?$') { Write-Warning "skipping registry entry '$h' - not a host[:port]"; continue }
    if ($h -and -not $registries.Contains($h)) { $registries.Add($h) }
}
if ($registries.Count) {
    Write-Host "   insecure registries: $($registries -join ', ')"
} else {
    Write-Host '   no plain-HTTP registry needed (cloud / HTTPS registry)'
}

Write-Host "`n== 2/5  WSL engine: certificates, TLS listener, registries, SMB" -ForegroundColor Cyan
$daemonB64 = ''
if ($registries.Count) {
    $lines = @((& $Helper -Bash @'
echo "@@BEGIN@@"
if [ -s /etc/docker/daemon.json ]; then base64 -w 76 /etc/docker/daemon.json; fi
echo "@@END@@"
'@ | Out-String) -split "\r?\n" | ForEach-Object { $_.Trim() })
    $begin = [array]::IndexOf($lines, '@@BEGIN@@')
    $end   = [array]::IndexOf($lines, '@@END@@')
    if ($begin -lt 0 -or $end -le $begin) { throw 'Could not read /etc/docker/daemon.json from WSL' }
    # base64 wraps at 76 characters, so the log capture can't truncate it; join the lines again
    $currentB64  = if ($end -gt $begin + 1) { ($lines[($begin + 1)..($end - 1)] -join '').Trim() } else { '' }
    $currentJson = ''
    if ($currentB64) { $currentJson = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($currentB64)).TrimStart([char]0xFEFF) }
    try {
        $cfg = if ($currentJson.Trim()) { $currentJson | ConvertFrom-Json } else { New-Object PSObject }
        # an entry with whitespace (e.g. two names joined into one) keeps dockerd from starting
        $listedNow = @($cfg.'insecure-registries' | Where-Object { $_ })
        $existing  = @($listedNow | Where-Object { "$_" -notmatch '\s' })
        $invalid   = @($listedNow | Where-Object { "$_" -match '\s' })
        $missing   = @($registries | Where-Object { $existing -notcontains $_ })
        if ($missing.Count -or $invalid.Count) {
            $cfg | Add-Member -NotePropertyName 'insecure-registries' -NotePropertyValue ([string[]]($existing + $missing)) -Force
            $daemonB64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes(($cfg | ConvertTo-Json -Depth 20)))
            if ($invalid.Count) { Write-Host "   removing invalid daemon.json entries: $($invalid -join ' | ')" }
            if ($missing.Count) { Write-Host "   adding to daemon.json: $($missing -join ', ')" }
        } else {
            Write-Host '   daemon.json already lists them'
        }
    } catch {
        Write-Warning '/etc/docker/daemon.json in WSL is not valid JSON - fix it by hand; insecure registries not added'
    }
}

$bash = @'
set -eu
CHANGED=0
# put FILE BASE64: write FILE when its content differs; an empty BASE64 leaves FILE alone
put() {
  [ -n "$2" ] || return 0
  mkdir -p "$(dirname "$1")"
  echo "$2" | base64 -d > "$1.new"
  if cmp -s "$1.new" "$1"; then rm -f "$1.new"; else mv "$1.new" "$1"; echo "updated $1"; CHANGED=1; fi
}

T=/etc/docker/tls
W=/mnt/c/WSL-Setup/certs
mkdir -p "$T" && cd "$T"
# Keep the certificates only while the whole set exists. An interrupted earlier run or a
# deleted C:\WSL-Setup\certs leaves a partial set that would never work and never heal.
if [ -f ca.pem ] && [ -f server-cert.pem ] && [ -f server-key.pem ] && [ -f "$W/ca.pem" ] && [ -f "$W/cert.pem" ] && [ -f "$W/key.pem" ]; then
  echo "certificates already exist, keeping them"
else
  rm -f ca.pem ca-key.pem ca.srl server-cert.pem server-key.pem
  openssl genrsa -out ca-key.pem 4096 2>/dev/null
  openssl req -new -x509 -days 3650 -sha256 -key ca-key.pem -subj "/CN=wsl-docker-ca" -out ca.pem
  openssl genrsa -out server-key.pem 4096 2>/dev/null
  openssl req -new -key server-key.pem -subj "/CN=localhost" -out server.csr
  printf 'subjectAltName=IP:127.0.0.1,DNS:localhost\nextendedKeyUsage=serverAuth\n' > server.ext
  openssl x509 -req -days 3650 -sha256 -in server.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial -out server-cert.pem -extfile server.ext
  openssl genrsa -out key.pem 4096 2>/dev/null
  openssl req -new -key key.pem -subj "/CN=device-agent" -out client.csr
  printf 'extendedKeyUsage=clientAuth\n' > client.ext
  openssl x509 -req -days 3650 -sha256 -in client.csr -CA ca.pem -CAkey ca-key.pem -CAcreateserial -out cert.pem -extfile client.ext
  mkdir -p "$W"
  cp ca.pem cert.pem key.pem "$W/"
  rm -f key.pem cert.pem ./*.csr ./*.ext
  chmod 0400 ca-key.pem server-key.pem
  echo "certificates created"
  # dockerd must load the new server certificate even when override.conf is unchanged
  CHANGED=1
fi

# What docker ran with before this run - put back if it does not start with the new settings.
D=/etc/docker/daemon.json
O=/etc/systemd/system/docker.service.d/override.conf
for f in "$D" "$O"; do rm -f "$f.before"; [ ! -f "$f" ] || cp -p "$f" "$f.before"; done

# insecure registries, merged on the Windows side
put "$D" '@@DAEMON_JSON_B64@@'

# SMB volumes: the WSL kernel ships cifs as a module - load it now and at every boot
echo cifs > /etc/modules-load.d/cifs.conf
if grep -qw cifs /proc/filesystems || modprobe cifs 2>/dev/null; then
  echo "cifs: available (SMB volumes can be mounted)"
else
  echo "WARNING: cifs kernel module not available - SMB volumes will not mount (try 'wsl --update')"
fi

# keep the distro's own ExecStart and add the TLS listener to it
BASE=$(systemctl cat docker | grep -m1 '^ExecStart=/' | cut -d= -f2-)
[ -n "$BASE" ] || { echo "ERROR: could not read ExecStart of docker.service - override not written"; exit 1; }
mkdir -p "$(dirname "$O")"
cat > "$O.new" <<EOF
[Service]
ExecStart=
ExecStart=$BASE -H tcp://127.0.0.1:2376 --tlsverify --tlscacert=$T/ca.pem --tlscert=$T/server-cert.pem --tlskey=$T/server-key.pem
EOF
if cmp -s "$O.new" "$O"; then
  rm -f "$O.new"
else
  mv "$O.new" "$O"
  echo "updated $O"
  CHANGED=1
fi

# only bounce docker when something changed - a restart restarts every running app
if [ "$CHANGED" = 1 ] || ! systemctl is-active --quiet docker; then
  systemctl daemon-reload
  systemctl reset-failed docker 2>/dev/null || true
  if systemctl restart docker && sleep 3 && systemctl is-active --quiet docker; then
    echo "docker restarted: active"
  else
    echo "ERROR: docker does not start with the new settings:"
    journalctl -u docker --no-pager -n 6 -o cat | grep -i -E 'error|fail' | head -3 || true
    for f in "$D" "$O"; do if [ -f "$f.before" ]; then mv -f "$f.before" "$f"; else rm -f "$f"; fi; done
    systemctl daemon-reload
    systemctl reset-failed docker 2>/dev/null || true
    systemctl restart docker || true
    echo "previous settings restored, docker: $(systemctl is-active docker)"
    exit 1
  fi
else
  echo "docker configuration unchanged - not restarted"
fi
rm -f "$D.before" "$O.before"
ss -lnt | grep ':2376' || echo "WARNING: nothing listening on 2376 inside WSL"
'@
$bash = $bash.Replace('@@DAEMON_JSON_B64@@', $daemonB64)
$out = & $Helper -Bash $bash
$out | ForEach-Object { Write-Host "   $_" }
if (@($out | ForEach-Object { "$_".Trim() }) -match '^ERROR:') { throw 'Docker in WSL was not changed successfully - see the ERROR above. The agent was not touched.' }
if (-not (Test-Path (Join-Path $CertDir 'key.pem'))) { throw "Client certificates missing in $CertDir - see output above" }

Write-Host "`n== 3/5  Windows Docker CLI (installed by setup-docker-wsl.ps1)" -ForegroundColor Cyan
Write-Host "   $docker"
Write-Host "   $compose"
if (@($env:Path -split ';') -notcontains $BinDir) { $env:Path = "$env:Path;$BinDir" }

# The agent logs in with 'docker login' as SYSTEM. A credsStore left behind by Docker Desktop
# names a helper that is gone once Desktop is uninstalled, and every login would fail.
$sysDockerCfg = Join-Path $env:SystemRoot 'System32\config\systemprofile\.docker\config.json'
if (Test-Path $sysDockerCfg) {
    try {
        $dockerCfg = Get-Content $sysDockerCfg -Raw | ConvertFrom-Json
        $store = "$($dockerCfg.credsStore)"
        if ($store -and -not (Get-Command "docker-credential-$store" -ErrorAction SilentlyContinue)) {
            Copy-Item $sysDockerCfg "$sysDockerCfg.bak" -Force
            $dockerCfg.PSObject.Properties.Remove('credsStore')
            [IO.File]::WriteAllText($sysDockerCfg, ($dockerCfg | ConvertTo-Json -Depth 20), (New-Object Text.UTF8Encoding $false))
            Write-Host "   removed credsStore '$store' from SYSTEM's docker config (helper not installed, backup config.json.bak)"
        }
    } catch {
        Write-Warning "Could not check SYSTEM's docker config ($sysDockerCfg): $($_.Exception.Message)"
    }
}

Write-Host "`n== 4/5  Test from Windows" -ForegroundColor Cyan
Start-Sleep -Seconds 3
if (-not (Test-NetConnection 127.0.0.1 -Port 2376 -WarningAction SilentlyContinue).TcpTestSucceeded) {
    Write-Warning '127.0.0.1:2376 does not answer on the Windows side. WSL localhost forwarding is not active for this VM.'
    Write-Warning 'Fallback: networkingMode=mirrored in C:\Users\wsldocker\.wslconfig (ask before changing).'
    Write-Warning 'The agent service environment was NOT changed.'
    return
}
Write-Host '   port 2376 reachable'
$env:DOCKER_HOST       = $DockerHost
$env:DOCKER_TLS_VERIFY = '1'
$env:DOCKER_CERT_PATH  = $CertDir
if (Test-Path $docker) {
    & $docker version --format '   client {{.Client.Version}}  ->  engine {{.Server.Version}} ({{.Server.Os}})'
    # Windows PowerShell turns redirected native stderr into a terminating error under 'Stop'
    $ErrorActionPreference = 'Continue'
    $composeVersion = & $docker compose version --short 2>$null
    $composeExit    = $LASTEXITCODE
    $ErrorActionPreference = 'Stop'
    if ($composeExit -eq 0 -and $composeVersion) {
        Write-Host "   compose $composeVersion"
    } else {
        Write-Warning 'docker compose is not available - compose apps will not run'
    }
}

Write-Host "`n== 5/5  Agent service environment" -ForegroundColor Cyan
if (-not $svc) {
    Write-Host '   agent service not installed yet - re-run this script after "device-agent.exe service install"'
    return
}
$svcKey = "HKLM:\SYSTEM\CurrentControlSet\Services\$ServiceName"
$wanted = [ordered]@{
    DOCKER_HOST       = $DockerHost
    DOCKER_TLS_VERIFY = '1'
    DOCKER_CERT_PATH  = $CertDir
    # services keep the PATH they had at boot; this hands the agent the one with docker.exe now
    PATH              = [Environment]::GetEnvironmentVariable('Path', 'Machine')
}
# keep everything else, e.g. the proxy variables 'device-agent.exe service install' wrote
$kept    = @($ServiceEnv | Where-Object { $wanted.Keys -notcontains ($_ -split '=', 2)[0] })
$updated = @($kept) + @($wanted.Keys | ForEach-Object { "$_=$($wanted[$_])" })
$envChanged = -not (($ServiceEnv -join "`n") -ceq ($updated -join "`n"))
if ($envChanged) {
    Set-ItemProperty $svcKey -Name Environment -Value ([string[]]$updated) -Type MultiString
    Write-Host "   set DOCKER_HOST, DOCKER_TLS_VERIFY, DOCKER_CERT_PATH and PATH on the '$ServiceName' service"
    if ($kept.Count) { Write-Host "   kept: $(($kept | ForEach-Object { ($_ -split '=', 2)[0] }) -join ', ')" }
} else {
    Write-Host '   already configured'
}

# The agent loads its client certificate at start, so new certificates need a restart too.
$certsCreated = @($out | ForEach-Object { "$_".Trim() }) -contains 'certificates created'
$restart = ($envChanged -or $certsCreated) -and $svc.State -eq 'Running'
if (($envChanged -or $certsCreated) -and $svc.State -ne 'Running') {
    Write-Host "   service is $($svc.State) - the changes apply on its next start"
}

Write-Host "`nDone." -ForegroundColor Green
Write-Host "   engine:  $DockerHost (mutual TLS, client certs in $CertDir)"
Write-Host "   CLI:     $docker"
Write-Host "   compose: $compose"

if ($restart -and -not $RestartAgent) {
    Write-Host "`nRestart the agent to apply the changes (e.g. from the IronFlock platform)." -ForegroundColor Yellow
} elseif ($restart) {
    # The restart runs as a one-shot SYSTEM task, outside this shell. Started from the
    # IronFlock device terminal, this shell belongs to the agent's kill-on-close job and dies
    # with the agent - a stop issued from here would never be followed by the start.
    # The agent's own 'service stop' waits for it to drain; Restart-Service gives up early.
    $taskName = 'IronFlock-Agent-Restart'
    $exe      = $AgentExe.Replace("'", "''")
    $command  = "& '$exe' service stop; & '$exe' service start; Unregister-ScheduledTask -TaskName '$taskName' -Confirm:`$false"
    $encoded  = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($command))
    $action   = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -NonInteractive -EncodedCommand $encoded"
    $principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
    $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Minutes 10) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
    Register-ScheduledTask -TaskName $taskName -Action $action -Principal $principal -Settings $settings -Force | Out-Null
    Write-Host "`nRestarting the agent to apply the changes - the device is offline for up to a minute." -ForegroundColor Yellow
    Write-Host 'Run from the IronFlock device terminal, that terminal disconnects now: reconnect after a minute.' -ForegroundColor Yellow
    Start-ScheduledTask -TaskName $taskName
}
}
