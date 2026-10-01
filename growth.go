package main

// 国内站成长任务引擎：接取 → 事件上报点亮 → 进度落账 → 领奖。
//
// 移植自 workbuddy-hub/wb_tasks.py（633 行）。三条从实测中换来的硬约束，代码里必须守住：
//
//   1. 事件上报必须发 copilot.tencent.com（chat 侧），与桌面客户端真实上报地址一致。
//      发到别的域上游不推进进度 —— 表现就是"接取了一堆但一个都没点亮"。
//   2. 专家/团队类事件每次必须用**互不相同**的 id：上游按 (eventCode, id) 去重，
//      重复 id 进度永远不动。
//   3. 只认桌面端真实行为的任务（jump_url 是 workbuddy:// 深链）伪造无效，
//      诚实跳过并给出深链，不假装成功。
//
// 领奖走 claim，HTTP 400 时降级到 www.workbuddy.cn web 域重试一次。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	growthTasksPath  = "/v2/activity/growth/tasks"
	growthAcceptPath = "/v2/activity/growth/tasks/accept"
	growthClaimPath  = "/activity/growth/tasks/%s/claim"
	growthReportPath = "/v2/report"
	growthEnergyPath = "/v2/activity/growth/energy"
	growthStreakPath = "/activity/growth/streak"

	// growthWebBase 是领奖降级用的 web 域（上游偶发在 chat 域拒绝 claim）。
	growthWebBase = "https://www.workbuddy.cn"

	// growthTimeout 是单个账号整轮成长任务的预算：接取 + 逐任务上报 + 等待落账 +
	// 领奖，任务多时可达数分钟，必须独立于签到那 30s 的短超时。
	growthTimeout = 5 * time.Minute

	// growthGap 是相邻请求的防风控间隔。
	growthGap = 1 * time.Second
	// growthSettleGap 是上报后等上游落账的间隔。
	growthSettleGap = 1500 * time.Millisecond
)

// cnGrowthEnabled 国内站成长任务自动化开关（config.json checkin.cn.growth，默认开启）。
var cnGrowthEnabled = true

// -----------------------------------------------------------------------------
// 任务规格（与 hub TASK_SPECS 对齐）
// -----------------------------------------------------------------------------

type growthTaskSpec struct {
	Kind        string
	Target      int
	Reward      int
	Name        string
	Unforgeable bool // 真实捐款等无法伪造的动作
	Reason      string
}

var taskSpecs = map[string]growthTaskSpec{
	"create_canvas":       {Kind: "canvas", Target: 1, Reward: 300, Name: "创建设计任务"},
	"template_5":          {Kind: "template", Target: 5, Reward: 200, Name: "模板创建任务"},
	"expert_5":            {Kind: "expert", Target: 5, Reward: 200, Name: "使用专家助手"},
	"Expert_team_use_3":   {Kind: "team", Target: 3, Reward: 150, Name: "使用专家团队"},
	"skill_1":             {Kind: "skill", Target: 1, Reward: 100, Name: "体验技能"},
	"automation_1":        {Kind: "automation", Target: 1, Reward: 100, Name: "创建自动化任务"},
	"playbook_prompt":     {Kind: "playbook", Target: 1, Reward: 100, Name: "灵感案例使用"},
	"Expert_lighthouse":   {Kind: "lighthouse", Target: 1, Reward: 100, Name: "轻量云专家使用"},
	"Buddy_App":           {Kind: "buddy5", Target: 1, Reward: 100, Name: "进入 Buddy 应用"},
	"Buddy_App_QQ":        {Kind: "buddy5", Target: 1, Reward: 100, Name: "企鹅教师助手"},
	"Hp_Appearance":       {Kind: "skin", Target: 1, Reward: 100, Name: "应用主题外观"},
	"chat_5":              {Kind: "chat", Target: 5, Reward: 100, Name: "发起 5 次对话"},
	"Model_chat_GLM5.2":   {Kind: "glmchat", Target: 1, Reward: 100, Name: "体验 GLM-5.2"},
	"black_cat":           {Kind: "cat", Target: 3, Reward: 100, Name: "夜猫子任务 (23:00-08:00)"},
	"RichMeow_Chat":       {Kind: "richmeow", Target: 1, Reward: 100, Name: "桌面对话事件链"},
	"Library_read":        {Kind: "library", Target: 1, Reward: 100, Name: "浏览资料库"},
	"first_buddy":         {Kind: "buddy_first", Target: 1, Reward: 0, Name: "领养首只猫猫"},
	"Expert_Philanthropy": {Unforgeable: true, Reward: 0, Name: "公益爱心捐赠", Reason: "真实捐款动作"},
}

// desktopOnlyTasks 上游只认桌面客户端真实点击的任务，伪造事件会被忽略或落到 heartbeat，
// 进度永远 0/1，claim 必然 400。诚实跳过并给出深链。
var desktopOnlyTasks = map[string]string{
	"RichMeow_Chat": "在桌面端发起 1 次对话",
	"Library_read":  "在桌面端打开「资料库」并读完介绍文档",
	"Buddy_App":     "在桌面端左上角「发现应用」进入任意一个 Buddy 应用",
	"Buddy_App_QQ":  "在桌面端「发现应用」进入「企鹅教师助手」",
}

// nightTaskCodes 只在 23:00-08:00 上报才计数，每天 1 次、累计 3 天。
var nightTaskCodes = map[string]bool{"black_cat": true}

// 专家/团队事件 id 池：必须每次取不同的 id，否则上游按 (eventCode, id) 去重不推进。
var expertIDPool = [][2]string{
	{"ex_PZw8Gu81HfN4", "运维工程师"}, {"ex_ROsDtJbzADFV", "产品经理"},
	{"ex_SMUnl0nJbPix", "UI设计师"}, {"ex_ZTR062oVBOCW", "数据分析师"},
	{"ex_a3sSSFBy8qaC", "后端架构师"}, {"ex_aG1kvKbq8lPx", "文案策划"},
	{"ex_al1vxtUOYQ10", "测试专家"}, {"ex_cZfiyuET9UQP", "安全顾问"},
	{"ex_eggOvQuVP0hq", "算法工程师"}, {"ex_hSwsQjkSKnkX", "前端工程师"},
	{"ex_mMbwwmFA9n9P", "项目管理专家"}, {"ex_uAQE5POfk7Zh", "增长运营专家"},
	{"ex_uZzSAScSy7FZ", "行业研究员"}, {"ex_LHywGrZOtG7G", "数据分析师"},
	{"ex_NX5C8GBciVed", "测试架构师"}, {"ex_DdCsaoq4AtcO", "云端运维专家"},
	{"ex_KzqKQguubrNQ", "内容创作专家"}, {"ex_2cvvUZQhDyeJ", "腾讯轻量云专家"},
}

var teamIDPool = [][2]string{
	{"CloudOpsTeam", "运维专家团队"}, {"CloudContentTeam", "内容专家团队"},
	{"CloudDevTeam", "研发专家团队"}, {"ProductStrategyTeam", "产品战略团队"},
	{"MarketingCampaignTeam", "营销活动团队"}, {"SalesBattleTeam", "销售作战团队"},
	{"DesignEngineTeam", "设计引擎团队"}, {"HrOperationsTeam", "人力运营团队"},
}

// -----------------------------------------------------------------------------
// 数据结构
// -----------------------------------------------------------------------------

type growthTask struct {
	Code         string
	Name         string
	JumpURL      string
	Status       string
	Current      int
	Target       int
	RewardCredit int
	RewardEnergy int
}

type growthResult struct {
	EarnedCredit int
	Logs         []string
}

// -----------------------------------------------------------------------------
// 请求基建
// -----------------------------------------------------------------------------

// growthHeaders 成长任务接口的出站头：chat 侧鉴权 + web 平台标识。
func growthHeaders(prof *upstreamProfile, auth *StoredAuth) func(*http.Request) {
	return func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
		r.Header.Set("X-Client-Platform", "web")
		if auth.Account.UID != "" {
			r.Header.Set("X-User-Id", auth.Account.UID)
		}
		if auth.Account.EnterpriseID != "" {
			r.Header.Set("X-Enterprise-Id", auth.Account.EnterpriseID)
		}
	}
}

// growthEnvelope 是成长任务接口统一包络。
type growthEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// growthRequest 发一个成长任务请求并原样返回业务包络。
// 不做 code!=0 即报错的处理 —— 调用方需要区分「HTTP 400 要降级」与
// 「200 + 业务码非零（典型：task not completed，进度还没落账就来领奖）」。
func growthRequest(ctx context.Context, method, url string, headers func(*http.Request), body io.Reader) (growthEnvelope, int, error) {
	var env growthEnvelope
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return env, 0, err
	}
	if headers != nil {
		headers(req)
	}
	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		return env, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &env)
	}
	if resp.StatusCode >= 400 {
		return env, resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	return env, resp.StatusCode, nil
}

// -----------------------------------------------------------------------------
// 接口封装
// -----------------------------------------------------------------------------

// fetchGrowthTasks 查询成长任务列表及当前状态。
func fetchGrowthTasks(ctx context.Context, prof *upstreamProfile, auth *StoredAuth) ([]growthTask, error) {
	env, _, err := growthRequest(ctx, http.MethodGet, prof.Base+growthTasksPath, growthHeaders(prof, auth), nil)
	if err != nil {
		return nil, err
	}
	var data struct {
		Tasks []struct {
			TaskCode     string `json:"task_code"`
			Title        string `json:"title"`
			Description  string `json:"description"`
			JumpURL      string `json:"jump_url"`
			AcceptStatus string `json:"accept_status"`
			Progress     struct {
				Current int `json:"current"`
				Target  int `json:"target"`
			} `json:"progress"`
			RewardCredit int `json:"reward_credit"`
			RewardEnergy int `json:"reward_energy"`
		} `json:"tasks"`
	}
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &data)
	}
	out := make([]growthTask, 0, len(data.Tasks))
	for _, t := range data.Tasks {
		spec := taskSpecs[t.TaskCode]
		name := t.Title
		if name == "" {
			name = spec.Name
		}
		if name == "" {
			name = t.TaskCode
		}
		target := t.Progress.Target
		if target == 0 {
			target = spec.Target
		}
		if target == 0 {
			target = 1
		}
		status := t.AcceptStatus
		if status == "" {
			status = "not_accepted"
		}
		reward := t.RewardCredit
		if reward == 0 {
			reward = spec.Reward
		}
		out = append(out, growthTask{
			Code: t.TaskCode, Name: name, JumpURL: t.JumpURL, Status: status,
			Current: t.Progress.Current, Target: target,
			RewardCredit: reward, RewardEnergy: t.RewardEnergy,
		})
	}
	return out, nil
}

type acceptOutcome struct {
	OK       bool
	Accepted []string
	Failed   []string
	Msg      string
}

// acceptGrowthTasks 批量接取任务（每批 20 个）。
func acceptGrowthTasks(ctx context.Context, prof *upstreamProfile, auth *StoredAuth, codes []string) acceptOutcome {
	out := acceptOutcome{OK: true}
	if len(codes) == 0 {
		return out
	}
	for i := 0; i < len(codes); i += 20 {
		end := i + 20
		if end > len(codes) {
			end = len(codes)
		}
		part := codes[i:end]
		payload, _ := json.Marshal(map[string]any{"task_codes": part})
		env, status, err := growthRequest(ctx, http.MethodPost, prof.Base+growthAcceptPath,
			growthHeaders(prof, auth), strings.NewReader(string(payload)))
		if err != nil {
			out.OK = false
			out.Msg = fmt.Sprintf("HTTP %d: %v", status, err)
			out.Failed = append(out.Failed, part...)
			continue
		}
		if env.Code != 0 {
			out.OK = false
			out.Msg = env.Msg
			out.Failed = append(out.Failed, part...)
			continue
		}
		var data struct {
			Results []struct {
				TaskCode string `json:"task_code"`
				Status   string `json:"status"`
			} `json:"results"`
		}
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &data)
		}
		seen := map[string]bool{}
		for _, r := range data.Results {
			seen[r.TaskCode] = true
			if r.Status == "accepted" || r.Status == "already_accepted" {
				out.Accepted = append(out.Accepted, r.TaskCode)
			} else {
				out.Failed = append(out.Failed, r.TaskCode)
			}
		}
		for _, c := range part {
			if !seen[c] {
				out.Failed = append(out.Failed, c)
			}
		}
		time.Sleep(growthGap)
	}
	return out
}

type claimOutcome struct {
	OK      bool
	Credit  int
	Energy  int
	Message string
}

// claimGrowthTask 领取任务奖励；HTTP 400 时降级到 web 域重试一次。
func claimGrowthTask(ctx context.Context, prof *upstreamProfile, auth *StoredAuth, code string) claimOutcome {
	url := prof.Base + fmt.Sprintf(growthClaimPath, code)
	env, status, err := growthRequest(ctx, http.MethodPost, url, growthHeaders(prof, auth), strings.NewReader("{}"))
	if err == nil && env.Code == 0 {
		var data struct {
			Credit int `json:"credit"`
			Energy int `json:"energy"`
		}
		if len(env.Data) > 0 {
			_ = json.Unmarshal(env.Data, &data)
		}
		return claimOutcome{OK: true, Credit: data.Credit, Energy: data.Energy}
	}
	msg := env.Msg
	if err != nil {
		msg = err.Error()
	}
	// chat 域被拒（典型 HTTP 400）时降级到 web 域。
	if status == 400 || err != nil {
		webURL := growthWebBase + fmt.Sprintf(growthClaimPath, code)
		webHdr := func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
			r.Header.Set("Accept", "application/json, text/plain, */*")
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", growthWebBase)
			r.Header.Set("Referer", growthWebBase+"/profile/growth-center")
			r.Header.Set("x-client-platform", "web")
			r.Header.Set("User-Agent", webUserAgent)
			if auth.Account.UID != "" {
				r.Header.Set("X-User-Id", auth.Account.UID)
			}
			r.Header.Set("X-Domain", growthWebBase)
		}
		wenv, _, werr := growthRequest(ctx, http.MethodPost, webURL, webHdr, strings.NewReader("{}"))
		if werr == nil && wenv.Code == 0 {
			var data struct {
				Credit int `json:"credit"`
				Energy int `json:"energy"`
			}
			if len(wenv.Data) > 0 {
				_ = json.Unmarshal(wenv.Data, &data)
			}
			return claimOutcome{OK: true, Credit: data.Credit, Energy: data.Energy}
		}
		if werr != nil && msg == "" {
			msg = werr.Error()
		}
	}
	return claimOutcome{OK: false, Message: msg}
}

// buildGrowthEvent 构造指定类型的规范事件数据。
func buildGrowthEvent(uid, kind string, idx int, expert [2]string) map[string]any {
	now := time.Now().UnixMilli()
	cid := fmt.Sprintf("wb-task-%d-%d", now, idx)
	rid := cid + "-req"

	switch kind {
	case "canvas":
		return map[string]any{"eventCode": "wbx_design_canvas_task_create", "timestamp": now,
			"reportDelay": 0, "conversationId": cid, "requestId": rid,
			"source": "summon_keyword", "isCustomModel": false, "name": "",
			"inputLength": 12, "id": fmt.Sprintf("wbx-canvas-%d", now), "cost": 0,
			"isSuccessful": true, "userId": uid}
	case "template":
		return map[string]any{"eventCode": "agent_task_created_with_template", "timestamp": now,
			"reportDelay": 0, "isCustomModel": true, "id": fmt.Sprintf("%d", idx),
			"name": "幻灯片", "requestId": rid, "conversationId": cid, "userId": uid}
	case "expert", "team", "lighthouse":
		etype := "agent"
		if kind == "team" {
			etype = "team"
		}
		exID, exName := expert[0], expert[1]
		if exID == "" {
			switch kind {
			case "lighthouse":
				exID, exName = "ex_2cvvUZQhDyeJ", "腾讯轻量云专家"
			case "team":
				exID, exName = "CloudOpsTeam", "运维专家团队"
			default:
				exID, exName = "ContentCreator", "内容创作专家"
			}
		}
		return map[string]any{"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
			"mode": "CLOUD", "id": exID, "name": exName, "expertTitle": exName,
			"type": "02-Engineering", "expertType": etype, "source": "builtin",
			"version": "1.0.2", "cost": 0, "characterCount": 12, "conversationId": cid,
			"requestId": rid, "messageId": rid, "requestModelId": "deepseek-v4-flash",
			"requestModelName": "DeepSeek V4 Flash", "userId": uid}
	case "skill":
		return map[string]any{"eventCode": "skill_info", "timestamp": now, "reportDelay": 0,
			"skillId": "skill_2096525080079265792", "name": "pptx", "userId": uid}
	case "automation":
		return map[string]any{"eventCode": "automated_task_create_suc", "timestamp": now,
			"reportDelay": 0, "name": "每周工作整理", "type": "cron", "source": "manually",
			"modelId": "deepseek-v4-flash", "modelIsThinking": false,
			"conversationId": cid, "requestId": rid,
			"schedule": map[string]any{"type": "recurring",
				"rrule": "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0"},
			"prompt": "每周五自动整理本周工作", "userId": uid}
	case "playbook":
		return map[string]any{"eventCode": "playbook_prompt_send", "timestamp": now,
			"reportDelay": 0, "id": "worker-ledger-freedom-dashboard", "name": "打工人小账本",
			"type": "other", "promptLength": 10, "isOfficial": 1,
			"source": "discover", "conversationId": cid, "requestId": rid, "userId": uid}
	case "skin":
		return map[string]any{"eventCode": "appearance_skin_apply", "timestamp": now,
			"reportDelay": 0, "action": "apply", "source": "settings_close",
			"id": "theme-tkmw7j", "vipLevel": "free", "series": "craft", "type": "unknown",
			"name": "和平精英激战金秋", "userId": uid}
	case "chat", "glmchat", "cat":
		modelID, modelName := "deepseek-v4-flash", "DeepSeek V4 Flash"
		if kind == "glmchat" || kind == "cat" {
			modelID, modelName = "glm-5.2", "GLM-5.2"
		}
		mode := "craft"
		if kind == "cat" {
			mode = "night"
		}
		return map[string]any{"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
			"mode": mode, "conversationId": cid, "requestId": rid, "inputLength": 12,
			"requestModelId": modelID, "requestModelName": modelName, "isPlan": false,
			"agentName": "default", "agentType": "conversation", "userId": uid}
	}
	return map[string]any{"eventCode": "heartbeat", "timestamp": now, "userId": uid}
}

// reportGrowthEvents 上报事件数组到 copilot.tencent.com（与桌面客户端真实上报地址一致）。
func reportGrowthEvents(ctx context.Context, prof *upstreamProfile, auth *StoredAuth, events []map[string]any) bool {
	payload, err := json.Marshal(events)
	if err != nil {
		return false
	}
	env, _, err := growthRequest(ctx, http.MethodPost, prof.Base+growthReportPath,
		growthHeaders(prof, auth), strings.NewReader(string(payload)))
	if err != nil {
		return false
	}
	return env.Code == 0
}

// inNightWindow 夜猫子窗口 23:00-08:00，按 UTC+8 判定（不依赖机器本地时区）。
func inNightWindow() bool {
	h := time.Now().In(displayLoc).Hour()
	return h >= 23 || h < 8
}

// -----------------------------------------------------------------------------
// 主流程
// -----------------------------------------------------------------------------

// runGrowthTasks 为单个国内站账号跑完整成长任务自动化。
// 结果只记日志、不影响签到返回值；单账号失败不影响其他账号。
func runGrowthTasks(ctx context.Context, auth *StoredAuth, prof *upstreamProfile, path string) growthResult {
	res := growthResult{}
	add := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		res.Logs = append(res.Logs, line)
		log.Printf("[Growth] 账号 %s %s", path, line)
	}

	tasks, err := fetchGrowthTasks(ctx, prof, auth)
	if err != nil {
		add("获取任务清单失败: %v", err)
		return res
	}
	if len(tasks) == 0 {
		add("未获取到可处理的任务")
		return res
	}

	// 1. 批量接取未接取任务
	var unaccepted []string
	for _, t := range tasks {
		if t.Status == "not_accepted" && !taskSpecs[t.Code].Unforgeable {
			unaccepted = append(unaccepted, t.Code)
		}
	}
	if len(unaccepted) > 0 {
		add("发现 %d 个待接取任务，正在批量接取", len(unaccepted))
		acc1 := acceptGrowthTasks(ctx, prof, auth, unaccepted)
		if len(acc1.Failed) > 0 {
			shown := acc1.Failed
			if len(shown) > 5 {
				shown = shown[:5]
			}
			add("接取未成功 %d 个: %s", len(acc1.Failed), strings.Join(shown, ", "))
		}
		if len(acc1.Accepted) > 0 {
			add("已接取 %d 个任务", len(acc1.Accepted))
		}
		// 复核一次：上游偶尔瞬时拒绝整批，复查后仍未接取的再补一次，
		// 否则后续上报全部作用在未接取任务上，进度恒为 0。
		tasks, err = fetchGrowthTasks(ctx, prof, auth)
		if err != nil {
			add("接取后复核失败: %v", err)
			return res
		}
		var still []string
		for _, t := range tasks {
			if t.Status == "not_accepted" && !taskSpecs[t.Code].Unforgeable {
				still = append(still, t.Code)
			}
		}
		if len(still) > 0 {
			add("仍有 %d 个未接取，重试一次", len(still))
			retry := acceptGrowthTasks(ctx, prof, auth, still)
			if len(retry.Accepted) > 0 {
				add("重试接取成功 %d 个", len(retry.Accepted))
			}
			tasks, err = fetchGrowthTasks(ctx, prof, auth)
			if err != nil {
				add("重试后复核失败: %v", err)
				return res
			}
		}
	}

	// 2. 逐任务点亮与领奖
	for _, t := range tasks {
		if ctx.Err() != nil {
			add("本轮被取消，剩余任务顺延")
			break
		}
		spec, known := taskSpecs[t.Code]
		if !known || spec.Unforgeable {
			continue
		}
		if t.Status == "claimed" {
			continue
		}

		// 已完成的先结算：可能被真实操作或夜间调度点亮过，不能因"只能靠真实操作"就丢掉奖励。
		if t.Status == "completed" || t.Current >= t.Target {
			r := claimGrowthTask(ctx, prof, auth, t.Code)
			if r.OK {
				res.EarnedCredit += r.Credit
				add("任务 [%s] 领奖成功: +%d 积分", spec.Name, r.Credit)
			} else {
				add("任务 [%s] 领奖失败: %s", spec.Name, orUnknown(r.Message))
			}
			time.Sleep(growthGap)
			continue
		}

		if reason, isDesktop := desktopOnlyTasks[t.Code]; isDesktop {
			jump := t.JumpURL
			if jump == "" {
				jump = "workbuddy://chat"
			}
			add("任务 [%s] 需真实操作完成: %s（深链 %s），跳过事件伪造", spec.Name, reason, jump)
			continue
		}

		if nightTaskCodes[t.Code] && !inNightWindow() {
			add("任务 [%s] 仅 23:00-08:00 上报计数，当前不在窗口，跳过（每日 01:00 自动执行，累计 3 天）", spec.Name)
			continue
		}

		if t.Status == "not_accepted" {
			add("任务 [%s] 仍未接取，跳过（先解决接取失败）", spec.Name)
			continue
		}

		need := t.Target - t.Current
		if need < 1 {
			need = 1
		}
		add("正在点亮任务 [%s]（需上报 %d 次）", spec.Name, need)

		var pool [][2]string
		if spec.Kind == "expert" {
			pool = expertIDPool
		} else if spec.Kind == "team" {
			pool = teamIDPool
		}
		reportOK := true
		for i := 0; i < need; i++ {
			var expert [2]string
			if len(pool) > 0 {
				expert = pool[(t.Current+i)%len(pool)]
			}
			ev := buildGrowthEvent(auth.Account.UID, spec.Kind, i, expert)
			if !reportGrowthEvents(ctx, prof, auth, []map[string]any{ev}) {
				reportOK = false
			}
			if i < need-1 {
				time.Sleep(growthGap)
			}
		}
		if !reportOK {
			add("任务 [%s] 部分事件上报失败（上游拒绝），继续尝试领奖", spec.Name)
		}
		time.Sleep(growthSettleGap)

		// 等上游把进度落账：先快查几次，只有确实在涨才继续等，
		// 免得每个卡住的任务都空等（整轮要几分钟）。
		prog := t.Current
		for attempt := 0; attempt < 6; attempt++ {
			fresh, ferr := fetchGrowthTasks(ctx, prof, auth)
			if ferr != nil {
				break
			}
			found := false
			for _, f := range fresh {
				if f.Code == t.Code {
					found = true
					prog = f.Current
					if prog >= t.Target || f.Status == "completed" || f.Status == "claimed" {
						break
					}
					if prog > t.Current {
						time.Sleep(2500 * time.Millisecond)
					}
					break
				}
			}
			if !found {
				break
			}
			if prog >= t.Target {
				break
			}
			if attempt < 2 {
				time.Sleep(growthSettleGap)
				continue
			}
			break
		}
		if prog < t.Target {
			add("任务 [%s] 已上报但进度 %d/%d 未达成，领奖顺延到下次运行", spec.Name, prog, t.Target)
			time.Sleep(growthGap)
			continue
		}

		r := claimGrowthTask(ctx, prof, auth, t.Code)
		if r.OK {
			res.EarnedCredit += r.Credit
			add("任务 [%s] 点亮并领奖成功: +%d 积分", spec.Name, r.Credit)
		} else {
			add("任务 [%s] 进度已达 %d/%d 但领奖失败: %s", spec.Name, prog, t.Target, orUnknown(r.Message))
		}
		time.Sleep(growthGap)
	}

	// 猫猫旅行不在这里处理：checkinAccount 在签到成功后已经调用 performBuddyTravel，
	// 同一轮里重复派遣既浪费请求也可能触发上游限流。

	add("成长任务本轮结束，累计到账 +%d 积分", res.EarnedCredit)
	return res
}
