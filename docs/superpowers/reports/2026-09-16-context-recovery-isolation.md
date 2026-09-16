# Paca / Hermes 本轮本地验收

2026-09-16。已完成批准范围内的上下文恢复、权限隔离与撤权停止修复；专用实例为 http://localhost:3300。应用提交 `34f5445f5f040d2e4b5444620359a35e26c5e726`，分支 `codex/context-recovery-isolation`。未推送、合并或部署生产；原 localhost:3000 实例未改动。

## 已完成行为

- 任务详情、编号、列表/搜索/聚合、管理写入及相关评论、活动、标注、关联、附件、自动化入口统一使用当前对象权限；旧任务创建者不按可编辑 reporter 回填。
- 私聊 owner、项目、Agent、conversation 与实例身份由后端持久记录派发；任务上下文引用在接收、排队、运行与恢复时重验。经理任务权限不能读取他人私聊。
- Hermes 使用逐会话 home/work/tmp 和实际 macOS 沙箱；同任务 A/B 原生历史搜索、按对方 ID 加载、memory/file 读取均隔离，同时保留自己的工具正常对照。
- 原生 session 绑定原子持久化，首次 prompt 在保存成功后发送；停止、重连、bridge 重启、子进程退出及闲置回收后加载原 session。恢复验证原生身份和非空匹配历史；不把旧重播存成新事件或执行旧工具。归属/映射/load 错误保留历史并拒绝空上下文继续。
- 权限变更通过持久代次和参与进程屏障，停止受影响排队/活动运行并等待原生清理；投递前查询当前收件人，在同一同步 emit 前核对代次。撤权后的延迟 allow 和 Socket.IO 缓存重播均被拒绝。
- OPEN 异步确认期间保留原事件，OPEN 后重新授权；API 409 只重试原事件。真实栈已确认合法收件人的任务事件不再丢失。

## 验收结果与复用规则

`acceptance-results.json` 逐项记录有效结果、原始来源和文件 SHA256，共 **50 项通过**。这不是一次重跑50项：先保留代码未变的通过结果，再覆盖本次受影响项；原失败文件没有改写。

| 检查组 | 有效结果 | 本轮来源 |
| --- | --- | --- |
| 任务/API/通知/实时 | 15/15 | 原基础15中13项通过；修复后仅重验实时2项，A/B各收到自己1条、对方0条 |
| 真实模型、恢复、工具隔离及撤权 | 19/19 | 原live19中16项沿用；重验排队、流式、重连撤权3项全通过 |
| 私聊读取/控制/搜索/实时/真实回复 | 8/8 | 新短A/B私聊，真实回复“ACP 连通”，对方实时内容0 |
| 真实HTTP/MinIO附件 | 8/8 | 上传/下载字节、任务绑定、上传者、跨doc完成、撤权pending保持及恢复完成 |

原live19实际结果是18通过/1失败，失败源于忙碌检测只接受回答chunk而遗漏真实思考chunk；所有队列/归属/停止控制均已有证据。仅修正忙碌判据，保留流式撤权必须有回答chunk的要求，新增一个实际lambda的RED→GREEN检查，并重验撤权3项。不能把原18/19追溯写成通过。

原基础15的两项实时失败暴露了真实OPEN竞态；首次子集包装缺少局部marker的NameError、修正包装后仍失败的结果也保留。生产修复有真实unit/network RED→GREEN，并通过专用栈定向复验。

补充：原A/B混合历史分别34/5条仍可读，续聊请求均404，历史逐条不变；未伪造迁移。浏览器已确认A/B自己的任务与通知、B访问A任务显示“未找到任务”、B访问A私聊404，A/B正常私聊分别键盘续聊返回指定标记并Finished，撤权对话显示Failed及“Authorization revoked”。截图与界面记录在文档项目 `output/playwright/paca-recovery-final/`。鼠标发送的既有Devtools遮挡未经修复验收，本轮不宣称鼠标发送通过；开发HMR仍有3000连接错误，页面渲染和上述操作通过。

## 代码测试、规格与独立审查

最终31个场景/28组唯一绑定均通过测试体与SUT审计，且逐个找到已实际执行的PASS记录。绑定审计本身不是31次新E2E；完整映射见 `runtime/evidence/final-sdd/task-5-final-review.md`。Task2的更广CRUD/视图/Sprint/关联/自动化矩阵继续引用已审查原证据，不用一个复合测试代替全部矩阵。

| 最终有效测试证据 | 实际结果 |
| --- | --- |
| API race | 446条测试/子测试PASS，无测试skip；两个无测试包的package skip单独保留 |
| Runner race | 136条PASS，无失败/跳过 |
| Native bridge race | 116条PASS；另一个环境相关 `TestUS012BindingSyncFailure` skip，历史Linux实际fsync EIO证据另存 |
| API真实E2E race | 69条PASS，11顶层，无失败/跳过 |
| Runner真实E2E race | 8条PASS，2顶层，无失败/跳过 |
| ACP最终race | 28条PASS，20顶层，无失败/跳过 |
| OPEN定向检查 | TypeScript及Bun5测试/12断言通过；真实PG/Valkey/HTTP/Socket.IO网络race1测试通过，内含Bun1测试/22断言 |

这些计数有父子及重复覆盖，不能相加为独立测试总数。精确命令、退出码、日志校验值见 `runtime/evidence/final-sdd/task-5-freeze-manifest.json`；早期三个race命令的timeout未在上下文中保留，清单已如实注明。真实联调命令/退出码在 `runtime/final-acceptance/`，原始结果在 `runs/`。最终Darwin arm64桥接构建退出0，二进制SHA256 `4fb5216d105ac2cf6e21b4852ba004b3edffce73cad798d599b7035d70ab72ef`。

各主要阶段均独立复审，整合审查及两次定向补修均APPROVED；没有为每个辅助脚本重新启动完整流程。最终应用66文件聚合SHA256 `40e6b99866dc2e92f8ddf8b77937a9eb82960451fab429848c65e15f7bb34225`，源码及31份Task5证据哈希逐一吻合。消融记录单列实际保留/撤回的删减；不虚报性能收益。

## 交付边界

本轮验收无剩余阻塞。以下是保留边界，不作为未经授权的新开发项：

- API/runner/realtime必须使用同一稳定实例ID和匹配内部密钥；不宣称整套未配置实例ID的旧栈兼容。已确认专用实例配置一致、4个当前boot入册、屏障打开，迁移000054–000058已应用。
- 失联持久参与者需要实际停止/隔离证明，不能TTL清理、清空注册表或flush Redis。API/runner漏OPEN或控制连接重连可能需要后续协调变更/恢复；当前未加周期重传。
- 屏障串行化实例授权变更，会暂停无关受保护工作。当前缓冲不提供持久消息队列保证；非409依赖错误及过期allow仍拒绝投递。
- 提交撤权前已交给网络的字节不能撤回；旧对象存储签名URL的TTL未改。外部自动化ACK单独失败仍可能重新创建run，不保证跨外部副作用exactly-once。
- 原混合Hermes home不整体迁移；部分损坏原生历史的非空检查不证明所有历史完整。沙箱TCP443规则不检查TLS，也不是域名白名单。
- 没有实际运行Goose原生镜像/模型；Goose取消以固定上游源码和真实HTTP/SSE协议检查为证，不把容器删除请求当原生停止证明。本轮真实模型使用Hermes。

回退/启动步骤见 `回退说明.md`；版本、备份和证据索引见 `local-version-manifest.json`、`backup-manifest.json`、`evidence-manifest.json`。原草稿保留在私有 `runtime/pre-final-report-drafts/`。

本报告中产物相对路径以 `/Users/xxp/Documents/ChatGPT/新一版AI本位/outputs/paca-recovery-20260915/` 为根；源码/独立审查原件在本工作树。
