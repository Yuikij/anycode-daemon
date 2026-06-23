$ErrorActionPreference = "Stop"

$repo = "Yuikij/anycode-daemon"
$binName = "anycode-daemon-windows-amd64.exe"
$installDir = "$env:LOCALAPPDATA\anycode\bin"
$exePath = "$installDir\anycode.exe"

Write-Host "Fetching latest release from $repo..."
$releaseUrl = "https://api.github.com/repos/$repo/releases/latest"
$release = Invoke-RestMethod -Uri $releaseUrl -UseBasicParsing

$assetUrl = $null
foreach ($asset in $release.assets) {
    if ($asset.name -eq $binName) {
        $assetUrl = $asset.browser_download_url
        break
    }
}

if (-not $assetUrl) {
    Write-Error "Could not find $binName in the latest release."
    exit 1
}

if (-not (Test-Path -Path $installDir)) {
    New-Item -ItemType Directory -Force -Path $installDir | Out-Null
}

Write-Host "Downloading $assetUrl to $exePath..."
Invoke-WebRequest -Uri $assetUrl -OutFile $exePath -UseBasicParsing

# Add to PATH if not already there
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notmatch [regex]::Escape($installDir)) {
    Write-Host "Adding $installDir to user PATH..."
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$installDir", "User")
    # Update current session PATH so commands work immediately
    $env:Path += ";$installDir"
}

Write-Host ""
Write-Host "AnyCode Daemon installed successfully!"
Write-Host "To get started, please open a new PowerShell window or type:"
Write-Host "  anycode register"
Write-Host "  anycode start"
