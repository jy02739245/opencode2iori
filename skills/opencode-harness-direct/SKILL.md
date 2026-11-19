---
name: opencode-harness-direct
description: "启动 opencode2api 直连服务，用 deepseek-v4.1-flash-free 执行真实 DSH 连续对话、read/edit/skill 和辅助请求 SSE 回归，并排查辅助请求 429。"
---

# OpenCode 直连 Harness 回归

启动当前 opencode2api 项目的本地服务，用 `deepseek-v4.1-flash-free` 运行真实 Harness 连续对话、工具适配和辅助请求流式回归。

## 前置核对

- 配套脚本为项目内 `tools/harness_direct.py`。当前项目位置 `/Users/xizhiming/development/opencode2iori`；优先使用当前工作目录中的脚本，项目迁移后重新定位。
- 先核对本会话技能和工具目录；涉及 DSH 事实时加载 `dsh-doc`，代码修改遵循 `ponytail`。
- 需要 `python3`（仅标准库）、`go`、`dsh`、项目现有 `config.json`。凭据仅进入请求或子进程环境，输出与报告均省略凭据。
- 保留主模型与原生辅助任务，检查真实 SSE 消费链和响应类型。

## 执行

1. 运行 `python3 tools/harness_direct.py prepare`，记录输出的真实 `run_dir`。脚本保留账号配置，将 API 监听设为 `127.0.0.1:8080`、`proxies:["direct"]`、清空 proxyfile、关闭 WARP；配置有修改时先保存权限 0600 的备份。当前代码构建到独立 `.local/harness-direct-*` 目录。构建失败时读取该目录的 `build.log`。
2. 查看已知服务作业，复用或停止本会话管理的旧网关。后台运行 `python3 tools/harness_direct.py serve --run-dir '[RUN_DIR]'` 并记录作业 ID。仅处理已核实属于本项目的服务。服务日志写入该目录的 `service.log`。
3. 运行 `python3 -u tools/harness_direct.py test --run-dir '[RUN_DIR]'`，跟踪后台作业并及时报告进展。测试通过真正的 `dsh headless --json --session-id` 执行，使用隔离 DSH_HOME 和独立工作区，清除代理环境变量。
4. 四轮依次检查记忆随机标签、同会话回忆、实际调用 `read/edit/skill`、工具调用后继续回忆。检查实际文件、tool_result 和最终输出。逐行转发的本地观察器记录每个请求的 stream、状态和返回类型。
5. 读取 `verification-summary.json`、`requests.json`、`real-summary.json`、`service.log`。核对所有请求 `stream:true`、成功响应为 `text/event-stream`、同 session 四轮完成、三种工具成功。`core_checks_passed` 仅表示主会话/工具/流式检查，辅助 HTTP 状态单列于 `auxiliary_statuses` 和 `auxiliary_requests_successful`。核心失败时脚本非零退出。
6. 返回 API `http://127.0.0.1:8080/v1`、管理页 `http://127.0.0.1:8081`、实际结果及证据链接。保留用户要求运行的网关；结束无关测试作业。用户未要求时不提交或推送。

## 最终报告格式

- 最后给用户的测试报告必须以表格展示，并同步生成表格版 `REPORT.md`。先用一句话概括结论，再展示表格，避免仅用连续段落或项目符号罗列结果。
- 表格使用「检查项 / 结果 / 实测说明 / 证据」四列；按实际证据覆盖模型与直连配置、DSH 版本、同会话完成轮次、`read/edit/skill` 与实际文件核对、主请求与辅助请求 HTTP 状态、全部请求 `stream:true`、成功响应 SSE 类型、原生辅助流是否收到 `[DONE]`，以及实际检查过的服务健康状态。API、管理页和证据链接也放入表格。
- 结果区分「通过 / 失败 / 未完成 / 未检查」；核心检查与辅助流完整消费单列。辅助 HTTP 200、客户端取消或诊断重放成功均不直接等于原生辅助任务完成。诊断仅在实际执行时列出；尚未执行的检查标明「未检查」。失败原因与取消原因写入对应行。
- 会话界面支持 GenUI 时，加载 `genui`，使用 `dsh-ui` 的 `table` 组件展示最终表格并校验规格；其他界面使用 Markdown 表格。落盘的 `REPORT.md` 使用标准 Markdown 表格，保留可点击的实际证据链接，不写入 GenUI JSON。
- 若补充脚本未采集的上游状态、健康检查或诊断结论，先核对真实日志或实测结果，再补入表格，不推断或硬编码通过。

## 辅助 429 诊断

`stream:true` 选择传输模式，上游仍可能在响应头阶段返回 HTTP 错误。匿名通道原本已强制上游流式；错误日志应在请求解析后及时记录客户端 stream 偏好。

- `requests.json` 保留辅助请求正文和错误正文；`first-main-request.json` 保留隔离测试生成的首个主请求。二者均省略认证头。
- 需要复现时执行 `python3 tools/harness_direct.py diagnose --run-dir '[RUN_DIR]'`。它只做三次有限对照：原始无工具标题、加入捕获到的真实 DSH read 声明、再次原始标题；模型、消息、stream 和出口保持一致。只记录响应，不执行诊断响应中的工具调用。该对照只改变客户端声明；当前网关会给三种请求都前置整套官方工具，因此历史 429/200/429 对照不再预期复现。
- 读取 `auxiliary-diagnosis.json`，检查 HTTP 状态、SSE、`upstream_legs` 和 `synthetic_title`。服务日志中的 `anon:200` 与 `anon:429` 比客户端状态更能区分上游成功和本地兜底。
- 2026-11-19 实测：原始无工具标题 429；加入真实 DSH read 声明并经现有官方契约适配后上游 200/SSE；移除该声明再次 429。反向对照中，成功主请求去掉 tools 也返回 429。证据支持当前免费端点对请求形态的限制，服务端完整判定实现尚未公开。以当前实测为准。
- V1/V2 均先声明整套官方工具，包括空工具辅助请求；客户端同名定义只从上游请求中移除，使用官方名称、参数、描述和顺序；客户端 schema 仍保留给返回转换。其余客户端工具以原有不透明名称追加。上游返回后恢复客户端工具名，转换已知 read/edit/skill/write 字段别名；必填别名优先于可选的官方拼写。客户端明确设置 additionalProperties:false 时移除未声明的根字段；省略或允许额外字段时保留。保留嵌套值，避免凭空补值或强制类型转换。整套声明本身不会给客户端新增工具执行器；客户端未安装的官方工具仍没有本地实现。
- 已识别的标题可能触发原有 `titleFallback`，得到 `chatcmpl-title-*` 的本地合成 SSE。此时客户端 200 仍可能对应上游 429，应明确区分。仅补标题识别规则不等于解决上游 429。
- headless 每次只处理一项任务，主回答结束后进程退出；原生标题可能已取得上游 200/SSE，随后因客户端退出出现 `client_canceled` 或 `BrokenPipeError`。这不等于 429，也不算辅助任务完整完成；查看 `auxiliary_requests_successful`，并用 `diagnose` 单独读取完整 SSE。对照重放成功只证明请求链路成功，不替代原生标题任务完成检查。
- 保存失败与对照证据，保持真实辅助错误可见。必要的修改应配合针对性回归和真实 Harness 复测。
