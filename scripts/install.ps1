# dokploy-tunnel - doktunnel installer for Windows.
#
# Usage (PowerShell 5.1+ or PowerShell 7+):
#   irm https://raw.githubusercontent.com/alebak/dokploy-tunnel/main/scripts/install.ps1 | iex
#
# Environment variables:
#   DOKTUNNEL_VERSION      Version to install (e.g. 0.1.0 or v0.1.0). Default: latest release.
#   DOKTUNNEL_INSTALL_DIR  Target directory. Default: %LOCALAPPDATA%\Programs\doktunnel.
#
# Test only (never needed to install doktunnel):
#   DOKTUNNEL_TEST_DOWNLOAD_BASE
#                          Download the archive and checksums.txt from this
#                          http://127.0.0.1:<port>[/path] or http://localhost:<port>[/path]
#                          URL instead of the GitHub release. The platform CI
#                          workflow uses it to install locally built snapshot
#                          artifacts. Requires DOKTUNNEL_VERSION; the checksum is
#                          still verified.
#
# The script runs inside a function and reports failures with `throw` instead of
# `exit`, so piping it to `iex` never closes the caller's PowerShell session.

function Install-Doktunnel {
    [CmdletBinding()]
    param()

    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'

    $owner = 'alebak'
    $repo = 'dokploy-tunnel'
    $binary = 'doktunnel'

    # Windows PowerShell 5.1 may default to TLS 1.0, which GitHub rejects.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    Write-Host ''
    Write-Host 'dokploy-tunnel - doktunnel installer' -ForegroundColor Cyan
    Write-Host ''

    # Detect architecture. PROCESSOR_ARCHITEW6432 is set when a 32-bit shell runs on a 64-bit OS.
    $rawArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    switch ($rawArch) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { throw "Unsupported architecture: $rawArch" }
    }

    # Only this machine is accepted, so the override cannot point an install
    # at another server.
    $testBase = $null
    if ($env:DOKTUNNEL_TEST_DOWNLOAD_BASE) {
        $testBase = $env:DOKTUNNEL_TEST_DOWNLOAD_BASE.TrimEnd('/')
        if ($testBase -cnotmatch '^http://(127\.0\.0\.1|localhost):[0-9]+(/[A-Za-z0-9._/-]*)?\z') {
            throw 'DOKTUNNEL_TEST_DOWNLOAD_BASE is test-only and must be http://127.0.0.1:<port> or http://localhost:<port>'
        }
        if (-not $env:DOKTUNNEL_VERSION) { throw 'DOKTUNNEL_TEST_DOWNLOAD_BASE requires DOKTUNNEL_VERSION' }
    }

    if ($env:DOKTUNNEL_VERSION) {
        $version = $env:DOKTUNNEL_VERSION.TrimStart('v')
    } else {
        try {
            $latest = Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$owner/$repo/releases/latest"
        } catch {
            throw "Failed to resolve the latest release from GitHub: $($_.Exception.Message)"
        }
        if (-not $latest.tag_name) { throw 'Could not determine the latest version.' }
        $version = $latest.tag_name.TrimStart('v')
    }

    $archive = "${binary}_${version}_windows_${arch}.zip"
    $baseUrl = if ($testBase) { $testBase } else { "https://github.com/$owner/$repo/releases/download/v$version" }
    $installDir = if ($env:DOKTUNNEL_INSTALL_DIR) { $env:DOKTUNNEL_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\$binary" }

    Write-Host "[ok]      Platform: windows_$arch" -ForegroundColor Green
    Write-Host "[ok]      Version:  $version" -ForegroundColor Green

    $workDir = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString())
    New-Item -ItemType Directory -Path $workDir | Out-Null

    try {
        $archivePath = Join-Path $workDir $archive
        $checksumsPath = Join-Path $workDir 'checksums.txt'

        Write-Host ''
        Write-Host "Downloading $archive..."
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/$archive" -OutFile $archivePath
            Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/checksums.txt" -OutFile $checksumsPath
        } catch {
            throw "Failed to download release files for v${version}: $($_.Exception.Message)"
        }

        Write-Host 'Verifying checksum...'
        $expected = $null
        foreach ($line in Get-Content -LiteralPath $checksumsPath) {
            $parts = $line -split '\s+', 2
            if ($parts.Count -eq 2 -and $parts[1].TrimStart('*') -eq $archive) {
                $expected = $parts[0].ToLowerInvariant()
                break
            }
        }
        if (-not $expected) { throw "No checksum found for $archive in checksums.txt" }

        $actual = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($expected -ne $actual) {
            throw "Checksum mismatch for $archive (expected $expected, got $actual)"
        }
        Write-Host '[ok]      Checksum verified' -ForegroundColor Green

        Write-Host 'Extracting...'
        $extractDir = Join-Path $workDir 'extract'
        Expand-Archive -LiteralPath $archivePath -DestinationPath $extractDir -Force
        $exe = Join-Path $extractDir "$binary.exe"
        if (-not (Test-Path -LiteralPath $exe)) { throw "Archive does not contain $binary.exe" }

        New-Item -ItemType Directory -Path $installDir -Force | Out-Null
        Copy-Item -LiteralPath $exe -Destination (Join-Path $installDir "$binary.exe") -Force
    } finally {
        Remove-Item -LiteralPath $workDir -Recurse -Force -ErrorAction SilentlyContinue
    }

    Write-Host ''
    Write-Host "[ok]      Installed $binary to $installDir\$binary.exe" -ForegroundColor Green

    # Add the install directory to the user PATH (no administrator rights required).
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $entries = @()
    if ($userPath) { $entries = $userPath -split ';' | Where-Object { $_ } }
    if ($entries -notcontains $installDir) {
        [Environment]::SetEnvironmentVariable('Path', (($entries + $installDir) -join ';'), 'User')
        Write-Host "[ok]      Added $installDir to your user PATH" -ForegroundColor Green
    }
    if (($env:Path -split ';') -notcontains $installDir) {
        $env:Path = "$env:Path;$installDir"
        Write-Host 'Open a new terminal for the PATH change to apply to other sessions.' -ForegroundColor Cyan
    }

    Write-Host ''
    Write-Host "Run '$binary --version' to check the installation."
    Write-Host ''
}

Install-Doktunnel
