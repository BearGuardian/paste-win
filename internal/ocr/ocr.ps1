# Recognize text in an image using the OCR engine built into Windows,
# and write the result to stdout as UTF-8.
#
# Why shell out to PowerShell instead of calling the API from Go:
# Windows.Media.Ocr is a WinRT API. Calling it from Go means hand-writing
# COM/WinRT vtable interop, which is not practical; and Tesseract-style
# engines need cgo, which conflicts with this project's CGO_ENABLED=0
# build constraint. PowerShell 5.1 ships the WinRT projection, so it is
# the only route available on a stock Windows box.
#
# This file is deliberately ASCII-only. PowerShell 5.1 reads .ps1 files
# using the ANSI code page unless they carry a UTF-8 BOM, so any non-ASCII
# text here would be mis-decoded and can break parsing on a GBK system.
#
# Usage:
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File ocr.ps1 -Path <image>
#
# Exit codes: 0 ok / 2 no OCR engine / 3 image could not be read

param([Parameter(Mandatory = $true)][string]$Path)

$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8

Add-Type -AssemblyName System.Runtime.WindowsRuntime | Out-Null

# WinRT async methods return IAsyncOperation<T>, which PowerShell cannot
# await directly. Grab the generic AsTask overload from
# System.Runtime.WindowsRuntime and close it over the result type.
$asTaskGeneric = [System.WindowsRuntimeSystemExtensions].GetMethods() |
    Where-Object {
        $_.Name -eq 'AsTask' -and
        $_.GetParameters().Count -eq 1 -and
        $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation`1'
    } |
    Select-Object -First 1

function Await($operation, $resultType) {
    $task = $asTaskGeneric.MakeGenericMethod($resultType).Invoke($null, @($operation))
    $task.Wait(-1) | Out-Null
    $task.Result
}

# Load the WinRT types first, otherwise the [Type,Assembly,ContentType=...]
# literals below cannot be resolved.
[Windows.Storage.StorageFile, Windows.Storage, ContentType = WindowsRuntime] | Out-Null
[Windows.Graphics.Imaging.BitmapDecoder, Windows.Graphics.Imaging, ContentType = WindowsRuntime] | Out-Null
[Windows.Media.Ocr.OcrEngine, Windows.Media.Ocr, ContentType = WindowsRuntime] | Out-Null

# Prefer the user's own languages; fall back to Simplified Chinese.
$engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromUserProfileLanguages()
if (-not $engine) {
    $lang = New-Object Windows.Globalization.Language 'zh-Hans-CN'
    $engine = [Windows.Media.Ocr.OcrEngine]::TryCreateFromLanguage($lang)
}
if (-not $engine) {
    [Console]::Error.WriteLine('no OCR engine available')
    exit 2
}

try {
    $file = Await ([Windows.Storage.StorageFile]::GetFileFromPathAsync($Path)) ([Windows.Storage.StorageFile])
    $stream = Await ($file.OpenAsync([Windows.Storage.FileAccessMode]::Read)) ([Windows.Storage.Streams.IRandomAccessStream])
    $decoder = Await ([Windows.Graphics.Imaging.BitmapDecoder]::CreateAsync($stream)) ([Windows.Graphics.Imaging.BitmapDecoder])
    $bitmap = Await ($decoder.GetSoftwareBitmapAsync()) ([Windows.Graphics.Imaging.SoftwareBitmap])
}
catch {
    [Console]::Error.WriteLine("could not read image: $_")
    exit 3
}

$result = Await ($engine.RecognizeAsync($bitmap)) ([Windows.Media.Ocr.OcrResult])
Write-Output $result.Text
