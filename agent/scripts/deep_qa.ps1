# PrintBridge deep QA
#
# Builds the agent, starts a throwaway instance in a temp directory, and runs the
# protocol checks in deep_qa.mjs through the built SDK. Never touches a real printer
# unless one is named with -Printer, so it is safe to run on a live till's machine.
#
# Works on stock Windows PowerShell 5.1 as well as PowerShell 7:
#
#   powershell -ExecutionPolicy Bypass -File agent/scripts/deep_qa.ps1
#   powershell -ExecutionPolicy Bypass -File agent/scripts/deep_qa.ps1 -Port 9801 -Printer "XP-80C"

param(
    [int]$Port = 9799,
    [string]$Printer = ""
)

$ErrorActionPreference = "Stop"

$root = Resolve-Path (Join-Path $PSScriptRoot "..\..")
$agentDir = Join-Path $root "agent"
$sdkDir = Join-Path $root "sdk"
$work = Join-Path $env:TEMP ("printbridge-qa-" + [guid]::NewGuid().ToString("N").Substring(0, 8))

New-Item -ItemType Directory -Force -Path $work | Out-Null
Write-Host "=== PrintBridge deep QA ==="
Write-Host "workspace : $work"
Write-Host "port      : $Port"
if ($Printer) { Write-Host "printer   : $Printer" }

# 1. Build the agent and make sure the SDK bundle exists.
Push-Location $agentDir
go build -o "$work\printbridge-agent.exe" .
Pop-Location

if (-not (Test-Path (Join-Path $sdkDir "dist\index.js"))) {
    Write-Host "building the SDK..."
    Push-Location $sdkDir
    npm run build | Out-Null
    Pop-Location
}

# 2. A throwaway configuration. max_attempts = 1 keeps the dead-letter path quick.
$config = @{
    server = @{ port = $Port }
    queue  = @{ max_attempts = 1; max_depth = 50; max_concurrent_jobs = 2 }
} | ConvertTo-Json -Depth 5
$config | Set-Content (Join-Path $work "config.json") -Encoding UTF8

# 3. Start the agent.
$agent = Start-Process -FilePath "$work\printbridge-agent.exe" `
    -ArgumentList "-headless", "-db", "$work\qa.db", "-config", "$work\config.json" `
    -PassThru -WindowStyle Hidden -RedirectStandardError "$work\agent.log"

$exitCode = 1
try {
    $health = $null
    for ($i = 0; $i -lt 40; $i++) {
        try {
            # Invoke-WebRequest -UseBasicParsing is the reliable form on Windows
            # PowerShell 5.1: Invoke-RestMethod needs the IE engine and times out on its
            # first call in a script context.
            $response = Invoke-WebRequest "http://localhost:$Port/health" -UseBasicParsing -TimeoutSec 5
            $health = $response.Content | ConvertFrom-Json
            if ($health.service -eq "printbridge-agent") { break }
        } catch { }
        Start-Sleep -Milliseconds 400
    }

    if (-not $health -or $health.service -ne "printbridge-agent") {
        throw "the agent did not answer /health on port $Port"
    }

    Write-Host ("[ok]   /health :: service={0} status={1} port={2}" -f $health.service, $health.status, $health.port)

    # 4. Protocol checks through the SDK.
    $env:PB_QA_PORT = "$Port"
    $env:PB_QA_PRINTER = $Printer
    node (Join-Path $PSScriptRoot "deep_qa.mjs")
    $exitCode = $LASTEXITCODE
}
finally {
    if ($agent -and -not $agent.HasExited) {
        Stop-Process -Id $agent.Id -Force -ErrorAction SilentlyContinue
    }
    Write-Host "`n--- agent log (last 10 lines) ---"
    Get-Content (Join-Path $work "agent.log") -ErrorAction SilentlyContinue | Select-Object -Last 10
    Write-Host "`nworkspace kept for inspection: $work"
}

exit $exitCode
