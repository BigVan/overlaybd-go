#Requires -RunAsAdministrator
param(
    [string]$TargetName = 'iqn.2026-11.io.overlaybd:disk',
    [ValidateRange(1, 65535)][int]$Port = 3260
)

$ErrorActionPreference = 'Stop'
Start-Service MSiSCSI
$portal = @(Get-IscsiTargetPortal | Where-Object {
    $_.TargetPortalAddress -eq '127.0.0.1' -and $_.TargetPortalPortNumber -eq $Port
})
if ($portal.Count -eq 0) {
    New-IscsiTargetPortal -TargetPortalAddress '127.0.0.1' -TargetPortalPortNumber $Port | Out-Null
} else {
    Update-IscsiTargetPortal -TargetPortalAddress '127.0.0.1' -TargetPortalPortNumber $Port | Out-Null
}
$sessions = @(Get-IscsiSession | Where-Object TargetNodeAddress -eq $TargetName)
if ($sessions.Count -eq 0) {
    Connect-IscsiTarget -NodeAddress $TargetName -TargetPortalAddress '127.0.0.1' -TargetPortalPortNumber $Port -IsPersistent $false -IsMultipathEnabled $false -AuthenticationType NONE | Out-Null
}
for ($attempt = 0; $attempt -lt 30; $attempt++) {
    $disks = @(Get-IscsiSession | Where-Object TargetNodeAddress -eq $TargetName | Get-Disk)
    if ($disks.Count -gt 0) {
        $disks | Select-Object Number, FriendlyName, SerialNumber, Size, PartitionStyle, IsOffline, IsReadOnly
        return
    }
    Start-Sleep -Seconds 1
}
throw 'iSCSI session connected but no disk appeared within 30 seconds'
