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

- `summary` 必须是非空字符串。
- `releases` 至少包含一项。
- `module` 只能是 `sdk`、`memory`、`sdkx` 或 `voice`。
- `bump` 只能是 `patch` 或 `minor`。
- 同一文件中不能重复声明同一模块。
- 多个未消费 changeset 声明同一模块时，发布计划采用最高 bump（`minor` 高于 `patch`）。
- changeset 是否已消费由文件是否存在于对应模块的最新 `module/vX.Y.Z` tag 中决定。

CLI 是独立 Go module。从仓库根目录运行：

```sh
cd tools/releasegate
GOWORK=off go run . validate --repo ../.. --base origin/main
GOWORK=off go run . plan --repo ../..
GOWORK=off go run . plan --repo ../.. --json
```

`plan --json` 输出模块计划、GitHub Actions matrix 和待创建 tags。若没有未消费的发布意图，命令成功并输出空数组。
