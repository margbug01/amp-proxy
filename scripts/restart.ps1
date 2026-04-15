# Idempotent restart script for amp-proxy on Windows.
# Kills any running instance, rebuilds, and relaunches in the background
# with logs tailed to run.log. Safe to run repeatedly.

# Stop any running amp-proxy process; ignore if none.
Stop-Process -Name amp-proxy -Force -ErrorAction SilentlyContinue

# Give the OS a moment to release the listening port.
Start-Sleep -Milliseconds 500

# Operate from the repository root (one level up from this script).
Push-Location (Join-Path $PSScriptRoot '..')
try {
    # Rebuild the binary in place.
    & go build -o amp-proxy.exe .\cmd\amp-proxy
    if ($LASTEXITCODE -ne 0) {
        Write-Error "amp-proxy build failed (exit code $LASTEXITCODE)"
        exit 1
    }

    # Relaunch hidden. logrus writes to stderr by default, so we aim stderr
    # at run.log (which matches what NOTICE.md, the restart docs, and the
    # memory system all tell the operator to tail). stdout is routed to
    # run.log.err mostly as a safety net — amp-proxy does not currently
    # write anything to stdout.
    Start-Process -FilePath .\amp-proxy.exe `
        -ArgumentList '--config', '.\config.local.yaml' `
        -WindowStyle Hidden `
        -RedirectStandardOutput .\run.log.err `
        -RedirectStandardError  .\run.log

    # Let the process boot before tailing the log.
    Start-Sleep -Seconds 1

    Write-Output "amp-proxy restarted; see .\run.log"
    Get-Content .\run.log -Tail 10
}
finally {
    Pop-Location
}
