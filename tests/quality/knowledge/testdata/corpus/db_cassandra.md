# Cassandra

## 概述
Apache Cassandra 是一款最初由 Facebook 开发的分布式 NoSQL 数据库，2008 年开源后成为 Apache 顶级项目，专为大规模写入和高可用场景设计。

## 主要特征
- 采用去中心化对等架构，无单点故障。
- 数据按一致性哈希分布到多个节点，支持线性水平扩展。
- 提供可调一致性级别，权衡性能与正确性。
- 写入性能极高，使用 LSM 树存储引擎。
- 支持 CQL（Cassandra Query Language），语法接近 SQL。

## 应用与生态
Cassandra 被 Apple、Netflix、Uber 等公司用于海量数据存储和实时分析。DataStax 是 Cassandra 商业化背后的主要公司，提供企业版和云托管服务。AstraDB 是基于 Cassandra 的 Serverless 托管平台，将其能力以 API 形式开放，便于在云原生应用中集成。
