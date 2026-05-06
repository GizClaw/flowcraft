# DuckDB

## 概述
DuckDB 是荷兰 CWI 研究所开发的嵌入式分析型数据库，于 2019 年首次发布，被誉为"分析领域的 SQLite"。

## 主要特征
- 列式存储 + 向量化执行，适合复杂分析查询。
- 嵌入式部署，作为库直接链接到应用进程内。
- 完整支持 ANSI SQL，并能直接查询 Parquet、CSV、Arrow 等文件。
- 支持读取远程对象存储（S3、GCS）中的数据文件。
- 提供 Python、R、Java、JavaScript 等多语言绑定。

## 应用与影响
DuckDB 极大地降低了数据分析门槛，常被用于 Jupyter Notebook 数据探索、ETL 中间步骤和单机数据科学工作流。它与 pandas 和 polars 形成互补，在一些场景下提供更优雅的 SQL 表达。MotherDuck 是基于 DuckDB 的云端协作平台，由原作者团队主导。
