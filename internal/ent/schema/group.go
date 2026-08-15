package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// Group 平台容量池（无内嵌 key 语义——Phase 3a 重建为独立 keys 表）：
// visibility(public|private)；private 授予对象 = 用户（group_assignments）。
type Group struct{ ent.Schema }

func (Group) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("name").Unique(),
		field.Enum("visibility").Values("public", "private").Default("public"),
		field.Int("price_multiplier").Default(10000), // 万分数（T3.5 价格倍率）：组默认 ×1；0 = 免费
		// protocol_convert 分组级协议转换（只补差，W5 消费）：JSON 数组，空数组 =
		// off = 不转换；多方向并存按客户端格式命中（chat 请求走 chat_to_*、anthropic
		// 请求走 mess_to_resp、resp 请求走 resp_to_mess）。元素 ∈ 4 方向枚举（off 不
		// 进数组）；非法方向/同客户端格式冲突（chat_to_resp + chat_to_mess）校验在
		// service 层。默认空数组（对齐旧单值 .Default("off") 语义——缺省 = off；
		// 直连 ent 建组的调用方恒有值）。beta 期无迁移：旧单值 string 数据不兼容
		// （README 已声明升级 = 全新建），不迁移接受。
		field.JSON("protocol_convert", []string{}).Default([]string{}),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional().Nillable(), // 软删除时间戳（nil = 存活）；null 语义 = 未删除
		field.Time("created_at").Default(time.Now),
	}
}

func (Group) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("accounts", Account.Type).Ref("groups"),
		edge.To("keys", Key.Type),
		edge.To("assignments", GroupAssignment.Type),
	}
}
