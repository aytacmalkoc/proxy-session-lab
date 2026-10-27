$AppDir = $PSScriptRoot
$Python = Join-Path $AppDir '.venv\Scripts\python.exe'
if (-not (Test-Path $Python)) {
    Write-Error "Standalone virtual environment not found: $AppDir\.venv"
    exit 2
}
$LocalBrowsers = Join-Path $AppDir '.browsers'
if ((Test-Path $LocalBrowsers) -and -not $env:PLAYWRIGHT_BROWSERS_PATH) {
    $env:PLAYWRIGHT_BROWSERS_PATH = $LocalBrowsers
}
& $Python (Join-Path $AppDir 'app.py') @args
exit $LASTEXITCODE
