# Deploy all built binaries to RPi via scp.
# Requires SSH key auth set up to sash710@192.168.1.42.

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

$rpiHost = "sash710@192.168.1.42"
$rpiTarget = "$rpiHost`:~/unifix-server/bin/"

Write-Host "Ensuring target directory exists on RPi..."
ssh $rpiHost "mkdir -p ~/unifix-server/bin"

Write-Host "Copying binaries..."
scp bin\unifix-server-linux-arm64 $rpiTarget
scp bin\mock-linux-arm64 $rpiTarget
scp bin\mqtt-spy-linux-arm64 $rpiTarget
scp bin\genkey-linux-arm64 $rpiTarget
scp bin\license-server-linux-arm64 $rpiTarget

Write-Host "Setting executable permissions..."
ssh $rpiHost "chmod +x ~/unifix-server/bin/*-linux-arm64"

Write-Host "Done. Files on RPi:"
ssh $rpiHost "ls -la ~/unifix-server/bin/"
