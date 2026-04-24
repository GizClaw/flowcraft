# Elasticsearch

## 概述
Elasticsearch 是 Elastic 公司基于 Apache Lucene 构建的分布式开源搜索与分析引擎，于 2010 年首次发布，是 ELK 技术栈的核心。

## 主要特征
- 使用倒排索引实现高速全文检索，支持模糊、短语、布尔查询。
- 通过分片与副本机制提供水平扩展和高可用。
- 内置丰富的聚合功能，可作为实时数据分析引擎使用。
- 通过 RESTful API 暴露所有功能，开发集成简便。
- 支持中文分词器（如 ik、smart_chinese）以适应多语言场景。

## 应用与生态
Elasticsearch 是日志分析（搭配 Logstash 与 Kibana 形成 ELK Stack）、商品搜索、安全事件检测等场景的标配方案。OpenSearch 是 AWS 在 Elasticsearch 7.10 基础上 fork 出的开源分支，因许可证之争而独立维护。Lucene 作为底层索引库为整个搜索领域提供了坚实基础。
