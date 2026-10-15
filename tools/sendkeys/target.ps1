# SendInput 端到端验证:起一个 WinForms 文本框,聚焦它,等外部注入 Ctrl+V,
# 然后把文本框内容写文件。用于验证 winapi.SendCtrlV 是否真的能注入按键。
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing

$form = New-Object System.Windows.Forms.Form
$form.Text = 'PasteWinSendKeysTarget'
$form.Size = New-Object System.Drawing.Size(500, 200)
$form.StartPosition = 'CenterScreen'
$form.TopMost = $true

$tb = New-Object System.Windows.Forms.TextBox
$tb.Multiline = $true
$tb.Dock = 'Fill'
$tb.Font = New-Object System.Drawing.Font('Consolas', 12)
$form.Controls.Add($tb)

$form.Add_Shown({
    $form.Activate()
    $tb.Focus()
})

# 泵消息,让窗口保持响应并接收注入的按键
$deadline = (Get-Date).AddSeconds(9)
while ((Get-Date) -lt $deadline) {
    [System.Windows.Forms.Application]::DoEvents()
    Start-Sleep -Milliseconds 40
}

$out = $args[0]
if (-not $out) { $out = "$env:TEMP\pastewin_sendkeys_result.txt" }
Set-Content -Path $out -Value $tb.Text -Encoding UTF8
$form.Close()
