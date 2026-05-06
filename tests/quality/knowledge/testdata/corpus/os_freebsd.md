# FreeBSD

## 概述
FreeBSD 是源自 4.4BSD-Lite 的开源类 Unix 操作系统，由 FreeBSD 基金会维护，强调系统完整性、稳定性与许可证宽松。

## 主要特征
- 遵循 BSD 许可证，可被自由用于商业产品而无须开源衍生作品。
- 提供完整的内核 + 用户态组件，由同一开发团队维护。
- ZFS 文件系统在 FreeBSD 上具有一流的支持。
- Jail 机制是早期容器技术的代表，影响了后来的 LXC 与 Docker。
- Ports 与 Pkg 提供数万种软件包的源码与二进制安装方案。

## 应用与影响
FreeBSD 长期被网络设备厂商、CDN 公司和高性能服务器领域采用。Netflix 的视频分发节点大量基于 FreeBSD 调优。WhatsApp 在被 Facebook 收购前的服务器栈也以 FreeBSD 为主。macOS 内核 Darwin 借鉴了大量 FreeBSD 用户态组件，可见其在 BSD 家族中的核心地位。
