<#
  Removes the kill switch of EchoFog from Windows when the application cannot be started: every
  filter of the provider {d2c88793-6bad-4fdf-881b-f515c796bdd7}, then its sublayer
  {af629ec5-2cd7-4717-af0e-b4e3e4672d24}, then the provider. Also the DNS rule and the DNS policy
  values the kill switch set. The same as "efv2 killswitch remove".

  Run in an elevated PowerShell (64-bit):
      powershell -ExecutionPolicy Bypass -File .\killswitch-remove.ps1
  To only look (rights are still needed to read the filtering engine; nothing is changed):
      powershell -ExecutionPolicy Bypass -File .\killswitch-remove.ps1 -List

  Only filters whose provider key is the one above are touched; the filters of Windows Firewall and of
  other programs are read and left alone.
#>
param([switch]$List)

$ErrorActionPreference = 'Stop'
$providerKey = [Guid]'d2c88793-6bad-4fdf-881b-f515c796bdd7'
$sublayerKey = [Guid]'af629ec5-2cd7-4717-af0e-b4e3e4672d24'

if ([IntPtr]::Size -ne 8) { throw 'Use the 64-bit PowerShell.' }
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not (New-Object Security.Principal.WindowsPrincipal $id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this in an elevated PowerShell (Run as administrator).'
}

Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public static class EfWfp {
    [DllImport("fwpuclnt.dll")] public static extern uint FwpmEngineOpen0(IntPtr server, uint authn, IntPtr cred, IntPtr session, out IntPtr engine);
    [DllImport("fwpuclnt.dll")] public static extern uint FwpmEngineClose0(IntPtr engine);
    [DllImport("fwpuclnt.dll")] public static extern uint FwpmFilterCreateEnumHandle0(IntPtr engine, IntPtr template, out IntPtr handle);
    [DllImport("fwpuclnt.dll")] public static extern uint FwpmFilterEnum0(IntPtr engine, IntPtr handle, uint max, out IntPtr entries, out uint num);
    [DllImport("fwpuclnt.dll")] public static extern uint FwpmFilterDestroyEnumHandle0(IntPtr engine, IntPtr handle);
    [DllImport("fwpuclnt.dll")] public static extern uint FwpmFilterDeleteByKey0(IntPtr engine, ref Guid key);
    [DllImport("fwpuclnt.dll")] public static extern uint FwpmSubLayerDeleteByKey0(IntPtr engine, ref Guid key);
    [DllImport("fwpuclnt.dll")] public static extern uint FwpmProviderDeleteByKey0(IntPtr engine, ref Guid key);
    [DllImport("fwpuclnt.dll")] public static extern void FwpmFreeMemory0(ref IntPtr p);
}
'@

function Check([uint32]$code, [string]$what, [uint32[]]$ok = @()) {
    # 0 = success; the BFE answers "not found" with these when the object is already gone
    if ($code -ne 0 -and $ok -notcontains $code) { throw ("{0} failed: 0x{1:X8}" -f $what, $code) }
}

$engine = [IntPtr]::Zero
Check ([EfWfp]::FwpmEngineOpen0([IntPtr]::Zero, 10, [IntPtr]::Zero, [IntPtr]::Zero, [ref]$engine)) 'open the filtering engine'
try {
    # collect the keys of our filters: FWPM_FILTER0 holds filterKey at offset 0 and a pointer to
    # providerKey at offset 40 (64-bit layout)
    $handle = [IntPtr]::Zero
    Check ([EfWfp]::FwpmFilterCreateEnumHandle0($engine, [IntPtr]::Zero, [ref]$handle)) 'start listing filters'
    $ours = New-Object System.Collections.Generic.List[Guid]
    $seen = 0
    try {
        while ($true) {
            $entries = [IntPtr]::Zero; $num = [uint32]0
            Check ([EfWfp]::FwpmFilterEnum0($engine, $handle, 256, [ref]$entries, [ref]$num)) 'list filters'
            if ($num -eq 0) { break }
            for ($i = 0; $i -lt $num; $i++) {
                $filter = [Runtime.InteropServices.Marshal]::ReadIntPtr($entries, $i * 8)
                $seen++
                $pp = [Runtime.InteropServices.Marshal]::ReadIntPtr($filter, 40)
                if ($pp -eq [IntPtr]::Zero) { continue }
                $prov = [Runtime.InteropServices.Marshal]::PtrToStructure($pp, [Type][Guid])
                if ($prov -eq $providerKey) {
                    $ours.Add([Runtime.InteropServices.Marshal]::PtrToStructure($filter, [Type][Guid]))
                }
            }
            [EfWfp]::FwpmFreeMemory0([ref]$entries)
        }
    } finally {
        [void][EfWfp]::FwpmFilterDestroyEnumHandle0($engine, $handle)
    }
    Write-Host ("Filters in the engine: {0}; of the EchoFog kill switch: {1}" -f $seen, $ours.Count)
    if ($List) { return }

    foreach ($k in $ours) {
        $key = $k
        Check ([EfWfp]::FwpmFilterDeleteByKey0($engine, [ref]$key)) "delete filter $k" @(0x80320003)
    }
    $key = $sublayerKey
    Check ([EfWfp]::FwpmSubLayerDeleteByKey0($engine, [ref]$key)) 'delete the sublayer' @(0x80320007, 0x80320005)
    $key = $providerKey
    Check ([EfWfp]::FwpmProviderDeleteByKey0($engine, [ref]$key)) 'delete the provider' @(0x80320005)
    Write-Host ("Removed {0} filters, the sublayer and the provider of the EchoFog kill switch." -f $ours.Count)
} finally {
    [void][EfWfp]::FwpmEngineClose0($engine)
}

if (-not $List) {
    # the DNS side of the kill switch: the rule for all names and the policy values it set
    try {
        Get-DnsClientNrptRule | Where-Object { $_.Comment -eq 'EchoFog kill switch' } | Remove-DnsClientNrptRule -Force
    } catch { Write-Warning "DNS rule: $_" }
    $state = 'HKLM:\SOFTWARE\EchoFog\KillSwitchDNS'
    $policy = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows NT\DNSClient'
    if (Test-Path $state) {
        $s = Get-ItemProperty $state
        foreach ($n in 'DisableSmartNameResolution', 'DisableParallelAandAAAA') {
            if ($s."$n.had" -eq 1) { Set-ItemProperty -Path $policy -Name $n -Value ([int]$s."$n.val") -Type DWord }
            else { Remove-ItemProperty -Path $policy -Name $n -ErrorAction SilentlyContinue }
        }
        Remove-Item $state -Recurse -Force
    }
    Remove-Item (Join-Path $env:ProgramData 'EchoFog\killswitch.on') -ErrorAction SilentlyContinue
    ipconfig /flushdns | Out-Null
    Write-Host 'Done. The network is back.'
}
