# hey installer for Windows — https://github.com/heypkv/hey
#
#   irm https://heypkv.ai/hey.ps1 | iex
#
# Env overrides: HEY_INSTALL_DIR (default %LOCALAPPDATA%\Programs\hey),
# HEY_VERSION (default: latest release).
$ErrorActionPreference = "Stop"

$repo = "heypkv/hey"

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    default { throw "hey installer: unsupported architecture '$($env:PROCESSOR_ARCHITECTURE)'" }
}

if ($env:HEY_VERSION) {
    $ver = $env:HEY_VERSION.TrimStart("v")
    $tag = "v$ver"
} else {
    $release = Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest"
    $tag = $release.tag_name
    $ver = $tag.TrimStart("v")
}
if (-not $ver) { throw "hey installer: could not resolve the latest release" }

$asset = "hey_${ver}_windows_${arch}.zip"
$base = "https://github.com/$repo/releases/download/$tag"

$tmp = Join-Path $env:TEMP "hey-install-$([guid]::NewGuid().ToString('N'))"
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "hey installer: downloading $asset ($tag)"
    Invoke-WebRequest "$base/$asset" -OutFile (Join-Path $tmp $asset) -UseBasicParsing
    Invoke-WebRequest "$base/checksums.txt" -OutFile (Join-Path $tmp "checksums.txt") -UseBasicParsing

    $line = Select-String -Path (Join-Path $tmp "checksums.txt") -Pattern ([regex]::Escape($asset)) |
        Select-Object -First 1
    if (-not $line) { throw "hey installer: no checksum entry for $asset" }
    $want = ($line.Line -split "\s+")[0].ToLower()
    $got = (Get-FileHash (Join-Path $tmp $asset) -Algorithm SHA256).Hash.ToLower()
    if ($got -ne $want) {
        throw "hey installer: checksum mismatch for ${asset}:`n  want $want`n  got  $got"
    }

    Expand-Archive (Join-Path $tmp $asset) -DestinationPath $tmp -Force

    $dest = if ($env:HEY_INSTALL_DIR) { $env:HEY_INSTALL_DIR }
            else { Join-Path $env:LOCALAPPDATA "Programs\hey" }
    New-Item -ItemType Directory -Force -Path $dest | Out-Null
    Copy-Item (Join-Path $tmp "hey.exe") -Destination (Join-Path $dest "hey.exe") -Force

    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (($userPath -split ";") -notcontains $dest) {
        [Environment]::SetEnvironmentVariable("Path", "$userPath;$dest", "User")
        $env:Path += ";$dest"
        Write-Host "hey installer: added $dest to your user PATH (new terminals pick it up)"
    }

    Write-Host "hey $ver installed to $(Join-Path $dest 'hey.exe')"
    & (Join-Path $dest "hey.exe") version
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
