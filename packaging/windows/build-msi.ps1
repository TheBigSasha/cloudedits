#Requires -Version 5.1
[CmdletBinding()]
param(
    [string]$Version = "dev",
    [string]$OutDir = "",
    [switch]$SmokeTest
)

$ErrorActionPreference = "Stop"

$RootDir = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
if ([string]::IsNullOrWhiteSpace($OutDir)) {
    $OutDir = Join-Path $RootDir "dist"
}
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$OutDir = (Resolve-Path $OutDir).Path

# Windows Installer requires numeric Major.Minor.Build[.Revision].
$NumericVersion = $Version -replace '^v', ''
$NumericVersion = ($NumericVersion -split '-')[0]
if ($NumericVersion -notmatch '^\d+(\.\d+){1,3}$') {
    $NumericVersion = "0.0.0"
}

$WorkDir = Join-Path ([System.IO.Path]::GetTempPath()) ("cloudedits-msi-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $WorkDir | Out-Null

function Assert-ZeroDevSymbols {
    param([string]$BinaryPath)
    $pattern = 'devapi|registerDevRoutesImpl|handleDev(Reset|Fault|Stats)|devFaultConfig|faultReadSeeker'
    $nmOut = & go tool nm $BinaryPath 2>$null
    $nmHits = $nmOut | Select-String -Pattern $pattern
    if ($nmHits) {
        throw "dev_apis symbol leaked into release binary (nm): $BinaryPath`n$nmHits"
    }
    $bytes = [System.IO.File]::ReadAllBytes($BinaryPath)
    $ascii = [System.Text.Encoding]::ASCII.GetString($bytes)
    if ($ascii -match 'registerDevRoutesImpl|handleDev(Reset|Fault|Stats)|/__dev/(fault|reset|stats)') {
        throw "dev_apis marker leaked into release binary (bytes): $BinaryPath"
    }
    Write-Host "OK: zero dev_apis symbols in $BinaryPath"
}

function Get-PEMachineType {
    param([string]$ExePath)
    $fs = [System.IO.File]::OpenRead($ExePath)
    try {
        $br = New-Object System.IO.BinaryReader($fs)
        $fs.Seek(0x3C, [System.IO.SeekOrigin]::Begin) | Out-Null
        $peOffset = $br.ReadInt32()
        $fs.Seek($peOffset, [System.IO.SeekOrigin]::Begin) | Out-Null
        $sig = $br.ReadUInt32()
        if ($sig -ne 0x00004550) {
            throw "Invalid PE signature in $ExePath"
        }
        return $br.ReadUInt16()
    } finally {
        $fs.Dispose()
    }
}

try {
    Push-Location $RootDir

    $targets = @(
        @{ GoArch = "amd64"; WixArch = "x64";   ExpectedMachine = 0x8664; OutName = "cloudedits-windows-amd64.msi" },
        @{ GoArch = "arm64"; WixArch = "arm64"; ExpectedMachine = 0xAA64; OutName = "cloudedits-windows-arm64.msi" }
    )

    $wxsPath = Join-Path $RootDir "packaging\windows\cloudedits.wxs"
    $iconPath = Join-Path $RootDir "packaging\assets\cloudedits.ico"
    $licensePath = Join-Path $RootDir "LICENSE"
    $readmePath = Join-Path $RootDir "README.md"

    foreach ($t in $targets) {
        $goArch = $t.GoArch
        $wixArch = $t.WixArch
        $binDir = Join-Path $WorkDir $goArch
        New-Item -ItemType Directory -Force -Path $binDir | Out-Null
        $binPath = Join-Path $binDir "cloudedits.exe"

        Write-Host "==> Cross-compiling windows/$goArch ($Version)"
        $env:CGO_ENABLED = "0"
        $env:GOOS = "windows"
        $env:GOARCH = $goArch
        & go build -trimpath "-ldflags=-s -w -X main.version=$Version" -o $binPath ./cmd/cloudedits
        if ($LASTEXITCODE -ne 0) {
            throw "go build failed for windows/$goArch"
        }

        Assert-ZeroDevSymbols -BinaryPath $binPath

        $machine = Get-PEMachineType -ExePath $binPath
        if ($machine -ne $t.ExpectedMachine) {
            throw ("Unexpected PE machine header for windows/{0}: got 0x{1:X4}, expected 0x{2:X4}" -f $goArch, $machine, $t.ExpectedMachine)
        }
        Write-Host ("OK: verified PE machine header 0x{0:X4} for windows/{1}" -f $machine, $goArch)

        $msiOut = Join-Path $OutDir $t.OutName
        if (Test-Path $msiOut) {
            Remove-Item -Force $msiOut
        }

        Write-Host "==> Building $($t.OutName) with WiX v5 (arch=$wixArch, Version=$NumericVersion)"
        & wix build `
            -arch $wixArch `
            -d "Version=$NumericVersion" `
            -d "BinaryPath=$binPath" `
            -d "IconPath=$iconPath" `
            -d "LicensePath=$licensePath" `
            -d "ReadmePath=$readmePath" `
            -o $msiOut `
            $wxsPath
        if ($LASTEXITCODE -ne 0) {
            throw "wix build failed for $($t.OutName)"
        }

        $msiInfo = Get-Item $msiOut
        Write-Host ("Built {0} ({1:N0} bytes)" -f $msiInfo.FullName, $msiInfo.Length)

        if ($SmokeTest) {
            Write-Host "==> Smoke-testing $($t.OutName) via msiexec /a"
            $extractDir = Join-Path $WorkDir ("extract-" + $goArch)
            New-Item -ItemType Directory -Force -Path $extractDir | Out-Null

            $proc = Start-Process -FilePath "msiexec.exe" `
                -ArgumentList @("/a", "`"$msiOut`"", "/qn", "TARGETDIR=`"$extractDir`"") `
                -Wait -PassThru
            if ($proc.ExitCode -ne 0) {
                throw "msiexec /a failed for $($t.OutName) with exit code $($proc.ExitCode)"
            }

            $extractedExe = Get-ChildItem -Path $extractDir -Filter "cloudedits.exe" -Recurse | Select-Object -First 1
            if (-not $extractedExe) {
                throw "cloudedits.exe not found in extracted MSI directory $extractDir"
            }

            $extractedMachine = Get-PEMachineType -ExePath $extractedExe.FullName
            if ($extractedMachine -ne $t.ExpectedMachine) {
                throw ("Extracted PE machine mismatch: got 0x{0:X4}, expected 0x{1:X4}" -f $extractedMachine, $t.ExpectedMachine)
            }
            Write-Host ("OK: extracted MSI binary PE machine 0x{0:X4} ({1})" -f $extractedMachine, $extractedExe.FullName)

            $hostIsArm64 = ($env:PROCESSOR_ARCHITECTURE -eq "ARM64")
            if (($goArch -eq "amd64" -and -not $hostIsArm64) -or ($goArch -eq "arm64" -and $hostIsArm64)) {
                $verOut = & $extractedExe.FullName version --json
                if ($LASTEXITCODE -ne 0) {
                    throw "extracted cloudedits.exe version --json failed with exit code $LASTEXITCODE"
                }
                $verObj = $verOut | ConvertFrom-Json
                if ($verObj.version -ne $Version -or $verObj.os -ne "windows" -or $verObj.arch -ne $goArch) {
                    throw "version --json mismatch: $verOut"
                }
                Write-Host "OK: extracted MSI version --json verified: $verOut"
            }
        }
    }
} finally {
    Pop-Location
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $WorkDir
}
