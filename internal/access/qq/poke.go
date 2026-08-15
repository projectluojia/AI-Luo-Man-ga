// 戳一戳（notice/notify/poke）处理：识别他人戳机器人事件，随机文案回复，
// 群聊内再戳回去。纯平台事件处理，不经过 Echo/Agent，不创建任何持久状态。

package qq

import (
	"context"
	"math/rand/v2"

	"github.com/projectluojia/AI-Luo-Man-ga/internal/access"
	"github.com/projectluojia/AI-Luo-Man-ga/internal/observe"
)

// handleNotice 处理一条 OneBot notice 事件；目前只支持戳一戳，其余忽略。
func (a *Adapter) handleNotice(ctx context.Context, raw map[string]any) {
	if str(raw, "notice_type") != "notify" || str(raw, "sub_type") != "poke" {
		return
	}
	if str(raw, "target_id") != a.cfg.BotQQID {
		return // 戳的不是机器人
	}
	userID := str(raw, "user_id")
	groupID := str(raw, "group_id")
	if userID == "" {
		return
	}
	channel := "private"
	spaceID := "private"
	sessionID := userID
	if groupID != "" {
		channel = "group"
		spaceID = groupID
		sessionID = groupID
	}
	inbound := &access.InboundMessage{
		AppID:             a.cfg.AppID,
		Platform:          "qq",
		PlatformChannel:   channel,
		PlatformSpaceID:   spaceID,
		PlatformUserID:    userID,
		PlatformMessageID: "poke-" + userID + "-" + groupID,
		PlatformSessionID: sessionID,
		MessageType:       "text",
		Text:              "",
	}
	if !a.allowed(inbound) {
		return
	}
	if len(a.pokeReplies) > 0 {
		a.replyPlain(ctx, inbound, a.pokeReplies[rand.IntN(len(a.pokeReplies))])
	}
	if channel == "group" {
		// 群聊戳回去：向戳的人发送 group_poke。
		if err := a.send(map[string]any{
			"action": "group_poke",
			"params": map[string]any{"group_id": onebotInt(groupID), "user_id": onebotInt(userID)},
		}); err != nil {
			observe.Warn(ctx, "QQ 戳一戳回戳失败",
				observe.StringAttr("reason", err.Error()),
			)
		}
	}
}
