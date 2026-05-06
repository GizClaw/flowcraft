# Redis

## 概述
Redis 是 Salvatore Sanfilippo 于 2009 年开源的高性能内存键值数据库，全称为 Remote Dictionary Server，常用作缓存、消息队列和计数器。

## 主要特征
- 数据全部驻留内存，单线程处理命令保证原子性。
- 支持字符串、哈希、列表、集合、有序集合、位图、HyperLogLog 等多种数据结构。
- 通过 RDB 快照与 AOF 日志提供持久化方案。
- 提供主从复制、哨兵和 Cluster 集群方案。
- 支持发布订阅和 Stream 数据结构，可作消息总线。

## 应用与生态
Redis 是最流行的缓存方案，被电商、社交、实时排行榜等场景广泛采用。Redis Cluster 支持水平分片，可承载海量数据。Redis 6 引入了多线程 IO，提升了网络吞吐能力。RedisJSON、RediSearch 等模块进一步扩展了 Redis 的能力边界，使其超越传统键值存储。
