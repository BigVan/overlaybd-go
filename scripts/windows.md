# Windows 11 本机 iSCSI 磁盘

`overlaybd-go.exe` 读取本地 LSMT/ZFile，在回环 TCP 上提供 iSCSI Target；Windows 内置 Microsoft iSCSI Initiator 将 LUN 0 呈现为磁盘。程序直接处理块请求，不需要 WSL、Linux 虚拟机、自定义驱动或 Windows Server 的 iSCSI Target 角色。

## 构建和启动

Windows 上安装 Go 1.23+ 后，在仓库根目录执行：

```powershell
go build -o bin/overlaybd-go.exe ./cmd/overlaybd-go
New-Item -ItemType Directory -Force C:\overlaybd
.\bin\overlaybd-go.exe create --data C:\overlaybd\disk.data --index C:\overlaybd\disk.index --size 10GiB --hybrid
.\bin\overlaybd-go.exe iscsi --data C:\overlaybd\disk.data --index C:\overlaybd\disk.index --listen 127.0.0.1:3260 --target iqn.2026-11.io.overlaybd:disk
```

也可以在 Linux 上运行 `make windows`，复制生成的 `bin/overlaybd-go-windows-amd64.exe` 到 Windows 并重命名。`create` 拒绝覆盖已有文件；已有磁盘只需重新运行 `iscsi`。

`iscsi` 前台运行，就绪后输出包含 `portal`、`targetName`、`lun`、`size`、`readOnly` 的 JSON。保持该进程运行。默认端口已占用时，可选择另一个回环端口，并在连接时使用相同端口。

## 连接

另开一个**管理员 PowerShell**，在仓库根目录执行：

```powershell
.\scripts\connect-iscsi.ps1
```

该脚本启动 `MSiSCSI` 服务、添加或刷新回环 portal、连接指定 target，并显示关联磁盘。它不初始化或格式化磁盘，也不注册开机自动重连。自定义参数：

```powershell
.\scripts\connect-iscsi.ps1 -TargetName iqn.2026-11.io.overlaybd:disk -Port 3260
```

对应的系统命令如下（首次连接）：

```powershell
Start-Service MSiSCSI
New-IscsiTargetPortal -TargetPortalAddress 127.0.0.1 -TargetPortalPortNumber 3260
Connect-IscsiTarget -NodeAddress iqn.2026-11.io.overlaybd:disk -TargetPortalAddress 127.0.0.1 -TargetPortalPortNumber 3260 -IsPersistent $false -IsMultipathEnabled $false -AuthenticationType NONE
Get-IscsiSession | Where-Object TargetNodeAddress -eq 'iqn.2026-11.io.overlaybd:disk' | Get-Disk
```

## 首次初始化

确认输出对应新建磁盘，容量正确且分区样式为 RAW。以下操作仅用于新建空磁盘，会建立 GPT 和 NTFS：

```powershell
$disks = @(Get-IscsiSession | Where-Object TargetNodeAddress -eq 'iqn.2026-11.io.overlaybd:disk' | Get-Disk)
if ($disks.Count -ne 1 -or $disks[0].IsBoot -or $disks[0].IsSystem -or $disks[0].PartitionStyle -ne 'RAW') {
    throw '没有唯一的新建空磁盘，请检查 target 和磁盘状态'
}
$disk = $disks[0]
$disk | Set-Disk -IsOffline $false
$disk | Initialize-Disk -PartitionStyle GPT -PassThru |
    New-Partition -UseMaximumSize -AssignDriveLetter |
    Format-Volume -FileSystem NTFS -NewFileSystemLabel OverlayBD -Confirm:$false
```

之后可以通过分配的盘符访问。已有分区的磁盘不要重复初始化。

## 断开和重新打开

关闭使用该卷的应用和文件句柄，先在管理员 PowerShell 中离线、断开，然后在 Target 终端按 Ctrl-C：

```powershell
Get-IscsiSession | Where-Object TargetNodeAddress -eq 'iqn.2026-11.io.overlaybd:disk' | Get-Disk | Set-Disk -IsOffline $true
Disconnect-IscsiTarget -NodeAddress iqn.2026-11.io.overlaybd:disk -Confirm:$false
```

再次运行相同 `iscsi` 命令和连接脚本即可恢复。已有磁盘可能保持 Offline，可通过关联的 `Get-Disk | Set-Disk -IsOffline $false` 上线。需要彻底移除 portal 时使用 `Remove-IscsiTargetPortal`，共享同一 portal 的其他 Target 仍在使用时不要移除。

部分 Windows 版本的 `Remove-IscsiTargetPortal -TargetPortalPortNumber` 将端口传为 `Int32`，但 CIM 方法要求 `UInt16`，导致类型不匹配；省略端口则会使用默认 3260。清理非默认端口时，可显式调用 [CIM Remove 方法](https://learn.microsoft.com/en-us/previous-versions/windows/desktop/iscsidisc/msft-iscsitargetportal-remove)：

```powershell
$port = 3269
Get-IscsiTargetPortal | Where-Object {
    $_.TargetPortalAddress -eq '127.0.0.1' -and $_.TargetPortalPortNumber -eq $port
} | ForEach-Object {
    Invoke-CimMethod -InputObject $_ -MethodName Remove -Arguments @{
        TargetPortalAddress = '127.0.0.1'
        TargetPortalPortNumber = [uint16]$port
    }
}
```

## 范围和限制

- Windows 使用 buffered 文件后端（引擎 `0`）；`3/4` 是 Linux io_uring，Windows 会明确拒绝。append、hybrid、层叠加、commit、ZFile 及 CLI 文件操作共用同一磁盘格式。
- `--lower` 可重复使用；只有 lower 时导出只读 LUN。带 upper 时仍要求所有层容量一致。
- 仅监听回环 IP，不提供 CHAP 或 CRC digest；本机其他进程也可以连接。一个 Target 只接受一个正常会话、一个连接、一个在途 SCSI 命令；不支持多路径、共享集群磁盘或远程导出。
- 支持 READ/WRITE(6/10/12/16)、INQUIRY/VPD、REPORT LUNS、READ CAPACITY(10/16)、MODE SENSE、SYNCHRONIZE CACHE 与写 FUA。TRIM/UNMAP、持久预留、任务恢复和后台 Windows 服务暂未实现；Windows 仍可使用普通读写和格式化。
- 缓存页声明写缓存开启；FUA 和 SYNCHRONIZE CACHE 等待后端同步。中断未完整传输的写入不会提交给后端；电源故障等持久化限制与 README 中的 LSMT 说明一致。
- IQN 决定稳定磁盘标识。同一块磁盘重新打开时保留 IQN；不同磁盘使用不同 IQN。

## 验证

Linux/Windows 都可以运行 `go test -race ./iscsi`（Windows race 需要可用的 C 工具链），或运行不带 `-race` 的测试。Windows 文件后端测试：`go test ./lsmt ./zfile`。

管理员 PowerShell 可以运行 `scripts/test-windows-iscsi.ps1 -Executable <exe绝对路径>`，它仅使用新建临时 LSMT、独立 IQN、随机回环端口，并在确认磁盘属于该会话且为新建空盘后进行 NTFS 文件校验；结束时断开并移除该 portal。加 `-Hybrid` 可验证 hybrid 模式。测试还会重启 Target 并复查文件哈希，日志和镜像留在输出的临时目录中。

2026-11-14 已在 Windows 11 x64（系统版本 10.0.26200.0、Windows PowerShell 5.1）实测：LSMT、iSCSI 和 ZFile 原生测试通过；append 与 hybrid 均完成 128 MiB 本机 iSCSI 磁盘连接、GPT/NTFS 格式化、4 MiB 随机文件写入及显式刷新、Target 重启与重新连接后的 SHA-256 一致性校验。该验证覆盖功能与持久化流程，不代表性能、断电一致性或长期运行验证。

协议依据：[RFC 7143](https://www.rfc-editor.org/rfc/rfc7143.html)。Windows 命令依据：[New-IscsiTargetPortal](https://learn.microsoft.com/en-us/powershell/module/iscsi/new-iscsitargetportal)、[Connect-IscsiTarget](https://learn.microsoft.com/en-us/powershell/module/iscsi/connect-iscsitarget)、[Get-IscsiSession](https://learn.microsoft.com/en-us/powershell/module/iscsi/get-iscsisession)。
