package controlstream

import (
	"sort"
	"strings"
	"time"

	agentprotocolv1 "github.com/wangzw/castworks/pkg/agentprotocol/v1"
)

// inboxMessageText 返回一条收件箱消息写给 Claude Code 的文本。content 为
// text 时原样返回；为 subscription_completed 时按 warden.md“订阅完成事件的
// 写入”一节的模板生成。两者都没有时 ok 为 false，调用方按校验失败处理。
func inboxMessageText(m *agentprotocolv1.InboxMessage) (text string, ok bool) {
	switch content := m.GetContent().(type) {
	case *agentprotocolv1.InboxMessage_Text:
		return content.Text, true
	case *agentprotocolv1.InboxMessage_SubscriptionCompleted:
		if content.SubscriptionCompleted == nil {
			return "", false
		}
		return subscriptionCompletedText(content.SubscriptionCompleted), true
	default:
		return "", false
	}
}

// subscriptionCompletedText 把一个订阅完成事件写成投递给 Claude Code 的
// 文本。correlation 按键排序；occurred_at 用 RFC 3339；detail_truncated 为
// 真时在末尾加一行说明。detail 为空时不写内容行。
func subscriptionCompletedText(s *agentprotocolv1.SubscriptionCompleted) string {
	var b strings.Builder
	b.WriteString("[castworks] Subscription ")
	b.WriteString(s.GetSubId())
	b.WriteString(" completed.\n")
	b.WriteString("system: ")
	b.WriteString(s.GetSystem())
	b.WriteString("\nevent: ")
	b.WriteString(s.GetEvent())
	b.WriteString("\noutcome: ")
	b.WriteString(s.GetTerminal())
	b.WriteString("\ncorrelation:")
	if pairs := formatCorrelation(s.GetCorrelation()); pairs != "" {
		b.WriteString(" ")
		b.WriteString(pairs)
	}
	b.WriteString("\noccurred_at: ")
	b.WriteString(s.GetOccurredAt().AsTime().UTC().Format(time.RFC3339))
	b.WriteString("\nsource: ")
	b.WriteString(s.GetSource())
	b.WriteString("\ndetail:")
	if detail := s.GetDetail(); detail != "" {
		b.WriteString("\n")
		b.WriteString(detail)
	}
	if s.GetDetailTruncated() {
		b.WriteString("\n(detail truncated)")
	}
	return b.String()
}

// formatCorrelation 按键的字典序写出 correlation 项，形如 k=v，以逗号加空格
// 分隔。没有项时返回空串。
func formatCorrelation(correlation map[string]string) string {
	keys := make([]string, 0, len(correlation))
	for k := range correlation {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+correlation[k])
	}
	return strings.Join(pairs, ", ")
}
