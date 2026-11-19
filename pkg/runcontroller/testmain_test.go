package runcontroller_test

import (
	"os"
	"testing"

	"github.com/wangzw/castworks/internal/testutil/pgtest"
)

// TestMain drops the template databases NewPoolFromTemplate created once the
// tests finish. It covers the internal runcontroller tests too, which share
// this test binary.
func TestMain(m *testing.M) {
	os.Exit(pgtest.Main(m))
}
