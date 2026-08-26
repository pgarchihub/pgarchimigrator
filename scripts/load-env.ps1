# Loads KEY=VALUE lines from .env.local into the current PowerShell session
# as environment variables. Per TR-05, these variables must never be written
# to a file or a log — .env.local should already be in .gitignore.
#
# Usage (from the project root):
#   . .\scripts\load-env.ps1
#
# Note: the leading "." (dot-sourcing) matters — without it the script runs
# in its own sub-scope and the variables won't propagate to the main session.

$envFile = Join-Path $PSScriptRoot "..\.env.local"

if (-not (Test-Path $envFile)) {
    Write-Warning ".env.local not found: $envFile"
    Write-Warning "You need to create this file first (see the setup guide, Section 4)."
    return
}

Get-Content $envFile | ForEach-Object {
    $line = $_.Trim()
    if ($line -eq "" -or $line.StartsWith("#")) {
        return
    }
    $parts = $line -split "=", 2
    if ($parts.Length -eq 2) {
        $key = $parts[0].Trim()
        $value = $parts[1].Trim()
        Set-Item -Path "env:$key" -Value $value
        Write-Host "Loaded: $key"
    }
}
