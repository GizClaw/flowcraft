# PostgreSQL

## 概述
PostgreSQL 是历史悠久的开源对象关系型数据库管理系统，起源于加州大学伯克利分校的 POSTGRES 项目，被誉为"最先进的开源数据库"。

## 主要特征
- 完整支持 ACID 事务，使用 MVCC 实现多版本并发控制。
- 类型系统极为丰富，支持 JSON、JSONB、数组、范围类型等。
- 支持表分区、并行查询、外部数据包装器（FDW）。
- 通过扩展机制支持 PostGIS（地理信息）、TimescaleDB（时序）。
- pg_dump 与 WAL 归档提供成熟的备份与时间点恢复方案。

## 应用与生态
PostgreSQL 在金融、政府、地理信息、数据分析等领域广泛使用，被多家云厂商作为托管服务（Amazon RDS、Google Cloud SQL、Azure Postgres）提供。许多新一代数据系统如 CockroachDB、TimescaleDB、Citus 都构建在 PostgreSQL 协议或源代码之上，体现其强大的可扩展性。
