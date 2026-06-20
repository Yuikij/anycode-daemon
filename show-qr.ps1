$configDir = "$env:USERPROFILE\.anycode"
$configFile = "$configDir\config.json"

if (-not (Test-Path $configDir)) {
    New-Item -ItemType Directory -Path $configDir | Out-Null
}

$config = @{}
if (Test-Path $configFile) {
    try {
        $config = Get-Content $configFile -Raw | ConvertFrom-Json
    } catch {
        $config = @{}
    }
}

$needsRestart = $false
if (-not $config.deviceToken) {
    $bytes = New-Object Byte[] 16
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    $rng.GetBytes($bytes)
    $token = ([System.BitConverter]::ToString($bytes) -replace '-').ToLower()
    
    if ($config -is [System.Management.Automation.PSCustomObject]) {
        $config | Add-Member -Force -Type NoteProperty -Name "deviceToken" -Value $token
    } else {
        $config.deviceToken = $token
    }
    
    $config | ConvertTo-Json | Set-Content $configFile -Encoding utf8
    $needsRestart = $true
}

$token = $config.deviceToken

$logFile = "d:\code\anycode\daemon-go\daemon.log"
$hostIp = ""
$port = "9527"

if (Test-Path $logFile) {
    $line = Select-String -Path $logFile -Pattern "Connect from phone:\s+ws://([^:]+):(\d+)" | Select-Object -Last 1
    if ($line) {
        $hostIp = $line.Matches.Groups[1].Value
        $port = $line.Matches.Groups[2].Value
    }
}

if (-not $hostIp) {
    Write-Host "Warning: Could not parse IP from daemon.log. Using 127.0.0.1 fallback."
    $hostIp = "127.0.0.1"
}

if ($needsRestart) {
    Write-Host "======================================================"
    Write-Host "Generated a fixed connection token: $token"
    Write-Host "IMPORTANT: You need to RESTART the daemon (Option 2 then 1) to apply this token!"
    Write-Host "======================================================"
} else {
    $anycodeUrl = "anycode://?host=$hostIp&port=$port&token=$token"
    Write-Host "App Deep Link: $anycodeUrl"
    Write-Host "Opening QR code in browser..."
    $qrUrl = "https://api.qrserver.com/v1/create-qr-code/?size=400x400&data=" + [uri]::EscapeDataString($anycodeUrl)
    Start-Process $qrUrl
}
