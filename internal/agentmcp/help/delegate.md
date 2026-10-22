# 委派

`steve_fleet(requires?)` 返回其他 agent 的机器、能力、可用性和拒绝原因。选择器包括 `harness:codex`、`tool:docker`、`hardware:gpu`、`model:claude*`、`skill:<name>`、`mcp:github`、`tag:<name>`。network、credential、a2a 是观察项，不参与放置。

## 创建子任务

`steve_delegate(goal, agent?, requires?, refs?, expect?)` 将有明确边界的目标交给独立会话。提供必要背景、所需资料和完成条件；子任务不继承父会话全部上下文。

可以指定 agent，或用 requires 选择目标。refs 使用工具支持的内容引用，如 `git <commit-or-branch>`、`blob <digest>`。返回的 task_id 和 state 是后续查询身份；短暂等待后子任务可能仍在执行。

子任务使用目标机器上的隔离工作树，由项目快照物化。快照按收录规则包含未提交文件；是否有 Git commit 不是文件可见性的依据。不要要求子任务通过创建嵌套仓库来提交 Steve 结果。以实际快照、引用和工作区状态确认输入。

## 结果与落地

父回合可以结束，等待 Steve 把持久化结果送回原会话并续跑，无需循环轮询。需要主动查询时使用 `steve_await(task_id, wait_seconds?)`，单次最多等待 50 秒。

子任务结果绑定到 `steve/<子任务>/result`。主目录可写时合并落地，目录忙碌时排队，冲突单独记录。子任务完成不等于文件已落地，报告时引用实际 task_id、agent、node、产物和落地状态。

子任务共享父任务预算，受深度和环检测限制。父回合正常结束不停止子任务；用户停止或取消相关任务时，执行按平台停止规则处理。

## 选择合适的工作

独立路径、明确交付、需要另一机器工具或能力的工作适合委派。同一批文件的并行修改有合并成本。需要用户决定的范围、权限或目标不能让子 agent 猜测。
