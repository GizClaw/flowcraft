# Transformer

## 概述
Transformer 是 Vaswani 等人在 2017 年论文《Attention is All You Need》中提出的深度学习架构，用自注意力机制取代了传统的循环结构。

## 主要内容
- 核心组件是多头自注意力（Multi-Head Self-Attention）模块。
- 通过位置编码（Positional Encoding）显式引入序列位置信息。
- 编码器—解码器架构在机器翻译等序列到序列任务中表现优异。
- 残差连接与 Layer Normalization 是稳定训练的关键技巧。
- 计算可高度并行化，比 RNN 更适合大规模分布式训练。

## 应用与影响
Transformer 已成为现代大模型的基石架构，BERT、GPT、T5、LLaMA、PaLM、ChatGPT 全都基于其变体。Vision Transformer（ViT）将该架构推广到计算机视觉，AlphaFold2 用其加速蛋白质结构预测。Transformer 推动了"预训练+微调"范式，使大模型时代加速到来，被认为是过去十年最重要的算法突破之一。
