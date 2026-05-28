# Cross-compile carvilon-server for linux/amd64 (the cloud VPS).
#
# The VPS runs carvilon-server in the cloud role (-role=cloud): only
# the side-channel server, no mocks / db / UDM. So this script builds
# just carvilon-server; the other binaries (mock, mqtt-spy, genkey,
# carvilon-cli) are edge/dev tools and stay arm64-only via
# build-linux-arm64.ps1.
#
# Same source as the arm64 build - edge vs cloud is a runtime flag
# (-role), not a build-time choice.

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

$env:GOOS = "linux"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"

$ldflags = "-s -w"   # strip symbol table and debug info
$buildFlags = @("-trimpath", "-ldflags=$ldflags")

New-Item -ItemType Directory -Force -Path "bin" | Out-Null

Write-Host "Building carvilon-server (linux/amd64)..."
go build @buildFlags -o bin\carvilon-server-linux-amd64 .\server\cmd\carvilon-server

# Reset env so subsequent native builds work
$env:GOOS = ""
$env:GOARCH = ""
$env:CGO_ENABLED = ""

Write-Host "Done. Binary: bin\carvilon-server-linux-amd64"
Get-ChildItem bin\carvilon-server-linux-amd64
