# Release changesets

每个 `.release/*.json` 文件都是不可变的发布意图。文件合入后不得修改、重命名或删除；后续修正应新增 changeset。

```json
{
  "summary": "Add streaming retries",
  "releases": [
    {
      "module": "sdk",
      "bump": "patch"
    }
  ]
}
```

- `summary` 必须是非空单行字符串，且不能包含 releasegate 保留 marker。
- `releases` 至少包含一项。
- `module` 只能是 `sdk`、`memory`、`sdkx` 或 `voice`。
- `bump` 只能是 `patch` 或 `minor`。
- 同一文件中不能重复声明同一模块。
- 多个未消费 changeset 声明同一模块时，发布计划采用最高 bump（`minor` 高于 `patch`）。
- changeset 是否已消费由文件是否存在于对应模块的最新 `module/vX.Y.Z` tag 中决定。

CLI 是独立 Go module。从仓库根目录运行：

```sh
make release-check
make release-check BASE=origin/main
make release-plan
make release-changelog
```

`plan --json` 输出模块计划、GitHub Actions matrix 和待创建 tags。若没有未消费的发布意图，命令成功并输出空数组。

changeset 合入 `main` 后，`Release modules` workflow 会聚合所有待处理
`summary`，更新 `CHANGELOG.md` 的模块版本表和 release sections，并创建或
更新 `automation/release-changelog` Release PR。该 PR 的内容由
`make release-changelog` 确定；通常不需要在功能 PR 中手动提交生成结果。

Release PR 合并后，workflow 会再次验证 changelog、执行模块 gates，并在全部
检查通过后原子创建所有计划 tags。Release PR 未合并、changelog 不一致或任一
gate 失败时均不会创建 tag。
