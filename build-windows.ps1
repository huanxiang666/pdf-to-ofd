$ErrorActionPreference = 'Stop'
Push-Location $PSScriptRoot
try {
    go run github.com/akavel/rsrc@v0.10.2 -arch amd64 -manifest app.manifest -o rsrc_windows_amd64.syso
    if ($LASTEXITCODE -ne 0) { throw 'Manifest resource generation failed' }
    go build -ldflags='-s -w -H windowsgui' -o pdf-to-ofd.exe .
    if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
    Write-Host 'Built pdf-to-ofd.exe (Windows amd64 GUI)'
} finally { Pop-Location }
