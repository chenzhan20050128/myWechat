param(
    [string]$Base = "http://[::1]:18080",
    [int]$Port = 18080,
    [string]$Dsn = "wechat:wechat-dev-pw@tcp(127.0.0.1:3307)/wechat_dev",
    [int]$Users = 50,
    [int]$FriendsPerUser = 4,
    [int]$MessagesPerConversation = 20,
    [int]$MomentsPerUser = 10,
    [int]$Concurrency = 64,
    [int]$TargetRPS = 200,
    [int]$DurationSeconds = 60,
    [string]$Scenario = "mixed",
    [switch]$DisableRateLimit,
    [string]$Report = ".tmp\loadtest-report.json"
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

$env:WECHAT_MYSQL_DSN = $Dsn
$env:WECHAT_HTTP_ADDR = ":$Port"
$env:WECHAT_CACHE_DRIVER = "memory"
$env:WECHAT_STORAGE_DRIVER = "local"
$env:WECHAT_STORAGE_LOCAL_DIR = ".tmp\loadtest-objects"
$env:WECHAT_LOG_LEVEL = "warn"
$env:WECHAT_TRUSTED_PROXIES = "::1"
if ($DisableRateLimit) {
    $env:WECHAT_RATE_LIMIT_ENABLED = "false"
}

go run ./cmd/migrate up
go build -o .tmp\loadtest-api.exe ./cmd/api

$api = Start-Process -FilePath ".\.tmp\loadtest-api.exe" -WorkingDirectory $repo -PassThru -WindowStyle Hidden
$started = $false
try {
    for ($i = 0; $i -lt 50; $i++) {
        try {
            $health = Invoke-WebRequest -Uri "$Base/healthz" -UseBasicParsing -TimeoutSec 1
            if ($health.StatusCode -eq 200) {
                $started = $true
                break
            }
        } catch {
            Start-Sleep -Milliseconds 200
        }
    }
    if (-not $started) {
        throw "API did not become healthy at $Base"
    }

    go run ./cmd/loadtest `
        -base $Base `
        -users $Users `
        -friends-per-user $FriendsPerUser `
        -messages-per-conversation $MessagesPerConversation `
        -moments-per-user $MomentsPerUser `
        -concurrency $Concurrency `
        -target-rps $TargetRPS `
        -duration "$($DurationSeconds)s" `
        -scenario $Scenario `
        -report $Report
} finally {
    if ($api -and -not $api.HasExited) {
        Stop-Process -Id $api.Id -Force
        Wait-Process -Id $api.Id -ErrorAction SilentlyContinue
    }
}
