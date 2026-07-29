# PrintBridge Cross-Platform Release Build Script
$ErrorActionPreference = "Stop"

$GOROOT = "C:\Program Files\Go"
$env:GOROOT = $GOROOT
$env:PATH = "$GOROOT\bin;" + $env:PATH

$AgentDir = Resolve-Path "$PSScriptRoot\.."
$BuildDir = "$AgentDir\build"

Write-Host "=== PrintBridge Release Builder ==="

if (Test-Path $BuildDir) {
    Remove-Item -Recurse -Force $BuildDir
}

New-Item -ItemType Directory -Path "$BuildDir\windows-amd64" | Out-Null
New-Item -ItemType Directory -Path "$BuildDir\linux-amd64" | Out-Null

Set-Location $AgentDir

# 1. Windows x64
Write-Host "Building Windows x64 binary..."
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -ldflags="-s -w" -o "$BuildDir\windows-amd64\printbridge-agent.exe" .

# 2. Linux x64
Write-Host "Building Linux amd64 binary..."
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -ldflags="-s -w" -o "$BuildDir\linux-amd64\printbridge-agent" .

# Reset env variables
$env:GOOS = ""
$env:GOARCH = ""

Write-Host "=== Release Build Successfully Completed! ==="
Get-ChildItem -Recurse $BuildDir | Select-Object Name, Length
