package connectorhub

import (
	"context"
	"net/http"

	"github.com/wangzw/castworks/pkg/connectorhub/extsim"
)

// Connector 是一个外部系统的连接器。通用部分只经它处理设计中“由连接器
// 规范化”的部分：签名、事件类型、结论字段与状态查询。接口只用标准库
// 类型，连接器实现不引用本包，本包按外部系统定义名构造它们。
type Connector interface {
	// VerifySignature 以 key 校验请求头与请求体上的签名。
	VerifySignature(h http.Header, body, key []byte) bool
	// EventType 读取请求携带的规范化事件类型。
	EventType(h http.Header) (string, error)
	// Terminal 从 webhook 负载取出结论字段。
	Terminal(body []byte) (string, error)
	// CheckStatus 按外部系统定义中的 api 与订阅关联键查询外部对象的
	// 当前状态。已到终态时 reached 为真，terminal 取终态取值，body 为
	// 响应体原文；未到终态时 reached 为假且 err 为空；不可达时 err 非空。
	CheckStatus(ctx context.Context, api string, correlation map[string]string) (terminal string, reached bool, body []byte, err error)
}

// extsim 是 v0.2.0 唯一的内置连接器，按外部系统定义名 extsim 构造。
const extsimName = "extsim"

var _ Connector = (*extsim.Connector)(nil)

// connectorFactory 按状态查询基地址构造一个连接器。
type connectorFactory func(apiBaseURL string) Connector

// builtinConnectors 是外部系统定义名到连接器构造函数的表。定义名不在表
// 中时该定义拒绝装载。
var builtinConnectors = map[string]connectorFactory{
	extsimName: func(apiBaseURL string) Connector {
		return extsim.New(apiBaseURL, nil)
	},
}

// newBuiltinConnector 按定义名构造连接器。第二个返回值表示定义名是否有
// 对应的连接器。
func newBuiltinConnector(name, apiBaseURL string) (Connector, bool) {
	factory, ok := builtinConnectors[name]
	if !ok {
		return nil, false
	}
	return factory(apiBaseURL), true
}
