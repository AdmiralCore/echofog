<#
  Runs the WFP integration test of the EchoFog kill switch INSIDE A VIRTUAL MACHINE.

  The test installs real persistent Windows Filtering Platform filters that block the network of the
  machine. NEVER run this on a working or a shared computer. Use Hyper-V (a checkpoint first) or
  Windows Sandbox.

  On the host (builds the test binary once):
      cd echofog-core-oss
      $env:GOWORK = 'off'
      go test -c -tags wfpvm -o wfpvm.test.exe ./internal/v2/tun/winwfp
  Copy wfpvm.test.exe and this script into the VM, then in the VM, in an elevated PowerShell:
      .\run-wfpvm.ps1

  If the VM loses its network and the test did not finish: efv2 killswitch remove (elevated), or
  the PowerShell script deploy/v2/killswitch-remove.ps1 (docs/kill-switch-windows.md), or revert the checkpoint.
#>
param([string]$Test = (Join-Path $PSScriptRoot 'wfpvm.test.exe'))

$ErrorActionPreference = 'Stop'
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not (New-Object Security.Principal.WindowsPrincipal $id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this in an elevated PowerShell inside the virtual machine.'
}
$bios = (Get-CimInstance Win32_ComputerSystem)
$text = ("$($bios.Manufacturer) $($bios.Model)").ToLower()
if ($text -notmatch 'virtual|vmware|qemu|kvm|xen|parallels|hyper-v') {
    throw "This does not look like a virtual machine ($text). Refusing: the test cuts the network."
}
if (-not (Test-Path $Test)) { throw "Test binary not found: $Test" }

$env:ECHOFOG_WFP_VM = 'VM'
& $Test -test.v -test.run TestRealFiltersInTheVM -test.timeout 10m
$code = $LASTEXITCODE

# Whatever happened, show the state and leave the network back.
$left = (netsh wfp show filters file="$env:TEMP\ef-after.xml") | Out-Null
$xml = Get-Content "$env:TEMP\ef-after.xml" -Raw
$n = ([regex]::Matches($xml, '(?i)<providerKey>\{d2c88793-6bad-4fdf-881b-f515c796bdd7\}</providerKey>')).Count
Write-Host "EchoFog filters left after the test: $n"
if ($n -ne 0) { Write-Warning 'Filters are left; remove them: efv2 killswitch remove'; $code = 1 }
exit $code
