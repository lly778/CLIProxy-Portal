package httpserver

import (
	"fmt"
	"strings"

	"cliproxy-portal/internal/agy"
)

func agyObservationDetail(events []agy.Observation) string {
	if len(events) == 0 {
		return ""
	}
	var text strings.Builder
	text.WriteString("\nAGY 阶段与工具回执（仅记录传输事实）")
	base := events[0].At
	var summaries, modelCalls, cacheHits int
	var mainCalls, awaits, descriptions, replays, contractHits, contractMisses, rejected, direct, relayed, serverSearches int
	runtimeHash := ""
	issued, received, delivered, official, searches := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	remoteIssued, remoteStarted, remoteResults := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var tokens int64
	var unknown bool
	for i, v := range events {
		if v.RuntimeSHA != "" {
			runtimeHash = v.RuntimeSHA
		}
		switch v.Kind {
		case "remote_tool":
			key := v.SessionID + ":" + v.CallID
			if v.State == "issued" {
				remoteIssued[key] = true
			}
			if v.State == "execution_started" {
				remoteStarted[key] = true
			}
			if v.State == "result_received" {
				remoteResults[key] = true
			}
			fmt.Fprintf(&text, "\n+%d ms remote_mcp %s %s [%s] outcome=%s", v.At-base, v.Tool, v.State, v.CallID, v.Outcome)
		case "contract_cache":
			if v.State == "hit" {
				contractHits++
			} else {
				contractMisses++
			}
		case "tool_rejected":
			rejected++
			fmt.Fprintf(&text, "\n工具参数拒绝：%s route=%s reason=%s projection=%d", v.Tool, v.Route, v.Reason, v.ProjectionVersion)
		case "phase":
			fmt.Fprintf(&text, "\n+%d ms %s", v.At-base, v.State)
			for _, next := range events[i+1:] {
				if next.Kind == "phase" {
					fmt.Fprintf(&text, "（阶段耗时 %d ms）", max(int64(0), next.At-v.At))
					break
				}
			}
			if v.State == "session_ready" {
				fmt.Fprintf(&text, " session=%s", v.SessionID)
			}
		case "session":
			fmt.Fprintf(&text, "\n%s reason=%s session=%s", v.State, v.Reason, v.SessionID)
		case "tool", "official_tool":
			key := v.SessionID + ":" + v.CallID
			if v.Kind == "official_tool" {
				official[key] = true
			} else if v.Category == "tool_search" && v.State == "issued" {
				searches[key] = true
			} else if v.State == "issued" {
				issued[key] = true
				if v.Route == "portal_call_tool" {
					relayed++
				} else if v.Route == "direct" {
					direct++
				}
			}
			if v.Kind == "tool" && v.State == "result_received" {
				received[key] = true
			}
			if v.Kind == "tool" && v.State == "result_delivered" {
				delivered[key] = true
			}
			fmt.Fprintf(&text, "\n+%d ms %s %s %s [%s] outcome=%s", v.At-base, v.Category, v.Tool, v.State, v.CallID, v.Outcome)
			if v.BatchID != "" {
				fmt.Fprintf(&text, " batch=%s", v.BatchID)
			}
		case "batch":
			fmt.Fprintf(&text, "\nbatch=%s %s calls=%s", v.BatchID, v.State, strings.Join(v.CallIDs, ","))
		case "bridge", "http_replay":
			if v.Category == "server_tool_search" {
				serverSearches++
			}
			if v.Category == "await" {
				awaits++
			}
			if v.Category == "describe" {
				descriptions++
			}
			if v.Kind == "http_replay" {
				replays++
			}
			fmt.Fprintf(&text, "\n+%d ms %s %s", v.At-base, v.Category, v.State)
			if v.OriginRequestID != "" {
				fmt.Fprintf(&text, " origin=%s", v.OriginRequestID)
			}
		case "response":
			fmt.Fprintf(&text, "\nresponse=%s session=%s", v.ResponseID, v.SessionID)
		case "model_usage":
			mainCalls += v.ModelCalls
		case "summary_usage":
			summaries++
			modelCalls += v.ModelCalls
			tokens += v.Usage.Total
			unknown = unknown || !v.Usage.Known || v.State != "completed"
		case "compaction":
			if p := v.Compaction; p != nil {
				fmt.Fprintf(&text, "\n摘要 %s %s：输入 %d 项/%d bytes，覆盖 %d 项，保留 %d 项，图片 %d，输出 %d bytes；工具事实 %d；source=%s summary=%s", p.Source, v.State, p.InputItems, p.InputBytes, p.CoveredItems, p.TailItems, p.Images, p.OutputBytes, p.ToolRecords, p.SourceHash, p.SummaryHash)
			}
		case "summary_cache":
			cacheHits++
		}
	}
	fmt.Fprintf(&text, "\n分类：客户端调用生成 %d，工具发现 %d，官方工具 %d；收到结果 %d，交回 AGY %d；await %d，describe %d，HTTP 复用 %d", len(issued), len(searches), len(official), len(received), len(delivered), awaits, descriptions, replays)
	if direct+relayed+contractHits+contractMisses+rejected+serverSearches > 0 {
		fmt.Fprintf(&text, "\n契约：直接调用 %d，通用转接 %d；描述缓存命中 %d、读取 %d；拒绝 %d；服务端目录搜索 %d", direct, relayed, contractHits, contractMisses, rejected, serverSearches)
	}
	if len(remoteIssued)+len(remoteStarted)+len(remoteResults) > 0 {
		fmt.Fprintf(&text, "\n远程 MCP：请求生成 %d，实际开始派发 %d，取得结果/明确终态 %d；审批不是执行完成", len(remoteIssued), len(remoteStarted), len(remoteResults))
	}
	if runtimeHash != "" {
		fmt.Fprintf(&text, "\nAGY runtime SHA256=%s", runtimeHash)
	}
	if mainCalls > 0 {
		fmt.Fprintf(&text, "\n主会话已确认模型调用：%d", mainCalls)
	}
	if summaries > 0 || cacheHits > 0 {
		fmt.Fprintf(&text, "\n内部摘要：%d 次提示，%d 次已确认模型调用，缓存复用 %d 次；已确认 %d tokens（另计入用量汇总）", summaries, modelCalls, cacheHits, tokens)
		if unknown {
			text.WriteString("；部分用量未知")
		}
	}
	return text.String()
}
