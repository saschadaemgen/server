# Cross-compile all unifix binaries for linux/arm64 (Raspberry Pi)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

$env:GOOS = "linux"
$env:GOARCH = "arm64"
$env:CGO_ENABLED = "0"

$ldflags = "-s -w"   # strip symbol table and debug info
$buildFlags = @("-trimpath", "-ldflags=$ldflags")

New-Item -ItemType Directory -Force -Path "bin" | Out-Null

Write-Host "Building unifix-server..."
go build @buildFlags -o bin\unifix-server-linux-arm64 .\server\cmd\unifix-server

Write-Host "Building mock..."
go build @buildFlags -o bin\mock-linux-arm64 .\mock\cmd\mock

Write-Host "Building mqtt-spy..."
go build @buildFlags -o bin\mqtt-spy-linux-arm64 .\mock\cmd\mqtt-spy

Write-Host "Building genkey..."
go build @buildFlags -o bin\genkey-linux-arm64 .\server\cmd\genkey

Write-Host "Building unifix-cli..."
go build @buildFlags -o bin\unifix-cli-linux-arm64 .\server\cmd\unifix-cli

Write-Host "Building license-server..."
go build @buildFlags -o bin\license-server-linux-arm64 .\license-server\cmd\license-server

# Reset env so subsequent native builds work
$env:GOOS = ""
$env:GOARCH = ""
$env:CGO_ENABLED = ""

Write-Host "Done. Binaries in bin\"
Get-ChildItem bin\
