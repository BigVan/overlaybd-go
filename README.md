# overlaybd-go

参考 [containerd/overlaybd](https://github.com/containerd/overlaybd) 实现的最小 Go 本地块存储。Linux 通过 `io_uring` 接入 ublk；Windows 11 通过本机 iSCSI Target 与系统内置 Initiator 接入磁盘。无需 CGo、liburing、libublksrv 或运行时调用 overlaybd 程序。

## 功能与范围

| 模块 | 已实现 |
| --- | --- |
| ublk | CLI 创建、前台运行、停止/删除；读写、flush、FUA、discard、write-zeroes；只读设备 |
| iSCSI | 回环 TCP、单会话、LUN 0；发现、读写、SCSI flush/FUA、只读层；Windows/Linux |
| LSMT | 上游 v1.1 append / hybrid 格式；data/index 文件对；覆盖写、空洞、零区间、索引重放；提交为只读层 |
| 文件 I/O | 可写 data 支持 buffered psync、buffered io_uring、O_DIRECT io_uring；index 与 lower 保持 buffered |
| 分层 | 多个本地只读层加可选可写顶层；上层覆盖下层；自动识别 ZFile 包装 |
| ZFile | LZ4、ZSTD；4–64 KiB 独立压缩块；随机解压读取；头尾、索引和数据块 CRC32C 校验 |

这是功能最小版：Linux ublk 使用单硬件队列，iSCSI 使用单会话、单连接；提供可选文件 I/O 后端和内存区间索引。暂不支持远程镜像、OCI/containerd 集成、在线快照、sparse/warp LSMT、ZFile 字典/压缩索引、tar 包装或 ublk 崩溃后自动恢复。LSMT 仅接受 tag 为 0 的本地映射；带非零 tag 的预标记层也会被拒绝，避免把外部文件引用误当成本地数据。

两种平台共用 LSMT/ZFile 磁盘格式，构建均要求 Go 1.23+，支持 amd64/arm64。按平台选择创盘流程：

- [Windows：通过 iSCSI 创盘](#windows通过-iscsi-创盘)
- [Linux：通过 ublk 创盘](#linux通过-ublk-创盘)

`create` 默认创建 append 层，每次写入追加数据及映射；加 `--hybrid` 可启用已有映射的原地覆盖。新建文件不会覆盖已有文件，重新使用磁盘时不要再次 `create` 或格式化。

## Windows：通过 iSCSI 创盘

在 Windows 11 上运行 `overlaybd-go.exe`，通过回环 TCP 提供 iSCSI Target，再由系统内置 Microsoft iSCSI Initiator 连接为本机磁盘。无需 WSL、Linux 虚拟机、自定义驱动或 Windows Server 的 iSCSI Target 角色。Windows 仅支持 `--io-engine 0`，使用原生 buffered 文件 I/O 和排他文件锁。

### 1. 构建并创建磁盘文件

在仓库根目录的 PowerShell 中执行，以下创建一块 10 GiB 的 hybrid 磁盘：

```powershell
go build -o bin/overlaybd-go.exe ./cmd/overlaybd-go
New-Item -ItemType Directory -Force C:\overlaybd
.\bin\overlaybd-go.exe create --data C:\overlaybd\disk.data --index C:\overlaybd\disk.index --size 10GiB --hybrid
```

也可以在 Linux 上执行 `make windows`，将生成的 `bin/overlaybd-go-windows-amd64.exe` 复制到 Windows 的 `bin` 目录并重命名为 `overlaybd-go.exe`。使用预编译程序时，Windows 无需安装 Go。

### 2. 启动 iSCSI Target

```powershell
.\bin\overlaybd-go.exe iscsi --data C:\overlaybd\disk.data --index C:\overlaybd\disk.index --listen 127.0.0.1:3260 --target iqn.2026-11.io.overlaybd:disk
```

就绪后输出包含 `portal`、`targetName`、`lun`、`size`、`readOnly` 的 JSON。程序在前台运行，保持该终端开启。端口被占用时，可修改 `--listen` 的端口，并在连接时使用相同端口。

### 3. 通过本机 Initiator 连接

另开一个**管理员 PowerShell**，在仓库根目录执行：

```powershell
.\scripts\connect-iscsi.ps1 -TargetName iqn.2026-11.io.overlaybd:disk -Port 3260
```

脚本会启动 `MSiSCSI` 服务、添加或刷新 portal、连接 Target 并显示关联磁盘；不会格式化磁盘或注册开机自动重连。如果只复制了可执行文件，首次连接也可以直接运行系统命令：

```powershell
Start-Service MSiSCSI
New-IscsiTargetPortal -TargetPortalAddress 127.0.0.1 -TargetPortalPortNumber 3260
Connect-IscsiTarget -NodeAddress iqn.2026-11.io.overlaybd:disk -TargetPortalAddress 127.0.0.1 -TargetPortalPortNumber 3260 -IsPersistent $false -IsMultipathEnabled $false -AuthenticationType NONE
Get-IscsiSession | Where-Object TargetNodeAddress -eq 'iqn.2026-11.io.overlaybd:disk' | Get-Disk
```

### 4. 首次初始化并格式化

确认上一步显示的是新建磁盘、容量为 10 GiB。**以下操作仅用于新建空磁盘**，会建立 GPT、NTFS 和盘符；已有分区的磁盘跳过此步。

```powershell
$disks = @(Get-IscsiSession | Where-Object TargetNodeAddress -eq 'iqn.2026-11.io.overlaybd:disk' | Get-Disk)
if ($disks.Count -ne 1 -or $disks[0].IsBoot -or $disks[0].IsSystem -or $disks[0].PartitionStyle -ne 'RAW' -or $disks[0].IsReadOnly) {
    throw '没有唯一的新建可写空磁盘，请检查 target 和磁盘状态'
}
$disk = $disks[0]
$disk | Set-Disk -IsOffline $false
$disk | Initialize-Disk -PartitionStyle GPT -PassThru |
    New-Partition -UseMaximumSize -AssignDriveLetter |
    Format-Volume -FileSystem NTFS -NewFileSystemLabel OverlayBD -Confirm:$false
```

之后可通过分配的盘符访问文件。

### 5. 断开和重新打开

关闭使用该卷的应用和文件句柄，在管理员 PowerShell 中先离线、断开：

```powershell
Get-IscsiSession | Where-Object TargetNodeAddress -eq 'iqn.2026-11.io.overlaybd:disk' | Get-Disk | Set-Disk -IsOffline $true
Disconnect-IscsiTarget -NodeAddress iqn.2026-11.io.overlaybd:disk -Confirm:$false
```

再在 Target 终端按 Ctrl-C，程序同步后端并退出。重新使用时，重复第 2、3 步，保持原来的 data/index 路径和 IQN；若磁盘仍为 Offline，将它上线：

```powershell
Get-IscsiSession | Where-Object TargetNodeAddress -eq 'iqn.2026-11.io.overlaybd:disk' | Get-Disk | Set-Disk -IsOffline $false
```

当前 iSCSI 仅支持回环地址、单会话、LUN 0 和一个在途 SCSI 命令，无 CHAP、多路径或后台 Windows 服务；不对其他机器导出。不同磁盘应使用不同 IQN。portal 移除、兼容性说明和实测结果见 [Windows 操作说明](scripts/windows.md)。

## Linux：通过 ublk 创盘

Linux ublk 使用 ioctl 编码命令，要求 Linux 6.6+、`CONFIG_BLK_DEV_UBLK` 和可用的 `io_uring`。运行 ublk 通常需要 root；本地文件操作不需要 root。

### 1. 构建并加载内核模块

```bash
make build
sudo modprobe ublk_drv
./bin/overlaybd-go --help
```

### 2. 创建磁盘文件并启动 ublk

```bash
# 创建 1 GiB 虚拟磁盘，初始只占用两个文件头。
./bin/overlaybd-go create --data disk.data --index disk.index --size 1GiB

# 前台运行；自动分配未使用的设备编号。
sudo ./bin/overlaybd-go serve --data disk.data --index disk.index
```

启动成功后输出 JSON，例如：

```json
{"id":0,"path":"/dev/ublkb0","size":1073741824,"readOnly":false}
```

### 3. 首次格式化并挂载

另一个终端中，对**实际返回的设备路径**操作。以下 `mkfs.ext4` 仅用于新建空磁盘；已有文件系统的磁盘直接挂载。

```bash
sudo mkfs.ext4 /dev/ublkb0
sudo mkdir -p /mnt/overlaybd-go
sudo mount /dev/ublkb0 /mnt/overlaybd-go
```

之后可通过 `/mnt/overlaybd-go` 访问文件。

### 4. 卸载和重新打开

先卸载文件系统：

```bash
sudo umount /mnt/overlaybd-go
```

卸载后，在服务终端按 Ctrl-C，程序会同步后端并停止、删除设备。也可以显式执行 `sudo ./bin/overlaybd-go delete --id 0`。同一对 data/index 文件可重新 `serve`，恢复此前持久化的数据。强制杀死进程后残留的设备需要显式 `delete`。

`serve --id N` 指定编号，`--queue-depth N` 设置深度（1–128，默认 32）。这是前台服务，不会自动 daemonize。

重新执行第 2 步中的 `serve` 命令，然后挂载实际返回的设备路径即可。设备编号可能改变，不需要再次 `create` 或 `mkfs`。`serve` 根据文件头自动识别 append/hybrid 模式。

### 可选：选择文件 I/O 后端

`serve`、`read`、`write` 和 `commit` 支持 `--io-engine 0|3|4`，默认值为 `0`。这些编号与 C++ OverlayBD 的文件后端 `ioEngine` 对应；ublk 请求队列本身也使用 io_uring，两者是不同层次。

| `--io-engine` | 可写 upper 的 data 文件 | index 与只读 lower |
|---|---|---|
| `0`（默认） | buffered psync，通过普通文件读写使用 page cache | buffered |
| `3` | buffered io_uring，数据读写仍使用 page cache | buffered |
| `4` | `O_DIRECT` io_uring，数据读写绕过 page cache | buffered |

后端在打开文件时选择，不改变 append/hybrid 格式；同一对文件关闭后可以切换后端重新打开。`create` 只创建格式，不接收 `--io-engine`。引擎 `3`/`4` 要求同时提供 `--data` 和 `--index`，不能用于只有 `--lower` 的只读设备。`commit --io-engine` 选择待提交 upper 的读取后端，输出只读层仍按原方式写入。

```bash
# 可写 data 使用 buffered io_uring。
sudo ./bin/overlaybd-go serve --data disk.data --index disk.index --io-engine 3

# 服务停止后，可用 direct io_uring 操作同一文件对。
./bin/overlaybd-go write --data disk.data --index disk.index --io-engine 4 --offset 512 --input sector.bin
./bin/overlaybd-go read --data disk.data --index disk.index --io-engine 4 --offset 513 --length 511 --output sector-part.bin
./bin/overlaybd-go commit --data disk.data --index disk.index --io-engine 3 --output direct-base.lsmt
```

示例中的 `sector.bin` 应包含 512 字节数据。三个后端都保留 **512 字节对齐的逻辑写入**和**任意字节偏移/长度的读取**；调用方无需为 direct 后端对齐内存缓冲区。direct 后端将新的物理 extent 按 4 KiB 对齐并补齐空间，映射只引用有效逻辑数据。hybrid 覆盖如果物理起点或该段长度不是 4 KiB 的倍数，会把该段重定位到新追加的 extent，因此部分扇区覆盖可能使 data 文件增长。

## LSMT 提交与 ZFile

以下存储操作两种平台均支持。文件操作示例使用 Linux 命令写法；Windows 将程序路径改为 `.\bin\overlaybd-go.exe`，并替换为实际的文件路径。导出块设备时，Linux 使用 `serve`，Windows 使用 `iscsi`。

先停止使用该可写层的服务，再提交：

```bash
./bin/overlaybd-go commit --data disk.data --index disk.index --output base.lsmt
./bin/overlaybd-go zfile compress --input base.lsmt --output base.zfile --algorithm lz4
./bin/overlaybd-go zfile verify --input base.zfile

# 以只读块设备暴露压缩层，按需解压，不先展开整个镜像。
sudo ./bin/overlaybd-go serve --lower base.zfile

# 或叠加新的可写层。所有层的虚拟大小必须一致。
./bin/overlaybd-go create --data upper.data --index upper.index --size 1GiB
sudo ./bin/overlaybd-go serve --lower base.zfile --data upper.data --index upper.index
```

可重复 `--lower`，顺序从底层到顶层。提交仅包含当前层的映射，保留空洞和显式零区间；不自动合并下层。层关系由命令行顺序确定，当前版本不强制校验 parent UUID 链。

Windows 导出只读层或叠加层时使用以下对应命令；连接方式与前面的 Windows 创盘流程相同。`upper.data/index` 应预先创建，容量与 lower 一致：

```powershell
.\bin\overlaybd-go.exe iscsi --lower base.zfile --target iqn.2026-11.io.overlaybd:disk
```

或在停止上述 Target 后导出带可写 upper 的磁盘：

```powershell
.\bin\overlaybd-go.exe iscsi --lower base.zfile --data upper.data --index upper.index --target iqn.2026-11.io.overlaybd:disk
```

ZFile 也可独立压缩普通文件：

```bash
./bin/overlaybd-go zfile compress --input input.bin --output input.zfile --algorithm zstd --block-size 4KiB
./bin/overlaybd-go zfile decompress --input input.zfile --output restored.bin
```

默认 4 KiB 压缩块。Go 实现支持 64 KiB 块；部分上游旧版本对不可压缩的 64 KiB 块存在读取缓冲区限制，即使文件由其自身生成也无法验证。需与这些版本交换文件时使用默认 4 KiB 或不超过 32 KiB 的块。

## 不创建设备的文件 I/O

两种平台均可直接读写文件，不需要启动 ublk/iSCSI。Windows 使用 `.\bin\overlaybd-go.exe` 和实际文件路径执行相同子命令。

```bash
# input.bin 的长度和写入偏移均须为 512 字节的倍数。
./bin/overlaybd-go write --data disk.data --index disk.index --offset 0 --input input.bin
./bin/overlaybd-go read --data disk.data --index disk.index --offset 0 --length 4KiB --output result.bin
./bin/overlaybd-go read --lower base.zfile --offset 513 --length 100 --output result-part.bin
```

读取支持任意字节偏移；未映射区间返回零。所有写入必须在创建时设定的大小以内。大小支持字节整数或 `B/KiB/MiB/GiB/TiB`。新建文件使用排他创建，已有文件不会被覆盖。

## Go 接口与持久化

- `lsmt.Create` / `CreateHybrid` / `OpenRW`：默认使用 buffered psync；返回 `ReadAt`、`WriteAt`、`Zero`、`Sync`、`Commit`、`Close` 接口，`Hybrid` 查询写入模式。
- `lsmt.OpenRWWithOptions`：通过 `OpenOptions.IOEngine` 选择 `PSync`、`IOUringBuffered` 或 `IOUringDirect`，只影响可写 data 文件。
- `lsmt.OpenRO`：自动识别普通 LSMT 或 ZFile 包装的 LSMT。
- `lsmt.NewStack`：成功后拥有传入的 layers，`Close` 关闭全部层；失败时所有权仍属于调用者。
- `zfile.Open` / `NewReader`：提供 `ReadAt`、`Size`、`Close`。`NewReader` 不关闭调用者提供的源。
- `zfile.Compress`：流式生成 ZFile，目标应为空且位于文件起点。
- `ublk.Serve`：阻塞运行至 context 取消，返回前清理设备；后端由调用者关闭。它会在固定的 ublk 队列 OS 线程上调用后端可选的 `BindIOThread`；io_uring 文件后端据此让该线程的 buffered 写入和 direct I/O 使用专用 inline ring。
- `iscsi.Serve`：在回环 TCP 上导出 LUN 0，阻塞至 context 取消；退出前关闭连接、等待在途操作并同步后端，后端由调用者关闭。

下面的 io_uring 示例用于 Linux；Windows 将 `IOEngine` 设置为 `lsmt.PSync`。

```go
layer, err := lsmt.OpenRWWithOptions("disk.data", "disk.index", lsmt.OpenOptions{
    IOEngine: lsmt.IOUringBuffered, // 或 lsmt.IOUringDirect / lsmt.PSync
})
if err != nil {
    return err
}
defer layer.Close()
```

可写文件使用排他文件锁（Linux 为 advisory `flock`，Windows 为 `LockFileEx`），防止本实现同时打开多个写句柄。普通写不逐次调用 `Sync`/`fsync`：引擎 `0`/`3` 完成数据及所需索引的 page cache 写入后返回，引擎 `4` 等待 direct 数据写入完成，而索引仍进入 page cache。显式 `Sync`、块设备 flush/FUA 和正常关闭时，等待在途操作完成，再先同步 data、后同步 index；io_uring 后端的数据同步使用 `IORING_OP_FSYNC`，索引使用普通文件 `Sync`。CLI 文件操作正常退出会关闭文件，因此仍包含结束时同步。未同步写入的持久性及数据/索引落盘顺序不保证；异常断电可能留下不完整或不一致的尾部，hybrid 原地覆盖也可能发生撕裂写。重新打开时重放完整索引记录，并截去未完成的最后一条索引和不足一个扇区的数据尾部。上游格式本身没有索引日志校验和：损坏的完整记录会被检查并拒绝，不能保证修复所有断电撕裂写。创建文件名的目录持久化由调用者负责。

ZFile Reader 和 LSMT File 可并发调用。可写 LSMT 使用 B-tree 区间索引，替换 k 个相交区间需要 O((k+1) log n) 操作；只读层保留紧凑有序数组。单文件最多 8,388,608 条索引记录/压缩块，LSMT 写索引达到限制后应提交并建立新的顶层。没有后台压缩或应用层块缓存；buffered 后端使用 OS page cache，direct data I/O 绕过它。

LSMT 数据追加使用原子偏移分配，`WriteAt` 不持有串行化整个写入的独占锁。索引日志追加和映射发布保持相同顺序，内存索引有独立读写锁；普通 I/O 使用共享生命周期锁，`Sync`、关闭和提交等待在途操作完成。并发重叠写按映射发布顺序可见，跨多个映射的大写入不保证整体原子性。

Linux ublk 服务的引擎 `0` 在单个队列线程上同步处理请求；引擎 `3`/`4` 均支持多请求在途，并在队列空闲且只有一个请求时直接在队列线程处理，减少 worker 交接。引擎 `3` 的普通读取也在队列线程处理，数据 I/O 仍通过 io_uring 提交。并发路径在 flush/FUA 屏障和停止服务前等待相应在途操作完成。

文件后端的 buffered 写入和 direct I/O 识别已绑定的队列 OS 线程，在该线程使用专用的 inline ring；worker 使用共享的 128-entry ring，最多保留 64 个在途请求。buffered 读取继续使用各自的 ring。共享提交器可将同一文件中连续且按 4 KiB 对齐的写入合并为 `WRITEV`，每次最多 1 MiB / 64 个 iovec；读取不参与合并。Go 缓冲区固定至对应 CQE 完成后才解除固定。ublk 数据队列和单请求文件 ring（包括 inline ring 与 buffered 读取 ring）启用 `COOP_TASKRUN`；共享文件提交器使用 `flags=0`。

hybrid 的逻辑范围锁与 direct 文件 I/O 的物理范围锁均使用 4096 个锁条带，协调重叠范围的读写与清零；不同范围可以并行，条带哈希冲突只影响并发度。它们不使用覆盖整个文件的写入独占锁。

## 验证

```bash
make test
make check

# 可选：与已安装的上游工具进行双向格式互操作验证。
OVERLAYBD_UPSTREAM_BIN=/opt/overlaybd/bin go test ./integration -v

# 真实 ublk 冒烟：append/hybrid × 引擎 0/3/4，共六组；自动清理临时文件和设备。
make integration

# 强制执行真实 io_uring 文件后端测试，内核不可用时失败而不是跳过。
FILEIO_REQUIRE_URING=1 go test -race ./internal/fileio -count=1

# 真实 ublk 内核队列：验证多在途、flush/FUA 屏障和停止时排空。
sudo env OVERLAYBD_UBLK_TEST=1 go test -race ./ublk -run TestConcurrentKernelQueue -count=1 -v

# 两种模式交替交给 Go / C++ 读写，验证 payload 与原地覆盖。
sudo python3 scripts/interop-lsmt.py --cpp-bin /opt/overlaybd/bin

# 默认 psync 两种模式的 QD64 随机覆盖、CRC32C 校验和索引重放。
sudo python3 scripts/verify-ublk-fio.py --fio /path/to/fio --output /tmp/overlaybd-fio-verify

# Go/C++ × append/hybrid × 引擎 3/4：每组五阶段，共 40 项 CRC32C 校验。
# 包含写入后校验、同引擎重开、3↔4 重开、psync 重开、跨 Go/C++ 重开。
sudo python3 scripts/verify-ioengines.py --go-bin ./bin/overlaybd-go \
  --cpp-bin /opt/overlaybd/bin --fio /path/to/fio --output /tmp/overlaybd-ioengine-verify

# 10 GiB 映射规模的局部写入回归检查（稀疏数据，不替代设备基准）。
python3 scripts/check-write-benchmark.py
```

普通单元测试不需要 root 或 ublk；真实 io_uring 测试在内核不可用或被权限策略禁用时可能跳过，`FILEIO_REQUIRE_URING=1` 可让文件后端测试严格失败。`OVERLAYBD_UBLK_TEST=1` 单独启用需要设备权限的内核队列测试。测试覆盖随机覆盖写、零区间、层叠加、提交/重开、非对齐缓冲区与字节读取、512 字节逻辑写入、direct 物理补齐、部分尾部恢复、损坏文件、并发 I/O 和 CLI；ZFile 包含由上游工具生成的小型固定样本。CRC 验证还检查实际 data/index 的 `O_DIRECT` 标志，并用块设备写入计数确认只读重开校验没有写入。

Go/C++ 文件引擎 `0`/`3`/`4` 与 loop 的 **10 GiB、三轮、390 项**完整矩阵已完成，数据完整性审计通过。按 ioEngine 分组的 9 组 PNG/SVG 柱形图、全部原始结果与复现命令见 [Go io_uring 完整对照报告](docs/performance-go-uring-10g-2026-11-05.md)。按同模式、同引擎三轮中位数比较，**70/90 项达到 Go ≥ C++ 90%**：引擎 `0` 为 29/30，`3` 为 26/30，`4` 为 15/30；目标尚未全部达到，剩余差距主要集中在 direct 随机读和 hybrid direct 随机写。顺序写存在明显轮次波动，完整范围已保留在图中。

以下报告保留为历史记录，不与新版本的同批次测试混合统计：

- [Go psync append/hybrid、C++ 与 loop 的 10 GiB 对照](docs/performance-go-cpp-loop-10g-2026-11-05.md)：对应新增 Go 文件 io_uring 之前的实现；服务就绪后的线程亲和性未核验。
- [C++ io_uring 补充对照与柱形图](docs/performance-ioengines-10g-2026-11-05.md)：Go 仅作为当时的 psync 基准，Go io_uring 未在该批中测量；同样未核验队列线程亲和性。
- [旧的 1 GiB 报告](docs/performance-go-vs-cpp-2026-11-04.md)：对应移除逐次数据同步之前的实现。

上述两份 10 GiB 报告只在启动时设置 `taskset`，而 C++ 的 libublksrv 队列初始化会覆盖设置，因此不能作为严格 CPU 受控的比较。新正式测试已在服务就绪后重新绑定全部已枚举线程，并通过 374 个服务实例的启动和结束核验；当前结论以新的 [Go io_uring 完整对照报告](docs/performance-go-uring-10g-2026-11-05.md) 为准。

## 参考

- [LSMT 文件布局](https://github.com/containerd/overlaybd/blob/main/src/overlaybd/lsmt/file.cpp) 与 [映射编码](https://github.com/containerd/overlaybd/blob/main/src/overlaybd/lsmt/index.h)
- [ZFile 文件布局](https://github.com/containerd/overlaybd/blob/main/src/overlaybd/zfile/zfile.cpp) 与 [压缩参数](https://github.com/containerd/overlaybd/blob/main/src/overlaybd/zfile/compressor.h)
- [Linux ublk 文档](https://docs.kernel.org/block/ublk.html)、[ublk UAPI](https://github.com/torvalds/linux/blob/v6.8/include/uapi/linux/ublk_cmd.h)、[io_uring UAPI](https://github.com/torvalds/linux/blob/v6.8/include/uapi/linux/io_uring.h)

采用 Apache-2.0 许可证。上游 OverlayBD 的格式定义和实现参考归 The Overlaybd Authors 所有。
