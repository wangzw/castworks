package connectorhub_test

import (
	"os"
	"testing"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
)

// TestMain 在测试结束后删除 NewPoolFromTemplate 创建的模板库，也覆盖同二进制
// 中属于包内测试（package connectorhub）的那些测试。
func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}
