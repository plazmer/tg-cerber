param(
    [ValidateSet("amd64", "arm64")]
    [string]$Arch = "amd64",
    [string]$OutputDir = "dist"
)

$ErrorActionPreference = "Stop"

if (-not (Test-Path $OutputDir)) {
    New-Item -ItemType Directory -Path $OutputDir | Out-Null
}

$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = $Arch

$outputPath = Join-Path $OutputDir "tg-anti-spam-linux-$Arch"
go build -o $outputPath ./cmd/bot

Write-Host "Build completed: $outputPath"
